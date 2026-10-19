// Forseti flow runtime (Phase 26 F2/F3).
//
// Loads .forseti/flow.yaml for the session's project and enforces it:
//   * tool_call: pre_tool deny patterns + pre_edit gates (hard block)
//   * turn_end / agent_before_settle: post_turn gates (continue with a
//     remediation message, bounded by defaults.max_continues)
//   * before_agent_start / input: the active phase + evidence checklist
//   * /flow status|next|review|waive|init|explain|abort
//   * tools: flow_next, flow_review, flow_complete
//
// Agentic phases are driven by the flow itself: flow_review runs a FRESH
// `pi -p` process (never an in-session self-review), writes its artifact and
// returns the verdict; flow_next evaluates the phase gate and applies the
// verdict edges.
//
// The extension shells out to forseti-flow for evidence evaluation, state and
// waivers, so the CLI and the session share one cache and one ledger.
import type { ExtensionAPI, ExtensionContext } from "@earendil-works/pi-coding-agent";
import { Type } from "typebox";
import { existsSync, mkdirSync, readFileSync, renameSync, unlinkSync, writeFileSync } from "node:fs";
import { dirname, isAbsolute, join } from "node:path";
import { homedir } from "node:os";

// ---------------------------------------------------------------------------
// Types (mirror the flow JSON emitted by `forseti-flow validate --json`)

interface Evidence {
	kind: string;
	run?: string;
	file?: string;
	verdict?: { file: string; labels: string[] };
	role?: string;
	criteria?: string;
	inputs?: string[];
	model?: string;
}

interface Rule {
	require?: string[];
	severity?: string;
	deny?: { match: string; reason?: string }[];
	pattern?: string;
}

interface Phase {
	id: string;
	actor?: string;
	role?: string;
	prompt?: string;
	run?: string;
	from?: string[];
	produce?: string[];
	inputs?: string[];
	gate?: { require?: string[]; severity?: string };
	max_visits?: number;
	model?: string;
	route?: string;
	criteria?: string;
	verdict?: Record<string, string>;
}

interface Flow {
	id: string;
	mode?: string;
	trigger?: string;
	defaults?: { severity?: string; max_continues?: number; max_cycles?: number; timeout?: string };
	evidence?: Record<string, Evidence>;
	on?: Record<string, Rule>;
	phases?: Phase[];
	waivers?: { enabled?: boolean; ledger?: string; ttl?: string; never?: string[] };
}

interface FlowFile {
	version: number;
	flows: Flow[];
}

interface CheckEvidence {
	id: string;
	status: string;
	source?: string;
	detail?: string;
}

interface CheckResult {
	flow: string;
	stage: string;
	ok: boolean;
	blocked: boolean;
	messages?: string[];
	warnings?: string[];
	evidence?: CheckEvidence[];
}

interface Runtime {
	path: string;
	bin: string;
	cwd: string;
	file: FlowFile;
	error: string | null;
	aborted: boolean;
	phase: string | null;
	visits: Record<string, number>;
	completed: Set<string>;
	continues: Record<string, number>;
	warned: Record<string, boolean>;
	human: Record<string, boolean>;
	blocked: boolean;
	statusPath: string;
	session: string;
	lastContinueAt: number;
	checkCache: Map<string, { at: number; result: CheckResult | null }>;
}

// ---------------------------------------------------------------------------
// Discovery

function findFlowFile(cwd: string): string | null {
	let dir = cwd;
	for (;;) {
		const p = join(dir, ".forseti", "flow.yaml");
		if (existsSync(p)) return p;
		const parent = dirname(dir);
		if (parent === dir) return null;
		dir = parent;
	}
}

function walkUpForBin(start: string): string {
	let dir = start;
	for (;;) {
		const p = join(dir, "crew", "bin", "forseti-flow");
		if (existsSync(p)) return p;
		const parent = dirname(dir);
		if (parent === dir) return "";
		dir = parent;
	}
}

function findBin(cwd: string): string {
	const candidates = [
		process.env.FORSETI_FLOW_BIN,
		process.env.FORSETI_REPO ? join(process.env.FORSETI_REPO, "crew", "bin", "forseti-flow") : "",
		walkUpForBin(cwd),
		join(homedir(), ".local", "bin", "forseti-flow"),
		join(cwd, "crew", "bin", "forseti-flow"),
	].filter(Boolean) as string[];
	for (const c of candidates) if (existsSync(c)) return c;
	return "forseti-flow";
}

function stripAnsi(s: string): string {
	return s.replace(/\x1b\[[0-9;]*m/g, "");
}

// A missing binary must degrade the flow, never break the session.
async function safeExec(pi: ExtensionAPI, command: string, args: string[], options?: any): Promise<{ stdout: string; stderr: string; code: number; killed: boolean }> {
	try {
		return await pi.exec(command, args, options);
	} catch (e) {
		return { stdout: "", stderr: e instanceof Error ? e.message : String(e), code: 127, killed: false };
	}
}

async function loadRuntime(pi: ExtensionAPI, ctx: ExtensionContext): Promise<Runtime | null> {
	if (process.env.FORSETI_FLOW_DISABLE) return null;
	if (!ctx.isProjectTrusted()) return null;
	const path = findFlowFile(ctx.cwd);
	if (!path) return null;
	const bin = findBin(ctx.cwd);
	const root = path.endsWith(join(".forseti", "flow.yaml")) ? dirname(dirname(path)) : ctx.cwd;
	const rt: Runtime = {
		path, bin, cwd: root, file: { version: 0, flows: [] }, error: null, aborted: false,
		phase: null, visits: {}, completed: new Set(), continues: {}, warned: {}, human: {}, blocked: false,
		statusPath: join(root, ".forseti", "flow-status.json"), session: ctx.sessionManager.getSessionId(),
		lastContinueAt: 0, checkCache: new Map(),
	};
	const res = await safeExec(pi, bin, ["validate", "-f", path, "--json"], { cwd: root });
	if (res.code !== 0) {
		rt.error = stripAnsi((res.stderr || res.stdout || "flow validation failed").trim());
		return rt;
	}
	try {
		rt.file = JSON.parse(res.stdout) as FlowFile;
	} catch (e) {
		rt.error = `flow JSON: ${e instanceof Error ? e.message : String(e)}`;
		return rt;
	}
	const active = activeFlow(rt);
	rt.phase = active ? entryPhase(active) : null;
	return rt;
}

function publishStatus(rt: Runtime | null): void {
	if (!rt || rt.error) {
		if (rt?.error) {
			try {
				mkdirSync(dirname(rt.statusPath), { recursive: true });
				writeFileSync(rt.statusPath, JSON.stringify({
					flow: "?", mode: "repo", phase: "", status: "error", error: rt.error,
					session: rt.session, ts: new Date().toISOString(),
				}), "utf8");
			} catch { /* advisory */ }
		}
		return;
	}
	const active = activeFlow(rt);
	const total = active?.phases?.length ?? 0;
	const done = rt.completed.size;
	let status = rt.aborted ? "aborted" : rt.blocked ? "blocked" : "active";
	if (!rt.aborted && !rt.blocked && total > 0 && rt.phase === null && done >= total) status = "done";
	const data = {
		flow: active?.id ?? rt.file.flows[0]?.id ?? "?",
		mode: active?.mode ?? rt.file.flows[0]?.mode ?? "repo",
		phase: rt.phase ?? "",
		total, done, status,
		agent: process.env.FORSETI_AGENT_NAME || "",
		session: rt.session,
		ts: new Date().toISOString(),
	};
	try {
		mkdirSync(dirname(rt.statusPath), { recursive: true });
		const tmp = `${rt.statusPath}.${process.pid}.tmp`;
		writeFileSync(tmp, JSON.stringify(data), "utf8");
		renameSync(tmp, rt.statusPath);
	} catch { /* advisory */ }
}

function clearStatus(rt: Runtime | null): void {
	if (!rt) return;
	try {
		const cur = JSON.parse(readFileSync(rt.statusPath, "utf8"));
		if (cur.session === rt.session) unlinkSync(rt.statusPath);
	} catch { /* missing or foreign */ }
}

function activeFlow(rt: Runtime): Flow | null {
	return rt.file.flows.find((f) => (f.phases?.length ?? 0) > 0) ?? null;
}

function entryPhase(flow: Flow): string | null {
	const phases = flow.phases ?? [];
	const entry = phases.find((p) => !(p.from?.length));
	return entry?.id ?? phases[0]?.id ?? null;
}

function phaseOf(flow: Flow, id: string | null): Phase | undefined {
	if (!id) return undefined;
	return (flow.phases ?? []).find((p) => p.id === id);
}

function phaseIndex(flow: Flow, id: string): number {
	return (flow.phases ?? []).findIndex((p) => p.id === id);
}

// ---------------------------------------------------------------------------
// Evidence + checks

function checkKey(flow: Flow, stage: string): string {
	return `${flow.id}:${stage}`;
}

async function runCheck(pi: ExtensionAPI, rt: Runtime, flow: Flow, stage: string, assumed: string[] = []): Promise<CheckResult | null> {
	const key = checkKey(flow, stage) + ":" + assumed.join(",");
	const cached = rt.checkCache.get(key);
	const now = Date.now();
	if (cached && now - cached.at < 1500) return cached.result;
	const args = ["check", "--flow", flow.id, "--stage", stage, "--dir", rt.cwd, "--json"];
	for (const id of assumed) args.push("--assume", id);
	const res = await safeExec(pi, rt.bin, args, { cwd: rt.cwd, timeout: 600_000 });
	if (res.code === 2) {
		rt.checkCache.set(key, { at: now, result: null });
		return null;
	}
	let result: CheckResult | null = null;
	try {
		result = JSON.parse(res.stdout) as CheckResult;
	} catch {
		result = null;
	}
	rt.checkCache.set(key, { at: now, result });
	if (result?.blocked) rt.blocked = true;
	publishStatus(rt);
	return result;
}

async function evalEvidence(pi: ExtensionAPI, rt: Runtime, flow: Flow, id: string, assumed: string[] = []): Promise<CheckEvidence | null> {
	const args = ["eval", "--flow", flow.id, "--evidence", id, "--dir", rt.cwd, "--json"];
	for (const a of assumed) args.push("--assume", a);
	const res = await safeExec(pi, rt.bin, args, { cwd: rt.cwd, timeout: 600_000 });
	if (res.code === 2) return null;
	try {
		return JSON.parse(res.stdout) as CheckEvidence;
	} catch {
		return null;
	}
}

async function confirmHumans(pi: ExtensionAPI, ctx: ExtensionContext, rt: Runtime, flow: Flow, ids: string[]): Promise<string[]> {
	if (!ctx.hasUI) return [];
	const assumed: string[] = [];
	for (const id of ids) {
		const e = flow.evidence?.[id];
		if (!e || e.kind !== "human") continue;
		if (rt.human[id] === undefined) {
			rt.human[id] = await ctx.ui.confirm("Forseti flow", e.prompt || `Confirm ${id}?`);
			rt.checkCache.clear();
		}
		if (rt.human[id]) assumed.push(id);
	}
	return assumed;
}

async function missingEvidence(pi: ExtensionAPI, rt: Runtime, flow: Flow, ids: string[], assumed: string[] = []): Promise<string[]> {
	const missing: string[] = [];
	for (const id of ids) {
		const e = await evalEvidence(pi, rt, flow, id, assumed);
		if (!e || (e.status !== "pass" && e.status !== "waived")) {
			missing.push(`${id}: ${e ? `${e.status} — ${e.detail ?? ""}` : "evaluation failed"}`);
		}
	}
	return missing;
}

function reasonFor(flow: Flow, r: CheckResult): string {
	const parts = (r.messages ?? []).map((m) => m);
	if (parts.length === 0 && r.evidence) {
		for (const e of r.evidence) if (e.status !== "pass" && e.status !== "waived") parts.push(`${e.id} ${e.status}: ${e.detail ?? ""}`);
	}
	return `forseti flow ${flow.id} (${r.stage}) blocked: ${parts.join("; ") || "gate not satisfied"}`;
}

// ---------------------------------------------------------------------------
// pre_tool deny matching

function toolText(event: any): string {
	const input = event?.input ?? {};
	if (typeof input.command === "string") return input.command;
	if (typeof input.path === "string") return input.path;
	try {
		return JSON.stringify(input);
	} catch {
		return "";
	}
}

function normalizeShell(cmd: string): string {
	return cmd
		.replace(/\bgit\s+-C\s+(\S+|"[^"]*"|'[^']*')\s+/g, "git ")
		.replace(/^\s*sudo\s+/, "")
		.replace(/\s+/g, " ")
		.trim();
}

function matchesDeny(match: string, event: any): boolean {
	const idx = match.indexOf(":");
	if (idx > 0) {
		const prefix = match.slice(0, idx);
		const value = match.slice(idx + 1);
		const text = toolText(event);
		if (prefix === "bash") {
			return event.toolName === "bash" && (text.includes(value) || normalizeShell(text).includes(value));
		}
		if (prefix === "bash-re") {
			try {
				return event.toolName === "bash" && new RegExp(value, "i").test(normalizeShell(text));
			} catch {
				return false;
			}
		}
		if (prefix === "edit" || prefix === "write") {
			return event.toolName === prefix && text.includes(value);
		}
		if (prefix === "edit-re" || prefix === "write-re") {
			try {
				return event.toolName === prefix.slice(0, -3) && new RegExp(value, "i").test(text);
			} catch {
				return false;
			}
		}
		if (prefix === "tool") return event.toolName === value;
		if (prefix === "tool-re") {
			try {
				return new RegExp(value, "i").test(event.toolName);
			} catch {
				return false;
			}
		}
	}
	return toolText(event).includes(match);
}

function denyReason(flows: Flow[], event: any): string | null {
	for (const f of flows) {
		for (const d of f.on?.pre_tool?.deny ?? []) {
			if (matchesDeny(d.match, event)) {
				return `forseti flow ${f.id}: ${d.reason ?? `blocked by pre_tool (${d.match})`}`;
			}
		}
	}
	return null;
}

// ---------------------------------------------------------------------------
// Rendering

function renderSection(rt: Runtime): string {
	if (rt.error) return `## Forseti flow\nFlow file ${rt.path} is invalid: ${rt.error}`;
	if (!rt.file.flows.length) return "";
	const lines: string[] = ["## Forseti flow", `Project flow file: ${rt.path}`];
	for (const f of rt.file.flows) {
		const stages = Object.keys(f.on ?? {}).filter((s) => s !== "pre_tool").join(", ") || "none";
		const evidence = Object.keys(f.evidence ?? {}).join(", ") || "none";
		lines.push(`- **${f.id}** (${f.mode ?? "repo"}): lifecycle gates: ${stages}; evidence: ${evidence}.`);
	}
	const active = activeFlow(rt);
	const p = phaseOf(active, rt.phase);
	if (active && p) {
		lines.push(`Current phase: **${p.id}** (${p.actor ?? "agent"}).`);
		if (p.prompt) lines.push(`Phase instruction: ${p.prompt}`);
		if (p.criteria) lines.push(`Acceptance criteria: ${p.criteria}`);
		if (p.gate?.require?.length) lines.push(`Phase gate: ${p.gate.require.join(", ")} (${p.gate.severity ?? "flow default"}).`);
		if (p.verdict) lines.push(`Verdict labels: ${Object.keys(p.verdict).join(", ")} → call flow_review.`);
		lines.push("Call `flow_next` when this phase is done; `flow_review` for a reviewer phase; `flow_complete` at the end. The flow blocks the turn until the gate is satisfied.");
	} else {
		lines.push("Repo mode: lifecycle gates are enforced at the tool/commit/CI boundaries; no phase tools are active.");
	}
	lines.push("The user can run `/flow status`, `/flow next`, `/flow review`, `/flow waive <stage> <evidence> <reason>`, `/flow abort`.");
	return lines.join("\n");
}

function phaseRemediation(flow: Flow, p: Phase, missing: string[]): string {
	const head = `Forseti flow ${flow.id} phase "${p.id}" is not finished.`;
	const body = missing.length
		? `Missing evidence:\n- ${missing.join("\n- ")}`
		: "The phase gate is not satisfied.";
	const action = p.actor === "reviewer"
		? "Call `flow_review` to run the fresh reviewer."
		: "Finish the phase work, then call `flow_next`.";
	return `${head}\n${body}\n${action}\nIf this is intentional, tell the user to run /flow waive (or /flow abort).`;
}

// ---------------------------------------------------------------------------
// Agentic state machine

function reviewEvidence(flow: Flow): { id: string; e: Evidence } | null {
	for (const [id, e] of Object.entries(flow.evidence ?? {})) {
		if (e.kind === "review") return { id, e };
	}
	return null;
}

function reviewArtifact(rt: Runtime, flow: Flow, p: Phase): string {
	const rev = reviewEvidence(flow);
	const file = rev?.e.verdict?.file || `.forseti/out/review-${p.id}.md`;
	return isAbsolute(file) ? file : join(rt.cwd, file);
}

function parseVerdict(text: string): string | null {
	const lines = [...text.matchAll(/^[ \t>*-]*VERDICT[ \t]*:[ \t]*([A-Za-z0-9][A-Za-z0-9_-]*)/gim)];
	if (lines.length) return lines[lines.length - 1][1].toLowerCase();
	const bare = [...text.matchAll(/^[ \t>*-]*(LGTM|LBTM)[ \t]*$/gim)];
	if (bare.length) return bare[bare.length - 1][1].toLowerCase();
	return null;
}

async function advance(pi: ExtensionAPI, rt: Runtime, ctx: ExtensionContext): Promise<string> {
	const flow = activeFlow(rt);
	if (!flow) return "No agentic flow is active (repo mode only).";
	const p = phaseOf(flow, rt.phase);
	if (!p) return "No active phase.";
	if (p.gate?.require?.length) {
		const assumed = await confirmHumans(pi, ctx, rt, flow, p.gate.require);
		const missing = await missingEvidence(pi, rt, flow, p.gate.require, assumed);
		if (missing.length) return phaseRemediation(flow, p, missing);
	}
	if (p.actor === "reviewer" && !existsSync(reviewArtifact(rt, flow, p))) {
		return `Phase "${p.id}" is a reviewer phase. Call flow_review first.`;
	}
	const phases = flow.phases ?? [];
	const current = phaseIndex(flow, p.id);
	let target: Phase | undefined;
	if (p.verdict) {
		const artifact = reviewArtifact(rt, flow, p);
		let label: string | null = null;
		try {
			label = parseVerdict(readFileSync(artifact, "utf8"));
		} catch { /* missing */ }
		if (!label) return `No verdict in ${artifact}; call flow_review.`;
		const targetID = p.verdict[label];
		if (!targetID) return `Verdict "${label}" has no edge in phase "${p.id}".`;
		target = phaseOf(flow, targetID);
		if (!target) return `Verdict "${label}" targets unknown phase "${targetID}".`;
		if (phaseIndex(flow, target.id) <= current) {
			rt.visits[target.id] = (rt.visits[target.id] ?? 0) + 1;
			const max = p.max_visits ?? flow.defaults?.max_cycles ?? 3;
			if (rt.visits[target.id] > max) {
				return `Loop exhausted: ${target.id} visited ${rt.visits[target.id]} times (max ${max}). Stop and ask the user.`;
			}
		}
	} else {
		target = phases.find((q) =>
			(q.from ?? []).includes(p.id) &&
			(q.from ?? []).every((f) => f === p.id || rt.completed.has(f)),
		);
	}
	rt.completed.add(p.id);
	rt.blocked = false;
	publishStatus(rt);
	if (!target) {
		return `Flow ${flow.id} complete: no phase follows "${p.id}".`;
	}
	// A loop back re-opens the target and everything after it.
	const targetIdx = phaseIndex(flow, target.id);
	for (const id of [...rt.completed]) {
		if (phaseIndex(flow, id) >= targetIdx) rt.completed.delete(id);
	}
	rt.phase = target.id;
	publishStatus(rt);
	const gate = target.gate?.require?.length ? ` Gate: ${target.gate.require.join(", ")}.` : "";
	return `Next phase: **${target.id}** (${target.actor ?? "agent"}).\n${target.prompt ?? target.criteria ?? ""}${gate}`;
}

async function runReview(pi: ExtensionAPI, rt: Runtime): Promise<string> {
	const flow = activeFlow(rt);
	if (!flow) return "No agentic flow is active.";
	const p = phaseOf(flow, rt.phase);
	if (!p) return "No active phase.";
	if (p.actor !== "reviewer") return `Phase "${p.id}" is not a reviewer phase.`;
	const rev = reviewEvidence(flow);
	const labels = Object.keys(p.verdict ?? {}).length
		? Object.keys(p.verdict ?? {})
		: rev?.e.verdict?.labels ?? ["lgtm", "lbtm"];
	let context = "";
	if ((p.inputs ?? []).includes("diff")) {
		const diff = await safeExec(pi, "git", ["diff", "--no-color", "HEAD"], { cwd: rt.cwd, timeout: 30_000 });
		context = (diff.code === 0 ? diff.stdout : "").slice(0, 12_000);
	}
	const prompt = [
		`You are a reviewer for the Forseti flow "${flow.id}", phase "${p.id}".`,
		p.criteria ? `Acceptance criteria: ${p.criteria}` : "Acceptance criteria: no blocking defect remains.",
		context ? `Review this diff:\n\n${context}` : "Review the working tree for correctness, tests, docs drift, and scope creep.",
		"List defects worst-first (what is wrong, why it matters, the fix). If nothing material is wrong, say so plainly.",
		`End with exactly one line: ${labels.map((l) => `VERDICT: ${l.toUpperCase()}`).join(" or ")}.`,
	].join("\n\n");
	const model = p.model || rev?.e.model || process.env.FORSETI_FLOW_MODEL || "opencode-go/beam-free";
	const piBin = process.env.FORSETI_PI_BIN || "pi";
	const res = await safeExec(pi, "env", [
		"FORSETI_FLOW_DISABLE=1", piBin, "-p", prompt, "--model", model, "-a",
	], { cwd: rt.cwd, timeout: 600_000 });
	const output = stripAnsi((res.stdout || res.stderr || "").trim());
	if (res.code !== 0 || !output) return `Reviewer failed (exit ${res.code}): ${output.slice(0, 2000)}`;
	const artifact = reviewArtifact(rt, flow, p);
	mkdirSync(dirname(artifact), { recursive: true });
	writeFileSync(artifact, output + "\n", "utf8");
	const verdict = parseVerdict(output);
	return verdict
		? `verdict: ${verdict}\nartifact: ${artifact}\n\n${output.slice(0, 4000)}`
		: `No VERDICT line from the reviewer. Artifact: ${artifact}\n\n${output.slice(0, 4000)}`;
}

// ---------------------------------------------------------------------------
// Settle gate

function continueEntry(text: string): any {
	return { type: "custom_message", customType: "forseti-flow", content: text, display: true };
}

async function settle(pi: ExtensionAPI, rt: Runtime, ctx: ExtensionContext, event: any): Promise<any> {
	if (rt.error || rt.aborted) return;
	if (event?.outcome && event.outcome !== "completed") return;
	const now = Date.now();
	if (now - rt.lastContinueAt < 1200) return;

	// 1. The active phase's gate (agentic mode).
	const flow = activeFlow(rt);
	const p = phaseOf(flow, rt.phase);
	if (flow && p) {
		const missing: string[] = [];
		if (p.gate?.require?.length) {
			const assumed = await confirmHumans(pi, ctx, rt, flow, p.gate.require);
			missing.push(...(await missingEvidence(pi, rt, flow, p.gate.require, assumed)));
		}
		const needsReview = p.actor === "reviewer" && !existsSync(reviewArtifact(rt, flow, p));
		if (missing.length || needsReview) {
			rt.blocked = true;
			publishStatus(rt);
			const limit = flow.defaults?.max_continues ?? 2;
			const used = rt.continues[flow.id] ?? 0;
			if (used < limit) {
				rt.continues[flow.id] = used + 1;
				rt.lastContinueAt = now;
				const text = phaseRemediation(flow, p, missing);
				ctx.ui.notify(`forseti flow: ${p.id} is not finished (continuing)`, "warning");
				return { continue: true, entries: [continueEntry(text)] };
			}
			ctx.ui.notify(`forseti flow: ${p.id} still unsatisfied after ${limit} continuations — /flow abort to stop`, "warning");
		}
	}

	// 2. Repo-mode post_turn gates.
	for (const f of rt.file.flows) {
		if (!f.on?.post_turn) continue;
		const assumed = await confirmHumans(pi, ctx, rt, f, f.on.post_turn.require ?? []);
		const r = await runCheck(pi, rt, f, "post_turn", assumed);
		if (!r) continue;
		if (r.blocked) {
			const limit = f.defaults?.max_continues ?? 2;
			const used = rt.continues[f.id] ?? 0;
			if (used < limit) {
				rt.continues[f.id] = used + 1;
				rt.lastContinueAt = now;
				ctx.ui.notify(`forseti flow: ${f.id} post_turn gate is not satisfied (continuing)`, "warning");
				return { continue: true, entries: [continueEntry(`${reasonFor(f, r)}\n\n${(r.messages ?? []).join("\n")}\nFix the missing evidence, then finish. /flow waive <stage> <evidence> <reason> to override.`)] };
			}
			ctx.ui.notify(`forseti flow: ${f.id} post_turn still blocked after ${limit} continuations — /flow abort to stop`, "warning");
		} else if ((r.warnings?.length ?? 0) > 0 && !rt.warned[f.id]) {
			rt.warned[f.id] = true;
			ctx.ui.notify(`forseti flow: ${f.id} warning — ${(r.warnings ?? []).join("; ")}`, "warning");
		}
	}
	rt.blocked = false;
	publishStatus(rt);
}

// ---------------------------------------------------------------------------
// Extension factory

export default function forsetiFlow(pi: ExtensionAPI) {
	let runtime: Runtime | null = null;
	// Models can issue several tool calls in one batch; serialize every state
	// transition so flow_next/flow_review/settle cannot interleave.
	let opChain: Promise<unknown> = Promise.resolve();
	function serial<T>(fn: () => Promise<T>): Promise<T> {
		const run = opChain.then(fn, fn);
		opChain = run.then(() => undefined, () => undefined);
		return run;
	}

	async function refresh(ctx: ExtensionContext): Promise<void> {
		runtime = await loadRuntime(pi, ctx);
		publishStatus(runtime);
		if (runtime?.error) ctx.ui.notify(`forseti flow: ${runtime.error}`, "error");
		else if (runtime) { publishStatus(runtime); ctx.ui.notify(`forseti flow: ${runtime.file.flows.map((f) => `${f.id} (${f.mode ?? "repo"})`).join(", ")}`, "info"); }
	}

	pi.on("session_start", async (_event, ctx) => {
		await refresh(ctx);
	});
	pi.on("session_shutdown", async () => {
		clearStatus(runtime);
		runtime = null;
	});

	pi.on("before_agent_start", async (event) => {
		const opts = (event as any).systemPromptOptions;
		if (!opts?.sections) return;
		if (runtime) opts.sections["forseti_flow"] = renderSection(runtime);
		else delete opts.sections["forseti_flow"];
	});

	pi.on("input", async (event) => {
		if (!runtime || runtime.aborted) return { action: "continue" };
		const text = event.text ?? "";
		if (text.trimStart().startsWith("/") || text.trim() === "") return { action: "continue" };
		runtime.continues = {};
		runtime.warned = {};
		const active = activeFlow(runtime);
		const label = active && runtime.phase
			? `[Forseti flow ${active.id} · phase ${runtime.phase}]`
			: `[Forseti flow ${runtime.file.flows.map((f) => f.id).join(", ")}]`;
		return { action: "transform", text: `${label}\n\n${text}` };
	});

	pi.on("tool_call", async (event, ctx) => {
		const rt = runtime;
		if (!rt || rt.error || rt.aborted) return;
		const deny = denyReason(rt.file.flows, event);
		if (deny) { publishStatus(rt); return { block: true, reason: deny }; }
		if (event.toolName === "edit" || event.toolName === "write") {
			for (const f of rt.file.flows) {
				if (!f.on?.pre_edit) continue;
				const assumed = await confirmHumans(pi, ctx, rt, f, f.on.pre_edit.require ?? []);
				const r = await runCheck(pi, rt, f, "pre_edit", assumed);
				if (r?.blocked) {
					ctx.ui.notify(`forseti flow: pre_edit gate blocked ${event.toolName}`, "warning");
					return { block: true, reason: reasonFor(f, r) };
				}
			}
		}
	});

	pi.on("turn_end", async (event, ctx) => {
		const rt = runtime;
		if (!rt) return;
		return serial(() => settle(pi, rt, ctx, event));
	});
	pi.on("agent_before_settle", async (event, ctx) => {
		const rt = runtime;
		if (!rt) return;
		return serial(() => settle(pi, rt, ctx, event));
	});

	// ---- tools -------------------------------------------------------------
	pi.registerTool({
		name: "flow_next",
		label: "flow_next",
		description: "Advance the active Forseti flow phase after its gate is satisfied. Call this when the current phase's work is done.",
		parameters: Type.Object({}),
		async execute(_toolCallId: string, _params: unknown, ctx: ExtensionContext) {
			const rt = runtime;
			if (!rt) return { content: [{ type: "text", text: "No Forseti flow is active." }], details: { ok: false } };
			if (!activeFlow(rt)) return { content: [{ type: "text", text: "This is a repo-mode flow; finish normally (lifecycle gates run at the boundaries)." }], details: { ok: true } };
			const text = await serial(() => advance(pi, rt, ctx));
			return { content: [{ type: "text", text }], details: { ok: true, phase: rt.phase } };
		},
	});

	pi.registerTool({
		name: "flow_review",
		label: "flow_review",
		description: "Run the fresh reviewer for the active reviewer phase. Writes the verdict artifact and returns the verdict label.",
		parameters: Type.Object({}),
		async execute() {
			const rt = runtime;
			if (!rt) return { content: [{ type: "text", text: "No Forseti flow is active." }], details: { ok: false } };
			const text = await serial(() => runReview(pi, rt));
			return { content: [{ type: "text", text }], details: { ok: true } };
		},
	});

	pi.registerTool({
		name: "flow_complete",
		label: "flow_complete",
		description: "Check whether the active flow is complete (no remaining phase gate).",
		parameters: Type.Object({}),
		async execute(_toolCallId: string, _params: unknown, ctx: ExtensionContext) {
			const rt = runtime;
			if (!rt) return { content: [{ type: "text", text: "No Forseti flow is active." }], details: { ok: false } };
			const flow = activeFlow(rt);
			const p = phaseOf(flow, rt.phase);
			if (!flow || !p) return { content: [{ type: "text", text: `Flow complete (${rt.file.flows.map((f) => f.id).join(", ")}).` }], details: { ok: true } };
			const assumed = p.gate?.require?.length ? await confirmHumans(pi, ctx, rt, flow, p.gate.require) : [];
			const missing = p.gate?.require?.length ? await missingEvidence(pi, rt, flow, p.gate.require, assumed) : [];
			const text = missing.length ? phaseRemediation(flow, p, missing) : `Phase "${p.id}" is satisfied. Call flow_next to advance.`;
			return { content: [{ type: "text", text }], details: { ok: missing.length === 0 } };
		},
	});

	// ---- commands ----------------------------------------------------------
	pi.registerCommand("flow", {
		description: "Forseti flow: status|next|review|waive|init|explain|abort",
		handler: async (args, ctx) => {
			const parts = args.trim().split(/\s+/).filter(Boolean);
			const sub = (parts[0] ?? "status").toLowerCase();
			if (!runtime) {
				ctx.ui.notify("forseti flow: no .forseti/flow.yaml in this project", "warning");
				return;
			}
			if (sub === "status") {
				const active = activeFlow(runtime);
				const lines = [`flow file: ${runtime.path}`];
				for (const f of runtime.file.flows) {
					lines.push(`${f.id} (${f.mode ?? "repo"}): ${Object.keys(f.on ?? {}).join(", ") || "no lifecycle gates"}`);
				}
				if (active) lines.push(`phase: ${runtime.phase ?? "none"} — completed: ${[...runtime.completed].join(", ") || "none"}`);
				if (runtime.aborted) lines.push("state: ABORTED for this session");
				ctx.ui.notify(lines.join("\n"), "info");
				return;
			}
			if (sub === "abort") {
				runtime.aborted = true;
				publishStatus(runtime);
				ctx.ui.notify("forseti flow: aborted for this session", "warning");
				return;
			}
			if (sub === "explain") {
				const res = await safeExec(pi, runtime.bin, ["explain", "-f", runtime.path], { cwd: runtime.cwd });
				ctx.ui.notify((res.stdout || res.stderr).slice(0, 4000), res.code === 0 ? "info" : "error");
				return;
			}
			if (sub === "next") {
				const text = await serial(() => advance(pi, runtime!, ctx));
				ctx.ui.notify(text, "info");
				return;
			}
			if (sub === "review") {
				const text = await serial(() => runReview(pi, runtime!));
				ctx.ui.notify(text.slice(0, 4000), "info");
				return;
			}
			if (sub === "init") {
				let preset = parts[1];
				if (!preset && ctx.hasUI) {
					preset = (await ctx.ui.select("Forseti flow preset", ["repo", "review-loop", "docs-required", "release"])) ?? "repo";
				}
				preset = preset ?? "repo";
				const res = await safeExec(pi, runtime.bin, ["init", "--preset", preset], { cwd: runtime.cwd });
				ctx.ui.notify((res.stdout || res.stderr).trim(), res.code === 0 ? "info" : "error");
				await refresh(ctx);
				return;
			}
			if (sub === "waive") {
				const stage = parts[1];
				const evidence = parts[2];
				const reason = parts.slice(3).join(" ");
				if (!stage || !evidence || !reason) {
					ctx.ui.notify("usage: /flow waive <stage> <evidence> <reason>", "warning");
					return;
				}
				const flow = activeFlow(runtime) ?? runtime.file.flows[0];
				const res = await safeExec(pi, runtime.bin, [
					"waive", "--flow", flow.id, "--stage", stage, "--evidence", evidence,
					"--reason", reason, "--dir", runtime.cwd,
				], { cwd: runtime.cwd });
				ctx.ui.notify((res.stdout || res.stderr).trim(), res.code === 0 ? "info" : "error");
				runtime.checkCache.clear();
				return;
			}
			ctx.ui.notify("forseti flow: status|next|review|waive|init|explain|abort", "info");
		},
	});

	// Live commands are enough; the initial load happens on session_start.
	void refresh;
}
