// hashtext query generator (free OpenRouter): `bun run scripts/hashtext-generate.ts`
//
// Generates realistic evaluation queries from stored blocks with a free LLM,
// then optionally freezes Laya-shaped training rows. This is the dataset
// quality step the keyword generator could not provide.
//
//   --per-block N     queries per block (default 3)
//   --limit N         at most N blocks (default all)
//   --out PATH        eval JSONL (default: <store>/generated.jsonl)
//   --train-out PATH  optional Laya choice rows (state/questions/expected)
//   --model ID        default nvidia/nemotron-3-super-120b-a12b:free
//   --fallback IDs    comma list tried on 429/5xx (default: gemma-4-31b, xing4.0)
//   --concurrency N   parallel requests (default 2; free tier is rate-limited)
//   --timeout MS      per request (default 90000)
//   --api-key FILE    default $OPENROUTER_API_KEY or ~/.config/forseti/openrouter.key
//   --dry-run         print prompts, call nothing
//   --json            machine-readable summary
//
// Output rows use the same `hashtext-eval/1` shape as the other scripts, tagged
// `generated`, `decision` and the block kind; abstention rows expect "other".
import { appendFile, readFile, writeFile } from "node:fs/promises";
import { homedir } from "node:os";
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

const STORE = process.env.HASHTEXT_DIR?.trim() || path.join(homedir(), ".hashtext");
process.env.HASHTEXT_DIR = STORE;
const plugin = await import("../hashtext-plugin/index.ts");
const { refreshMetas, allMetas, loadText, lexicalCandidates } = plugin;

const perBlock = Math.max(1, Number(args.get("per-block") ?? 3));
const limit = args.has("limit") ? Math.max(1, Number(args.get("limit"))) : Infinity;
const outPath = typeof args.get("out") === "string" ? String(args.get("out")) : path.join(STORE, "generated.jsonl");
const trainOut = typeof args.get("train-out") === "string" ? String(args.get("train-out")) : null;
const model = typeof args.get("model") === "string" ? String(args.get("model")) : "openrouter/free";
const fallbacks = String(
	args.get("fallback") ?? "nvidia/nemotron-3-super-120b-a12b:free,google/gemma-4-31b-it:free",
)
	.split(",")
	.map((value) => value.trim())
	.filter(Boolean);
const concurrency = Math.max(1, Math.min(4, Number(args.get("concurrency") ?? 3)));
const timeoutMs = Math.max(10_000, Number(args.get("timeout") ?? 45_000));
const dryRun = args.has("dry-run");
const asJson = args.has("json");
// Resumable by default when the output exists: rerunning continues where a
// timeout/rate limit stopped it. `--fresh` starts over.
const fresh = args.has("fresh");
const keyFile =
	typeof args.get("api-key") === "string"
		? String(args.get("api-key"))
		: process.env.OPENROUTER_API_KEY
			? null
			: path.join(homedir(), ".config", "forseti", "openrouter.key");

async function apiKey(): Promise<string> {
	if (process.env.OPENROUTER_API_KEY) return process.env.OPENROUTER_API_KEY.trim();
	if (!keyFile) return "";
	try {
		return (await readFile(keyFile, "utf8")).trim();
	} catch {
		return "";
	}
}

const KEY = await apiKey();
if (!KEY && !dryRun) {
	console.error("no OpenRouter API key: set OPENROUTER_API_KEY or pass --api-key FILE");
	process.exit(1);
}

const PROMPT = (name: string, description: string, kind: string, excerpt: string, count: number): string =>
	[
		"You are building an evaluation set for a context-retrieval system.",
		'A "block" is stored material (documentation, source code, logs) that an assistant can open.',
		`Write ${count} realistic requests a developer would send to a coding assistant that this block would help answer.`,
		"Rules:",
		"- natural language, 8 to 28 words; vary the intent (lookup, debug, summarize, implement, compare)",
		"- include concrete details from the block (identifiers, errors, headings, numbers)",
		"- do not copy the block title verbatim",
		'- never mention "block", "context", "document", "cache", "stored", or the retrieval system',
		"- different requests must cover different parts of the material",
		"- return ONLY a JSON array of strings, no prose and no code fence",
		"",
		`BLOCK: ${name || "(unnamed)"} — ${description || kind}`,
		"---",
		excerpt,
	].join("\n");

const ABSTAIN_QUERIES = [
	"what is the orbital period of Europa around Jupiter?",
	"give me a sourdough recipe with a 70 percent hydration starter",
	"explain how a rotary engine seals its apex",
	"who won the 1998 world cup final?",
	"translate 'good morning' into Finnish and Japanese",
	"what is the boiling point of mercury at one atmosphere?",
	"how do I prune a mature apple tree in late winter?",
	"what are the rules for a legal three-point turn?",
];

interface ChatResult {
	content: string;
	model: string;
}

async function chatOnce(target: string, prompt: string, attempt: number): Promise<ChatResult> {
	const res = await fetch("https://openrouter.ai/api/v1/chat/completions", {
		method: "POST",
		headers: {
			Authorization: `Bearer ${KEY}`,
			"Content-Type": "application/json",
			"HTTP-Referer": "https://github.com/alytaphoenix/forseti",
			"X-Title": "hashtext dataset generator",
		},
		signal: AbortSignal.timeout(timeoutMs),
		body: JSON.stringify({
			model: target,
			messages: [{ role: "user", content: prompt }],
			max_tokens: Math.max(700, Math.min(1_200, 200 + perBlock * 180)),
			temperature: 0.7,
			// Reasoning models emit raw control characters in `reasoning`, which
			// breaks strict JSON parsers; exclude it.
			reasoning: { exclude: true },
		}),
	});
	if (!res.ok) throw new Error(`${target}: HTTP ${res.status} (attempt ${attempt})`);
	const data = (await res.json()) as {
		choices?: Array<{ message?: { content?: string } }>;
		error?: { message?: string };
	};
	const content = data.choices?.[0]?.message?.content ?? "";
	if (!content.trim()) throw new Error(`${target}: empty response${data.error ? ` (${data.error.message})` : ""}`);
	return { content, model: target };
}

function parseQueries(content: string, count: number): string[] {
	let text = content.trim();
	text = text.replace(/^```(?:json)?/i, "").replace(/```$/i, "").trim();
	const start = text.indexOf("[");
	const end = text.lastIndexOf("]");
	if (start === -1 || end <= start) return [];
	let parsed: unknown;
	try {
		parsed = JSON.parse(text.slice(start, end + 1));
	} catch {
		return [];
	}
	if (!Array.isArray(parsed)) return [];
	const seen = new Set<string>();
	const queries: string[] = [];
	for (const item of parsed) {
		if (typeof item !== "string") continue;
		const query = item.trim().replace(/\s+/g, " ");
		if (query.length < 8 || query.length > 240) continue;
		if (/^(block|context|document|cache|stored)\b/i.test(query)) continue;
		const key = query.toLowerCase();
		if (seen.has(key)) continue;
		seen.add(key);
		queries.push(query);
		if (queries.length >= count) break;
	}
	return queries;
}

async function generateForBlock(
	meta: { name: string; description: string; kind: string },
	text: string,
): Promise<{ queries: string[]; model: string | null }> {
	const excerpt = text.length <= 3_000 ? text : `${text.slice(0, 2_000)}\n…\n${text.slice(-1_000)}`;
	const prompt = PROMPT(meta.name, meta.description, meta.kind, excerpt, perBlock);
	if (dryRun) return { queries: [`DRY RUN: ${meta.name}`], model: null };

	const targets = [model, ...fallbacks];
	let lastError = "";
	for (let attempt = 1; attempt <= 2; attempt++) {
		for (const target of targets) {
			try {
				const { content, model: used } = await chatOnce(target, prompt, attempt);
				const queries = parseQueries(content, perBlock);
				if (queries.length) return { queries, model: used };
				lastError = `${target}: unparseable output`;
			} catch (error) {
				lastError = error instanceof Error ? error.message : String(error);
			}
		}
		await new Promise((resolve) => setTimeout(resolve, 1_500 * attempt));
	}
	console.error(`skip ${meta.name}: ${lastError}`);
	return { queries: [], model: null };
}

await refreshMetas(true);
// -- resume ---------------------------------------------------------------
const rows: Array<Record<string, unknown>> = [];
const seen = new Set<string>();
const processed = new Set<string>();
if (!fresh) {
	try {
		const existing = await readFile(outPath, "utf8");
		for (const line of existing.split("\n")) {
			if (!line.trim()) continue;
			try {
				const row = JSON.parse(line) as Record<string, unknown>;
				const key = `${row.query}\u0000${row.expected}`;
				if (seen.has(key)) continue;
				seen.add(key);
				rows.push(row);
				const id = (row.source_block as { id?: string } | undefined)?.id;
				if (id) processed.add(id);
			} catch {
				// skip malformed lines
			}
		}
		if (rows.length) console.error(`resume: ${rows.length} rows, ${processed.size} blocks done`);
	} catch {
		// no previous output
	}
}

const metas = allMetas()
	.filter((meta) => !processed.has(meta.id))
	.slice(0, limit);
if (metas.length === 0 && rows.length === 0) {
	console.error(`no blocks in ${STORE}/blocks`);
	process.exit(1);
}

const usedModels = new Set<string>();
let generated = 0;
let failed = 0;
let done = 0;
let index = 0;

const workers = Array.from({ length: concurrency }, async () => {
	for (;;) {
		const i = index++;
		if (i >= metas.length) return;
		const meta = metas[i];
		let text = "";
		try {
			text = await loadText(meta as never);
		} catch {
			failed += 1;
			done += 1;
			continue;
		}
		if (text.length < 200) {
			done += 1;
			continue;
		}
		const { queries, model: used } = await generateForBlock(meta, text);
		if (used) usedModels.add(used);
		if (queries.length === 0) failed += 1;
		const added: Array<Record<string, unknown>> = [];
		for (const query of queries) {
			const key = `${query}\u0000${meta.id}`;
			if (seen.has(key)) continue;
			seen.add(key);
			const row = {
				schema: "hashtext-eval/1",
				query,
				expected: meta.id,
				tags: ["generated", "decision", "llm", meta.kind],
				source_block: { id: meta.id, name: meta.name, description: meta.description },
			};
			rows.push(row);
			added.push(row);
			generated += 1;
		}
		// Persist per block so a timeout never loses completed work.
		if (added.length) {
			await appendFile(outPath, added.map((row) => JSON.stringify(row)).join("\n") + "\n", "utf8");
		}
		done += 1;
		console.error(`… ${done}/${metas.length} blocks · ${generated} new queries · ${failed} failed`);
	}
});
await Promise.all(workers);

for (const query of ABSTAIN_QUERIES) {
	const key = `${query}\u0000other`;
	if (seen.has(key)) continue;
	seen.add(key);
	rows.push({
		schema: "hashtext-eval/1",
		query,
		expected: "other",
		tags: ["generated", "abstain", "decision", "llm"],
	});
}

if (rows.length === 0) {
	console.error("no queries generated");
	process.exit(1);
}

// Final compaction (dedupe from concurrent appends and earlier runs).
await writeFile(
	outPath,
	rows.map((row) => JSON.stringify(row)).join("\n") + "\n",
	"utf8",
);

// Frozen-candidate rows for a future head-only fine-tune (Laya's eval shape).
let trainRows = 0;
if (trainOut) {
	const lines: string[] = [];
	for (const row of rows) {
		const expected = String(row.expected);
		const candidates = await lexicalCandidates(String(row.query), 7);
		if (candidates.length === 0) continue;
		if (expected !== "other" && !candidates.some((meta) => meta.id === expected)) continue;
		const criteria: Record<string, string> = {
			other: "none of these blocks holds what the request needs",
		};
		for (const meta of candidates) {
			criteria[meta.id] = `${meta.name || "unnamed"}: ${meta.description || meta.kind}`;
		}
		lines.push(
			JSON.stringify({
				state: { input: row.query },
				questions: {
					pick: {
						type: "choice",
						instructions:
							"Which stored block is most likely to hold what this request needs? Choose 'other' when none fit.",
						criteria,
					},
				},
				expected: { pick: expected },
				tags: row.tags,
			}),
		);
		trainRows += 1;
	}
	await writeFile(trainOut, lines.join("\n") + "\n", "utf8");
}

const summary = {
	blocks: metas.length,
	queries: generated,
	abstain: ABSTAIN_QUERIES.length,
	failed_blocks: failed,
	models: [...usedModels],
	out: outPath,
	train_out: trainOut,
	train_rows: trainRows,
};
if (asJson) console.log(JSON.stringify(summary, null, 2));
else {
	console.log(
		`blocks ${summary.blocks} · queries ${summary.queries} (+${summary.abstain} abstain) · failed blocks ${summary.failed_blocks}`,
	);
	console.log(`models: ${summary.models.join(", ") || "none"}`);
	console.log(`wrote ${outPath}${trainOut ? ` and ${trainOut} (${trainRows} rows)` : ""}`);
}
