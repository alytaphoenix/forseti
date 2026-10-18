// hashtext telemetry labeller: `bun run scripts/hashtext-label.ts [options]`
//
// Turns recorded finds/outcomes into labelled eval rows. The signal is weak but
// real: an expand that matches Laya's pick is a positive, an expand of a
// different candidate is a negative for the pick and a positive for what the
// agent actually opened, and an immediate eviction downgrades the label.
//
//   --log PATH        decisions.jsonl (default: <store>/decisions.jsonl)
//   --out PATH        labelled JSONL (default: <store>/labeled.jsonl)
//   --review PATH     emit `expected<TAB>query` rows for human/LLM judging
//   --include-low     keep low-confidence labels (divergent / quick eviction)
//   --min-resident N  min turns before an eviction downgrades a label (default 2)
//   --window N        max turns between a find and its outcome (default 3)
//   --json            machine-readable summary
//
// Labels are emitted only when the agent actually expanded something; "no
// expand" is ambiguous (the task may not have needed the block), so those finds
// are reported as unlabelled instead of guessed.
import { readFile, writeFile } from "node:fs/promises";
import path from "node:path";

const args = new Map<string, string | true>();
for (let i = 2; i < process.argv.length; i++) {
	const arg = process.argv[i];
	if (!arg.startsWith("--")) continue;
	const next = process.argv[i + 1];
	if (next && !next.startsWith("--")) {
		args.set(arg.slice(2), next);
		i++;
	} else {
		args.set(arg.slice(2), true);
	}
}

const STORE = process.env.HASHTEXT_DIR?.trim() || path.join(process.env.HOME ?? "", ".hashtext");
const logPath = typeof args.get("log") === "string" ? String(args.get("log")) : path.join(STORE, "decisions.jsonl");
const outPath = typeof args.get("out") === "string" ? String(args.get("out")) : path.join(STORE, "labeled.jsonl");
const reviewPath = typeof args.get("review") === "string" ? String(args.get("review")) : null;
const includeLow = args.has("include-low");
const minResident = Math.max(0, Number(args.get("min-resident") ?? 2));
const windowTurns = Math.max(0, Number(args.get("window") ?? 3));
const asJson = args.has("json");

interface Candidate {
	id: string;
	name?: string;
	description?: string;
	kind?: string;
	tokens_est?: number;
}

interface Event {
	ts?: string;
	kind?: string;
	query?: string | null;
	pick?: string | null;
	candidates?: Candidate[];
	action?: string;
	ref?: string;
	matched_pick?: boolean | null;
	turns_since_pick?: number | null;
	resident_turns?: number | null;
}

interface Find {
	ts: string;
	query: string;
	pick: string | null;
	candidates: Candidate[];
	outcomes: Event[];
}

let raw = "";
try {
	raw = await readFile(logPath, "utf8");
} catch {
	console.error(`no decision log at ${logPath} (run sessions with the plugin, or pass --log)`);
	process.exit(1);
}

const finds: Find[] = [];
let current: Find | null = null;
for (const line of raw.split("\n")) {
	if (!line.trim()) continue;
	let event: Event;
	try {
		event = JSON.parse(line) as Event;
	} catch {
		continue;
	}
	if (event.kind === "find" && event.query) {
		current = {
			ts: String(event.ts ?? ""),
			query: String(event.query),
			pick: event.pick ? String(event.pick) : null,
			candidates: Array.isArray(event.candidates) ? event.candidates : [],
			outcomes: [],
		};
		finds.push(current);
		continue;
	}
	if (event.kind === "outcome" && current && event.query && event.query === current.query) {
		if (typeof event.turns_since_pick === "number" && event.turns_since_pick > windowTurns) continue;
		current.outcomes.push(event);
	}
}

interface Label {
	query: string;
	expected: string;
	source: string;
	confidence: "high" | "medium" | "low";
	pick: string | null;
	candidates: Candidate[];
	outcome: string;
}

const labels: Label[] = [];
let unlabelled = 0;
let abstainFinds = 0;

for (const find of finds) {
	if (!find.pick || find.pick === "other") {
		abstainFinds += 1;
		continue;
	}
	const expands = find.outcomes.filter((outcome) => outcome.action === "expand" && outcome.ref);
	if (expands.length === 0) {
		unlabelled += 1;
		continue;
	}

	const first = expands[0];
	const matched = first.ref === find.pick;
	const target = matched ? find.pick : String(first.ref);
	const eviction = find.outcomes.find(
		(outcome) => outcome.action === "evict" && outcome.ref === target,
	);
	const evictedFast =
		eviction && typeof eviction.resident_turns === "number" && eviction.resident_turns < minResident;

	let confidence: Label["confidence"] = matched ? "high" : "medium";
	if (evictedFast) confidence = "low";
	if (expands.some((outcome) => outcome.ref === target && outcome.matched_pick === true)) confidence = "high";

	if (confidence === "low" && !includeLow) {
		unlabelled += 1;
		continue;
	}

	labels.push({
		query: find.query,
		expected: target,
		source: matched ? "matched_expand" : "divergent_expand",
		confidence,
		pick: find.pick,
		candidates: find.candidates,
		outcome: evictedFast ? "expanded_then_evicted" : "expanded",
	});
}

if (labels.length === 0) {
	console.error(
		`no labelled rows (finds ${finds.length}, abstain ${abstainFinds}, unlabelled ${unlabelled}) — ` +
			"the agent must expand a picked block for the outcome to be usable",
	);
	process.exit(1);
}

// The eval scaffold's native row shape (query + expected); candidates are
// recomputed at eval time from the query, so this stays honest about retrieval.
const rows = labels.map((label) => ({
	schema: "hashtext-eval/1",
	query: label.query,
	expected: label.expected,
	tags: ["telemetry", label.confidence, label.source],
	source_block: {
		id: label.expected,
		name: label.candidates.find((candidate) => candidate.id === label.expected)?.name ?? "",
		description: label.candidates.find((candidate) => candidate.id === label.expected)?.description ?? "",
	},
}));
await writeFile(outPath, rows.map((row) => JSON.stringify(row)).join("\n") + "\n", "utf8");

if (reviewPath) {
	const seen = new Set<string>();
	const review = labels
		.filter((label) => {
			const key = `${label.expected}\t${label.query}`;
			if (seen.has(key)) return false;
			seen.add(key);
			return true;
		})
		.map((label) => `${label.expected}\t${label.query}`);
	await writeFile(reviewPath, review.join("\n") + "\n", "utf8");
}

const bySource: Record<string, number> = {};
const byConfidence: Record<string, number> = {};
for (const label of labels) {
	bySource[label.source] = (bySource[label.source] ?? 0) + 1;
	byConfidence[label.confidence] = (byConfidence[label.confidence] ?? 0) + 1;
}

const report = {
	finds: finds.length,
	abstain_finds: abstainFinds,
	labeled: labels.length,
	unlabelled,
	by_source: bySource,
	by_confidence: byConfidence,
	out: outPath,
	review: reviewPath,
};
if (asJson) {
	console.log(JSON.stringify(report, null, 2));
} else {
	console.log(
		`finds ${report.finds} · labelled ${report.labeled} (${JSON.stringify(bySource)}) · ` +
			`unlabelled ${report.unlabelled} · abstain ${report.abstain_finds}`,
	);
	console.log(`wrote ${outPath}${reviewPath ? ` and ${reviewPath}` : ""}`);
}
