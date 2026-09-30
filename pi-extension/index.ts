// Forseti pi extension (Phase 2b)
// Drives the forseti ttt editor pane from the pi agent:
//   /ttt jump <path> [line] [end_line] — open the file at the position in ttt
//   /ttt open <path>                   — same without position
//   /ttt follow on|off|status          — auto-jump to code pi edits (default off)
//   /ttt diff                          — open ttt's "Git: Open Changes" view
//   /herd agents                        — read-only herdr pass-through
//
// Mechanics: write {path,line,end_line} into the ttt forseti plugin dir, then
// POST `exec "Forseti: Jump"` to ttt's --listen control server (:4242).
// Verified surfaces: ttt exec vocabulary + plugin dir hand-off (docs/spikes.md),
// edit tool emits firstChangedLine (details).
import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";
import { writeFile } from "node:fs/promises";
import { homedir } from "node:os";
import path from "node:path";

const TTT_EXEC_URL = "http://127.0.0.1:4242/exec";
const JUMP_FILE = path.join(homedir(), ".config", "ttt", "plugins", "forseti", "jump.json");
const MIN_GAP_MS = 1200; // debounce follow-mode pushes; edits are frequent

export default function forseti(pi: ExtensionAPI) {
	let follow = false;
	let listenerWarned = false;
	let lastPush = 0;

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

	async function pushJump(
		ctx: { ui: { notify(msg: string, level?: string): void } },
		file: string,
		line?: number,
		endLine?: number,
	): Promise<void> {
		if (!file) { ctx.ui.notify("forseti: jump needs a path", "error"); return; }
		await writeFile(
			JUMP_FILE,
			JSON.stringify({ path: file, line, endLine, at: new Date().toISOString() }),
			"utf8",
		);
		const ok = await tttExec('exec "Forseti: Jump"');
		if (!ok && listenerWarned) {
			ctx.ui.notify("forseti: ttt listener (:4242) unreachable — run `herdr plugin action invoke forseti.open`", "warning");
		}
	}

	pi.registerCommand("ttt", {
		description: "Forseti: drive the ttt editor (jump|open|follow|diff)",
		handler: async (args, ctx) => {
			const parts = args.trim().split(/\s+/).filter(Boolean);
			const sub = parts[0] ?? "help";
			if (sub === "jump" || sub === "open") {
				const file = parts[1];
				const line = parts[2] !== undefined ? Number.parseInt(parts[2], 10) : undefined;
				const endLine = parts[3] !== undefined ? Number.parseInt(parts[3], 10) : undefined;
				await pushJump(ctx, file, line, endLine);
			} else if (sub === "follow") {
				const mode = (parts[1] ?? "status").toLowerCase();
				if (mode === "on" || mode === "true") {
					follow = true;
					ctx.ui.notify("forseti: follow ON — ttt will jump to code pi edits", "info");
				} else if (mode === "off" || mode === "false") {
					follow = false;
					ctx.ui.notify("forseti: follow OFF", "info");
				} else {
					ctx.ui.notify(`forseti: follow is ${follow ? "ON" : "OFF"} (usage: /ttt follow on|off)`, "info");
				}
			} else if (sub === "diff") {
				const ok = await tttExec('exec "Git: Open Changes"');
				if (!ok && listenerWarned) {
					ctx.ui.notify("forseti: ttt listener (:4242) unreachable — run forseti.open", "warning");
				}
			} else {
				ctx.ui.notify(
					"forseti: /ttt jump <path> [line] [end_line] · /ttt follow on|off · /ttt diff",
					"info",
				);
			}
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

	// follow mode: after a successful edit/write, jump the editor to the change.
	// tool_execution_end has no args — capture the path on start, key it by the
	// toolCallId, and pick it up on end (where result.details.firstChangedLine
	// lives for edits). Nested calls get their own ids but re-emit with
	// parentToolCallId; we ignore those to let the real tool trigger once.
	const pendingEdits = new Map<string, string>();

	pi.on("tool_execution_start", async (event) => {
		if (!follow) return;
		if (event.toolName !== "edit" && event.toolName !== "write") return;
		const file = (event.args as { path?: string } | undefined)?.path;
		if (file) pendingEdits.set(event.toolCallId, file);
	});

	pi.on("tool_execution_end", async (event) => {
		if (!follow || event.isError) return;
		if (event.parentToolCallId) return; // nested: the real tool triggers
		const file = pendingEdits.get(event.toolCallId);
		pendingEdits.delete(event.toolCallId);
		if (!file) return;
		const now = Date.now();
		if (now - lastPush < MIN_GAP_MS) return;
		lastPush = now;
		if (event.toolName === "write") {
			await pushJump({ ui: { notify: () => {} } } as never, file, 1);
		} else {
			const details = (event.result as { details?: { firstChangedLine?: number } } | undefined)?.details;
			const line = typeof details?.firstChangedLine === "number" ? details.firstChangedLine : undefined;
			await pushJump({ ui: { notify: () => {} } } as never, file, line);
		}
	});
}
