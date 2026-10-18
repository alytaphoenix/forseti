// Forseti pi extension (hashtext): fingerprinted context cache.
//
// The agent's window splits into two tiers:
//   HOT  — the normal conversation (system prompt, recent turns, expansions)
//   COLD — blocks stored compressed in the hashtext store and addressed by the
//          sha256 fingerprint of their plaintext
//
// The system prompt carries a one-line-per-block index (ref | name | desc |
// size). The model declares what it needs:
//   /hash list|put|expand|evict|stats|clear
//   model tools: hashtext_put, hashtext_list, hashtext_expand, hashtext_evict
//
// Mechanics: hashtext_expand turns a block resident and leaves a compact
// `[hashtext <ref> resident]` marker in the transcript. Before every LLM call
// the `context` handler replaces resident markers with the decompressed text,
// so the bulk is reconstructed per turn instead of being retained in history.
// hashtext_evict drops residency: the marker stays as a one-line stub and the
// content stops being re-materialized. Residency is per session and survives
// /reload (state file under the store, beside the blocks).
//
// The store itself is `hashtext` (Python CLI, sibling repo): content-addressed,
// zstd-compressed, dedup for free. This extension never writes its own cache
// format — it shells out to the CLI (HASHTEXT_BIN, default "hashtext") and
// keeps an in-memory decompression cache, safe because a block's identity is
// its content hash.
//
// Verified surfaces: pi 1.0.3 `dist/core/extensions/types.d.ts` — `context`
// (messages without system prompt, before every LLM call) and
// `before_agent_start` (`systemPromptOptions.sections` mutations feed the
// re-render; same pattern as the bundled MCP extension). See docs/spikes.md.
import type { ExtensionAPI, ExtensionContext } from "@earendil-works/pi-coding-agent";
import { Type } from "typebox";
import { spawn } from "node:child_process";
import { mkdir, readFile, rename, writeFile } from "node:fs/promises";
import { homedir } from "node:os";
import path from "node:path";

const STORE_DIR = process.env.HASHTEXT_DIR?.trim() || path.join(homedir(), ".hashtext");
const CLI_TIMEOUT_MS = 30_000;
// Inject expansions while context usage stays under this fraction of the
// window; blocks that do not fit are withheld (marker + note) instead of
// silently overflowing the request.
const HARD_CONTEXT_FRACTION = 0.9;
// Index lines in the system prompt; the rest are reachable via hashtext_list.
const MAX_INDEX_LINES = 200;
const SECTION_TAG = "hashtext";

const MARKER_RE = /\[hashtext ([0-9a-f]{4,64}) resident\]/g;
const MARKER_HINT = "[hashtext ";

const INSTALL_HINT =
	`hashtext not found (HASHTEXT_BIN='${process.env.HASHTEXT_BIN ?? "hashtext"}'). ` +
	"Install the CLI from the hashtext repo: `cd ~/repos/hashtext && uv tool install .` " +
	"(or pipx install .), then ensure it is on PATH.";

interface BlockMeta {
	id: string;
	sha256: string;
	name: string;
	description: string;
	kind: string;
	bytes: number;
	stored: number;
	codec: string;
	created: string;
}

interface CliResult {
	ok: boolean;
	stdout: string;
	stderr: string;
	missing: boolean;
}

// -- CLI bridge -------------------------------------------------------------

let resolvedBin: string | null = null;

function binCandidates(): string[] {
	return [
		process.env.HASHTEXT_BIN?.trim(),
		"hashtext",
		path.join(homedir(), ".local", "bin", "hashtext"),
		"/opt/homebrew/bin/hashtext",
		"/usr/local/bin/hashtext",
	].filter((candidate): candidate is string => Boolean(candidate));
}

function runBin(bin: string, args: string[], input?: string): Promise<CliResult> {
	return new Promise((resolve) => {
		const child = spawn(bin, args, {
			env: { ...process.env, HASHTEXT_DIR: STORE_DIR },
			stdio: ["pipe", "pipe", "pipe"],
		});
		const out: Buffer[] = [];
		const err: Buffer[] = [];
		let settled = false;
		let timedOut = false;
		const timer = setTimeout(() => {
			timedOut = true;
			child.kill("SIGKILL");
		}, CLI_TIMEOUT_MS);

		const finish = (result: CliResult): void => {
			if (settled) return;
			settled = true;
			clearTimeout(timer);
			resolve(result);
		};

		child.on("error", (error: NodeJS.ErrnoException) => {
			finish({
				ok: false,
				stdout: "",
				stderr: error.message,
				missing: error.code === "ENOENT",
			});
		});
		child.stdout.on("data", (chunk: Buffer) => out.push(chunk));
		child.stderr.on("data", (chunk: Buffer) => err.push(chunk));
		child.on("close", (code) => {
			const stdout = Buffer.concat(out).toString("utf8");
			const stderr = Buffer.concat(err).toString("utf8");
			finish({
				ok: code === 0 && !timedOut,
				stdout,
				stderr: timedOut ? `hashtext timed out after ${CLI_TIMEOUT_MS}ms` : stderr,
				missing: false,
			});
		});

		child.stdin.end(input ?? "");
	});
}

/**
 * Run the hashtext CLI, falling back through common install locations when the
 * first candidate is not on PATH. The resolved binary is remembered.
 */
async function runHashtext(args: string[], input?: string): Promise<CliResult> {
	const candidates = resolvedBin ? [resolvedBin] : binCandidates();
	let last: CliResult | null = null;
	for (const bin of candidates) {
		const result = await runBin(bin, args, input);
		if (!result.missing) {
			resolvedBin = bin;
			return result;
		}
		last = result;
	}
	return last ?? { ok: false, stdout: "", stderr: "no hashtext binary candidate", missing: true };
}

async function cliJson<T>(args: string[], input?: string): Promise<T> {
	const result = await runHashtext(args, input);
	if (!result.ok) {
		throw new Error(result.missing ? INSTALL_HINT : result.stderr.trim() || result.stdout.trim() || `hashtext ${args[0]} failed`);
	}
	try {
		return JSON.parse(result.stdout) as T;
	} catch {
		throw new Error(`hashtext ${args[0]}: unreadable JSON output`);
	}
}

// -- state ------------------------------------------------------------------

let blocks: BlockMeta[] = [];
let bySha = new Map<string, BlockMeta>();
let byId = new Map<string, BlockMeta>();
let blocksFetchedAt = 0;
/** Resident blocks in expansion order (full sha256). Order drives budgeting. */
let resident: string[] = [];
let residentSet = new Set<string>();
let residentLoadedForKey: string | null = null;
/** Decompressed block text; blocks are immutable, so this never goes stale. */
const contentCache = new Map<string, string>();
/** False when the store could not be reached — never overwrite residency then. */
let storeOk = true;
let cliWarned = false;

function setBlocks(metas: BlockMeta[]): void {
	blocks = metas;
	bySha = new Map(metas.map((meta) => [meta.sha256, meta]));
	byId = new Map(metas.map((meta) => [meta.id, meta]));
	blocksFetchedAt = Date.now();
}

async function refreshBlocks(force = false): Promise<BlockMeta[]> {
	if (!force && Date.now() - blocksFetchedAt < 5_000) return blocks;
	const payload = await cliJson<{ blocks: BlockMeta[] }>(["list", "--json"]);
	setBlocks(payload.blocks ?? []);
	return blocks;
}

/** Resolve a full sha256, a short id, or any unambiguous prefix. */
function resolveRef(ref: string): BlockMeta | undefined {
	const clean = ref.trim().toLowerCase().replace(/^#/, "");
	if (!clean) return undefined;
	const exact = bySha.get(clean) ?? byId.get(clean);
	if (exact) return exact;
	const matches = blocks.filter((meta) => meta.sha256.startsWith(clean) || meta.id.startsWith(clean));
	return matches.length === 1 ? matches[0] : undefined;
}

function markerFor(meta: BlockMeta): string {
	return `[hashtext ${meta.id} resident]`;
}

function expandedFor(meta: BlockMeta, text: string): string {
	const label = meta.name ? ` "${meta.name}"` : "";
	const body = text.endsWith("\n") ? text : `${text}\n`;
	return `[hashtext ${meta.id}${label} — expanded]\n${body}[/hashtext ${meta.id}]`;
}

function withheldNote(meta: BlockMeta, percent: number): string {
	return `[hashtext ${meta.id} withheld — context at ${percent}%; evict other blocks or compact to bring it in]`;
}

function estimatedTokens(text: string): number {
	// Matches the store's default counter: ~4 chars/token. Only the order of
	// magnitude matters for the budget guard.
	return Math.ceil(text.length / 4);
}

function sessionKey(ctx: ExtensionContext): string {
	try {
		const id = ctx.sessionManager.getSessionId();
		if (id) return id;
	} catch {
		// session manager unavailable in some modes
	}
	return `cwd-${ctx.cwd}`;
}

function residentPath(key: string): string {
	const slug = key.replace(/[^a-zA-Z0-9_.-]/g, "-").slice(0, 120) || "default";
	return path.join(STORE_DIR, "resident", `${slug}.json`);
}

async function loadResident(ctx: ExtensionContext): Promise<void> {
	const key = sessionKey(ctx);
	if (residentLoadedForKey === key) return;
	residentLoadedForKey = key;
	try {
		const raw = JSON.parse(await readFile(residentPath(key), "utf8")) as { refs?: unknown };
		resident = Array.isArray(raw.refs) ? raw.refs.filter((ref): ref is string => typeof ref === "string") : [];
	} catch {
		resident = []; // fresh session or unreadable state: start cold
	}
	residentSet = new Set(resident);
	try {
		await refreshBlocks();
		storeOk = true;
	} catch {
		// Store unreachable: keep the refs we read and never overwrite the
		// state file — a transient PATH problem must not erase residency.
		storeOk = false;
		return;
	}
	// Drop refs whose blocks were removed from the store underneath us.
	const known = new Set(blocks.map((meta) => meta.sha256));
	resident = resident.filter((sha) => known.has(sha));
	residentSet = new Set(resident);
}

async function saveResident(ctx: ExtensionContext): Promise<void> {
	if (!storeOk) return;
	const key = sessionKey(ctx);
	const file = residentPath(key);
	await mkdir(path.dirname(file), { recursive: true });
	const tmp = `${file}.tmp-${process.pid}`;
	await writeFile(tmp, `${JSON.stringify({ refs: resident, updated: new Date().toISOString() }, null, 2)}\n`, "utf8");
	await rename(tmp, file);
}

// -- per-turn reconstruction ------------------------------------------------

function collectMarkers(text: string, refs: Set<string>): void {
	if (!text.includes(MARKER_HINT)) return;
	for (const match of text.matchAll(MARKER_RE)) refs.add(match[1]);
}

function collectFromMessages(messages: unknown[]): Set<string> {
	const refs = new Set<string>();
	const walk = (value: unknown, key?: string): void => {
		if (typeof value === "string") {
			if (key === "text" || key === "content") collectMarkers(value, refs);
			return;
		}
		if (Array.isArray(value)) {
			for (const item of value) walk(item);
			return;
		}
		if (value && typeof value === "object") {
			for (const [childKey, child] of Object.entries(value)) walk(child, childKey);
		}
	};
	for (const message of messages) walk(message);
	return refs;
}

/** Replaces markers in text-bearing strings, reusing unchanged sub-objects. */
function replaceInText(value: unknown, key: string | undefined, fn: (text: string) => string): [unknown, boolean] {
	if (typeof value === "string") {
		if (key !== "text" && key !== "content") return [value, false];
		const next = fn(value);
		return next === value ? [value, false] : [next, true];
	}
	if (Array.isArray(value)) {
		let changed = false;
		const out = value.map((item) => {
			const [next, itemChanged] = replaceInText(item, undefined, fn);
			changed = changed || itemChanged;
			return next;
		});
		return changed ? [out, true] : [value, false];
	}
	if (value && typeof value === "object") {
		let changed = false;
		const out: Record<string, unknown> = {};
		for (const [childKey, child] of Object.entries(value)) {
			const [next, childChanged] = replaceInText(child, childKey, fn);
			changed = changed || childChanged;
			out[childKey] = next;
		}
		return changed ? [out, true] : [value, false];
	}
	return [value, false];
}

/**
 * Load the content for every resident block that appears in the transcript.
 * One batched `show` call per turn on a cache miss; immutable blocks make the
 * cache permanent for the session.
 */
async function ensureContents(metas: BlockMeta[]): Promise<void> {
	const missing = metas.filter((meta) => !contentCache.has(meta.sha256));
	if (missing.length === 0) return;
	const payload = await cliJson<{ blocks: Array<BlockMeta & { text: string }> }>([
		"show",
		...missing.map((meta) => meta.sha256),
		"--json",
	]);
	for (const block of payload.blocks ?? []) {
		if (typeof block.text === "string") contentCache.set(block.sha256, block.text);
	}
}

async function reconstruct(messages: unknown[], ctx: ExtensionContext): Promise<unknown[]> {
	const markerRefs = collectFromMessages(messages);
	if (markerRefs.size === 0) return messages;

	const wanted = new Map<string, BlockMeta>();
	for (const ref of markerRefs) {
		const meta = resolveRef(ref);
		if (meta && residentSet.has(meta.sha256)) wanted.set(meta.sha256, meta);
	}
	if (wanted.size === 0) return messages;

	try {
		await ensureContents([...wanted.values()]);
	} catch (error) {
		if (!cliWarned) {
			cliWarned = true;
			ctx.ui.notify(`forseti: hashtext expansion failed — ${error instanceof Error ? error.message : String(error)}`, "warning");
		}
		return messages;
	}

	// Budget: expansions are injected in residency order while they fit under
	// the context ceiling. Unknown usage (before the first response) injects all.
	const usage = ctx.getContextUsage?.();
	const usedTokens = usage?.tokens;
	const windowTokens = usage?.contextWindow ?? 0;
	const percent = usage?.percent ?? 0;
	let budget = Number.POSITIVE_INFINITY;
	if (typeof usedTokens === "number" && windowTokens > 0) {
		budget = Math.floor(windowTokens * HARD_CONTEXT_FRACTION) - usedTokens;
	}

	const allow = new Set<string>();
	const withheld = new Set<string>();
	let spent = 0;
	for (const sha of resident) {
		if (!wanted.has(sha)) continue;
		const text = contentCache.get(sha);
		if (text === undefined) continue;
		const cost = estimatedTokens(text);
		if (spent + cost <= budget) {
			allow.add(sha);
			spent += cost;
		} else {
			withheld.add(sha);
		}
	}

	const substitute = (text: string): string => {
		if (!text.includes(MARKER_HINT)) return text;
		return text.replace(MARKER_RE, (whole, ref: string) => {
			const meta = resolveRef(ref);
			if (!meta || !residentSet.has(meta.sha256)) return whole;
			const content = contentCache.get(meta.sha256);
			if (content === undefined) return whole;
			if (allow.has(meta.sha256)) return expandedFor(meta, content);
			if (withheld.has(meta.sha256)) return withheldNote(meta, Math.round(percent));
			return whole;
		});
	};

	let changed = false;
	const out = messages.map((message) => {
		const [next, messageChanged] = replaceInText(message, undefined, substitute);
		changed = changed || messageChanged;
		return next;
	});
	return changed ? out : messages;
}

// -- index rendering --------------------------------------------------------

function humanBytes(bytes: number): string {
	if (bytes < 1024) return `${bytes}B`;
	let value = bytes;
	for (const unit of ["KB", "MB", "GB"]) {
		value /= 1024;
		if (value < 1024 || unit === "GB") return `${value.toFixed(1)}${unit}`;
	}
	return `${value.toFixed(1)}GB`;
}

function indexLine(meta: BlockMeta): string {
	const residentFlag = residentSet.has(meta.sha256) ? " [resident]" : "";
	return `${meta.id} | ${meta.name || "(unnamed)"} | ${meta.description || "-"} | ${humanBytes(meta.bytes)}→${humanBytes(meta.stored)}${residentFlag}`;
}

function renderIndexSection(): string {
	if (blocks.length === 0) return "";
	const shown = blocks.slice(0, MAX_INDEX_LINES);
	const hidden = blocks.length - shown.length;
	const lines = [
		`Cold context cache — ${blocks.length} block(s) stored fingerprinted and compressed, not in context.`,
		...shown.map(indexLine),
	];
	if (hidden > 0) lines.push(`… ${hidden} more (use hashtext_list)`);
	lines.push(
		"Expand a block with hashtext_expand when the task needs its contents; evict with hashtext_evict when done. " +
			"Expanded blocks are re-materialized from the store on every model call, so the transcript only keeps a marker.",
	);
	return lines.join("\n");
}

// -- command helpers --------------------------------------------------------

async function expandRef(ref: string, ctx: ExtensionContext): Promise<{ ok: boolean; text: string }> {
	await refreshBlocks().catch(() => blocks);
	const meta = resolveRef(ref);
	if (!meta) return { ok: false, text: `no block matching '${ref}'` };
	try {
		await ensureContents([meta]);
	} catch (error) {
		return { ok: false, text: error instanceof Error ? error.message : String(error) };
	}
	if (!residentSet.has(meta.sha256)) {
		resident.push(meta.sha256);
		residentSet.add(meta.sha256);
		await saveResident(ctx).catch(() => {});
	}
	return { ok: true, text: `${markerFor(meta)}\nexpanded ${meta.id} (${meta.name || "unnamed"}) — content is reconstructed into context from the next model call.` };
}

async function evictRef(ref: string): Promise<{ ok: boolean; text: string }> {
	await refreshBlocks().catch(() => blocks);
	const meta = resolveRef(ref);
	if (!meta) return { ok: false, text: `no block matching '${ref}'` };
	if (!residentSet.has(meta.sha256)) return { ok: true, text: `${meta.id} (${meta.name || "unnamed"}) is already cold` };
	resident = resident.filter((sha) => sha !== meta.sha256);
	residentSet.delete(meta.sha256);
	return { ok: true, text: `evicted ${meta.id} (${meta.name || "unnamed"}) — it stays in the cold index; the transcript keeps a one-line marker` };
}

function installFailure(ctx: ExtensionContext, error: unknown): { content: Array<{ type: "text"; text: string }>; isError: true } {
	const message = error instanceof Error ? error.message : String(error);
	if (!cliWarned) {
		cliWarned = true;
		ctx.ui.notify(`forseti: ${message}`, "warning");
	}
	return { content: [{ type: "text", text: message }], isError: true };
}

export default function forsetiHashtext(pi: ExtensionAPI) {
	// ---- system prompt: the cold index --------------------------------------
	pi.on("before_agent_start", async (event, ctx) => {
		try {
			await loadResident(ctx);
			await refreshBlocks();
		} catch (error) {
			if (!cliWarned) {
				cliWarned = true;
				ctx.ui.notify(`forseti: hashtext unavailable — ${error instanceof Error ? error.message : String(error)}`, "warning");
			}
		}
		const section = renderIndexSection();
		if (section) event.systemPromptOptions.sections[SECTION_TAG] = section;
		else delete event.systemPromptOptions.sections[SECTION_TAG];
	});

	// ---- per-turn reconstruction: markers -> content -------------------------
	pi.on("context", async (event, ctx) => {
		try {
			await loadResident(ctx);
			const messages = await reconstruct(event.messages, ctx);
			return messages === event.messages ? undefined : { messages: messages as typeof event.messages };
		} catch {
			return undefined; // never break a turn on cache trouble
		}
	});

	// ---- persistence across reload/resume -----------------------------------
	pi.on("session_start", async (_event, ctx) => {
		residentLoadedForKey = null; // force a reload for the new/loaded session
		await loadResident(ctx).catch(() => {});
	});
	pi.on("session_shutdown", async (_event, ctx) => {
		await saveResident(ctx).catch(() => {});
	});

	// ---- commands ------------------------------------------------------------
	pi.registerCommand("hash", {
		description: "Forseti: fingerprinted context cache (list|put|expand|evict|stats|clear)",
		getArgumentCompletions: (prefix) => {
			const items = ["list", "put", "expand", "evict", "stats", "clear"];
			return items.filter((item) => item.startsWith(prefix)).map((value) => ({ value, label: value }));
		},
		handler: async (args, ctx) => {
			const parts = args.trim().split(/\s+/).filter(Boolean);
			const sub = (parts[0] ?? "list").toLowerCase();
			try {
				await loadResident(ctx);
				if (sub === "list") {
					await refreshBlocks(true);
					ctx.ui.notify(renderIndexSection() || "hashtext: no blocks", "info");
					return;
				}
				if (sub === "stats") {
					const payload = await cliJson<{ stats: Record<string, unknown> }>(["stats", "--json"]);
					const stats = payload.stats;
					ctx.ui.notify(
						`hashtext store ${stats.root}\nblocks: ${stats.blocks} · original ${humanBytes(Number(stats.bytes))} · stored ${humanBytes(Number(stats.stored))} (${Math.round(Number(stats.ratio) * 100)}%)` +
							`\nresident in this session: ${resident.length}`,
						"info",
					);
					return;
				}
				if (sub === "put") {
					const file = parts[1];
					if (!file) {
						ctx.ui.notify("hashtext: /hash put <file> [name]", "warning");
						return;
					}
					const text = await readFile(file, "utf8");
					const name = parts[2] ?? path.basename(file);
					const payload = await cliJson<{ block: BlockMeta; deduped: boolean }>(
						["put", "-", "--name", name, "--json"],
						text,
					);
					await refreshBlocks(true);
					ctx.ui.notify(
						`hashtext: ${payload.deduped ? "deduped" : "stored"} ${payload.block.id} (${name}) ${humanBytes(payload.block.bytes)}→${humanBytes(payload.block.stored)}`,
						"info",
					);
					return;
				}
				if (sub === "expand" || sub === "evict") {
					const ref = parts[1];
					if (!ref) {
						ctx.ui.notify(`hashtext: /hash ${sub} <ref>`, "warning");
						return;
					}
					const result = sub === "expand" ? await expandRef(ref, ctx) : await evictRef(ref);
					if (result.ok) await saveResident(ctx).catch(() => {});
					ctx.ui.notify(result.text, result.ok ? "info" : "error");
					return;
				}
				if (sub === "clear") {
					resident = [];
					residentSet = new Set();
					await saveResident(ctx).catch(() => {});
					ctx.ui.notify("hashtext: evicted every resident block", "info");
					return;
				}
				ctx.ui.notify("hashtext: /hash list · put <file> [name] · expand <ref> · evict <ref> · stats · clear", "info");
			} catch (error) {
				ctx.ui.notify(`forseti: ${error instanceof Error ? error.message : String(error)}`, "error");
			}
		},
	});

	// ---- model tools ---------------------------------------------------------
	pi.registerTool({
		name: "hashtext_put",
		label: "hashtext_put",
		description:
			"Store text in the fingerprinted context cache as a cold block. Use for material the task may need later but does not need in context now (large logs, research notes, file snapshots, prior outputs). Returns the block ref; the block appears in the cold index and is expanded on demand.",
		promptSnippet: "hashtext_put: stash text as a compressed, fingerprinted context block.",
		parameters: Type.Object({
			text: Type.String({ description: "The full text to store" }),
			name: Type.Optional(Type.String({ description: "Short label for the index (e.g. 'ci log 2026-10-18')" })),
			description: Type.Optional(Type.String({ description: "One-line description of what it holds" })),
			kind: Type.Optional(Type.String({ description: "Block kind: note (default) | log | doc | code | data" })),
		}),
		async execute(_toolCallId, params, _signal, _onUpdate, ctx) {
			try {
				const args = ["put", "-", "--json"];
				if (params.name) args.push("--name", params.name);
				if (params.description) args.push("--desc", params.description);
				if (params.kind) args.push("--kind", params.kind);
				const payload = await cliJson<{ block: BlockMeta; deduped: boolean }>(args, params.text);
				await refreshBlocks(true);
				const verb = payload.deduped ? "deduped to" : "stored";
				return {
					content: [{
						type: "text",
						text: `${verb} ${payload.block.id} (${payload.block.name || "unnamed"}) ${humanBytes(payload.block.bytes)}→${humanBytes(payload.block.stored)}. Cold — expand with hashtext_expand when needed.`,
					}],
					details: { ok: true, ref: payload.block.id, deduped: payload.deduped },
				};
			} catch (error) {
				return installFailure(ctx, error);
			}
		},
	});

	pi.registerTool({
		name: "hashtext_list",
		label: "hashtext_list",
		description:
			"List the cold context cache: ref, name, description, original→stored size, and whether the block is resident in this session. Use when you need to find a block by topic, or to check what is currently expanded.",
		promptSnippet: "hashtext_list: show the cold block index and residency.",
		parameters: Type.Object({}),
		async execute(_toolCallId, _params, _signal, _onUpdate, ctx) {
			try {
				await loadResident(ctx);
				await refreshBlocks(true);
				const section = renderIndexSection();
				return { content: [{ type: "text", text: section || "no blocks" }], details: { ok: true, blocks: blocks.length, resident: resident.length } };
			} catch (error) {
				return installFailure(ctx, error);
			}
		},
	});

	pi.registerTool({
		name: "hashtext_expand",
		label: "hashtext_expand",
		description:
			"Expand a cold block so its contents are reconstructed into context on the next model call. The transcript keeps only a one-line marker, so the bulk is not retained across turns. Expand only what the current task needs.",
		promptSnippet: "hashtext_expand: bring a cold block's contents into context.",
		promptGuidelines: ["Expand a cold block only when the current task needs its contents, and evict blocks you have finished with."],
		parameters: Type.Object({
			ref: Type.String({ description: "Block ref from the index (full fingerprint or an unambiguous prefix)" }),
		}),
		async execute(_toolCallId, params, _signal, _onUpdate, ctx) {
			await loadResident(ctx).catch(() => {});
			const result = await expandRef(params.ref, ctx);
			if (!result.ok) return { content: [{ type: "text", text: result.text }], isError: true };
			return { content: [{ type: "text", text: result.text }], details: { ok: true } };
		},
	});

	pi.registerTool({
		name: "hashtext_evict",
		label: "hashtext_evict",
		description:
			"Evict a resident block: its contents stop being reconstructed into context, leaving a one-line marker. Use when a block is no longer relevant, to keep the context window small.",
		promptSnippet: "hashtext_evict: drop a block from the hot context set.",
		parameters: Type.Object({
			ref: Type.String({ description: "Block ref (full fingerprint or an unambiguous prefix)" }),
		}),
		async execute(_toolCallId, params, _signal, _onUpdate, ctx) {
			await loadResident(ctx).catch(() => {});
			const result = await evictRef(params.ref);
			if (result.ok) await saveResident(ctx).catch(() => {});
			return result.ok
				? { content: [{ type: "text", text: result.text }], details: { ok: true } }
				: { content: [{ type: "text", text: result.text }], isError: true };
		},
	});
}
