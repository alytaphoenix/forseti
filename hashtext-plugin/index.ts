// Forseti pi extension (hashtext): fingerprinted context cache.
//
// The agent's window splits into two tiers:
//   HOT  — the normal conversation (system prompt, recent turns, expansions)
//   COLD — blocks stored compressed in the hashtext store and addressed by the
//          sha256 fingerprint of their plaintext
//
// The system prompt carries a COMPACT index: a single line for the resident
// set, the most recent cold blocks, and a count for the rest (reachable with
// hashtext_list / hashtext_find). Cold blocks are expanded only when the task
// needs them, and expansion is progressive: large blocks arrive as an outline
// or a query-focused excerpt, and the model pulls line ranges on demand.
//
//   /hash list|find|put|expand|evict|read|grep|stats|gc|archive|auto|clear
//   tools: hashtext_put, hashtext_list, hashtext_find, hashtext_expand,
//          hashtext_evict, hashtext_read, hashtext_grep
//
// Mechanics: hashtext_expand turns a block resident and leaves a compact
// `[hashtext <ref> resident]` marker in the transcript. Before every LLM call
// the `context` handler replaces resident markers with the block's view
// (outline / focus / full), so the bulk is reconstructed per turn and is never
// retained in history. A second marker for the same block becomes a back
// reference, so content is materialized once per request.
//
// The store is read NATIVELY (metadata JSON + Node's zstd + sha256 verify),
// which keeps per-prompt overhead at ~5 ms instead of a ~38 ms CLI spawn and
// lets the extension grep/range-read across blocks. Writes still go through
// the `hashtext` CLI (single format owner); reads fall back to it if native
// decoding ever fails.
//
// Hardening: labels are sanitized before they reach the prompt, expansion
// wrappers carry a per-session nonce (unescaped closing tags in block content
// cannot terminate them early), refs distinguish "not found" from "ambiguous",
// residency is capped, and every cache failure is fail-open.
//
// Verified surfaces: pi 1.0.3 `dist/core/extensions/types.d.ts` — `context`
// (messages without system prompt, before every LLM call), `before_agent_start`
// (`systemPromptOptions.sections` mutations feed the re-render), `turn_end`
// (`toolResults` for archival). Node 22.22 zstd: `zstdDecompressSync` incl.
// `{dictionary}`. See docs/spikes.md (S41/S44). Telemetry, picker eval and
// isotonic calibration: `scripts/hashtext-{dataset,eval,calibrate}.ts` (S45/S46).
import type { ExtensionAPI, ExtensionContext } from "@earendil-works/pi-coding-agent";
import { Type } from "typebox";
import { spawn } from "node:child_process";
import { createHash, randomBytes } from "node:crypto";
import { mkdir, readFile, readdir, rename, stat, writeFile } from "node:fs/promises";
import { homedir } from "node:os";
import path from "node:path";
import { zstdDecompressSync } from "node:zlib";

const STORE_DIR = process.env.HASHTEXT_DIR?.trim() || path.join(homedir(), ".hashtext");
const WRITE_TIMEOUT_MS = 30_000;
const READ_TIMEOUT_MS = 5_000;
// Standing prompt cost: resident line + this many recent blocks + a count.
const MAX_INDEX_LINES = 20;
const MAX_RESIDENT = 8;
// Progressive disclosure: above this size an `auto` expansion returns an
// outline instead of the full text.
const AUTO_OUTLINE_CHARS = 6_000;
const FOCUS_MAX_CHARS = 3_000;
const OUTLINE_MAX_ENTRIES = 30;
const READ_MAX_LINES = 400;
const GREP_MAX_MATCHES = 50;
// Request-time archival of bulky tool output (cold by default, opt-out).
const ARCHIVE_CHARS = 8_000;
const ARCHIVE_MAX_PER_TURN = 4;
// Inject expansions while context usage stays under this fraction of the
// window; blocks that do not fit are withheld with a visible note.
const HARD_CONTEXT_FRACTION = 0.9;
const SECTION_TAG = "hashtext";

const LAYA_URL = (process.env.FORSETI_LAYA_URL || "http://127.0.0.1:8751").replace(/\/$/, "");
const LAYA_TIMEOUT_MS = 3_000;
const LAYA_SUGGEST_CONFIDENCE = 0.6;
const LAYA_AUTO_CONFIDENCE = 0.85;
const LAYA_EVICT_CONFIDENCE = 0.8;

const MARKER_RE = /\[hashtext ([0-9a-f]{4,64}) resident\]/g;
const MARKER_HINT = "[hashtext ";
const NONCE = randomBytes(3).toString("hex");
const DECISIONS_FILE = path.join(STORE_DIR, "decisions.jsonl");
const DECISIONS_MAX_BYTES = 8 * 1024 * 1024;
const CALIBRATION_FILE = path.join(STORE_DIR, "calibration.json");

const INSTALL_HINT =
	`hashtext not found (HASHTEXT_BIN='${process.env.HASHTEXT_BIN ?? "hashtext"}'). ` +
	"Install the CLI from the hashtext repo: `cd ~/repos/hashtext && uv tool install .` " +
	"(or pipx install .), then ensure it is on PATH. Reads work without it; writes need it.";

type ExpandMode = "auto" | "full" | "outline" | "focus" | "range";

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
	tokens_est: number;
}

interface ResidentEntry {
	sha: string;
	mode: ExpandMode;
	focus: string;
	start?: number;
	end?: number;
	added: number;
}

interface ArchiveEntry {
	sha: string;
	id: string;
	name: string;
	synopsis: string;
	tool: string;
	added: number;
}

interface Settings {
	archive: boolean;
	auto: boolean;
}

interface CandidateDetail {
	id: string;
	name: string;
	description: string;
	kind: string;
	tokens_est: number;
}

interface PendingPick {
	query: string;
	pick: string;
	confidence: number;
	turn: number;
	candidates: string[];
}

interface SessionState {
	version: 2;
	turn: number;
	blocks: ResidentEntry[];
	archive: Record<string, ArchiveEntry>;
	settings: Settings;
}

// -- CLI bridge (writes; read fallback) -------------------------------------

interface CliResult {
	ok: boolean;
	stdout: string;
	stderr: string;
	missing: boolean;
}

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

function runBin(bin: string, args: string[], input: string | undefined, timeoutMs: number): Promise<CliResult> {
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
		}, timeoutMs);

		const finish = (result: CliResult): void => {
			if (settled) return;
			settled = true;
			clearTimeout(timer);
			resolve(result);
		};

		child.on("error", (error: NodeJS.ErrnoException) => {
			finish({ ok: false, stdout: "", stderr: error.message, missing: error.code === "ENOENT" });
		});
		child.stdout.on("data", (chunk: Buffer) => out.push(chunk));
		child.stderr.on("data", (chunk: Buffer) => err.push(chunk));
		child.on("close", (code) => {
			const stdout = Buffer.concat(out).toString("utf8");
			const stderr = Buffer.concat(err).toString("utf8");
			finish({
				ok: code === 0 && !timedOut,
				stdout,
				stderr: timedOut ? `hashtext timed out after ${timeoutMs}ms` : stderr,
				missing: false,
			});
		});

		child.stdin.end(input ?? "");
	});
}

async function runHashtext(args: string[], input?: string, timeoutMs = WRITE_TIMEOUT_MS): Promise<CliResult> {
	const candidates = resolvedBin ? [resolvedBin] : binCandidates();
	let last: CliResult | null = null;
	for (const bin of candidates) {
		const result = await runBin(bin, args, input, timeoutMs);
		if (!result.missing) {
			resolvedBin = bin;
			return result;
		}
		last = result;
	}
	return last ?? { ok: false, stdout: "", stderr: "no hashtext binary candidate", missing: true };
}

async function cliJson<T>(args: string[], input?: string, timeoutMs = WRITE_TIMEOUT_MS): Promise<T> {
	const result = await runHashtext(args, input, timeoutMs);
	if (!result.ok) {
		throw new Error(
			result.missing
				? INSTALL_HINT
				: result.stderr.trim() || result.stdout.trim() || `hashtext ${args[0]} failed`,
		);
	}
	try {
		return JSON.parse(result.stdout) as T;
	} catch {
		throw new Error(`hashtext ${args[0]}: unreadable JSON output`);
	}
}

// -- native store reads ------------------------------------------------------

const blocksDir = (): string => path.join(STORE_DIR, "blocks");

let metas: BlockMeta[] = [];
let bySha = new Map<string, BlockMeta>();
let byId = new Map<string, BlockMeta>();
let metasAt = 0;
let nativeOk = true;

function setMetas(next: BlockMeta[]): void {
	metas = next;
	bySha = new Map(next.map((meta) => [meta.sha256, meta]));
	byId = new Map(next.map((meta) => [meta.id, meta]));
	metasAt = Date.now();
	contentIndex = null; // block set changed: rebuild lazily
}

export async function refreshMetas(force = false): Promise<BlockMeta[]> {
	if (!force && Date.now() - metasAt < 2_000) return metas;
	try {
		const shards = await readdir(blocksDir());
		const files: string[] = [];
		for (const shard of shards) {
			if (!/^[0-9a-f]{2}$/.test(shard)) continue;
			for (const name of await readdir(path.join(blocksDir(), shard))) {
				if (name.endsWith(".json")) files.push(path.join(blocksDir(), shard, name));
			}
		}
		const loaded: BlockMeta[] = [];
		for (const file of files) {
			try {
				const raw = JSON.parse(await readFile(file, "utf8")) as Partial<BlockMeta>;
				if (!raw.sha256 || !raw.id) continue;
				const bytes = Number(raw.bytes ?? 0);
				loaded.push({
					id: String(raw.id),
					sha256: String(raw.sha256),
					name: String(raw.name ?? ""),
					description: String(raw.description ?? ""),
					kind: String(raw.kind ?? "note"),
					bytes,
					stored: Number(raw.stored ?? 0),
					codec: String(raw.codec ?? "zstd"),
					created: String(raw.created ?? ""),
					tokens_est: Number(raw.tokens_est ?? (bytes ? (bytes + 3) >> 2 : 0)),
				});
			} catch {
				// a torn metadata file is skipped; gc cleans it up
			}
		}
		loaded.sort((a, b) => (a.created === b.created ? (a.id < b.id ? 1 : -1) : a.created < b.created ? 1 : -1));
		setMetas(loaded);
		nativeOk = true;
	} catch {
		// No store yet, or the directory vanished: empty is a valid state.
		setMetas([]);
		nativeOk = false;
	}
	return metas;
}

type RefLookup = { meta: BlockMeta } | { error: "not_found" | "ambiguous"; ref: string; count?: number };

async function lookupRef(ref: string): Promise<RefLookup> {
	const clean = String(ref ?? "").trim().toLowerCase().replace(/^#/, "");
	if (!clean || !/^[0-9a-f]+$/.test(clean)) return { error: "not_found", ref: clean };
	const exact = bySha.get(clean) ?? byId.get(clean);
	if (exact) return { meta: exact };
	const matches = metas.filter((meta) => meta.sha256.startsWith(clean) || meta.id.startsWith(clean));
	if (matches.length === 1) return { meta: matches[0] };
	if (matches.length > 1) return { error: "ambiguous", ref: clean, count: matches.length };
	return { error: "not_found", ref: clean };
}

// Bounded LRU of decompressed text: blocks are immutable, so entries never
// go stale — only cold ones are evicted.
const textCache = new Map<string, string>();
let textCacheBytes = 0;
const TEXT_CACHE_MAX_BYTES = 16 * 1024 * 1024;
const TEXT_CACHE_MAX_ENTRIES = 64;

function cacheText(sha: string, text: string): void {
	const previous = textCache.get(sha);
	if (previous !== undefined) textCacheBytes -= Buffer.byteLength(previous);
	textCache.delete(sha);
	textCache.set(sha, text);
	textCacheBytes += Buffer.byteLength(text);
	while (textCache.size > TEXT_CACHE_MAX_ENTRIES || textCacheBytes > TEXT_CACHE_MAX_BYTES) {
		const oldest = textCache.keys().next();
		if (oldest.done) break;
		const value = textCache.get(oldest.value)!;
		textCache.delete(oldest.value);
		textCacheBytes -= Buffer.byteLength(value);
	}
}

function cachedText(sha: string): string | undefined {
	const value = textCache.get(sha);
	if (value === undefined) return undefined;
	textCache.delete(sha);
	textCache.set(sha, value);
	return value;
}

export function allMetas(): BlockMeta[] {
	return metas;
}

export function setMetasForTests(next: BlockMeta[]): void {
	setMetas(next);
}

export async function loadText(meta: BlockMeta): Promise<string> {
	const cached = cachedText(meta.sha256);
	if (cached !== undefined) return cached;

	if (nativeOk && meta.codec === "zstd") {
		try {
			const blob = await readFile(path.join(blocksDir(), meta.sha256.slice(0, 2), `${meta.sha256}.zst`));
			const data = zstdDecompressSync(blob);
			if (createHash("sha256").update(data).digest("hex") !== meta.sha256) {
				throw new Error("fingerprint mismatch");
			}
			const text = data.toString("utf8");
			cacheText(meta.sha256, text);
			return text;
		} catch {
			// Fall through to the CLI; a corrupt payload then fails loudly there.
		}
	}

	const payload = await cliJson<{ blocks: Array<{ text: string }> }>(
		["show", meta.sha256, "--json"],
		undefined,
		READ_TIMEOUT_MS,
	);
	const text = payload.blocks?.[0]?.text;
	if (typeof text !== "string") throw new Error(`hashtext show returned no text for ${meta.id}`);
	cacheText(meta.sha256, text);
	return text;
}

// -- state -------------------------------------------------------------------

let turn = 0;
let resident: ResidentEntry[] = [];
let residentSet = new Set<string>();
let archive = new Map<string, ArchiveEntry>();
let settings: Settings = { archive: true, auto: false };
let loadedKey: string | null = null;
let storeOk = true;
let cliWarned = false;
let layaWarned = false;
/** Last Laya pick/auto-expand, so the next expand/evict can be attributed. */
let pendingPick: PendingPick | null = null;

// Consumer-side confidence calibration: isotonic points fitted from labelled
// decisions (`scripts/hashtext-calibrate.ts`). Applies to the gating number
// (`answer_confidence`) only; argmax is unaffected.
interface CalibrationPoint {
	x: number;
	y: number;
}
let calibration: CalibrationPoint[] | null = null;
let calibrationMtime = 0;
let calibrationCheckedAt = 0;

async function loadCalibration(): Promise<void> {
	if (Date.now() - calibrationCheckedAt < 5_000) return;
	calibrationCheckedAt = Date.now();
	try {
		const info = await stat(CALIBRATION_FILE);
		if (info.mtimeMs === calibrationMtime) return;
		const payload = JSON.parse(await readFile(CALIBRATION_FILE, "utf8")) as {
			points?: Array<[number, number]>;
		};
		const points = (payload.points ?? [])
			.filter(
				(point): point is [number, number] =>
					Array.isArray(point) &&
					point.length === 2 &&
					Number.isFinite(point[0]) &&
					Number.isFinite(point[1]),
			)
			.map(([x, y]) => ({
				x: Math.min(1, Math.max(0, Number(x))),
				y: Math.min(1, Math.max(0, Number(y))),
			}))
			.sort((a, b) => a.x - b.x);
		calibration = points.length >= 2 ? points : null;
		calibrationMtime = info.mtimeMs;
	} catch {
		calibration = null;
		calibrationMtime = 0;
	}
}

export function calibrateConfidence(raw: number): number {
	if (!calibration || calibration.length < 2) return raw;
	const x = Math.min(1, Math.max(0, raw));
	if (x <= calibration[0].x) return calibration[0].y;
	for (let i = 1; i < calibration.length; i++) {
		const a = calibration[i - 1];
		const b = calibration[i];
		if (x <= b.x) {
			const span = b.x - a.x || 1;
			return a.y + ((x - a.x) / span) * (b.y - a.y);
		}
	}
	return calibration[calibration.length - 1].y;
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

function statePath(key: string): string {
	const slug = key.replace(/[^a-zA-Z0-9_.-]/g, "-").slice(0, 120) || "default";
	return path.join(STORE_DIR, "resident", `${slug}.json`);
}

function setResident(entries: ResidentEntry[]): void {
	resident = entries;
	residentSet = new Set(entries.map((entry) => entry.sha));
}

async function loadState(ctx: ExtensionContext): Promise<void> {
	const key = sessionKey(ctx);
	if (loadedKey === key) return;
	loadedKey = key;
	let saved: Partial<SessionState> & { refs?: unknown } = {};
	try {
		saved = JSON.parse(await readFile(statePath(key), "utf8")) as typeof saved;
	} catch {
		saved = {};
	}
	const blocks: ResidentEntry[] = Array.isArray(saved.blocks)
		? saved.blocks
				.filter((entry): entry is ResidentEntry => Boolean(entry && typeof entry.sha === "string"))
				.map((entry) => ({
					sha: entry.sha,
					mode: (entry.mode ?? "auto") as ExpandMode,
					focus: typeof entry.focus === "string" ? entry.focus : "",
					added: Number(entry.added ?? 0),
				}))
		: Array.isArray(saved.refs)
			? saved.refs
					.filter((ref): ref is string => typeof ref === "string")
					.map((sha) => ({ sha, mode: "auto" as ExpandMode, focus: "", added: 0 }))
			: [];
	setResident(blocks);
	archive = new Map(
		Object.entries(saved.archive ?? {}).filter(([, entry]) => entry && typeof entry.sha === "string"),
	);
	settings = {
		archive: saved.settings?.archive !== false,
		auto: saved.settings?.auto === true,
	};
	turn = Number(saved.turn ?? 0);

	await loadCalibration().catch(() => {});
	try {
		await refreshMetas(true);
		storeOk = true;
	} catch {
		storeOk = false;
		return;
	}
	// Drop refs whose blocks disappeared; prune empty state.
	const known = new Set(metas.map((meta) => meta.sha256));
	setResident(resident.filter((entry) => known.has(entry.sha)));
	const liveIds = new Set(metas.map((meta) => meta.sha256));
	for (const [callId, entry] of archive) if (!liveIds.has(entry.sha)) archive.delete(callId);
}

async function saveState(ctx: ExtensionContext): Promise<void> {
	if (!storeOk) return;
	const file = statePath(sessionKey(ctx));
	const next: SessionState = {
		version: 2,
		turn,
		blocks: resident,
		archive: Object.fromEntries(archive),
		settings,
	};
	await mkdir(path.dirname(file), { recursive: true });
	// The session owns its state: writes are authoritative. (Unioning the
	// on-disk file here would resurrect blocks that were just evicted.)
	const tmp = `${file}.tmp-${process.pid}`;
	await writeFile(tmp, `${JSON.stringify(next, null, 2)}\n`, "utf8");
	await rename(tmp, file);
}

function currentResident(sha: string): ResidentEntry | undefined {
	return resident.find((entry) => entry.sha === sha);
}

// -- labels / rendering ------------------------------------------------------

function sanitizeLabel(value: string, limit: number): string {
	return String(value ?? "")
		.replace(/[\u0000-\u001f\u007f]+/g, " ")
		.replace(/\s+/g, " ")
		.trim()
		.replace(/[|<>]/g, (char) => (char === "|" ? "/" : char === "<" ? "(" : ")"))
		.slice(0, limit);
}

function displayRef(meta: BlockMeta): string {
	if (meta.id.length <= 8) return meta.id;
	const prefix = meta.id.slice(0, 8);
	const collision = metas.some((other) => other.sha256 !== meta.sha256 && other.sha256.startsWith(prefix));
	return collision ? meta.id : prefix;
}

function fmtTokens(tokens: number): string {
	if (tokens >= 1000) return `${(tokens / 1000).toFixed(tokens >= 10_000 ? 0 : 1)}k`;
	return String(tokens);
}

function indexLine(meta: BlockMeta): string {
	const name = sanitizeLabel(meta.name, 24) || "(unnamed)";
	const desc = sanitizeLabel(meta.description, 60);
	const suffix = desc ? ` — ${desc}` : "";
	return `${displayRef(meta)} ${name}${suffix} · ${fmtTokens(meta.tokens_est)} tok`;
}

function renderIndexSection(): string {
	if (metas.length === 0) return "";
	const residentMetas = resident
		.map((entry) => bySha.get(entry.sha))
		.filter((meta): meta is BlockMeta => Boolean(meta));
	const residentLine = residentMetas.length
		? `resident: ${residentMetas
				.map((meta) => `${displayRef(meta)} ${sanitizeLabel(meta.name, 20) || "(unnamed)"}`)
				.join(", ")}`
		: "";
	const shown = metas.filter((meta) => !residentSet.has(meta.sha256)).slice(0, MAX_INDEX_LINES);
	const hidden = metas.length - residentMetas.length - shown.length;
	const lines = [
		`Cold context cache — ${metas.length} block(s) compressed, not in context. ` +
			`Expand one when the task needs it; large blocks arrive as an outline or a focused excerpt.`,
	];
	if (residentLine) lines.push(residentLine);
	for (const meta of shown) lines.push(indexLine(meta));
	if (hidden > 0) lines.push(`… ${hidden} more — hashtext_list or hashtext_find`);
	lines.push(
		"hashtext_expand (ref, mode=auto|full|outline|focus, focus=\"query\") · hashtext_read (line range) · " +
			"hashtext_evict when done. Expanded blocks are re-materialized each model call; the transcript keeps a marker.",
	);
	return lines.join("\n");
}

// -- views (progressive disclosure) -----------------------------------------

interface View {
	text: string;
	label: string;
}

function clip(text: string, max: number): string {
	if (text.length <= max) return text;
	return `${text.slice(0, max)}\n… (${text.length - max} chars omitted — ask for a range or focus)`;
}

function outlineView(text: string): View {
	const rows = text.split("\n");
	const headings: string[] = [];
	const paragraphs: string[] = [];
	let paragraphStart = 0;
	for (let i = 0; i < rows.length; i++) {
		const row = rows[i];
		if (/^#{1,6}\s+\S/.test(row)) headings.push(`L${i + 1} ${sanitizeLabel(row, 100)}`);
		if (row.trim() === "") {
			if (i > paragraphStart) paragraphs.push(`L${paragraphStart + 1}-${i} ${sanitizeLabel(rows[paragraphStart], 100)}`);
			paragraphStart = i + 1;
		}
	}
	if (paragraphStart < rows.length) {
		paragraphs.push(`L${paragraphStart + 1}-${rows.length} ${sanitizeLabel(rows[paragraphStart], 100)}`);
	}
	const entries = (headings.length >= 3 ? headings : paragraphs).slice(0, OUTLINE_MAX_ENTRIES);
	const total = headings.length >= 3 ? headings.length : paragraphs.length;
	const omitted = total - entries.length;
	return {
		label: `outline (${rows.length} lines)`,
		text: [
			`outline — ${rows.length} lines:`,
			...entries,
			...(omitted > 0 ? [`… ${omitted} more sections`] : []),
			`use hashtext_read(ref, start, end) for a range, or hashtext_expand(ref, focus="...") for relevant excerpts`,
		].join("\n"),
	};
}

function focusView(text: string, query: string): View {
	const terms = [
		...new Set(
			query
				.toLowerCase()
				.split(/[^a-z0-9_]+/)
				.filter((term) => term.length >= 3)
				.slice(0, 8),
		),
	];
	if (terms.length === 0) return outlineView(text);

	const rows = text.split("\n");
	const phrase = query.toLowerCase().trim();
	const scored: Array<{ index: number; score: number }> = [];
	for (let i = 0; i < rows.length; i++) {
		const row = rows[i].toLowerCase();
		let score = 0;
		for (const term of terms) {
			if (row.includes(term)) score += 1;
		}
		if (phrase.length >= 6 && row.includes(phrase)) score += 6;
		if (score > 0) scored.push({ index: i, score });
	}
	if (scored.length === 0) return outlineView(text);

	// Best matching lines, each with two lines of context, merged into ranges.
	scored.sort((a, b) => b.score - a.score || a.index - b.index);
	const ranges: Array<{ start: number; end: number }> = [];
	for (const hit of scored.slice(0, 8)) {
		const from = Math.max(0, hit.index - 2);
		const to = Math.min(rows.length - 1, hit.index + 2);
		const last = ranges[ranges.length - 1];
		if (last && from <= last.end + 1) last.end = Math.max(last.end, to);
		else ranges.push({ start: from, end: to });
	}
	ranges.sort((a, b) => a.start - b.start);

	const sections: string[] = [];
	let cursor = 0;
	for (const range of ranges) {
		if (range.start > cursor) sections.push(`… lines ${cursor + 1}-${range.start} omitted …`);
		sections.push(rows.slice(range.start, range.end + 1).join("\n"));
		cursor = range.end + 1;
	}
	if (cursor < rows.length) sections.push(`… lines ${cursor + 1}-${rows.length} omitted …`);
	const body = clip(sections.join("\n"), FOCUS_MAX_CHARS);
	return {
		label: `focus "${sanitizeLabel(query, 40)}" (${scored.length} matching lines)`,
		text: [`excerpts for "${sanitizeLabel(query, 60)}" — ${rows.length} lines total:`, body].join("\n"),
	};
}

function rangeView(text: string, start?: number, end?: number): View {
	const rows = text.split("\n");
	const lo = Math.max(1, Math.floor(start ?? 1));
	const hi = Math.min(rows.length, Math.floor(end ?? Math.min(rows.length, lo + READ_MAX_LINES - 1)));
	const body = rows.slice(lo - 1, hi).join("\n");
	return {
		label: `lines ${lo}-${hi} of ${rows.length}`,
		text: clip(`lines ${lo}-${hi} of ${rows.length}:\n${body}`, FOCUS_MAX_CHARS * 2),
	};
}

function renderView(
	text: string,
	mode: ExpandMode,
	focus: string,
	start?: number,
	end?: number,
): View {
	const resolved: ExpandMode =
		mode === "auto" ? (text.length > AUTO_OUTLINE_CHARS ? "outline" : "full") : mode;
	if (resolved === "focus") return focusView(text, focus);
	if (resolved === "outline") return outlineView(text);
	if (resolved === "range") return rangeView(text, start, end);
	return { label: "full", text };
}

const viewCache = new Map<string, View>();

function cachedView(meta: BlockMeta, text: string, entry: ResidentEntry): View {
	const key = `${meta.sha256}:${entry.mode}:${entry.focus}:${entry.start ?? ""}:${entry.end ?? ""}`;
	const cached = viewCache.get(key);
	if (cached) return cached;
	const view = renderView(text, entry.mode, entry.focus, entry.start, entry.end);
	viewCache.set(key, view);
	if (viewCache.size > 128) viewCache.delete(viewCache.keys().next().value as string);
	return view;
}

function markerFor(meta: BlockMeta): string {
	return `[hashtext ${meta.id} resident]`;
}

function expandedFor(meta: BlockMeta, view: View): string {
	const name = sanitizeLabel(meta.name, 40);
	const label = name ? ` "${name}"` : "";
	return `[hashtext ${meta.id}${label} — ${view.label} #${NONCE}]\n${view.text}\n[/hashtext ${meta.id} #${NONCE}]`;
}

function withheldNote(meta: BlockMeta, percent: number): string {
	return `[hashtext ${meta.id} withheld — context at ${percent}%; evict other blocks or compact to bring it in]`;
}

function archivedStub(entry: ArchiveEntry, tool: string): string {
	return `[hashtext ${entry.id} archived ${tool} result: ${sanitizeLabel(entry.synopsis, 120)} — hashtext_expand to read]`;
}

// -- expansion / eviction ----------------------------------------------------

function isExpandMode(value: unknown): value is ExpandMode {
	return value === "auto" || value === "full" || value === "outline" || value === "focus" || value === "range";
}

async function expandRef(
	ref: string,
	options: { mode?: ExpandMode; focus?: string; start?: number; end?: number } = {},
	ctx?: ExtensionContext,
): Promise<{ ok: boolean; text: string }> {
	await refreshMetas();
	const found = await lookupRef(ref);
	if ("error" in found) {
		return {
			ok: false,
			text:
				found.error === "ambiguous"
					? `ref '${ref}' matches ${found.count} blocks — use a longer prefix`
					: `no block matching '${ref}'`,
		};
	}
	const meta = found.meta;
	let text: string;
	try {
		text = await loadText(meta);
	} catch (error) {
		return { ok: false, text: error instanceof Error ? error.message : String(error) };
	}

	const mode: ExpandMode = isExpandMode(options.mode) ? options.mode : "auto";
	const entry: ResidentEntry = {
		sha: meta.sha256,
		mode,
		focus: options.focus ?? "",
		start: options.start,
		end: options.end,
		added: turn,
	};
	await logDecision({
		kind: "outcome",
		action: "expand",
		ref: meta.id,
		name: sanitizeLabel(meta.name, 80),
		mode,
		matched_pick: pendingPick ? pendingPick.pick === meta.id : null,
		query: pendingPick?.query ?? null,
		turns_since_pick: pendingPick ? turn - pendingPick.turn : null,
	});
	const existing = currentResident(meta.sha256);
	if (existing) Object.assign(existing, entry);
	else resident.push(entry);
	residentSet.add(meta.sha256);

	const evicted: string[] = [];
	while (resident.length > MAX_RESIDENT) {
		const dropped = resident.shift()!;
		residentSet.delete(dropped.sha);
		const droppedMeta = bySha.get(dropped.sha);
		if (droppedMeta) evicted.push(`${droppedMeta.id} (${sanitizeLabel(droppedMeta.name, 24) || "unnamed"})`);
	}
	if (ctx) await saveState(ctx).catch(() => {});

	const view = renderView(text, mode, entry.focus, options.start, options.end);
	const notes = [
		`expanded ${meta.id} (${sanitizeLabel(meta.name, 40) || "unnamed"}) — ${view.label}, ${fmtTokens(Math.ceil(view.text.length / 4))} tok, reconstructed from the next model call.`,
	];
	if (mode === "auto" && text.length > AUTO_OUTLINE_CHARS) {
		notes.push(`large block: use hashtext_read(${meta.id}, start, end) or hashtext_expand(${meta.id}, focus="...") for details.`);
	}
	if (evicted.length) notes.push(`evicted oldest resident(s) at the cap of ${MAX_RESIDENT}: ${evicted.join(", ")}`);
	return { ok: true, text: `${markerFor(meta)}\n${notes.join("\n")}` };
}

async function evictRef(ref: string): Promise<{ ok: boolean; text: string }> {
	await refreshMetas();
	const found = await lookupRef(ref);
	if ("error" in found) {
		return {
			ok: false,
			text:
				found.error === "ambiguous"
					? `ref '${ref}' matches ${found.count} blocks — use a longer prefix`
					: `no block matching '${ref}'`,
		};
	}
	const meta = found.meta;
	if (!residentSet.has(meta.sha256)) {
		return { ok: true, text: `${meta.id} (${sanitizeLabel(meta.name, 40) || "unnamed"}) is already cold` };
	}
	const entry = currentResident(meta.sha256);
	await logDecision({
		kind: "outcome",
		action: "evict",
		ref: meta.id,
		name: sanitizeLabel(meta.name, 80),
		resident_turns: entry ? turn - entry.added : null,
		matched_pick: pendingPick ? pendingPick.pick === meta.id : null,
	});
	setResident(resident.filter((candidate) => candidate.sha !== meta.sha256));
	return {
		ok: true,
		text: `evicted ${meta.id} (${sanitizeLabel(meta.name, 40) || "unnamed"}) — it stays in the cold index; the transcript keeps a one-line marker`,
	};
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

/** Replace archived tool results with a stub (or marker when resident). */
function projectArchived(messages: unknown[]): { messages: unknown[]; changed: boolean } {
	if (archive.size === 0) return { messages, changed: false };
	let changed = false;
	const out = messages.map((message) => {
		const msg = message as { role?: string; toolCallId?: string; toolName?: string; content?: unknown };
		if (msg?.role !== "toolResult" || !msg.toolCallId) return message;
		const entry = archive.get(msg.toolCallId);
		if (!entry) return message;
		changed = true;
		const text = residentSet.has(entry.sha)
			? markerFor({ id: entry.id } as BlockMeta)
			: archivedStub(entry, msg.toolName ?? entry.tool ?? "tool");
		return { ...(message as object), content: [{ type: "text", text }] };
	});
	return { messages: out, changed };
}

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

async function reconstruct(messages: unknown[], ctx: ExtensionContext): Promise<unknown[]> {
	const projected = projectArchived(messages);
	const markerRefs = collectFromMessages(projected.messages);
	if (markerRefs.size === 0) return projected.messages;

	const wanted = new Map<string, BlockMeta>();
	for (const ref of markerRefs) {
		const found = await lookupRef(ref);
		if ("meta" in found && residentSet.has(found.meta.sha256)) wanted.set(found.meta.sha256, found.meta);
	}
	if (wanted.size === 0) return projected.messages;

	// Load in parallel; a single unreadable block must not sink the request.
	const contents = new Map<string, string>();
	await Promise.all(
		[...wanted.values()].map(async (meta) => {
			try {
				contents.set(meta.sha256, await loadText(meta));
			} catch {
				// left absent: the marker stays literal
			}
		}),
	);

	const usage = ctx.getContextUsage?.();
	const usedTokens = usage?.tokens;
	const windowTokens = usage?.contextWindow ?? 0;
	const percent = usage?.percent ?? 0;
	let budget = Number.POSITIVE_INFINITY;
	if (typeof usedTokens === "number" && windowTokens > 0) {
		budget = Math.floor(windowTokens * HARD_CONTEXT_FRACTION) - usedTokens;
	}

	const allowed = new Set<string>();
	const withheld = new Set<string>();
	let spent = 0;
	for (const entry of resident) {
		const meta = bySha.get(entry.sha);
		const text = contents.get(entry.sha);
		if (!meta || text === undefined) continue;
		const view = cachedView(meta, text, entry);
		const cost = Math.ceil(view.text.length / 4);
		if (spent + cost <= budget) {
			allowed.add(entry.sha);
			spent += cost;
		} else {
			withheld.add(entry.sha);
		}
	}

	const materialized = new Set<string>();
	const substitute = (text: string): string => {
		if (!text.includes(MARKER_HINT)) return text;
		return text.replace(MARKER_RE, (whole, ref: string) => {
			const meta = byId.get(ref) ?? metas.find((candidate) => candidate.sha256.startsWith(ref));
			if (!meta || !residentSet.has(meta.sha256)) return whole;
			const content = contents.get(meta.sha256);
			if (content === undefined) return whole;
			if (withheld.has(meta.sha256)) return withheldNote(meta, Math.round(percent));
			if (!allowed.has(meta.sha256)) return whole;
			if (materialized.has(meta.sha256)) return `[hashtext ${meta.id} resident → see above]`;
			materialized.add(meta.sha256);
			const entry = currentResident(meta.sha256)!;
			return expandedFor(meta, cachedView(meta, content, entry));
		});
	};

	let changed = projected.changed;
	const out = projected.messages.map((message) => {
		const [next, messageChanged] = replaceInText(message, undefined, substitute);
		changed = changed || messageChanged;
		return next;
	});
	return changed ? out : messages;
}

// -- request-time archival ---------------------------------------------------

async function archiveResults(toolResults: unknown[], ctx: ExtensionContext): Promise<void> {
	if (!settings.archive) return;
	if (!storeOk) return;
	let count = 0;
	for (const raw of toolResults) {
		if (count >= ARCHIVE_MAX_PER_TURN) break;
		const result = raw as {
			toolCallId?: string;
			toolName?: string;
			content?: Array<{ type?: string; text?: string }>;
		};
		if (!result?.toolCallId || !result.toolName) continue;
		if (result.toolName.startsWith("hashtext_")) continue;
		if (archive.has(result.toolCallId)) continue;
		const text = (result.content ?? [])
			.filter((part) => part?.type === "text" && typeof part.text === "string")
			.map((part) => part.text as string)
			.join("\n");
		if (text.length < ARCHIVE_CHARS) continue;

		const synopsis = sanitizeLabel(text, 120) || "(no preview)";
		const name = sanitizeLabel(`${result.toolName} output`, 40);
		try {
			const payload = await cliJson<{ block: BlockMeta }>(
				["put", "-", "--name", name, "--desc", synopsis, "--kind", "tool-output", "--json"],
				text,
			);
			archive.set(result.toolCallId, {
				sha: payload.block.sha256,
				id: payload.block.id,
				name,
				synopsis,
				tool: result.toolName,
				added: turn,
			});
			count += 1;
			await refreshMetas(true);
		} catch (error) {
			if (!cliWarned) {
				cliWarned = true;
				ctx.ui.notify(
					`forseti: hashtext archival skipped — ${error instanceof Error ? error.message : String(error)}`,
					"warning",
				);
			}
			return;
		}
	}
	if (count > 0) await saveState(ctx).catch(() => {});
}

// -- Laya (bounded decision over shortlisted blocks) -------------------------

interface LayaPick {
	choice: string;
	/** Calibrated top probability (`answer_confidence`) — the field to gate on. */
	confidence: number;
	/** Normalized entropy (`confidence` in the wire response) — decisiveness, not P(correct). */
	entropy_confidence: number;
	probabilities?: Record<string, unknown>;
	latency_ms: number;
}

export async function layaChoose(
	state: string,
	instructions: string,
	criteria: Record<string, string>,
): Promise<LayaPick | null> {
	const started = performance.now();
	try {
		const res = await fetch(`${LAYA_URL}/v1/systemone`, {
			method: "POST",
			headers: { "Content-Type": "application/json" },
			signal: AbortSignal.timeout(LAYA_TIMEOUT_MS),
			body: JSON.stringify({
				state: { input: state },
				questions: { pick: { type: "choice", instructions, criteria } },
			}),
		});
		if (!res.ok) return null;
		const data = (await res.json()) as {
			answers?: {
				pick?: {
					choice?: string;
					confidence?: number;
					answer_confidence?: number;
					probabilities?: Record<string, unknown>;
				};
			};
		};
		const answer = data.answers?.pick;
		if (!answer?.choice) return null;
		// `confidence` is normalized entropy (decisiveness); `answer_confidence` is
		// max(p), the calibrated quantity the library fits and ECE measures.
		// Gating on entropy silently made the auto threshold unreachable.
		const entropy = Number(answer.confidence ?? 0);
		const calibrated = Number(answer.answer_confidence ?? answer.confidence ?? 0);
		return {
			choice: answer.choice,
			confidence: calibrated,
			entropy_confidence: entropy,
			probabilities: answer.probabilities,
			latency_ms: Math.round(performance.now() - started),
		};
	} catch {
		return null;
	}
}

const STOPWORDS = new Set([
	"the", "and", "for", "with", "that", "this", "from", "what", "when", "where", "which",
	"how", "why", "does", "did", "was", "were", "are", "you", "your", "can", "could", "should",
	"would", "about", "into", "have", "has", "had", "not", "but", "all", "any", "our", "its",
]);

/** term -> indices into `metas`; built lazily when name/description scoring is thin. */
let contentIndex: Map<string, Set<number>> | null = null;
let contentIndexAt = 0;
const CONTENT_INDEX_TTL_MS = 60_000;
const CONTENT_SCAN_BYTES = 512 * 1024;

function contentTokens(text: string): string[] {
	return text
		.toLowerCase()
		.split(/[^a-z0-9_]+/)
		.filter((token) => token.length >= 3 && !STOPWORDS.has(token));
}

async function ensureContentIndex(): Promise<Map<string, Set<number>>> {
	if (contentIndex && Date.now() - contentIndexAt < CONTENT_INDEX_TTL_MS) return contentIndex;
	const index = new Map<string, Set<number>>();
	for (let i = 0; i < metas.length; i++) {
		let text: string;
		try {
			text = await loadText(metas[i]);
		} catch {
			continue;
		}
		if (text.length > CONTENT_SCAN_BYTES) text = text.slice(0, CONTENT_SCAN_BYTES);
		const seen = new Set(contentTokens(text));
		for (const token of seen) {
			let bucket = index.get(token);
			if (!bucket) {
				bucket = new Set();
				index.set(token, bucket);
			}
			bucket.add(i);
		}
	}
	contentIndex = index;
	contentIndexAt = Date.now();
	return index;
}

export async function lexicalCandidates(query: string, limit = 7): Promise<BlockMeta[]> {
	const terms = [
		...new Set(
			query
				.toLowerCase()
				.split(/[^a-z0-9_]+/)
				.filter((term) => term.length >= 3 && !STOPWORDS.has(term))
				.slice(0, 12),
		),
	];
	if (terms.length === 0) return [];
	const phrase = query.toLowerCase().trim();
	const scored = metas
		.filter((meta) => !residentSet.has(meta.sha256))
		.map((meta) => {
			const name = meta.name.toLowerCase();
			const desc = meta.description.toLowerCase();
			const kind = meta.kind.toLowerCase();
			let score = 0;
			for (const term of terms) {
				if (name.includes(term)) score += 4;
				if (desc.includes(term)) score += 3;
				if (kind.includes(term)) score += 1;
			}
			if (phrase.length >= 5 && (name.includes(phrase) || desc.includes(phrase))) score += 5;
			return { meta, score };
		})
		.filter((item) => item.score > 0);

	// Thin metadata match means the query is really about block content: scan
	// the store (native zstd, ~8 ms for 200 blocks) instead of guessing.
	if (scored.length < 3) {
		try {
			const index = await ensureContentIndex();
			const contentScores = new Map<string, number>();
			for (const term of terms) {
				const bucket = index.get(term);
				if (!bucket) continue;
				for (const i of bucket) {
					const meta = metas[i];
					if (!meta || residentSet.has(meta.sha256)) continue;
					contentScores.set(meta.sha256, (contentScores.get(meta.sha256) ?? 0) + 1.5);
				}
			}
			for (const [sha, score] of contentScores) {
				const existing = scored.find((item) => item.meta.sha256 === sha);
				if (existing) existing.score += score;
				else {
					const meta = bySha.get(sha);
					if (meta) scored.push({ meta, score });
				}
			}
		} catch {
			// content scan is best-effort
		}
	}

	scored.sort((a, b) => b.score - a.score || (a.meta.created < b.meta.created ? 1 : -1));
	return scored.slice(0, limit).map((item) => item.meta);
}

function candidateDetails(candidates: BlockMeta[]): CandidateDetail[] {
	return candidates.map((meta) => ({
		id: meta.id,
		name: sanitizeLabel(meta.name, 80),
		description: sanitizeLabel(meta.description, 160),
		kind: meta.kind,
		tokens_est: meta.tokens_est,
	}));
}

export async function findBlocks(query: string): Promise<{ candidates: BlockMeta[]; pick: LayaPick | null }> {
	await refreshMetas();
	const candidates = await lexicalCandidates(query);
	if (candidates.length === 0) return { candidates, pick: null };
	const criteria: Record<string, string> = {
		other: "none of these blocks holds what the request needs",
	};
	for (const meta of candidates) {
		criteria[meta.id] = `${sanitizeLabel(meta.name, 40) || "unnamed"}: ${sanitizeLabel(meta.description, 120) || meta.kind}`;
	}
	const pick = await layaChoose(
		query,
		"Which stored block is most likely to hold what this request needs? Choose 'other' when none fit.",
		criteria,
	);
	// Telemetry: the candidate set + decision is the raw material for tuning and eval.
	await logDecision({
		kind: "find",
		query: query.slice(0, 500),
		candidates: candidateDetails(candidates),
		pick: pick?.choice ?? null,
		confidence: pick?.confidence ?? null,
		entropy_confidence: pick?.entropy_confidence ?? null,
		calibrated: pick ? calibrateConfidence(pick.confidence) : null,
		latency_ms: pick?.latency_ms ?? null,
		probabilities: pick?.probabilities ?? null,
	});
	pendingPick =
		pick && pick.choice !== "other"
			? {
					query: query.slice(0, 500),
					pick: pick.choice,
					confidence: pick.confidence,
					turn,
					candidates: candidates.map((meta) => meta.id),
				}
			: null;
	return { candidates, pick };
}

async function logDecision(entry: Record<string, unknown>): Promise<void> {
	try {
		await mkdir(path.dirname(DECISIONS_FILE), { recursive: true });
		// Keep the log bounded: one rotated file at a time.
		try {
			const info = await stat(DECISIONS_FILE);
			if (info.size > DECISIONS_MAX_BYTES) {
				await rename(DECISIONS_FILE, `${DECISIONS_FILE}.1`);
			}
		} catch {
			// no log yet
		}
		await writeFile(
			DECISIONS_FILE,
			`${JSON.stringify({ ts: new Date().toISOString(), ...entry })}\n`,
			{ encoding: "utf8", flag: "a" },
		);
	} catch {
		// logging is advisory
	}
}

// -- command helpers ---------------------------------------------------------

function installFailure(ctx: ExtensionContext, error: unknown): { content: Array<{ type: "text"; text: string }>; isError: true } {
	const message = error instanceof Error ? error.message : String(error);
	if (!cliWarned) {
		cliWarned = true;
		ctx.ui.notify(`forseti: ${message}`, "warning");
	}
	return { content: [{ type: "text", text: message }], isError: true };
}

function residentSummary(): string {
	if (resident.length === 0) return "no resident blocks";
	return resident
		.map((entry, index) => {
			const meta = bySha.get(entry.sha);
			const name = meta ? sanitizeLabel(meta.name, 24) || "unnamed" : entry.sha.slice(0, 8);
			return `${index + 1}. ${entry.sha.slice(0, 8)} ${name} (${entry.mode}${entry.focus ? `:${sanitizeLabel(entry.focus, 24)}` : ""})`;
		})
		.join("\n");
}

export default function forsetiHashtext(pi: ExtensionAPI) {
	// ---- system prompt: compact cold index + optional Laya suggestion -------
	pi.on("before_agent_start", async (event, ctx) => {
		try {
			await loadState(ctx);
			await refreshMetas();
		} catch (error) {
			if (!cliWarned) {
				cliWarned = true;
				ctx.ui.notify(`forseti: hashtext unavailable — ${error instanceof Error ? error.message : String(error)}`, "warning");
			}
		}
		let section = renderIndexSection();

		// Laya suggestion/auto-expand (opt-in via /hash auto on).
		if (settings.auto) {
			const prompt = event.prompt?.trim() ?? "";
			if (prompt && !prompt.startsWith("/")) {
				try {
					const { candidates, pick } = await findBlocks(prompt);
					if (pick && pick.choice !== "other") {
						const meta = candidates.find((candidate) => candidate.id === pick.choice);
						if (meta) {
							const gate = calibrateConfidence(pick.confidence);
							if (gate >= LAYA_AUTO_CONFIDENCE) {
								await expandRef(meta.sha256, { mode: "auto" }, ctx);
								await logDecision({
									kind: "auto_expand",
									query: prompt.slice(0, 200),
									pick: meta.id,
									name: meta.name,
									confidence: pick.confidence,
									calibrated: gate,
									candidates: candidates.map((candidate) => candidate.id),
								});
								section = renderIndexSection();
							} else if (gate >= LAYA_SUGGEST_CONFIDENCE) {
								section +=
									`\nlikely: ${displayRef(meta)} ${sanitizeLabel(meta.name, 24)} (${gate.toFixed(2)}) — hashtext_expand`;
								await logDecision({
									kind: "suggest",
									query: prompt.slice(0, 200),
									pick: meta.id,
									confidence: pick.confidence,
									calibrated: gate,
								});
							}
						}
					}
				} catch {
					// Laya is advisory: a dead endpoint never affects the prompt.
				}
			}
		}

		if (section) event.systemPromptOptions.sections[SECTION_TAG] = section;
		else delete event.systemPromptOptions.sections[SECTION_TAG];
	});

	// ---- per-turn reconstruction: archives + markers -> views --------------
	pi.on("context", async (event, ctx) => {
		try {
			await loadState(ctx);
			if (metas.length === 0) await refreshMetas();
			const messages = await reconstruct(event.messages, ctx);
			return messages === event.messages ? undefined : { messages: messages as typeof event.messages };
		} catch {
			return undefined; // never break a turn on cache trouble
		}
	});

	// ---- turn bookkeeping ---------------------------------------------------
	pi.on("turn_start", async () => {
		turn += 1;
	});
	pi.on("turn_end", async (event, ctx) => {
		try {
			await loadState(ctx);
			await archiveResults(event.toolResults ?? [], ctx);
			// Optional age-based demotion (off unless HASHTEXT_TTL_TURNS is set).
			const ttl = Number(process.env.HASHTEXT_TTL_TURNS ?? 0);
			if (ttl > 0 && resident.length > 0) {
				const before = resident.length;
				setResident(resident.filter((entry) => turn - entry.added <= ttl));
				if (resident.length !== before) await saveState(ctx).catch(() => {});
			}
			// Laya janitor: choose the least-needed resident block, if enabled.
			if (settings.auto && resident.length >= 3) {
				const criteria: Record<string, string> = { keep: "keep every resident block for the next step" };
				for (const entry of resident) {
					const meta = bySha.get(entry.sha);
					criteria[entry.sha] = meta
						? `${sanitizeLabel(meta.name, 40) || "unnamed"}: ${sanitizeLabel(meta.description, 100) || meta.kind}`
						: entry.sha.slice(0, 8);
				}
				const pick = await layaChoose(
					`Recent work: ${event.message?.role ?? "turn"} ended. Decide what the next step needs.`,
					"Which resident block is least likely to be needed next? Choose 'keep' if all are still relevant.",
					criteria,
				);
				if (pick && pick.choice !== "keep" && pick.confidence >= LAYA_EVICT_CONFIDENCE) {
					const entry = resident.find((candidate) => candidate.sha === pick.choice);
					if (entry) {
						setResident(resident.filter((candidate) => candidate.sha !== entry.sha));
						await saveState(ctx).catch(() => {});
						await logDecision({ kind: "auto_evict", pick: entry.sha.slice(0, 12), confidence: pick.confidence });
					}
				}
			}
		} catch {
			// archival/demotion are best-effort
		}
	});

	// ---- persistence across reload/resume -----------------------------------
	pi.on("session_start", async (_event, ctx) => {
		loadedKey = null;
		await loadState(ctx).catch(() => {});
	});
	pi.on("session_shutdown", async (_event, ctx) => {
		await saveState(ctx).catch(() => {});
	});

	// ---- commands ------------------------------------------------------------
	pi.registerCommand("hash", {
		description: "Forseti: fingerprinted context cache (list|find|put|expand|evict|read|grep|stats|gc|archive|auto|clear)",
		getArgumentCompletions: (prefix) => {
			const items = ["list", "find", "put", "expand", "evict", "read", "grep", "stats", "gc", "archive", "auto", "clear"];
			return items.filter((item) => item.startsWith(prefix)).map((value) => ({ value, label: value }));
		},
		handler: async (args, ctx) => {
			const parts = args.trim().split(/\s+/).filter(Boolean);
			const sub = (parts[0] ?? "list").toLowerCase();
			try {
				await loadState(ctx);
				if (sub === "list" || sub === "find") {
					await refreshMetas(true);
					const query = parts.slice(1).join(" ");
					if (sub === "find" && query) {
						const { candidates, pick } = await findBlocks(query);
						const lines = candidates.map((meta) => indexLine(meta));
						if (pick && pick.choice !== "other") {
							const chosen = candidates.find((meta) => meta.id === pick.choice);
							lines.unshift(
								`laya: ${chosen ? `${chosen.id} ${sanitizeLabel(chosen.name, 24)}` : pick.choice} (confidence ${pick.confidence.toFixed(2)})`,
							);
						}
						ctx.ui.notify(lines.join("\n") || "hashtext: no matching blocks", "info");
						return;
					}
					ctx.ui.notify(`${renderIndexSection() || "hashtext: no blocks"}\n\nresident:\n${residentSummary()}`, "info");
					return;
				}
				if (sub === "stats") {
					const payload = await cliJson<{ stats: Record<string, unknown> }>(["stats", "--json"], undefined, READ_TIMEOUT_MS);
					const stats = payload.stats;
					ctx.ui.notify(
						`hashtext store ${stats.root}\nblocks: ${stats.blocks} · original ${stats.bytes} B · stored ${stats.stored} B (${Math.round(Number(stats.ratio) * 100)}%)\n` +
							`resident in this session: ${resident.length}/${MAX_RESIDENT} · archive: ${archive.size} · auto: ${settings.auto ? "on" : "off"}` +
							`\ncalibration: ${calibration ? `${calibration.length} points` : "none (raw answer_confidence)"}`,
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
					const name = parts.slice(2).join(" ") || path.basename(file);
					const payload = await cliJson<{ block: BlockMeta; deduped: boolean }>(
						["put", "-", "--name", name, "--json"],
						text,
					);
					await refreshMetas(true);
					ctx.ui.notify(
						`hashtext: ${payload.deduped ? "deduped" : "stored"} ${payload.block.id} (${name}) · ${fmtTokens(payload.block.tokens_est)} tok`,
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
					const result =
						sub === "expand"
							? await expandRef(ref, { mode: isExpandMode(parts[2]) ? parts[2] : "auto", focus: parts.slice(3).join(" ") }, ctx)
							: await evictRef(ref);
					if (sub === "evict" && result.ok) await saveState(ctx).catch(() => {});
					ctx.ui.notify(result.text, result.ok ? "info" : "error");
					return;
				}
				if (sub === "read") {
					const ref = parts[1];
					if (!ref) {
						ctx.ui.notify("hashtext: /hash read <ref> [start] [end]", "warning");
						return;
					}
					const found = await lookupRef(ref);
					if ("error" in found) {
						ctx.ui.notify(`hashtext: no block matching '${ref}'`, "error");
						return;
					}
					const view = rangeView(await loadText(found.meta), Number(parts[2] ?? 1), parts[3] ? Number(parts[3]) : undefined);
					ctx.ui.notify(view.text, "info");
					return;
				}
				if (sub === "grep") {
					const pattern = parts.slice(1).join(" ");
					if (!pattern) {
						ctx.ui.notify("hashtext: /hash grep <pattern>", "warning");
						return;
					}
					const regex = new RegExp(pattern, "i");
					const hits: string[] = [];
					for (const meta of metas) {
						const rows = (await loadText(meta)).split("\n");
						for (let i = 0; i < rows.length && hits.length < GREP_MAX_MATCHES; i++) {
							if (regex.test(rows[i])) hits.push(`${meta.id}:${i + 1}: ${clip(rows[i], 160)}`);
						}
						if (hits.length >= GREP_MAX_MATCHES) break;
					}
					ctx.ui.notify(hits.join("\n") || "hashtext: no matches", "info");
					return;
				}
				if (sub === "gc") {
					const payload = await cliJson<{ gc: Record<string, unknown> }>(["gc", "--json"]);
					ctx.ui.notify(`hashtext gc: ${JSON.stringify(payload.gc)}`, "info");
					await refreshMetas(true);
					return;
				}
				if (sub === "log") {
					const count = Math.min(Math.max(Number(parts[1] ?? 10), 1), 50);
					let raw = "";
					try {
						raw = await readFile(DECISIONS_FILE, "utf8");
					} catch {
						ctx.ui.notify("hashtext: no decisions logged yet", "info");
						return;
					}
					const rows = raw.trim().split("\n").filter(Boolean).slice(-count);
					const lines = rows.map((row) => {
						try {
							const parsed = JSON.parse(row) as Record<string, unknown>;
							return `${String(parsed.ts ?? "").slice(11, 19)} ${String(parsed.kind ?? "?")}` +
								`${parsed.action ? `:${parsed.action}` : ""} ${parsed.pick ?? parsed.ref ?? ""}` +
								`${parsed.confidence !== undefined && parsed.confidence !== null ? ` conf=${Number(parsed.confidence).toFixed(2)}` : ""}` +
								`${parsed.matched_pick !== undefined && parsed.matched_pick !== null ? ` matched=${parsed.matched_pick}` : ""}`;
						} catch {
							return row.slice(0, 120);
						}
					});
					ctx.ui.notify(lines.join("\n") || "hashtext: no decisions", "info");
					return;
				}
				if (sub === "archive") {
					const mode = (parts[1] ?? "status").toLowerCase();
					if (mode === "on" || mode === "off") {
						settings.archive = mode === "on";
						await saveState(ctx).catch(() => {});
					} else if (mode === "clear") {
						archive = new Map();
						await saveState(ctx).catch(() => {});
					}
					ctx.ui.notify(
						`hashtext: request-time archival ${settings.archive ? "ON" : "OFF"} · ${archive.size} archived result(s)` +
							`\n(archival stores tool output > ${ARCHIVE_CHARS} chars cold; the transcript keeps the original)`,
						"info",
					);
					return;
				}
				if (sub === "auto") {
					const mode = (parts[1] ?? "status").toLowerCase();
					if (mode === "on" || mode === "off") {
						settings.auto = mode === "on";
						await saveState(ctx).catch(() => {});
					}
					ctx.ui.notify(
						`hashtext: laya assist ${settings.auto ? "ON" : "OFF"} (${LAYA_URL})` +
							"\nfind: shortlist → laya choice · auto: expand ≥" +
							`${LAYA_AUTO_CONFIDENCE} · suggest ≥${LAYA_SUGGEST_CONFIDENCE} · evict ≥${LAYA_EVICT_CONFIDENCE}`,
						"info",
					);
					return;
				}
				if (sub === "clear") {
					setResident([]);
					await saveState(ctx).catch(() => {});
					ctx.ui.notify("hashtext: evicted every resident block", "info");
					return;
				}
				ctx.ui.notify(
					"hashtext: /hash list · find <query> · put <file> [name] · expand <ref> [mode] [query] · evict <ref> · " +
						"read <ref> [start] [end] · grep <pattern> · stats · gc · archive on|off · auto on|off · clear",
					"info",
				);
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
			"Store text in the fingerprinted context cache as a cold block. Use for material the task may need later but not now (large logs, research notes, file snapshots, prior outputs). Returns the block ref; it appears in the cold index and is expanded on demand.",
		promptSnippet: "hashtext_put: stash text as a compressed, fingerprinted context block.",
		parameters: Type.Object({
			text: Type.String({ description: "The full text to store" }),
			name: Type.Optional(Type.String({ description: "Short label for the index (e.g. 'ci log 2026-10-18')" })),
			description: Type.Optional(Type.String({ description: "One-line description of what it holds" })),
			kind: Type.Optional(Type.String({ description: "Block kind: note (default) | log | doc | code | data | tool-output" })),
		}),
		async execute(_toolCallId, params, _signal, _onUpdate, ctx) {
			try {
				const args = ["put", "-", "--json"];
				if (params.name) args.push("--name", params.name);
				if (params.description) args.push("--desc", params.description);
				if (params.kind) args.push("--kind", params.kind);
				const payload = await cliJson<{ block: BlockMeta; deduped: boolean }>(args, params.text);
				await refreshMetas(true);
				const verb = payload.deduped ? "deduped to" : "stored";
				return {
					content: [{
						type: "text",
						text: `${verb} ${payload.block.id} (${sanitizeLabel(payload.block.name, 40) || "unnamed"}) · ${fmtTokens(payload.block.tokens_est)} tok. Cold — expand with hashtext_expand when needed.`,
					}],
					details: { ok: true, ref: payload.block.id, deduped: payload.deduped, tokens: payload.block.tokens_est },
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
			"List the cold context cache: ref, name, description, expansion cost, and residency. Use to browse what exists; prefer hashtext_find when you know the topic.",
		promptSnippet: "hashtext_list: browse the cold block index.",
		parameters: Type.Object({
			query: Type.Optional(Type.String({ description: "Optional substring filter over name/description" })),
		}),
		async execute(_toolCallId, params, _signal, _onUpdate, ctx) {
			try {
				await loadState(ctx);
				await refreshMetas(true);
				const needle = (params.query ?? "").toLowerCase().trim();
				const rows = metas
					.filter((meta) => !needle || `${meta.name} ${meta.description} ${meta.kind}`.toLowerCase().includes(needle))
					.slice(0, 100);
				const body = rows.map((meta) => `${indexLine(meta)}${residentSet.has(meta.sha256) ? " [resident]" : ""}`).join("\n");
				return {
					content: [{ type: "text", text: body || "no blocks" }],
					details: { ok: true, blocks: rows.length, resident: resident.length },
				};
			} catch (error) {
				return installFailure(ctx, error);
			}
		},
	});

	pi.registerTool({
		name: "hashtext_find",
		label: "hashtext_find",
		description:
			"Find the cold block most likely to hold what you need. Lexical shortlist, then the local Laya decision model picks one (with abstention). Use before hashtext_list when you are looking for a topic.",
		promptSnippet: "hashtext_find: locate a cold block by topic.",
		parameters: Type.Object({
			query: Type.String({ description: "What you are looking for (paraphrase freely)" }),
		}),
		async execute(_toolCallId, params, _signal, _onUpdate, ctx) {
			try {
				await loadState(ctx);
				const { candidates, pick } = await findBlocks(params.query);
				if (candidates.length === 0) return { content: [{ type: "text", text: "no matching blocks" }], details: { ok: true, count: 0 } };
				const lines = candidates.map((meta) => indexLine(meta));
				let chosen = "";
				if (pick && pick.choice !== "other") {
					chosen = pick.choice;
					lines.unshift(`laya pick: ${pick.choice} (confidence ${pick.confidence.toFixed(2)}) — hashtext_expand ${pick.choice}`);
				} else {
					lines.unshift("laya: no confident pick — review the candidates below");
				}
				return {
					content: [{ type: "text", text: lines.join("\n") }],
					details: { ok: true, pick: chosen || null, candidates: candidates.map((meta) => meta.id) },
				};
			} catch (error) {
				return installFailure(ctx, error);
			}
		},
	});

	pi.registerTool({
		name: "hashtext_expand",
		label: "hashtext_expand",
		description:
			"Expand a cold block so its contents are reconstructed into context on the next model call. Large blocks default to an OUTLINE (mode=auto); ask for mode=full, mode=focus with a query for relevant excerpts, or a line range. The transcript keeps only a one-line marker.",
		promptSnippet: "hashtext_expand: bring a cold block (or part of it) into context.",
		promptGuidelines: [
			"Expand a cold block only when the task needs it, and evict blocks you have finished with.",
			"For blocks over a few thousand tokens prefer the default outline/focused excerpt, then read line ranges.",
		],
		parameters: Type.Object({
			ref: Type.String({ description: "Block ref from the index (fingerprint or unambiguous prefix)" }),
			mode: Type.Optional(Type.String({ description: "auto (default) | full | outline | focus | range" })),
			focus: Type.Optional(Type.String({ description: "Query for mode=focus: only matching excerpts are injected" })),
			start: Type.Optional(Type.Integer({ description: "Start line for mode=range (1-based)" })),
			end: Type.Optional(Type.Integer({ description: "End line for mode=range (inclusive)" })),
		}),
		async execute(_toolCallId, params, _signal, _onUpdate, ctx) {
			await loadState(ctx).catch(() => {});
			const result = await expandRef(
				params.ref,
				{
					mode: isExpandMode(params.mode) ? params.mode : "auto",
					focus: params.focus,
					start: params.start,
					end: params.end,
				},
				ctx,
			);
			if (!result.ok) return { content: [{ type: "text", text: result.text }], isError: true };
			return { content: [{ type: "text", text: result.text }], details: { ok: true } };
		},
	});

	pi.registerTool({
		name: "hashtext_evict",
		label: "hashtext_evict",
		description:
			"Evict a resident block: its contents stop being reconstructed into context, leaving a one-line marker. Use when a block is no longer relevant, to keep the window small.",
		promptSnippet: "hashtext_evict: drop a block from the hot context set.",
		parameters: Type.Object({
			ref: Type.String({ description: "Block ref (fingerprint or unambiguous prefix)" }),
		}),
		async execute(_toolCallId, params, _signal, _onUpdate, ctx) {
			await loadState(ctx).catch(() => {});
			const result = await evictRef(params.ref);
			if (result.ok) await saveState(ctx).catch(() => {});
			return result.ok
				? { content: [{ type: "text", text: result.text }], details: { ok: true } }
				: { content: [{ type: "text", text: result.text }], isError: true };
		},
	});

	pi.registerTool({
		name: "hashtext_read",
		label: "hashtext_read",
		description:
			"Read an exact line range from a cold block without expanding the whole block. Lines are numbered, so follow-up ranges are cheap. Use this to drill into an outline or a focused excerpt.",
		promptSnippet: "hashtext_read: read a line range from a cold block.",
		parameters: Type.Object({
			ref: Type.String({ description: "Block ref" }),
			start: Type.Optional(Type.Integer({ description: "First line (1-based, default 1)" })),
			end: Type.Optional(Type.Integer({ description: "Last line (inclusive, default start+399)" })),
		}),
		async execute(_toolCallId, params, _signal, _onUpdate, ctx) {
			try {
				await refreshMetas();
				const found = await lookupRef(params.ref);
				if ("error" in found) {
					return { content: [{ type: "text", text: `no block matching '${params.ref}'` }], isError: true };
				}
				const text = await loadText(found.meta);
				const rows = text.split("\n");
				const lo = Math.max(1, params.start ?? 1);
				const hi = Math.min(rows.length, params.end ?? lo + READ_MAX_LINES - 1);
				if (lo > hi) return { content: [{ type: "text", text: `block has ${rows.length} lines` }], details: { ok: true } };
				const body = rows.slice(lo - 1, hi).map((row, index) => `${lo + index}: ${row}`).join("\n");
				return {
					content: [{ type: "text", text: `lines ${lo}-${hi} of ${rows.length} (${found.meta.id}):\n${body}` }],
					details: { ok: true, total_lines: rows.length },
				};
			} catch (error) {
				return installFailure(ctx, error);
			}
		},
	});

	pi.registerTool({
		name: "hashtext_grep",
		label: "hashtext_grep",
		description:
			"Regex search across cold blocks (or one block), returning matching lines with refs and line numbers. Use it instead of expanding a large block when you only need a few hits.",
		promptSnippet: "hashtext_grep: search cold blocks by regex, line-addressed.",
		parameters: Type.Object({
			pattern: Type.String({ description: "Regular expression (case-insensitive)" }),
			refs: Type.Optional(Type.Array(Type.String(), { description: "Limit to these blocks (default: all)" })),
			max: Type.Optional(Type.Integer({ description: `Max matches (default ${GREP_MAX_MATCHES})` })),
		}),
		async execute(_toolCallId, params, _signal, _onUpdate, ctx) {
			try {
				await refreshMetas();
				let regex: RegExp;
				try {
					regex = new RegExp(params.pattern, "i");
				} catch (error) {
					return { content: [{ type: "text", text: `invalid pattern: ${error instanceof Error ? error.message : error}` }], isError: true };
				}
				const limit = Math.min(Math.max(params.max ?? GREP_MAX_MATCHES, 1), 200);
				let pool: BlockMeta[];
				if (params.refs?.length) {
					pool = [];
					for (const ref of params.refs) {
						const found = await lookupRef(ref);
						if ("meta" in found) pool.push(found.meta);
					}
				} else {
					pool = metas;
				}
				const hits: string[] = [];
				for (const meta of pool) {
					const rows = (await loadText(meta)).split("\n");
					for (let i = 0; i < rows.length && hits.length < limit; i++) {
						if (regex.test(rows[i])) hits.push(`${meta.id}:${i + 1}: ${clip(rows[i], 200)}`);
					}
					if (hits.length >= limit) break;
				}
				return {
					content: [{ type: "text", text: hits.join("\n") || "no matches" }],
					details: { ok: true, matches: hits.length, scanned: pool.length },
				};
			} catch (error) {
				return installFailure(ctx, error);
			}
		},
	});
}
