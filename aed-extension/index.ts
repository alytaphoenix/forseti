// Forseti pi extension (aed): use agentic-edit (aed) to make safe, verified
// file edits from a pi session.
//
//   /aed edit  <action> --files a.go b.go [--apply true|false] [--verify true|false]
//   /aed grep  <pattern> --files *.go [-E]
//
// The model-facing tools are `aed_edit` and `aed_grep`. They shell out to the
// `aed` binary (env AED_BIN, default "aed") and return its stdout/stderr. The
// extension never edits files itself; it delegates to agentic-edit, which
// previews the diff, writes a .bak backup, runs a safety denylist, and (by
// default) verifies the change landed after applying.
import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";
import { Type } from "typebox";
import { execFile } from "node:child_process";
import { promisify } from "node:util";

const execFileAsync = promisify(execFile);
const AED_BIN = process.env.AED_BIN?.trim() || "aed";
const CMD_TIMEOUT_MS = 60_000;

// runAed runs the aed binary and normalises the result. A missing binary is
// reported with an install hint so the model knows how to fix it.
async function runAed(args: string[]): Promise<{ ok: boolean; text: string }> {
	try {
		const { stdout } = await execFileAsync(AED_BIN, args, {
			timeout: CMD_TIMEOUT_MS,
			maxBuffer: 4 * 1024 * 1024,
		});
		const text = String(stdout || "").trim();
		return { ok: true, text: text || "(aed produced no output)" };
	} catch (err: unknown) {
		const e = err as { stderr?: unknown; message?: string };
		const msg = e?.stderr ? String(e.stderr).trim() : String(e?.message || err);
		const hint = /no such file|not found|enoent/i.test(msg)
			? `\naed not found at '${AED_BIN}'. Put it on PATH, or build it: clone agentic-edit and run 'go build -o aed ./cmd/aed', then set AED_BIN to its path.`
			: "";
		return { ok: false, text: msg + hint };
	}
}

export default function forsetiAed(pi: ExtensionAPI) {
	// ---- /aed command (interactive passthrough, mirrors /ttt and /herd) -----
	pi.registerCommand("aed", {
		description:
			"Forseti: use agentic-edit (aed) to make safe, verified file edits. Subcommands: edit, grep.",
		handler: async (args: string) => {
			const parts = args.trim().split(/\s+/).filter(Boolean);
			const sub = parts[0] ?? "help";
			if (sub === "edit" || sub === "grep") {
				const res = await runAed(parts);
				return { content: [{ type: "text", text: res.text }] };
			}
			return {
				content: [
					{
						type: "text",
						text:
							"Usage:\n" +
							"  /aed edit <action> --files a.go b.go [--apply true|false] [--verify true|false]\n" +
							"  /aed grep  <pattern> --files *.go [-E]\n" +
							"Tools for the model: aed_edit, aed_grep.",
					},
				],
			};
		},
	});

	// ---- model-facing tools -------------------------------------------------
	pi.registerTool({
		name: "aed_edit",
		label: "aed_edit",
		description:
			"Make a safe, verifiable file edit with agentic-edit (aed). It turns a natural-language instruction into awk/sed/grep edits, previews the diff, writes a .bak backup, runs a safety denylist, and (by default) verifies the change landed after applying. Use when the user wants a file changed — rename a variable/identifier, fix a string literal, transform or filter lines, delete matching lines (e.g. TODOs), etc. Prefer this over hand-writing sed/awk; it is safer and self-verifying.",
		promptSnippet: "Edit files using agentic-edit (aed) for safe, verified changes.",
		parameters: Type.Object({
			action: Type.String({ description: "Natural-language edit instruction, e.g. replace \"v1\" with \"v3\"" }),
			files: Type.Array(Type.String(), {
				description: "Target files (one or more). Absolute paths or paths relative to the pi workspace cwd.",
			}),
			apply: Type.Boolean({
				description: "Apply the edit and verify (true) vs dry-run preview only (false). Default true.",
			}),
			verify: Type.Boolean({
				description: "Run the post-apply verification guard. Default true. Ignored when apply is false.",
			}),
		}),
		async execute(_toolCallId, params) {
			if (!params.action) return { content: [{ type: "text", text: "aed_edit error: action required" }], isError: true };
			if (!params.files || params.files.length === 0) {
				return { content: [{ type: "text", text: "aed_edit error: at least one file is required" }], isError: true };
			}
			const args = ["edit", "-a", params.action, ...params.files.flatMap((f: string) => ["-f", f])];
			if (params.apply === false) {
				args.push("--dry-run");
			} else {
				args.push("-y");
				args.push(params.verify === false ? "--no-verify" : "--verify");
			}
			const { ok, text } = await runAed(args);
			const mode = params.apply === false ? "DRY-RUN preview" : "applied";
			return {
				content: [{ type: "text", text: `[aed ${mode}]\n${text}` }],
				isError: !ok,
				details: { ok, mode, files: params.files },
			};
		},
	});

	pi.registerTool({
		name: "aed_grep",
		label: "aed_grep",
		description:
			"Search files with agentic-edit's grep wrapper (returns 0 even when nothing matches). Use to find lines matching a pattern before deciding on an edit, or to confirm a change landed.",
		promptSnippet: "Search files with agentic-edit (aed) grep.",
		parameters: Type.Object({
			pattern: Type.String({ description: "grep pattern (basic regex by default)" }),
			files: Type.Array(Type.String(), { description: "Target files/globs (one or more)" }),
			extended: Type.Boolean({ description: "Use extended regex (-E). Default false." }),
		}),
		async execute(_toolCallId, params) {
			if (!params.pattern) return { content: [{ type: "text", text: "aed_grep error: pattern required" }], isError: true };
			if (!params.files || params.files.length === 0) {
				return { content: [{ type: "text", text: "aed_grep error: at least one file is required" }], isError: true };
			}
			const args = ["grep", ...(params.extended ? ["-E"] : []), params.pattern, ...params.files];
			const { ok, text } = await runAed(args);
			return { content: [{ type: "text", text: text || "(no matches)" }], isError: !ok, details: { ok } };
		},
	});
}
