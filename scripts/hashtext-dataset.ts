// hashtext dataset builder: `bun run scripts/hashtext-dataset.ts [options]`
//
// Builds labelled rows for evaluating (or later fine-tuning) the block picker.
// Reuses the plugin's exact store/lexical code, so what you evaluate is what
// runs in a session.
//
//   --synthetic N       N labelled query rows generated from the store
//   --from-decisions    dump recorded find/outcome rows (unlabelled telemetry)
//   --out PATH          output JSONL (default: <store>/eval.jsonl)
//   --laya-out PATH     also emit laya.evals-format rows (frozen candidates)
//   --queries FILE      TSV `expected<TAB>query` rows (LLM-generated, best quality)
//
// Synthetic queries come in two flavours, tagged so the eval can separate them:
//   vocab   — natural-ish name+description text (easy)
//   content — keyword extraction from body lines (harder, closer to real use)
// Abstain rows (~20%) expect "other": the query must not match any block.
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

const plugin = await import("../hashtext-plugin/index.ts");
const { refreshMetas, allMetas, loadText, lexicalCandidates } = plugin;

const INSTRUCTIONS =
	"Which stored block is most likely to hold what this request needs? Choose 'other' when none fit.";
const ABSTAIN_QUERIES = [
	"reconcile the zorbatrix ledger with the flimflam schedule",
	"what is the airspeed velocity of an unladen swallow",
	"translate the quux protocol into seventeen dialects",
	"summarize the history of the invented sport of blernsball",
	"calculate the orbital period of a fictional moon",
];

interface Meta {
	id: string;
	sha256: string;
	name: string;
	description: string;
	kind: string;
	bytes: number;
	tokens_est: number;
}

function contentQuery(text: string, name: string): string {
	const rows = text.split("\n").filter((row) => row.trim().length > 20);
	if (rows.length === 0) return name;
	const start = Math.floor(rows.length / 3);
	const window = rows.slice(start, start + 3).join(" ");
	const nameWords = new Set(name.toLowerCase().split(/[^a-z0-9]+/).filter(Boolean));
	const words = [
		...new Set(
			window
				.toLowerCase()
				.replace(/[^a-z0-9 ]+/g, " ")
				.split(/\s+/)
				.filter((word) => word.length >= 4 && !/^\d+$/.test(word) && !nameWords.has(word)),
		),
	].slice(0, 8);
	return words.length >= 3
		? `what do the stored notes say about ${words.slice(0, 6).join(", ")}?`
		: name;
}

function vocabQuery(meta: Meta): string {
	const name = meta.name?.trim();
	const desc = meta.description?.trim();
	return [name, desc].filter(Boolean).join(" — ") || meta.id;
}

const outPath = typeof args.get("out") === "string" ? String(args.get("out")) : path.join(STORE, "eval.jsonl");
const layaOut = typeof args.get("laya-out") === "string" ? String(args.get("laya-out")) : null;
const syntheticCount = args.has("synthetic") ? Math.max(1, Number(args.get("synthetic") ?? 30)) : 0;
const fromDecisions = args.has("from-decisions");
const queriesFile = typeof args.get("queries") === "string" ? String(args.get("queries")) : null;

await refreshMetas(true);
const metas = allMetas();
if (metas.length === 0) {
	console.error(`no blocks in ${STORE}/blocks — nothing to build a dataset from`);
	process.exit(1);
}

const rows: Array<Record<string, unknown>> = [];

// -- LLM-generated queries (best quality; one `expected<TAB>query` per line) --
if (queriesFile) {
	const raw = await readFile(queriesFile, "utf8");
	for (const line of raw.split("\n")) {
		if (!line.trim() || line.startsWith("#")) continue;
		const [expected, ...rest] = line.split("\t");
		const query = rest.join("\t").trim();
		if (!expected?.trim() || !query) continue;
		rows.push({
			schema: "hashtext-eval/1",
			query,
			expected: expected.trim(),
			tags: ["authored", "decision"],
		});
	}
}

// -- synthetic labelled rows ------------------------------------------------
if (syntheticCount > 0) {
	const perBlock = Math.max(1, Math.ceil(syntheticCount / metas.length));
	let made = 0;
	for (const meta of metas) {
		if (made >= syntheticCount) break;
		let text: string;
		try {
			text = await loadText(meta as never);
		} catch {
			continue;
		}
		const variants: Array<{ query: string; tag: string; probe: string }> = [
			{ query: vocabQuery(meta), tag: "vocab", probe: "decision" },
			{ query: contentQuery(text, meta.name || meta.id), tag: "content", probe: "retrieval" },
		];
		for (const variant of variants.slice(0, perBlock)) {
			if (made >= syntheticCount) break;
			rows.push({
				schema: "hashtext-eval/1",
				query: variant.query,
				expected: meta.id,
				tags: ["synthetic", variant.tag, variant.probe],
				source_block: { id: meta.id, name: meta.name, description: meta.description },
			});
			made++;
		}
	}
	// ~20% abstention rows, deterministic.
	const abstainCount = Math.max(1, Math.round(rows.length * 0.2));
	for (let i = 0; i < abstainCount; i++) {
		const query = ABSTAIN_QUERIES[i % ABSTAIN_QUERIES.length];
		rows.push({
			schema: "hashtext-eval/1",
			query,
			expected: "other",
			tags: ["synthetic", "abstain", "decision"],
		});
	}
}

// -- telemetry dump (unlabelled; for later judging / training) ---------------
if (fromDecisions) {
	const file = path.join(STORE, "decisions.jsonl");
	let raw = "";
	try {
		raw = await readFile(file, "utf8");
	} catch {
		console.error(`no decision log at ${file}`);
	}
	for (const line of raw.split("\n")) {
		if (!line.trim()) continue;
		try {
			const parsed = JSON.parse(line) as Record<string, unknown>;
			if (parsed.kind !== "find" && parsed.kind !== "outcome") continue;
			rows.push({ schema: "hashtext-telemetry/1", ...parsed });
		} catch {
			// skip malformed lines
		}
	}
}

if (rows.length === 0) {
	console.error("no rows produced (use --synthetic N, --from-decisions, or --queries FILE)");
	process.exit(1);
}

await writeFile(outPath, rows.map((row) => JSON.stringify(row)).join("\n") + "\n", "utf8");

// -- optional laya.evals-format rows (candidate set frozen at build time) -----
if (layaOut) {
	const layaRows: string[] = [];
	for (const row of rows) {
		if (row.schema !== "hashtext-eval/1") continue;
		const expected = String(row.expected);
		if (expected === "other") continue;
		const candidates = await lexicalCandidates(String(row.query), 7);
		if (candidates.length === 0) continue;
		const criteria: Record<string, string> = {
			other: "none of these blocks holds what the request needs",
		};
		for (const meta of candidates) {
			criteria[meta.id] = `${meta.name || "unnamed"}: ${meta.description || meta.kind}`;
		}
		layaRows.push(
			JSON.stringify({
				state: { input: row.query },
				questions: { pick: { type: "choice", instructions: INSTRUCTIONS, criteria } },
				expected: { pick: expected },
				tags: row.tags,
			}),
		);
	}
	await writeFile(layaOut, layaRows.join("\n") + "\n", "utf8");
	console.log(`laya.evals rows: ${layaRows.length} -> ${layaOut}`);
}

const counts = rows.reduce<Record<string, number>>((acc, row) => {
	const kind = String(row.schema);
	acc[kind] = (acc[kind] ?? 0) + 1;
	return acc;
}, {});
console.log(`rows: ${rows.length} ${JSON.stringify(counts)} -> ${outPath}`);
console.log(`blocks: ${metas.length} · store: ${STORE}`);
