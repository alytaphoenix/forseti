// hashtext answer judge (free OpenRouter): `bun run scripts/hashtext-judge.ts`
//
// Section-level stores overlap: a generated query names its source block, but
// another block may legitimately answer it too. Strict "expected == source"
// labels then understate retrieval and ranking. This script asks a free model
// which candidate blocks could answer each query and records `acceptable`.
//
//   --data PATH    eval JSONL (default: <store>/generated.jsonl)
//   --out PATH     judged JSONL (default: <data base>.judged.jsonl)
//   --limit N      process at most N unjudged rows
//   --candidates N candidates shown to the judge (default 10)
//   --model ID     default openrouter/free
//   --concurrency  default 2
//   --json
//
// Rows without `acceptable` are judged; rerunning resumes. An empty array means
// "no candidate can answer" (which supports an abstention label).
import { readFile, writeFile } from "node:fs/promises";
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
const { refreshMetas, allMetas, lexicalCandidates, loadText } = plugin;

const dataPath = typeof args.get("data") === "string" ? String(args.get("data")) : path.join(STORE, "generated.jsonl");
const outPath =
	typeof args.get("out") === "string"
		? String(args.get("out"))
		: dataPath.replace(/\.jsonl$/, "") + ".judged.jsonl";
const limit = args.has("limit") ? Math.max(1, Number(args.get("limit"))) : Infinity;
const candidateCount = Math.min(8, Math.max(3, Number(args.get("candidates") ?? 6)));
const snippetChars = Math.max(0, Math.min(2_000, Number(args.get("snippet") ?? 600)));
const model = typeof args.get("model") === "string" ? String(args.get("model")) : "openrouter/free";
const concurrency = Math.max(1, Math.min(4, Number(args.get("concurrency") ?? 2)));
const timeoutMs = Number(args.get("timeout") ?? 60_000);
const asJson = args.has("json");

async function apiKey(): Promise<string> {
	if (process.env.OPENROUTER_API_KEY) return process.env.OPENROUTER_API_KEY.trim();
	try {
		return (await readFile(path.join(homedir(), ".config", "forseti", "openrouter.key"), "utf8")).trim();
	} catch {
		return "";
	}
}
const KEY = await apiKey();
if (!KEY) {
	console.error("no OpenRouter API key: set OPENROUTER_API_KEY or add ~/.config/forseti/openrouter.key");
	process.exit(1);
}

interface Row {
	schema?: string;
	query?: string;
	expected?: string;
	tags?: string[];
	acceptable?: string[];
	judge_model?: string;
}

let raw = "";
// Resume from the judged file when it exists: it contains every row plus the
// labels already earned.
const sourcePath = outPath !== dataPath ? await readFile(outPath, "utf8").then(() => outPath).catch(() => dataPath) : dataPath;
try {
	raw = await readFile(sourcePath, "utf8");
} catch {
	console.error(`no data at ${dataPath}`);
	process.exit(1);
}
const rows: Row[] = [];
for (const line of raw.split("\n")) {
	if (!line.trim()) continue;
	try {
		rows.push(JSON.parse(line) as Row);
	} catch {
		// skip malformed
	}
}

await refreshMetas(true);
const metas = allMetas();
const byId = new Map(metas.map((meta) => [meta.id, meta]));

const pending = rows.filter((row) => row.query && !Array.isArray(row.acceptable)).slice(0, limit);
if (pending.length === 0) {
	console.error(`nothing to judge (${rows.length} rows, all with acceptable labels)`);
	process.exit(0);
}

async function judge(row: Row): Promise<{ acceptable: string[]; model: string } | null> {
	const query = String(row.query);
	const expected = String(row.expected ?? "");
	const candidates = await lexicalCandidates(query, candidateCount);
	if (expected !== "other" && !candidates.some((meta) => meta.id === expected)) {
		const meta = byId.get(expected);
		if (meta) candidates.push(meta);
	}
	if (candidates.length === 0) return { acceptable: [], model: "none" };

	const lines: string[] = [];
	for (const meta of candidates) {
		let body = "";
		try {
			body = (await loadText(meta)).slice(0, snippetChars).replace(/\s+/g, " ").trim();
		} catch {
			// snippet is optional
		}
		lines.push(`[${meta.id}] ${meta.name || "(unnamed)"} — ${(meta.description || meta.kind).slice(0, 160)}`);
		if (body) lines.push(`    ${body}`);
	}
	const prompt = [
		"A developer request and candidate stored blocks are below.",
		"Which candidates contain material that could ANSWER the request? Judge the content, not the title.",
		"Return ONLY a JSON array of candidate ids. Use [] when none could answer.",
		"",
		`REQUEST: ${query}`,
		"CANDIDATES:",
		...lines,
	].join("\n");

	for (let attempt = 1; attempt <= 2; attempt++) {
		let lastError = "";
		try {
			const res = await fetch("https://openrouter.ai/api/v1/chat/completions", {
				method: "POST",
				headers: {
					Authorization: `Bearer ${KEY}`,
					"Content-Type": "application/json",
					"HTTP-Referer": "https://github.com/alytaphoenix/forseti",
					"X-Title": "hashtext judge",
				},
				signal: AbortSignal.timeout(timeoutMs),
				body: JSON.stringify({
					model,
					messages: [{ role: "user", content: prompt }],
					max_tokens: 800,
					temperature: 0,
					reasoning: { exclude: true },
				}),
			});
			if (!res.ok) {
				throw new Error(`HTTP ${res.status} ${(await res.text()).slice(0, 160)}`);
			}
			const data = (await res.json()) as {
				model?: string;
				choices?: Array<{ message?: { content?: string } }>;
			};
			let text = data.choices?.[0]?.message?.content ?? "";
			text = text.replace(/^```(?:json)?/i, "").replace(/```$/i, "").trim();
			const start = text.indexOf("[");
			const end = text.lastIndexOf("]");
			if (start === -1 || end <= start) {
				throw new Error(`unparseable: ${text.slice(0, 160)}`);
			}
			const valid = candidates.map((meta) => meta.id);
			let acceptable: string[] = [];
			try {
				const parsed = JSON.parse(text.slice(start, end + 1));
				if (Array.isArray(parsed)) {
					acceptable = parsed.filter((id): id is string => typeof id === "string" && valid.includes(id));
				}
			} catch {
				// The model sometimes wraps the array in prose; fall back to id scanning.
				acceptable = valid.filter((id) => text.includes(id));
			}
			return { acceptable: [...new Set(acceptable)], model: data.model ?? model };
		} catch (error) {
			lastError = error instanceof Error ? error.message : String(error);
		}
		if (attempt === 2) console.error(`judge failed for "${query.slice(0, 60)}": ${lastError}`);
	}
	return null;
}

let index = 0;
let judged = 0;
let failed = 0;
const workers = Array.from({ length: concurrency }, async () => {
	for (;;) {
		const i = index++;
		if (i >= pending.length) return;
		const row = pending[i];
		const result = await judge(row);
		if (result) {
			row.acceptable = result.acceptable;
			row.judge_model = result.model;
			judged += 1;
		} else {
			failed += 1;
		}
		// Persist periodically so a timeout does not lose progress.
		if ((judged + failed) % 5 === 0) {
			await writeFile(outPath, rows.map((entry) => JSON.stringify(entry)).join("\n") + "\n", "utf8");
		}
		console.error(`… ${judged + failed}/${pending.length} judged (${failed} failed)`);
	}
});
await Promise.all(workers);
await writeFile(outPath, rows.map((entry) => JSON.stringify(entry)).join("\n") + "\n", "utf8");

const summary = {
	rows: rows.length,
	judged,
	failed,
	with_labels: rows.filter((row) => Array.isArray(row.acceptable)).length,
	no_answer: rows.filter((row) => Array.isArray(row.acceptable) && row.acceptable.length === 0).length,
	out: outPath,
};
if (asJson) console.log(JSON.stringify(summary, null, 2));
else {
	console.log(`rows ${summary.rows} · judged ${summary.judged} · failed ${summary.failed}`);
	console.log(`labelled ${summary.with_labels} · no-answer ${summary.no_answer} -> ${outPath}`);
}
