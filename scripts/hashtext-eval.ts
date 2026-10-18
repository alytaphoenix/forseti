// hashtext picker eval: `bun run scripts/hashtext-eval.ts [options]`
//
// Measures the block picker end to end with the plugin's real shortlist and
// Laya client:
//   recall@k        was the expected block in the lexical shortlist?
//   lexical top-1   was it ranked first (the no-Laya baseline)?
//   pick accuracy   did Laya pick the expected block?
//   abstention      precision/recall over "other" (the auto-expand guard)
//   ECE             confidence calibration (10 bins)
//   latency         Laya round-trip
//
//   --data PATH     eval JSONL (default: <store>/eval.jsonl)
//   --k N           shortlist size (default 7)
//   --offline       skip Laya (recall + lexical baseline only)
//   --laya-url URL  endpoint (default: FORSETI_LAYA_URL or 127.0.0.1:8751)
//   --json          machine-readable report
//   --tags TAG,...  filter rows by tag
import { readFile } from "node:fs/promises";
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
process.env.HASHTEXT_DIR = STORE;
if (typeof args.get("laya-url") === "string") process.env.FORSETI_LAYA_URL = String(args.get("laya-url"));

const plugin = await import("../hashtext-plugin/index.ts");
const { refreshMetas, allMetas, lexicalCandidates, layaChoose } = plugin;

const INSTRUCTIONS =
	"Which stored block is most likely to hold what this request needs? Choose 'other' when none fit.";

const dataPath = typeof args.get("data") === "string" ? String(args.get("data")) : path.join(STORE, "eval.jsonl");
const k = Math.min(Math.max(Number(args.get("k") ?? 7), 1), 20);
const offline = args.has("offline");
const json = args.has("json");
const tagFilter = typeof args.get("tags") === "string" ? String(args.get("tags")).split(",").map((t) => t.trim()) : null;

interface Row {
	schema?: string;
	query?: string;
	expected?: string;
	tags?: string[];
	/** Blocks a judge said could answer this query (relaxes strict source labels). */
	acceptable?: string[];
}

await refreshMetas(true);
const metas = allMetas();
if (!json) console.log(`store: ${STORE} · blocks: ${metas.length} · shortlist k=${k}${offline ? " · offline" : ""}`);

const raw = await readFile(dataPath, "utf8");
const rows: Row[] = [];
for (const line of raw.split("\n")) {
	if (!line.trim()) continue;
	try {
		rows.push(JSON.parse(line) as Row);
	} catch {
		// skip malformed rows
	}
}
if (rows.length === 0) {
	console.error(`no rows in ${dataPath}`);
	process.exit(1);
}

interface Result {
	query: string;
	expected: string;
	tags: string[];
	accepted: string[];
	shouldAbstain: boolean;
	candidates: string[];
	rank: number; // -1 = not in shortlist
	lexicalTop1: string | null;
	pick: string | null;
	confidence: number | null;
	latency: number | null;
	correct: boolean;
	abstained: boolean;
	roundTripped: boolean;
}

const results: Result[] = [];
for (const row of rows) {
	const query = String(row.query ?? "").trim();
	const expected = String(row.expected ?? "").trim();
	const tags = Array.isArray(row.tags) ? row.tags.map(String) : [];
	if (!query || !expected) continue;
	if (tagFilter && !tags.some((tag) => tagFilter.includes(tag))) continue;

	const candidates = await lexicalCandidates(query, k);
	const ids = candidates.map((meta) => meta.id);
	// A judged `acceptable` set relaxes the strict source-block label: section-
	// level stores overlap, so another block can legitimately answer.
	const acceptable = Array.isArray(row.acceptable) ? row.acceptable.filter((id): id is string => typeof id === "string") : [];
	const accepted = new Set(acceptable);
	if (expected !== "other") accepted.add(expected);
	const shouldAbstain = expected === "other" && accepted.size === 0;
	const rank = shouldAbstain ? -2 : Math.max(-1, ids.findIndex((id) => accepted.has(id)));

	let pick: string | null = null;
	let confidence: number | null = null;
	let latency: number | null = null;
	let roundTripped = false;

	if (!offline && candidates.length > 0) {
		const criteria: Record<string, string> = {
			other: "none of these blocks holds what the request needs",
		};
		for (const meta of candidates) {
			criteria[meta.id] = `${meta.name || "unnamed"}: ${meta.description || meta.kind}`;
		}
		const answer = await layaChoose(query, INSTRUCTIONS, criteria);
		if (answer) {
			roundTripped = true;
			pick = answer.choice;
			confidence = answer.confidence;
			latency = answer.latency_ms;
		}
	}

	const effectivePick = pick ?? (candidates[0]?.id ?? null);
	const abstained = effectivePick === "other" || effectivePick === null;
	const correct = shouldAbstain ? abstained : effectivePick !== null && accepted.has(effectivePick);

	results.push({
		query,
		expected,
		tags,
		accepted: [...accepted],
		shouldAbstain,
		candidates: ids,
		rank,
		lexicalTop1: ids[0] ?? null,
		pick: effectivePick,
		confidence: pick ? confidence : null,
		latency,
		correct,
		abstained,
		roundTripped,
	});
}

if (results.length === 0) {
	console.error("no matching rows after filtering");
	process.exit(1);
}

const labelled = results.filter((row) => row.rank !== -2);
const abstainRows = results.filter((row) => row.shouldAbstain);
const inShortlist = labelled.filter((row) => row.rank >= 0);
const misses = labelled.length - inShortlist.length;

// Content-derived synthetic rows test retrieval, not judgement: the labels are
// keyword artefacts a human would not reliably agree with. Rank/abstention
// metrics come from decision rows (authored or clear name/description queries).
const decisionRows = results.filter((row) => !row.tags.includes("retrieval"));
const retrievalRows = results.filter((row) => row.tags.includes("retrieval"));
const decisionLabelled = decisionRows.filter((row) => row.rank !== -2);
const decisionAbstain = decisionRows.filter((row) => row.shouldAbstain);
const decisionInShortlist = decisionLabelled.filter((row) => row.rank >= 0);

const accuracy = (rows: Result[]): number =>
	rows.length ? rows.filter((row) => row.correct).length / rows.length : 0;

const truePositive = decisionAbstain.filter((row) => row.abstained).length;
const falseNegative = decisionAbstain.length - truePositive;
const falsePositive = decisionLabelled.filter((row) => row.abstained).length;
const trueNegative = decisionLabelled.length - falsePositive;
const precision = truePositive + falsePositive ? truePositive / (truePositive + falsePositive) : 0;
const recallAbstain = truePositive + falseNegative ? truePositive / (truePositive + falseNegative) : 0;

const calibrated = (decisionRows.length ? decisionRows : results).filter((row) => row.confidence !== null);
const bins = Array.from({ length: 10 }, () => ({ n: 0, confidence: 0, correct: 0 }));
for (const row of calibrated) {
	const idx = Math.min(9, Math.max(0, Math.floor((row.confidence as number) * 10)));
	bins[idx].n += 1;
	bins[idx].confidence += row.confidence as number;
	bins[idx].correct += row.correct ? 1 : 0;
}
let ece = 0;
for (const bin of bins) {
	if (bin.n === 0) continue;
	const conf = bin.confidence / bin.n;
	const acc = bin.correct / bin.n;
	ece += (bin.n / calibrated.length) * Math.abs(conf - acc);
}

const latencies = results.map((row) => row.latency).filter((value): value is number => typeof value === "number");
latencies.sort((a, b) => a - b);
const mean = latencies.length ? latencies.reduce((sum, value) => sum + value, 0) / latencies.length : 0;
const p95 = latencies.length ? latencies[Math.min(latencies.length - 1, Math.floor(latencies.length * 0.95))] : 0;

const perTag = new Map<string, Result[]>();
for (const row of results) {
	for (const tag of row.tags.length ? row.tags : ["untagged"]) {
		if (!perTag.has(tag)) perTag.set(tag, []);
		perTag.get(tag)!.push(row);
	}
}

// Switch matrix over ranking rows only (abstention is scored separately); if
// the model mostly ties, a reranker/fine-tune has no case.
const switchRows = decisionLabelled;
const acceptedPick = (row: Result, id: string | null): boolean => Boolean(id && row.accepted.includes(id));
const switchCounts = {
	both: switchRows.filter((row) => acceptedPick(row, row.lexicalTop1) && acceptedPick(row, row.pick)).length,
	lexical_only: switchRows.filter((row) => acceptedPick(row, row.lexicalTop1) && !acceptedPick(row, row.pick)).length,
	laya_only: switchRows.filter((row) => !acceptedPick(row, row.lexicalTop1) && acceptedPick(row, row.pick)).length,
	neither: switchRows.filter((row) => !acceptedPick(row, row.lexicalTop1) && !acceptedPick(row, row.pick)).length,
};

const report = {
	rows: results.length,
	labelled: labelled.length,
	abstain_rows: abstainRows.length,
	decision_rows: decisionRows.length,
	retrieval_rows: retrievalRows.length,
	shortlist_misses: misses,
	recall_at_k: labelled.length ? inShortlist.length / labelled.length : 0,
	retrieval_recall_at_k: retrievalRows.length
		? retrievalRows.filter((row) => row.rank >= 0).length / retrievalRows.length
		: 0,
	lexical_top1_accuracy: decisionLabelled.length
		? decisionLabelled.filter((row) => acceptedPick(row, row.lexicalTop1)).length / decisionLabelled.length
		: 0,
	pick_accuracy: accuracy(decisionLabelled.length ? decisionLabelled : decisionRows),
	pick_accuracy_in_shortlist: accuracy(decisionInShortlist),
	switch: switchCounts,
	headroom: Number(
		(
			(labelled.length ? inShortlist.length / labelled.length : 0) -
			(decisionLabelled.length
				? decisionLabelled.filter((row) => acceptedPick(row, row.lexicalTop1)).length / decisionLabelled.length
				: 0)
		).toFixed(3),
	),
	abstention: {
		precision: Number(precision.toFixed(3)),
		recall: Number(recallAbstain.toFixed(3)),
		false_abstain_rate: decisionLabelled.length ? falsePositive / decisionLabelled.length : 0,
		missed_abstain_rate: decisionAbstain.length ? falseNegative / decisionAbstain.length : 0,
		confusion: { truePositive, falsePositive, trueNegative, falseNegative },
	},
	ece: Number(ece.toFixed(3)),
	calibrated_rows: calibrated.length,
	latency_ms: { mean: Number(mean.toFixed(1)), p95: Number(p95.toFixed(1)) },
	laya_round_trips: results.filter((row) => row.roundTripped).length,
	per_tag: Object.fromEntries(
		[...perTag.entries()].map(([tag, tagRows]) => [
			tag,
			{
				rows: tagRows.length,
				recall_at_k: tagRows.filter((row) => row.rank >= 0 || row.shouldAbstain).length / tagRows.length,
				pick_accuracy: accuracy(tagRows),
			},
		]),
	),
};

if (json) {
	console.log(JSON.stringify(report, null, 2));
} else {
	const pct = (value: number): string => `${(value * 100).toFixed(1)}%`;
	console.log("");
	console.log(`rows                  ${report.rows} (labelled ${report.labelled}, abstain ${report.abstain_rows})`);
	console.log(`recall@${k}             ${pct(report.recall_at_k)} (${misses} shortlist misses)`);
	console.log(`lexical top-1         ${pct(report.lexical_top1_accuracy)}   ← no-Laya baseline`);
	console.log(`pick accuracy         ${pct(report.pick_accuracy)} (in-shortlist ${pct(report.pick_accuracy_in_shortlist)})`);
	console.log(`abstention            precision ${pct(report.abstention.precision)} · recall ${pct(report.abstention.recall)}`);
	console.log(`  false-abstain       ${pct(report.abstention.false_abstain_rate)} · missed-abstain ${pct(report.abstention.missed_abstain_rate)}`);
	console.log(`ECE                   ${report.ece.toFixed(3)} over ${report.calibrated_rows} decision rows`);
	console.log(`headroom              ${(report.headroom * 100).toFixed(1)}% (recall@${k} − lexical top-1)`);
	console.log(
		`switch                both ${report.switch.both} · lexical-only ${report.switch.lexical_only} · ` +
			`laya-only ${report.switch.laya_only} · neither ${report.switch.neither}`,
	);
	console.log(`laya latency          ${report.latency_ms.mean} ms mean · ${report.latency_ms.p95} ms p95 (${report.laya_round_trips} calls)`);
	console.log("");
	console.log(`  retrieval probes: recall@${k} ${pct(report.retrieval_recall_at_k)} (${report.retrieval_rows} rows — ranking not scored)`);
	for (const [tag, stats] of Object.entries(report.per_tag)) {
		console.log(`  ${tag.padEnd(12)} rows ${String(stats.rows).padStart(4)} · recall ${pct(stats.recall_at_k)} · acc ${pct(stats.pick_accuracy)}`);
	}
}
