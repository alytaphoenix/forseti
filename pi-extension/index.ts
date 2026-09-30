// Forseti pi extension (Phase 2b + 2c + 2d)
// Drives the forseti ttt editor pane from the pi agent:
//   /ttt jump <path> [line] [end_line] — open the file at the position in ttt
//   /ttt open <path>                   — same without position
//   /ttt follow on|off|status          — auto-jump to code pi edits (default off)
//   /ttt review on|off|status          — collect edits per turn; on turn end
//                                        open a Forseti "Review" tab in ttt
//   /ttt context on|off|status         — inject current editor context into
//                                        every prompt (default on)
//   /ttt diff                          — open ttt's "Git: Open Changes" view
//   /herd agents                        — read-only herdr pass-through
//   tools for the model: ttt_open, ttt_diff, ttt_read_context
//
// Mechanics: write {path,line,end_line} into the ttt forseti plugin dir, then
// POST `exec "Forseti: Jump"` to ttt's --listen control server (:4242).
// Verified surfaces: docs/spikes.md (ttt exec vocab, plugin dir hand-off,
// edit tool firstChangedLine, x-opencode-session header).
import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";
import { Type } from "typebox";
import { readFile, writeFile } from "node:fs/promises";
import { homedir } from "node:os";
import path from "node:path";

const TTT_EXEC_URL = "http://127.0.0.1:4242/exec";
const FORSETI_DIR = path.join(homedir(), ".config", "ttt", "plugins", "forseti");
const JUMP_FILE = path.join(FORSETI_DIR, "jump.json");
const CONTEXT_FILE = path.join(FORSETI_DIR, "context.json");
const REVIEW_FILE = path.join(FORSETI_DIR, "review.json");
const MIN_GAP_MS = 1200;
const VAULT = process.env.FORSETI_VAULT ?? path.join(homedir(), "forseti");

interface ForsetiContext {
	path?: string;
	line?: number;
	sel_start?: number;
	sel_end?: number;
	selection?: string;
	ts?: number;
}

async function tttExec(script: string): Promise<boolean> {
	try {
		const res = await fetch(TTT_EXEC_URL, { method: "POST", body: script });
		listenerWarned = false;
		return res.ok;
	} catch {
		listenerWarned = true;
		return false;
	}
}

let listenerWarned = false;

async function pushJump(
	ctx: { ui: { notify(msg: string, level?: string): void } },
	file: string,
	line?: number,
	endLine?: number,
): Promise<void> {
	if (!file) { ctx.ui.notify("forseti: jump needs a path", "error"); return; }
	await writeJump(file, line, endLine);
	const ok = await tttExec('exec "Forseti: Jump"');
	if (!ok && listenerWarned) {
		ctx.ui.notify("forseti: ttt listener (:4242) unreachable — run `herdr plugin action invoke forseti.open`", "warning");
	}
}

async function writeJump(file: string, line?: number, endLine?: number): Promise<void> {
	await writeFile(
		JUMP_FILE,
		JSON.stringify({ path: file, line, endLine, at: new Date().toISOString() }),
		"utf8",
	);
}

export default function forseti(pi: ExtensionAPI) {
	let follow = false;
	let review = false;
	let contextMode = true;
	let lastPush = 0;
	const turnEdits: { path: string; line?: number }[] = [];
	const pendingEdits = new Map<string, string>();
	let startOfTurn = Date.now();

	function jumpsSince(since: number, _e?: unknown): unknown {
		return review ? undefined : undefined; // collector uses pendingEdits map instead
	}

	// ---- commands -----------------------------------------------------------
	pi.registerCommand("ttt", {
		description: "Forseti: drive the ttt editor (jump|open|follow|review|context|diff)",
		handler: async (args, ctx) => {
			const parts = args.trim().split(/\s+/).filter(Boolean);
			const sub = parts[0] ?? "help";
			const mode = (parts[1] ?? "status").toLowerCase();
			if (sub === "jump" || sub === "open") {
				const file = parts[1];
				const line = parts[2] !== undefined ? Number.parseInt(parts[2], 10) : undefined;
				const endLine = parts[3] !== undefined ? Number.parseInt(parts[3], 10) : undefined;
				await pushJump(ctx, file, line, endLine);
				return;
			}
			if (sub === "follow") {
				follow = mode === "on" || mode === "true";
				if (follow && review) {
					follow = false;
					ctx.ui.notify("forseti: review mode already handles turn summaries; keep follow off", "info");
					return;
				}
				ctx.ui.notify(follow ? "forseti: follow ON — ttt jumps on every edit" : "forseti: follow OFF", "info");
				return;
			}
			if (sub === "review") {
				review = mode === "on" || mode === "true";
				if (review) follow = false; // review supersedes per-edit jumps
				ctx.ui.notify(`forseti: review ${review ? "ON" : "OFF"} (summary opens at turn end)`, "info");
				return;
			}
			if (sub === "context") {
				contextMode = mode === "on" || mode === "true";
				ctx.ui.notify(`forseti: editor context in prompts ${contextMode ? "ON" : "OFF"}`, "info");
				return;
			}
			if (sub === "diff") {
				const ok = await tttExec('exec "Git: Open Changes"');
				if (!ok && listenerWarned) {
					ctx.ui.notify("forseti: ttt listener (:4242) unreachable — run forseti.open", "warning");
				}
				return;
			}
			ctx.ui.notify(
				"forseti: /ttt jump <path> [line] [end] · open · follow · review · context · diff",
				"info",
			);
		},
	});

	pi.registerCommand("herd", {
		description: "Forseti: herdr read pass-through (agents)",
		handler: async (args, ctx) => {
			const sub = (args.trim().split(/\s+/)[0] ?? "agents").toLowerCase();
			if (sub === "agents") {
				const { stdout } = await pi.exec("herdr", ["agent", "list"]);
				const list = JSON.parse(stdout ?? "{}").result?.agents ?? [];
				ctx.ui.notify(
					list.length === 0
						? "herdr: no live agents"
						: list.map((a: { name: string; agent: string; agent_status: string }) => `${a.name} (${a.agent}): ${a.agent_status}`).join("\n"),
					"info",
				);
			} else {
				ctx.ui.notify("forseti: /herd agents (read-only v1)", "info");
			}
		},
	});

	// ---- Phase 2c-3: IDE-awareness (transform every prompt) ------------------
	pi.on("input", async (event) => {
		if (!contextMode) return { action: "continue" };
		const text = event.text ?? "";
		if (text.trimStart().startsWith("/")) return { action: "continue" };
		if (text.trim() === "") return { action: "continue" };
		let raw: string | null = null;
		try { raw = await readFile(CONTEXT_FILE, "utf8"); } catch { return { action: "continue" }; }
		let c: ForsetiContext;
		try { c = JSON.parse(raw) as ForsetiContext; } catch { return { action: "continue" }; }
		if (!c.path) return { action: "continue" };
		const sel = c.selection
			? `\nselection over lines ${c.sel_start ?? c.line ?? "?"}–${c.sel_end ?? c.line ?? "?"}:\n"""\n${(c.selection ?? "").slice(0, 4000)}\n"""`
			: "";
		const block = `[Forseti editor context] the user is currently viewing this file in ttt: ${c.path}${c.line ? ` at line ${c.line}` : ""}.${sel}`;
		return { action: "transform", text: `${block}\n\n${text}` };
	});

	// ---- Phase 2d-1: model-driven editor tools -------------------------------
	pi.registerTool({
		name: "ttt_open",
		label: "ttt_open",
		description:
			"Open a file in the Forseti ttt editor pane at a given position. Use when the user says 'show me X' or 'open Y', or to draw their attention to a file/line relevant to your work.",
		promptSnippet: "Open a file in the ttt editor pane for the user to see.",
		parameters: Type.Object({
			path: Type.String({ description: "File path (absolute or workspace-relative)" }),
			line: Type.Optional(Type.Number({ description: "1-based line to jump to" })),
			end: Type.Optional(Type.Number({ description: "1-based end line to highlight" })),
		}),
		async execute(_toolCallId, params) {
			if (!params.path) return { content: [{ type: "text", text: "ttt_open error: path required" }], isError: true };
			await writeJump(params.path, params.line, params.end);
			const ok = await tttExec('exec "Forseti: Jump"');
			return {
				content: [{ type: "text", text: ok ? `opened ${params.path}:${params.line ?? 1} in ttt` : "ttt listener unreachable (forseti.open not running?)" }],
				details: { ok },
			};
		},
	});

	pi.registerTool({
		name: "ttt_diff",
		label: "ttt_diff",
		description: "Open ttt's 'Git: Open Changes' review view.",
		parameters: Type.Object({}),
		async execute() {
			const ok = await tttExec('exec "Git: Open Changes"');
			return { content: [{ type: "text", text: ok ? "opened Git: Open Changes in ttt" : "ttt listener unreachable" }], details: { ok } };
		},
	});

	pi.registerTool({
		name: "ttt_read_context",
		label: "ttt_read_context",
		description:
			"Read what the user is currently looking at in the ttt editor (file, line, selection). Use when you need to know the user's visible context.",
		parameters: Type.Object({}),
		async execute() {
			let raw: string | undefined;
			try { raw = await readFile(CONTEXT_FILE, "utf8"); } catch { /* missing */ }
			if (!raw) return { content: [{ type: "text", text: "no editor context available" }], details: { ok: false } };
			return { content: [{ type: "text", text: raw }], details: { ok: true } };
		},
	});

	// ---- Phase 2d-2: review mode --------------------------------------------
	pi.on("turn_start", async () => {
		turnEdits.length = 0;
	});

	pi.on("tool_execution_start", async (event) => {
		if (event.toolName !== "edit" && event.toolName !== "write") return;
		const file = (event.args as { path?: string } | undefined)?.path;
		if (file) pendingEdits.set(event.toolCallId, file);
		if (follow && !review && file) {
			// keep pendingEdits for follow; push happens on end
		}
	});

	pi.on("tool_execution_end", async (event) => {
		if (event.isError) {
			pendingEdits.delete(event.toolCallId);
			return;
		}
		if (event.parentToolCallId) return; // nested: the real tool triggers
		const file = pendingEdits.get(event.toolCallId);
		pendingEdits.delete(event.toolCallId);
		if (!file) return;
		const edit = execCtxEdit(event, file);

		if (review) {
			turnEdits.push(edit); // collected; summary at turn_end
			return;
		}
		if (!follow) return;
		const now = Date.now();
		if (now - lastPush < MIN_GAP_MS) return;
		lastPush = now;
		await writeJump(edit.path, edit.line);
		await tttExec('exec "Forseti: Jump"');
	});

	function execCtxEdit(event: { result?: unknown; toolName: string }, file: string): { path: string; line?: number } {
		if (event.toolName === "write") return { path: file, line: 1 };
		const details = (event.result as { details?: { firstChangedLine?: number } } | undefined)?.details;
		const line = typeof details?.firstChangedLine === "number" ? details.firstChangedLine : undefined;
		return { path: file, line };
	}

	pi.on("turn_end", async (_event, ctx) => {
		if (!review || turnEdits.length === 0) return;
		try {
			await writeFile(
				REVIEW_FILE,
				JSON.stringify({ files: turnEdits, at: new Date().toISOString() }),
				"utf8",
			);
			turnEdits.length = 0;
			await tttExec('exec "Forseti: Review"');
		} catch (e) {
			ctx?.ui?.notify?.("forseti: review sync failed: " + String(e), "warning");
		}
	});

	// ---- Phase 4-2: vault (evergreen secondbrain) tools ----------------------
	function slugify(title: string): string {
		return title
			.toLowerCase()
			.replace(/[^a-z0-9]+/g, "-")
			.replace(/^-+|-+$/g, "")
			.slice(0, 80);
	}

	function inVault(abs: string): boolean {
		return abs.startsWith(path.join(VAULT, path.sep));
	}

	pi.registerTool({
		name: "vault_search",
		label: "vault_search",
		description:
			`Full-text search over the user's evergreen notes vault (${VAULT}). Use to recall the user's own notes, past decisions, and links (wikilinks [[slug]]).`,
		promptSnippet: "Search the second-brain vault for notes matching a query.",
		parameters: Type.Object({
			query: Type.String({ description: "Text to find in notes" }),
		}),
		async execute(_toolCallId, params) {
			const { stdout } = await pi.exec("rg", [
				"-i", "-n", "--glob", "*.md", "-m", "5", "-e", params.query, VAULT,
			]);
			const hits = (stdout ?? "").trim().split("\n").filter((l: string) => l.startsWith(VAULT)).slice(0, 20);
			if (hits.length === 0) return { content: [{ type: "text", text: `no vault matches for: ${params.query}` }] };
			return { content: [{ type: "text", text: hits.join("\n") }], details: { count: hits.length } };
		},
	});

	pi.registerTool({
		name: "vault_note",
		label: "vault_note",
		description:
			"Create an evergreen note in the vault. Title becomes the filename slug; uniqueness is enforced. Prefer statement-shaped titles.",
		promptSnippet: "Create a new evergreen note with [[wikilinks]] support.",
		parameters: Type.Object({
			title: Type.String({ description: "Note title (statement-shaped encouraged)" }),
			body: Type.Optional(Type.String({ description: "Initial note body" })),
		}),
		async execute(_toolCallId, params) {
			const slug = slugify(params.title);
			if (!slug) return { content: [{ type: "text", text: "vault_note error: title produced an empty slug" }], isError: true };
			const file = path.join(VAULT, "notes", `${slug}.md`);
			try {
				await readFile(file, "utf8");
				return { content: [{ type: "text", text: `note already exists: ${file}` }], details: { ok: false, exists: true } };
			} catch { /* fresh */ }
			const front = [
				"---",
				`title: ${params.title}`,
				`created: ${new Date().toISOString().slice(0, 10)}`,
				"type: evergreen",
				"tags: []",
				"---",
				"",
				`# ${params.title}`,
				"",
			].join("\n");
			await writeFile(file, front + (params.body ? params.body + "\n" : ""), "utf8");
			await writeJump(file);
			await tttExec('exec "Forseti: Jump"');
			return { content: [{ type: "text", text: `created [[${slug}]] → ${file} (opened in ttt)` }], details: { ok: true } };
		},
	});

	pi.registerTool({
		name: "vault_daily",
		label: "vault_daily",
		description: "Append a log line to today's daily note (daily/YYYY-MM-DD.md, creates it from the template).",
		promptSnippet: "Append to today's daily note in the vault.",
		parameters: Type.Object({
			text: Type.String({ description: "One log line to append under '## Log'" }),
		}),
		async execute(_toolCallId, params) {
			const today = new Date().toISOString().slice(0, 10);
			const file = path.join(VAULT, "daily", `${today}.md`);
			let content: string;
			try { content = await readFile(file, "utf8"); } catch {
				content = [
					"---", `title: ${today}`, `created: ${today}`, "type: daily", "---", "", "## Log", "",
				].join("\n");
			}
			if (!content.includes("## Log")) content += "\n## Log\n";
			content = content.trimEnd() + "\n" + `- ${params.text}\n`;
			await writeFile(file, content, "utf8");
			return { content: [{ type: "text", text: `logged to ${file}` }], details: { ok: true } };
		},
	});

	pi.registerTool({
		name: "vault_open",
		label: "vault_open",
		description:
			"Resolve a note by slug/title and open it in the ttt editor pane (Forseti jump). Use for 'show me my notes about X'.",
		promptSnippet: "Open a vault note in the ttt editor.",
		parameters: Type.Object({
			name: Type.String({ description: "Note slug or filename, with or without .md" }),
		}),
		async execute(_toolCallId, params) {
			const base = params.name.replace(/\.md$/, "");
			const candidates = [
				path.join(VAULT, "notes", `${base}.md`),
				path.join(VAULT, "daily", `${base}.md`),
				path.join(VAULT, `${base}.md`),
			];
			for (const c of candidates) {
				try { await readFile(c, "utf8"); await pushJump({ ui: { notify: () => {} } } as never, c); return { content: [{ type: "text", text: `opened ${c}` }], details: { ok: true } }; } catch { /* next */ }
			}
			const { stdout } = await pi.exec("rg", ["--files", VAULT, "-g", `*${base}*`]);
			const near = (stdout ?? "").trim().split("\n").slice(0, 5);
			return {
				content: [{ type: "text", text: `no note '${base}'. ${near.length ? `close matches:\n${near.join("\n")}` : "vault search found nothing"}` }],
				details: { ok: false },
			};
		},
	});
}