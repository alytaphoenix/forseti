// hashtext plugin smoke gate (bun): `bun run scripts/hashtext-smoke.ts`
//
// Drives the real extension (hashtext-plugin/index.ts) against the real
// hashtext CLI and a scratch store, stubbing only the pi ExtensionAPI. Covers
// the hot-swap contract end to end:
//   index section · expand marker · per-turn reconstruction · evict ·
//   budget withholding · put/dedup · /hash evict · missing-CLI degradation
//
// Requires: `hashtext` on PATH (or HASHTEXT_BIN), `bun`.
// Store is scratch (HASHTEXT_DIR), never ~/.hashtext.
import { execFileSync, spawnSync } from "node:child_process";
import { mkdirSync, readFileSync, rmSync } from "node:fs";
import { homedir } from "node:os";
import path from "node:path";

const HERE = path.dirname(new URL(import.meta.url).pathname);
const PLUGIN = path.join(HERE, "..", "hashtext-plugin", "index.ts");
const STORE = process.env.HASHTEXT_SMOKE_DIR || path.join(process.env.TMPDIR || "/tmp", "hashtext-plugin-smoke");
const SESSION = "smoke-session";
const TEXT = "BUILD LOG\n" + "error: the widget exploded ".repeat(40);

const CANDIDATES = [
	"hashtext",
	path.join(homedir(), ".local", "bin", "hashtext"),
	"/opt/homebrew/bin/hashtext",
	// dev loop: run the gate straight out of a source checkout
	path.join(homedir(), "repos", "hashtext", ".venv", "bin", "hashtext"),
];

const BIN =
	process.env.HASHTEXT_BIN?.trim() ||
	CANDIDATES.find((candidate) => {
		try {
			execFileSync(candidate, ["--help"], { stdio: "ignore" });
			return true;
		} catch {
			return false;
		}
	}) ||
	"";

if (!BIN) {
	console.error(
		"hashtext CLI not found. Install it (cd ~/repos/hashtext && uv tool install .) or set HASHTEXT_BIN.",
	);
	process.exit(1);
}

const assert = (cond: unknown, message: string) => {
	if (!cond) {
		console.error("FAIL:", message);
		process.exit(1);
	}
	console.log("ok:", message);
};

// ---- phase 2: missing CLI must degrade, never break a turn -----------------
if (process.argv[2] === "missing") {
	process.env.HASHTEXT_BIN = path.join(STORE, "definitely-not-here", "hashtext");
	process.env.HASHTEXT_DIR = path.join(STORE, "missing");
	const handlers: Record<string, Array<(event: any, ctx: any) => any>> = {};
	const tools: Record<string, any> = {};
	const notices: string[] = [];
	const plugin = (await import(PLUGIN)).default;
	plugin({
		on: (event: string, handler: any) => {
			(handlers[event] ??= []).push(handler);
		},
		registerTool: (def: any) => {
			tools[def.name] = def;
		},
		registerCommand: () => {},
	} as never);
	const ctx = {
		ui: { notify: (msg: string, level?: string) => notices.push(`[${level}] ${msg}`) },
		cwd: "/tmp",
		sessionManager: { getSessionId: () => SESSION },
		getContextUsage: () => ({ tokens: 10, contextWindow: 200_000, percent: 0.0001 }),
	};
	const start = { type: "before_agent_start", prompt: "", systemPrompt: "", systemPromptOptions: { sections: {} as Record<string, string> } };
	await handlers.before_agent_start[0](start, ctx);
	assert(!("hashtext" in start.systemPromptOptions.sections), "missing CLI: index section omitted, prompt unchanged");
	const expand = await tools.hashtext_expand.execute("t1", { ref: "deadbeef" }, undefined, undefined, ctx);
	assert(expand.isError && /hashtext not found|no block matching/.test(expand.content[0].text), "missing CLI: expand reports a usable error");
	const context = await handlers.context[0]({ type: "context", messages: [{ role: "user", content: "[hashtext deadbeef resident]", timestamp: 1 }] }, ctx);
	assert(context === undefined, "missing CLI: context handler leaves the transcript alone");
	assert(notices.some((notice) => notice.includes("hashtext")), "missing CLI: user is warned once");
	console.log("\nMISSING-CLI PASS");
	process.exit(0);
}

// ---- phase 1: real store ---------------------------------------------------
rmSync(STORE, { recursive: true, force: true });
process.env.HASHTEXT_BIN = BIN;
process.env.HASHTEXT_DIR = STORE;

const put = JSON.parse(
	execFileSync(BIN, ["put", "-", "--name", "ci-log", "--desc", "failing build", "--json"], {
		input: TEXT,
		env: { ...process.env },
	}).toString(),
);
const REF = put.block.id as string;
const SHA = put.block.sha256 as string;
console.log("stored:", REF, put.block.bytes, "->", put.block.stored);

const handlers: Record<string, Array<(event: any, ctx: any) => any>> = {};
const tools: Record<string, any> = {};
const commands: Record<string, any> = {};
const plugin = (await import(PLUGIN)).default;
plugin({
	on: (event: string, handler: any) => {
		(handlers[event] ??= []).push(handler);
	},
	registerTool: (def: any) => {
		tools[def.name] = def;
	},
	registerCommand: (name: string, opts: any) => {
		commands[name] = opts;
	},
} as never);

let usage = { tokens: 1000, contextWindow: 200_000, percent: 0.005 };
const notices: string[] = [];
const ctx = {
	ui: { notify: (msg: string, level?: string) => notices.push(`[${level}] ${msg}`) },
	cwd: "/tmp",
	sessionManager: { getSessionId: () => SESSION },
	getContextUsage: () => usage,
};
const fire = async (event: string, payload: any) => {
	let result: any;
	for (const handler of handlers[event] ?? []) result = (await handler(payload, ctx)) ?? result;
	return result;
};

const startEvent = {
	type: "before_agent_start",
	prompt: "hi",
	systemPrompt: "",
	systemPromptOptions: { sections: {} as Record<string, string> },
};
await fire("before_agent_start", startEvent);
assert(startEvent.systemPromptOptions.sections.hashtext?.includes(REF), "cold index lists the block ref");
assert(startEvent.systemPromptOptions.sections.hashtext?.includes("ci-log"), "index carries the name");

const expanded = await tools.hashtext_expand.execute("t1", { ref: REF }, undefined, undefined, ctx);
const marker = `[hashtext ${REF} resident]`;
assert(expanded.content[0].text.includes(marker), "expand returns the resident marker");
const residentFile = path.join(STORE, "resident", `${SESSION}.json`);
assert(JSON.parse(readFileSync(residentFile, "utf8")).refs.includes(SHA), "residency persisted to the store");

const transcript = [
	{ role: "user", content: "what broke the build?", timestamp: 1 },
	{
		role: "toolResult",
		toolCallId: "t1",
		toolName: "hashtext_expand",
		content: [{ type: "text", text: expanded.content[0].text }],
		isError: false,
		timestamp: 2,
	},
];
const rebuilt = await fire("context", { type: "context", messages: transcript });
const injected = JSON.stringify(rebuilt.messages);
assert(injected.includes("the widget exploded"), "resident block is reconstructed into context");
assert(injected.includes("— expanded"), "expansion is wrapped in a labeled block");
assert(!JSON.stringify(transcript).includes("the widget exploded"), "the transcript itself is never mutated");

await tools.hashtext_evict.execute("t2", { ref: REF }, undefined, undefined, ctx);
const afterEvict = (await fire("context", { type: "context", messages: transcript })) ?? { messages: transcript };
const kept = JSON.stringify(afterEvict.messages);
assert(!kept.includes("the widget exploded"), "evicted block content is gone");
assert(kept.includes(marker), "evicted marker stays as a one-line stub");
assert(JSON.parse(readFileSync(residentFile, "utf8")).refs.length === 0, "eviction persisted");

await tools.hashtext_expand.execute("t3", { ref: REF }, undefined, undefined, ctx);
usage = { tokens: 179_900, contextWindow: 200_000, percent: 0.8995 };
const tight = await fire("context", { type: "context", messages: transcript });
assert(JSON.stringify(tight.messages).includes("withheld"), "over-budget expansion is withheld with a note");
usage = { tokens: 1000, contextWindow: 200_000, percent: 0.005 };

const stored = await tools.hashtext_put.execute("t4", { text: TEXT, name: "dup" }, undefined, undefined, ctx);
assert(stored.details.deduped === true && stored.details.ref === REF, "identical content dedups to the same fingerprint");
const listed = await tools.hashtext_list.execute("t5", {}, undefined, undefined, ctx);
assert(listed.content[0].text.includes("[resident]"), "list shows residency");

await commands.hash.handler(`evict ${REF}`, ctx);
assert(JSON.parse(readFileSync(residentFile, "utf8")).refs.length === 0, "/hash evict clears residency");

const fresh = await tools.hashtext_put.execute("t6", { text: "unique-" + Date.now(), name: "u" }, undefined, undefined, ctx);
assert(fresh.details.ok, "new content stores under a fresh ref");

console.log("\nREAL-STORE PASS");
// Hermetic missing-CLI phase: no hashtext anywhere — empty PATH and a scratch
// HOME so the plugin's install-location fallbacks cannot resolve either.
mkdirSync(path.join(STORE, "home"), { recursive: true });
const child = spawnSync(process.execPath, [new URL(import.meta.url).pathname, "missing"], {
	env: {
		...process.env,
		HOME: path.join(STORE, "home"),
		PATH: "/usr/bin:/bin",
		HASHTEXT_BIN: path.join(STORE, "definitely-not-here", "hashtext"),
	},
	encoding: "utf8",
});
process.stdout.write(child.stdout ?? "");
process.stderr.write(child.stderr ?? "");
if (child.status !== 0) process.exit(child.status ?? 1);
console.log("\nHASHTEXT SMOKE PASS");
