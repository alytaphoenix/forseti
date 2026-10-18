// hashtext confidence calibration: `bun run scripts/hashtext-calibrate.ts [options]`
//
// Laya returns two confidence-like numbers for a choice: `confidence`
// (normalized entropy — decisiveness) and `answer_confidence` (max probability
// — the calibrated quantity). We gate on the latter, but on our 7-option block
// distribution it is still under-confident (correct picks at 0.24–0.37), so a
// 0.85 auto-expand threshold never fires.
//
// This fits a monotonic (isotonic) map from answer_confidence to empirical
// accuracy, using labelled eval rows. It changes confidence only: the argmax,
// and therefore accuracy, is untouched. Refit it as telemetry accumulates.
//
//   --data PATH          eval JSONL (default: <store>/eval.jsonl), decision rows
//   --write              write <store>/calibration.json (default: report only)
//   --min N              refuse to write below N samples (default 30)
//   --holdout F          fraction held out for the ECE check (default 0.3)
//   --target-precision P threshold suggestion target (default 0.95)
//   --json               machine-readable report
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
process.env.HASHTEXT_DIR = STORE;
if (typeof args.get("laya-url") === "string") process.env.FORSETI_LAYA_URL = String(args.get("laya-url"));

const plugin = await import("../hashtext-plugin/index.ts");
const { refreshMetas, lexicalCandidates, layaChoose } = plugin;

const INSTRUCTIONS =
	"Which stored block is most likely to hold what this request needs? Choose 'other' when none fit.";

const dataPath = typeof args.get("data") === "string" ? String(args.get("data")) : path.join(STORE, "eval.jsonl");
const write = args.has("write");
const minSamples = Number(args.get("min") ?? 30);
const holdoutFrac = Math.min(0.5, Math.max(0, Number(args.get("holdout") ?? 0.3)));
const targetPrecision = Math.min(1, Math.max(0, Number(args.get("target-precision") ?? 0.95)));
const asJson = args.has("json");
const calibrationPath = path.join(STORE, "calibration.json");

interface Row {
	query?: string;
	expected?: string;
	tags?: string[];
}

interface Sample {
	query: string;
	raw: number;
	correct: boolean;
}

await refreshMetas(true);
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

const samples: Sample[] = [];
for (const row of rows) {
	const query = String(row.query ?? "").trim();
	const expected = String(row.expected ?? "").trim();
	const tags = Array.isArray(row.tags) ? row.tags.map(String) : [];
	if (!query || !expected) continue;
	// Retrieval probes have noisy labels; calibrate only on decision rows.
	if (tags.includes("retrieval")) continue;
	const candidates = await lexicalCandidates(query, 7);
	if (candidates.length === 0) continue;
	if (expected !== "other" && !candidates.some((meta) => meta.id === expected)) continue;
	const criteria: Record<string, string> = {
		other: "none of these blocks holds what the request needs",
	};
	for (const meta of candidates) {
		criteria[meta.id] = `${meta.name || "unnamed"}: ${meta.description || meta.kind}`;
	}
	const answer = await layaChoose(query, INSTRUCTIONS, criteria);
	if (!answer) continue;
	const correct = expected === "other" ? answer.choice === "other" : answer.choice === expected;
	samples.push({ query, raw: answer.confidence, correct });
}

if (samples.length < 3) {
	console.error(`not enough decision samples (${samples.length}) — build a larger eval set first`);
	process.exit(1);
}

// Deterministic split so repeated runs compare the same rows.
const split = (sample: Sample): boolean => {
	let hash = 2166136261;
	for (let i = 0; i < sample.query.length; i++) {
		hash ^= sample.query.charCodeAt(i);
		hash = Math.imul(hash, 16777619);
	}
	return (hash >>> 0) / 2 ** 32 >= holdoutFrac;
};
const fitSamples = samples.filter(split);
const holdout = samples.filter((sample) => !split(sample));
const useForEval = holdout.length >= 5 ? holdout : samples;

type Point = [number, number];

/** Pool-adjacent-violators isotonic regression, with clamping endpoints. */
function fitIsotonic(rowsIn: Sample[]): Point[] {
	const sorted = [...rowsIn].sort((a, b) => a.raw - b.raw);
	const blocks: Array<{ sx: number; sy: number; n: number }> = [];
	for (const sample of sorted) {
		blocks.push({ sx: sample.raw, sy: sample.correct ? 1 : 0, n: 1 });
		while (blocks.length >= 2) {
			const last = blocks[blocks.length - 1];
			const prev = blocks[blocks.length - 2];
			if (prev.sy / prev.n <= last.sy / last.n) break;
			blocks.splice(-2, 2, { sx: prev.sx + last.sx, sy: prev.sy + last.sy, n: prev.n + last.n });
		}
	}
	const points: Point[] = blocks.map((block) => [block.sx / block.n, block.sy / block.n]);
	// Collapse duplicate x (keep the highest y so the map stays monotone).
	const deduped: Point[] = [];
	for (const point of points) {
		if (deduped.length && point[0] === deduped[deduped.length - 1][0]) {
			deduped[deduped.length - 1][1] = Math.max(deduped[deduped.length - 1][1], point[1]);
		} else {
			deduped.push(point);
		}
	}
	if (deduped[0][0] > 0) deduped.unshift([0, deduped[0][1]]);
	if (deduped[deduped.length - 1][0] < 1) deduped.push([1, deduped[deduped.length - 1][1]]);
	return deduped;
}

function apply(points: Point[], value: number): number {
	const x = Math.min(1, Math.max(0, value));
	if (x <= points[0][0]) return points[0][1];
	for (let i = 1; i < points.length; i++) {
		const [ax, ay] = points[i - 1];
		const [bx, by] = points[i];
		if (x <= bx) {
			const span = bx - ax || 1;
			return ay + ((x - ax) / span) * (by - ay);
		}
	}
	return points[points.length - 1][1];
}

function ece(pairs: Array<{ confidence: number; correct: boolean }>): number {
	if (pairs.length === 0) return 0;
	const bins = Array.from({ length: 10 }, () => ({ n: 0, conf: 0, correct: 0 }));
	for (const pair of pairs) {
		const idx = Math.min(9, Math.max(0, Math.floor(pair.confidence * 10)));
		bins[idx].n += 1;
		bins[idx].conf += pair.confidence;
		bins[idx].correct += pair.correct ? 1 : 0;
	}
	let total = 0;
	for (const bin of bins) {
		if (!bin.n) continue;
		total += (bin.n / pairs.length) * Math.abs(bin.conf / bin.n - bin.correct / bin.n);
	}
	return total;
}

const points = fitIsotonic(fitSamples.length >= 3 ? fitSamples : samples);
const eceInSample = useForEval === samples;
const rawPairs = useForEval.map((sample) => ({ confidence: sample.raw, correct: sample.correct }));
const calPairs = useForEval.map((sample) => ({ confidence: apply(points, sample.raw), correct: sample.correct }));

const thresholds = [0.3, 0.4, 0.5, 0.6, 0.7, 0.8, 0.85];
const operating = thresholds.map((threshold) => {
	const selected = useForEval.filter((sample) => apply(points, sample.raw) >= threshold);
	const precision = selected.length ? selected.filter((sample) => sample.correct).length / selected.length : null;
	return {
		threshold,
		coverage: selected.length / useForEval.length,
		precision: precision === null ? null : Number(precision.toFixed(3)),
		n: selected.length,
	};
});
const suggested = operating
	.filter((row) => row.precision !== null && row.precision >= targetPrecision && row.coverage >= 0.1)
	.sort((a, b) => a.threshold - b.threshold)[0];

const report = {
	rows_in: samples.length,
	fit_rows: fitSamples.length,
	holdout_rows: holdout.length,
	eval_rows: useForEval.length,
	accuracy: useForEval.filter((sample) => sample.correct).length / useForEval.length,
	ece_raw: Number(ece(rawPairs).toFixed(3)),
	ece_calibrated: Number(ece(calPairs).toFixed(3)),
	ece_in_sample: eceInSample,
	points: points.map(([x, y]) => [Number(x.toFixed(3)), Number(y.toFixed(3))]),
	operating_points: operating,
	suggested_threshold: suggested?.threshold ?? null,
	written: false as boolean | string,
};

if (write) {
	if (samples.length >= minSamples) {
		// Fit on every sample for the artifact; the ECE above is the honest estimate.
		const allPoints = fitIsotonic(samples);
		await writeFile(
			calibrationPath,
			JSON.stringify(
				{
					version: 1,
					kind: "hashtext-confidence-isotonic",
					metric: "answer_confidence",
					n: samples.length,
					fitted_at: new Date().toISOString(),
					points: allPoints.map(([x, y]) => [Number(x.toFixed(4)), Number(y.toFixed(4))]),
				},
				null,
				2,
			) + "\n",
			"utf8",
		);
		report.written = calibrationPath;
	} else {
		report.written = `skipped: ${samples.length} < --min ${minSamples}`;
	}
}

if (asJson) {
	console.log(JSON.stringify(report, null, 2));
} else {
	const pct = (value: number | null): string => (value === null ? "—" : `${(value * 100).toFixed(1)}%`);
	console.log(`decisions: ${samples.length} (fit ${fitSamples.length}, holdout ${holdout.length}) · accuracy ${pct(report.accuracy)}`);
	console.log(`ECE raw → calibrated: ${report.ece_raw} → ${report.ece_calibrated} (eval rows ${report.eval_rows})`);
	if (report.ece_in_sample) {
		console.log("  ⚠ in-sample fit (holdout < 5) — treat the calibrated ECE as an upper bound on the gain");
	}
	console.log("");
	console.log("threshold  coverage  precision   n");
	for (const row of report.operating_points) {
		console.log(
			`${row.threshold.toFixed(2).padStart(9)}  ${pct(row.coverage).padStart(8)}  ${pct(row.precision).padStart(9)}  ${String(row.n).padStart(3)}`,
		);
	}
	console.log("");
	console.log(`suggested threshold (precision ≥ ${(targetPrecision * 100).toFixed(0)}%): ${report.suggested_threshold ?? "none"}`);
	console.log(write ? `calibration: ${report.written}` : "calibration not written (use --write)");
}
