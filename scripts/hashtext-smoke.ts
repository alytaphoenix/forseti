// hashtext plugin smoke gate (bun): `bun run scripts/hashtext-smoke.ts`
//
// Drives the real extension (hashtext-plugin/index.ts) against the real
// hashtext CLI + store, stubbing only the pi ExtensionAPI and the Laya
// endpoint (a local HTTP server returning canned decisions). Covers:
//   compact/tiered index · expand modes (auto/full/outline/focus/range) ·
//   per-turn reconstruction · first-occurrence-only materialization ·
//   evict · budget withholding · read/grep · put/dedup · request-time
//   archival · Laya find/auto/janitor · /hash evict · missing-CLI degradation
//
// Requires: `hashtext` on PATH (or HASHTEXT_BIN), `bun`.
import { execFileSync, spawn, spawnSync } from "node:child_process";
import { createServer } from "node:http";
import { mkdirSync, readFileSync, rmSync } from "node:fs";
import { homedir } from "node:os";
import path from "node:path";

const HERE = path.dirname(new URL(import.meta.url).pathname);
const PLUGIN = path.join(HERE, "..", "hashtext-plugin", "index.ts");
const STORE = process.env.HASHTEXT_SMOKE_DIR || path.join(process.env.TMPDIR || "/tmp", "hashtext-plugin-smoke");
const SESSION = "smoke-session";
const LARGE = "BUILD LOG — stage report\n" + Array.from({ length: 600 }, (_, i) =>
	`line ${i + 1}: ${i % 37 === 0 ? "ERROR" : "INFO"} worker-${i % 8} request id=${1000000 + i} latency=${i % 900}ms status=${i % 37 === 0 ? 500 : 200}`,
).join("\n");

const CANDIDATES = [
	"hashtext",
	path.join(homedir(), ".local", "bin", "hashtext"),
	"/opt/homebrew/bin/hashtext",
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
	console.error("hashtext CLI not found. Install it (cd ~/repos/hashtext && uv tool install .) or set HASHTEXT_BIN.");
	process.exit(1);
}

const assert = (cond: unknown, message: string) => {
	if (!cond) {
		console.error("FAIL:", message);
		process.exit(1);
	}
	console.log("ok:", message);
};

const countOccurrences = (haystack: string, needle: string): number => haystack.split(needle).length - 1;

// ---- phase 2: missing CLI must degrade, never break a turn -----------------
if (process.argv[2] === "missing") {
	process.env.HASHTEXT_BIN = path.join(STORE, "definitely-not-here", "hashtext");
	process.env.HASHTEXT_DIR = STORE; // same store: native reads must still work
	const handlers: Record<string, Array<(event: any, ctx: any) => any>> = {};
	const tools: Record<string, any> = {};
	const notices: string[] = [];
	const plugin = (await import(PLUGIN)).default;
	plugin({
		on: (event: string, handler: any) => (handlers[event] ??= []).push(handler),
		registerTool: (def: any) => (tools[def.name] = def),
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
	assert(Boolean(start.systemPromptOptions.sections.hashtext), "missing CLI: native reads still render the index");
	const expand = await tools.hashtext_expand.execute("t1", { ref: process.env.SMOKE_REF }, undefined, undefined, ctx);
	assert(!expand.isError, "missing CLI: expand still works via native reads");
	const put = await tools.hashtext_put.execute("t2", { text: "new block" }, undefined, undefined, ctx);
	assert(put.isError && /hashtext not found/.test(put.content[0].text), "missing CLI: put reports the install hint");
	assert(notices.some((notice) => notice.includes("hashtext")), "missing CLI: user is warned once");
	console.log("\nMISSING-CLI PASS");
	process.exit(0);
}

// ---- canned Laya endpoint ---------------------------------------------------
const decisions: Array<{ kind: string; choice: string; confidence: number }> = [];
const laya = createServer((req, res) => {
	let body = "";
	req.on("data", (chunk) => (body += chunk));
	req.on("end", () => {
		const parsed = JSON.parse(body || "{}");
		const criteria = parsed?.questions?.pick?.criteria ?? {};
		const keys = Object.keys(criteria).filter((key) => key !== "other" && key !== "keep");
		const state = String(parsed?.state?.input ?? "");
		const choice = state.includes("NO_MATCH") ? "other" : keys[0] ?? "other";
		decisions.push({ kind: state.slice(0, 12), choice, confidence: 0.95 });
		res.writeHead(200, { "Content-Type": "application/json" });
		res.end(JSON.stringify({ model: "mock-laya", answers: { pick: { choice, confidence: 0.95, probabilities: {} } } }));
	});
});
await new Promise<void>((resolve) => laya.listen(0, "127.0.0.1", resolve));
const layaPort = (laya.address() as { port: number }).port;

// ---- phase 1: real store ---------------------------------------------------
rmSync(STORE, { recursive: true, force: true });
process.env.HASHTEXT_BIN = BIN;
process.env.HASHTEXT_DIR = STORE;
process.env.FORSETI_LAYA_URL = `http://127.0.0.1:${layaPort}`;

const put = JSON.parse(
	execFileSync(BIN, ["put", "-", "--name", "ci-log", "--desc", "failing build output", "--json"], {
		input: LARGE,
		env: { ...process.env },
	}).toString(),
);
const REF = put.block.id as string;
const SHA = put.block.sha256 as string;
process.env.SMOKE_REF = REF;
console.log("stored:", REF, put.block.bytes, "->", put.block.stored, `(${put.block.tokens_est} tok)`);

const handlers: Record<string, Array<(event: any, ctx: any) => any>> = {};
const tools: Record<string, any> = {};
const commands: Record<string, any> = {};
const plugin = (await import(PLUGIN)).default;
plugin({
	on: (event: string, handler: any) => (handlers[event] ??= []).push(handler),
	registerTool: (def: any) => (tools[def.name] = def),
	registerCommand: (name: string, opts: any) => (commands[name] = opts),
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
const contextMessages = async (messages: any[]) =>
	(await fire("context", { type: "context", messages }))?.messages ?? messages;

const startEvent = { type: "before_agent_start", prompt: "", systemPrompt: "", systemPromptOptions: { sections: {} as Record<string, string> } };
await fire("before_agent_start", startEvent);
const index = startEvent.systemPromptOptions.sections.hashtext ?? "";
assert(index.includes(REF.slice(0, 8)), "index uses a compact ref");
assert(index.includes("ci-log"), "index carries the name");
assert(/· \d+(?:\.\d)?k? tok/.test(index), "index shows expansion cost");
assert(index.length < 700, `index stays compact (${index.length} chars)`);

// -- auto expansion returns an outline for a large block ---------------------
const expanded = await tools.hashtext_expand.execute("t1", { ref: REF }, undefined, undefined, ctx);
const marker = `[hashtext ${REF} resident]`;
assert(expanded.content[0].text.includes(marker), "expand returns the resident marker");
assert(!expanded.content[0].text.includes(LARGE.slice(0, 200)), "expand response does not inline the block");

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
const outlineMessages = await contextMessages(transcript);
const outline = JSON.stringify(outlineMessages);
assert(outline.includes("outline —"), "large block materializes as an outline (progressive disclosure)");
assert(outline.includes("stage report"), "outline carries a section preview");
assert(!outline.includes("request id=1000000"), "full body is not materialized");
assert(outline.includes("#") && outline.includes("/hashtext " + REF), "expansion is wrapped with a nonce footer");
assert(!JSON.stringify(transcript).includes("request id=1000000"), "the transcript itself is never mutated");

// -- read / grep / focus -----------------------------------------------------
const read = await tools.hashtext_read.execute("t2", { ref: REF, start: 5, end: 6 }, undefined, undefined, ctx);
assert(/lines 5-6 of/.test(read.content[0].text), "read returns an exact line range");
assert(/^5: .+/m.test(read.content[0].text), "read numbers lines");

const grep = await tools.hashtext_grep.execute("t3", { pattern: "status=500", max: 3 }, undefined, undefined, ctx);
assert(grep.details.matches === 3, "grep caps matches");
assert(/^[0-9a-f]{12}:\d+: /m.test(grep.content[0].text), "grep returns ref:line: text");

await tools.hashtext_expand.execute("t4", { ref: REF, mode: "focus", focus: "latency 599" }, undefined, undefined, ctx);
const focused = JSON.stringify(await contextMessages(transcript));
assert(focused.includes("excerpts for"), "focus mode injects excerpts");
assert(focused.includes("lines") && focused.includes("omitted"), "focused view shows omitted ranges");
assert(countOccurrences(focused, "request id=1000599") >= 1, "focused view contains the matching line");
assert(countOccurrences(focused, "request id=1000000") === 0, "focused view excludes unrelated lines");

// -- full mode + duplicate-marker back-reference -----------------------------
await tools.hashtext_expand.execute("t5", { ref: REF, mode: "full" }, undefined, undefined, ctx);
const twice = [
	{ role: "user", content: `first ${marker} second ${marker}`, timestamp: 1 },
];
const once = JSON.stringify(await contextMessages(twice));
assert(once.includes("request id=1000000"), "mode=full materializes the body");
assert(once.includes("see above"), "a repeated marker becomes a back-reference");
assert(countOccurrences(once, "request id=1000000") === 1, "content is materialized once per request");

// -- evict -------------------------------------------------------------------
await tools.hashtext_evict.execute("t6", { ref: REF }, undefined, undefined, ctx);
const evicted = JSON.stringify(await contextMessages(twice));
assert(!evicted.includes("request id=1000000"), "evicted content stops materializing");
assert(evicted.includes(marker), "evicted marker stays as a one-line stub");

// -- budget guard ------------------------------------------------------------
await tools.hashtext_expand.execute("t7", { ref: REF, mode: "full" }, undefined, undefined, ctx);
usage = { tokens: 179_900, contextWindow: 200_000, percent: 0.8995 };
assert(JSON.stringify(await contextMessages(twice)).includes("withheld"), "over-budget expansion is withheld with a note");
usage = { tokens: 1000, contextWindow: 200_000, percent: 0.005 };

// -- put / dedup / list ------------------------------------------------------
const dup = await tools.hashtext_put.execute("t8", { text: LARGE, name: "dup" }, undefined, undefined, ctx);
assert(dup.details.deduped === true && dup.details.ref === REF, "identical content dedups to the same fingerprint");
const listed = await tools.hashtext_list.execute("t9", {}, undefined, undefined, ctx);
assert(listed.content[0].text.includes("[resident]"), "list shows residency");

// -- request-time archival ---------------------------------------------------
const big = "TOOL OUTPUT START\n" + Array.from({ length: 600 }, (_, i) => `row ${i} value=${i * 7}`).join("\n");
const toolResults = [{ toolCallId: "call-1", toolName: "bash", content: [{ type: "text", text: big }], isError: false }];
await fire("turn_end", { type: "turn_end", toolResults, message: { role: "assistant" } });
const stateAfter = JSON.parse(readFileSync(path.join(STORE, "resident", `${SESSION}.json`), "utf8"));
const archived = stateAfter.archive["call-1"];
assert(Boolean(archived), "large tool output is archived to a cold block");
const liveTranscript = [
	{ role: "toolResult", toolCallId: "call-1", toolName: "bash", content: [{ type: "text", text: big }], isError: false, timestamp: 1 },
];
const projected = JSON.stringify(await contextMessages(liveTranscript));
assert(!projected.includes("row 599"), "archived tool output is projected to a stub");
assert(projected.includes(archived.id), "the stub carries the block ref");
const restored = await tools.hashtext_expand.execute("t10", { ref: archived.id }, undefined, undefined, ctx);
assert(!restored.isError, "archived block can be expanded again");
const restoredContext = JSON.stringify(await contextMessages(liveTranscript));
assert(restoredContext.includes("row 599") || restoredContext.includes("outline"), "expanding the archive restores its content");

// -- /hash evict -------------------------------------------------------------
await commands.hash.handler(`evict ${REF}`, ctx);
const residentRefs = JSON.parse(readFileSync(path.join(STORE, "resident", `${SESSION}.json`), "utf8")).blocks.map((b: any) => b.sha);
assert(!residentRefs.includes(SHA), "/hash evict clears residency");

// -- Laya: find / auto / janitor --------------------------------------------
await commands.hash.handler("auto on", ctx);
const found = await tools.hashtext_find.execute("t11", { query: "ci log failing build output" }, undefined, undefined, ctx);
assert(found.details.pick === REF, "laya picks the top shortlisted block");

const telemetry = readFileSync(path.join(STORE, "decisions.jsonl"), "utf8")
	.trim()
	.split("\n")
	.map((line) => JSON.parse(line));
const findEntry = telemetry.find((entry: any) => entry.kind === "find" && entry.pick === REF);
assert(Boolean(findEntry), "telemetry logs the find: query, candidate set, pick");
assert(Array.isArray(findEntry?.candidates) && findEntry.candidates.length > 0, "telemetry stores candidate details");
assert(typeof findEntry?.confidence === "number", "telemetry stores confidence");

const autoStart = { type: "before_agent_start", prompt: "ci log failing build output", systemPrompt: "", systemPromptOptions: { sections: {} as Record<string, string> } };
await fire("before_agent_start", autoStart);
const autoState = JSON.parse(readFileSync(path.join(STORE, "resident", `${SESSION}.json`), "utf8"));
assert(autoState.blocks.some((entry: any) => entry.sha === SHA), "auto mode expands a confident pick");
assert(Boolean(autoState.settings.auto), "auto setting is persisted");
const outcome = readFileSync(path.join(STORE, "decisions.jsonl"), "utf8")
	.trim()
	.split("\n")
	.map((line) => JSON.parse(line))
	.find((entry: any) => entry.kind === "outcome" && entry.action === "expand" && entry.ref === REF && entry.matched_pick === true);
assert(Boolean(outcome), "telemetry attributes the expand to the Laya pick");

const cacheHits = await tools.hashtext_put.execute("t12", { text: "second block for janitor", name: "second" }, undefined, undefined, ctx);
const third = await tools.hashtext_put.execute("t13", { text: "third block for janitor", name: "third" }, undefined, undefined, ctx);
await tools.hashtext_expand.execute("t14", { ref: cacheHits.details.ref }, undefined, undefined, ctx);
await tools.hashtext_expand.execute("t15", { ref: third.details.ref }, undefined, undefined, ctx);
assert(decisions.length > 0, "laya received decisions");
const beforeJanitor = JSON.parse(readFileSync(path.join(STORE, "resident", `${SESSION}.json`), "utf8")).blocks.length;
await fire("turn_end", { type: "turn_end", toolResults: [], message: { role: "assistant" } });
const afterJanitor = JSON.parse(readFileSync(path.join(STORE, "resident", `${SESSION}.json`), "utf8")).blocks.length;
assert(beforeJanitor >= 3 && afterJanitor < beforeJanitor, "laya janitor evicts one resident block");

// -- native/CLI parity -------------------------------------------------------
await tools.hashtext_expand.execute("t16", { ref: REF, mode: "full" }, undefined, undefined, ctx);
const restoredFull = JSON.stringify(await contextMessages([{ role: "user", content: marker, timestamp: 1 }]));
const cliText = execFileSync(BIN, ["show", REF, "--json"], { env: { ...process.env } }).toString();
const cliBody = JSON.parse(cliText).blocks[0].text as string;
const sampleLine = cliBody.split("\n")[10];
assert(Boolean(sampleLine) && restoredFull.includes(sampleLine), "native read matches the CLI payload");

console.log("\nREAL-STORE PASS");

// ---- phase 3: dataset + eval scaffold (offline, deterministic) -------------
const datasetOut = path.join(STORE, "gate-eval.jsonl");
const dataset = spawnSync(
	process.execPath,
	["run", path.join(HERE, "hashtext-dataset.ts"), "--synthetic", "20", "--out", datasetOut],
	{ env: { ...process.env, HASHTEXT_DIR: STORE }, encoding: "utf8" },
);
if (dataset.status !== 0) {
	process.stderr.write(dataset.stderr ?? "");
	console.error("FAIL: dataset builder");
	process.exit(1);
}
const evalRun = spawnSync(
	process.execPath,
	["run", path.join(HERE, "hashtext-eval.ts"), "--data", datasetOut, "--offline", "--json"],
	{ env: { ...process.env, HASHTEXT_DIR: STORE }, encoding: "utf8" },
);
if (evalRun.status !== 0) {
	process.stderr.write(evalRun.stderr ?? "");
	console.error("FAIL: eval harness");
	process.exit(1);
}
const report = JSON.parse((evalRun.stdout ?? "").slice((evalRun.stdout ?? "").indexOf("{")));
assert(report.rows > 0, "dataset builder produced eval rows");
assert(report.decision_rows > 0, "decision rows are separated from retrieval probes");
assert(report.recall_at_k >= 0.9, `content-aware shortlist recall@k = ${(report.recall_at_k * 100).toFixed(0)}%`);
assert(typeof report.ece === "number", "eval reports calibration (ECE)");
console.log("\nDATASET/EVAL PASS");

function runAsync(command: string, argv: string[], env: Record<string, string | undefined>): Promise<{ status: number | null; stdout: string; stderr: string }> {
	return new Promise((resolve) => {
		const child = spawn(command, argv, { env });
		let stdout = "";
		let stderr = "";
		child.stdout.on("data", (chunk) => (stdout += chunk));
		child.stderr.on("data", (chunk) => (stderr += chunk));
		child.on("close", (status) => resolve({ status, stdout, stderr }));
		child.on("error", () => resolve({ status: 1, stdout, stderr }));
	});
}

// ---- phase 3b: calibration fitter -----------------------------------------
// Async spawn (not spawnSync): the canned Laya endpoint runs in this process,
// and a blocking spawn would starve its event loop.
const calRun = await runAsync(
	process.execPath,
	["run", path.join(HERE, "hashtext-calibrate.ts"), "--data", datasetOut, "--min", "3", "--write", "--json"],
	{ ...process.env, HASHTEXT_DIR: STORE },
);
if (calRun.status !== 0) {
	process.stderr.write(calRun.stderr ?? "");
	console.error("FAIL: calibration fitter");
	process.exit(1);
}
const calReport = JSON.parse((calRun.stdout ?? "").slice((calRun.stdout ?? "").indexOf("{")));
assert(calReport.rows_in > 0, "calibration reads decision samples");
assert(Array.isArray(calReport.points) && calReport.points.length >= 2, "calibration emits a monotone map");
assert(typeof calReport.written === "string" && calReport.written.includes("calibration.json"), "calibration artifact written");
assert(calReport.ece_calibrated <= calReport.ece_raw + 0.001, "calibration does not worsen ECE on the eval rows");
console.log("\nCALIBRATION PASS");

// ---- phase 3: missing CLI (same store; native reads must survive) ----------
mkdirSync(path.join(STORE, "home"), { recursive: true });
const child = spawnSync(process.execPath, [new URL(import.meta.url).pathname, "missing"], {
	env: {
		...process.env,
		HOME: path.join(STORE, "home"),
		PATH: "/usr/bin:/bin",
		HASHTEXT_BIN: path.join(STORE, "definitely-not-here", "hashtext"),
		HASHTEXT_DIR: STORE,
		SMOKE_REF: REF,
	},
	encoding: "utf8",
});
process.stdout.write(child.stdout ?? "");
process.stderr.write(child.stderr ?? "");
laya.close();
if (child.status !== 0) process.exit(child.status ?? 1);
console.log("\nHASHTEXT SMOKE PASS");
