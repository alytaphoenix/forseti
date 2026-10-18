# Forseti hashtext plugin

Agent-side component: a **fingerprinted context cache** for pi sessions. The
general conversation stays hot; bulky material goes cold and is brought back
only when (and to the extent) the task needs it.

```
/hash list                         compact index + residency
/hash find <query>                 lexical shortlist + Laya pick
/hash put <file> [name]            store a file as a cold block
/hash expand <ref> [mode] [query]  auto|full|outline|focus|range
/hash evict <ref>                  drop it from the hot set (marker remains)
/hash read <ref> [start] [end]     exact line range, numbered
/hash grep <pattern>               regex across cold blocks
/hash stats · gc · archive on|off · auto on|off · log [n] · clear
```

Model tools: `hashtext_put`, `hashtext_list`, `hashtext_find`,
`hashtext_expand`, `hashtext_evict`, `hashtext_read`, `hashtext_grep`.

## How it works

```
system prompt  <hashtext>
               Cold context cache — 137 blocks compressed, not in context.
               resident: 3b29d5cb ci-log, 7b1e22aa q3-financials
               9f2c1a44 auth-note — token refresh decisions · 2.3k tok
               a1b2c3d4 vendor-contract — renewal terms · 12k tok
               … 129 more — hashtext_list or hashtext_find
               </hashtext>                                    small, every turn

transcript     [hashtext 3b29d5cb resident]                 one-line markers
                            │
                            ▼  before every LLM call (pi `context` event)
               [hashtext 3b29d5cb "ci-log" — outline (601 lines) #9f1c2a]
               outline — 601 lines:
               L1-3 BUILD LOG — stage report
               L4-12 …
               [/hashtext 3b29d5cb #9f1c2a]
```

- **Identity is content.** The ref is the sha256 prefix of the plaintext;
  identical text dedups, changed text gets a new ref, invalidation is free.
- **The transcript never carries the bulk.** Expanding leaves a marker; a
  `context` hook substitutes the block's view per request. The transcript is
  cloned per call, so markers persist and are re-materialized every turn.
- **Progressive disclosure.** `mode=auto` returns an **outline** for blocks over
  ~6 KB, `focus` returns matching lines ±2 with omission markers, `range` reads
  lines, `full` is explicit. Large blocks cost ~100–300 tokens until you drill in.
- **One materialization per request.** A repeated marker becomes
  `[hashtext <ref> resident → see above]`.
- **Request-time archival.** Tool results over 8 KB are stored cold at
  `turn_end` (opt-out `/hash archive off`); the `context` projection replaces
  them with a stub carrying the ref. The session file keeps the original; the
  request does not. Four results per turn, `hashtext_*` results excluded.
- **Budget guard.** Expansions are injected in residency order while they fit
  under 90% of the window; the rest are withheld with a visible note. Residency
  is capped at 8 (oldest evicted, reported), and `HASHTEXT_TTL_TURNS` enables
  age-based demotion.

## Native reads, CLI writes

The store is `hashtext` (Python, sibling repo): content-addressed zstd blocks
plus JSON metadata. The extension **reads natively** — metadata scan, Node 22
`zstdDecompressSync`, sha256 verify — which is ~5 ms for 200 blocks instead of a
~38 ms CLI spawn, works when the CLI is not installed, and makes whole-store
grep/range reads cheap. Writes still go through the CLI, which stays the single
owner of the format. The smoke gate cross-checks native reads against
`hashtext show`. A per-session LRU (16 MB / 64 blocks) caches decompressed text;
blocks are immutable, so it never goes stale.

## Laya assist (opt-in)

`/hash auto on` enables the local Laya decision endpoint
(`FORSETI_LAYA_URL`, default `127.0.0.1:8751`):

- **find** — a lexical shortlist (≤7 candidates by name/description/kind, plus a
  native content scan when metadata matching is thin) is offered to Laya as a
  bounded choice with an explicit `other`. Measured: correct pick for 4/4
  topical queries at confidence 0.88–1.00, abstains on unrelated prompts;
  ~30 ms warm. (A binary "needs a block?" gate was tested and rejected — it
  answered "no" to everything.)
- **auto** — calibrated confidence ≥ 0.85 expands the pick; 0.60–0.85 appends a
  `likely: …` suggestion line to the index. "Calibrated" means the isotonic map
  from `<store>/calibration.json` when present (see below), otherwise Laya's
  `answer_confidence`.
- **janitor** — at `turn_end`, with ≥3 resident blocks, Laya picks the least
  likely needed (or `keep`); ≥ 0.80 evicts it. Bounded choice, not a per-block
  yes/no.
- Decisions are appended to `<store>/decisions.jsonl`; a dead endpoint is
  fail-open and never affects a turn.

## Hardening

- Labels are sanitized before they reach the prompt (control chars, `|`, `<`,
  `>`, length caps) in both the store and the renderer.
- Expansion wrappers carry a per-session nonce: a closing tag inside block
  content cannot terminate the wrapper early.
- Refs distinguish *not found* from *ambiguous* (use a longer prefix).
- Residency is authoritative per session — writes never resurrect evicted blocks.
- All cache failures are fail-open: a turn is never broken by a missing CLI,
  a torn metadata file, a corrupt payload, or a dead Laya endpoint.
- Store writes are atomic with fsync; `hashtext gc` removes orphan payloads,
  dangling metadata, and stale temps.

## Telemetry and tuning

Every pick is logged to `<store>/decisions.jsonl` (rotated at 8 MB):

```jsonc
{"kind":"find","query":"what broke the build?","candidates":[{"id","name","description","kind","tokens_est"}],
 "pick":"a3f2c1b9d4e0","confidence":0.93,"latency_ms":38,"probabilities":{...}}
{"kind":"outcome","action":"expand","ref":"a3f2c1b9d4e0","matched_pick":true,"turns_since_pick":0}
{"kind":"outcome","action":"evict","ref":"…","resident_turns":2,"matched_pick":false}
```

`/hash log [n]` shows the last entries. Two scripts turn that into a dataset and
a scorecard, reusing the plugin's *exact* shortlist and Laya client (no logic
drift):

```sh
# labelled rows: authored (best), synthetic (vocab/decision + content/retrieval)
bun run scripts/hashtext-dataset.ts --synthetic 200 --out ~/.hashtext/eval.jsonl
# or LLM-authored: expected<TAB>query per line
bun run scripts/hashtext-dataset.ts --queries queries.tsv --out ~/.hashtext/eval.jsonl
# dump telemetry for later judging
bun run scripts/hashtext-dataset.ts --from-decisions --out ~/.hashtext/train.jsonl

# turn recorded outcomes into labelled rows + a review TSV
bun run scripts/hashtext-label.ts --review ~/.hashtext/review.tsv

# scorecard: recall@k, lexical baseline, pick accuracy, abstention P/R, ECE, latency
bun run scripts/hashtext-eval.ts --offline          # retrieval + baseline
bun run scripts/hashtext-eval.ts                    # + Laya against the live endpoint
```

Measured on the first pass (8-block store; 41 eval rows, 18 authored):

- **Retrieval was the bottleneck**: name/description-only recall@7 was 56.3%
  (12.5% on content-derived queries). The content-aware pass took it to 100%.
- **Laya returns two confidence-like numbers.** `confidence` is normalized
  entropy (decisiveness); `answer_confidence` is max(p), the calibrated
  quantity the library fits and ECE measures. The plugin now gates on
  `answer_confidence` — gating on entropy made the 0.85 threshold unreachable.
- **Even so it is under-confident here**: correct picks at 0.24–0.37, ECE 0.27.
  `scripts/hashtext-calibrate.ts --write` fits a monotonic (isotonic) map from
  `answer_confidence` to empirical accuracy and writes
  `<store>/calibration.json`. On 32 decisions the in-sample ECE went 0.273 →
  0.068, and the 0.85 operating point selected 66% of picks at 95% precision —
  i.e. the threshold became usable. Accuracy is unchanged by construction
  (isotonic is monotone; the argmax can't move).
- Ranking tied the lexical baseline on decision rows; the abstention head is
  where Laya earns its place (7/7 abstentions correct).
- Synthetic keyword queries are tagged `retrieval` (recall only): their labels
  are too noisy for accuracy/ECE. Use authored or telemetry-derived queries for
  judgement metrics.

## Confidence calibration

```sh
bun run scripts/hashtext-calibrate.ts            # report: ECE, threshold table
bun run scripts/hashtext-calibrate.ts --write    # fit + write calibration.json
```

Fit on **decision** rows only, from authored queries or real telemetry. Defaults
refuse to write below 30 samples (`--min`); refit as telemetry accumulates. The
plugin logs raw, entropy, and calibrated confidence on every `find`, so the
operating points can be re-derived without another model pass.

### Labelling real sessions

`scripts/hashtext-label.ts` turns `find`/`outcome` records into labelled rows:

| Signal | Label |
|---|---|
| agent expanded the block Laya picked | expected = pick, `high` |
| agent expanded a different candidate | expected = that block, `medium` |
| labelled block evicted within `--min-resident` turns | downgraded, excluded unless `--include-low` |
| agent expanded nothing | unlabelled (absence is ambiguous) |

`--review FILE` emits `expected<TAB>query` rows for a human/LLM judge; judged
rows are the highest-quality input to `hashtext-calibrate.ts` and any future
fine-tune.

## Install

```sh
# 1 · the store CLI (once)
cd ~/repos/hashtext && uv tool install .
# 2 · the extension (user-level, every pi session)
cp hashtext-plugin/index.ts ~/.pi/agent/extensions/hashtext.ts
# or as a package:
pi install /path/to/forseti/hashtext-plugin
```

Environment: `HASHTEXT_DIR` (store, default `~/.hashtext`), `HASHTEXT_BIN`,
`FORSETI_LAYA_URL`, `HASHTEXT_TTL_TURNS` (default 0 = off).

## Gate

```sh
bun run scripts/hashtext-smoke.ts
```

Real store + real CLI, stubbed pi API, a canned Laya endpoint. Covers compact
index, expand modes, per-turn reconstruction, single materialization, evict,
budget withholding, read/grep, dedup, archival + projection, Laya
find/auto/janitor, `/hash evict`, native-vs-CLI parity, and missing-CLI
degradation (reads must keep working, writes must report the install hint).

## Limits

- Expanded content is re-sent on every model call while resident; the saving is
  that it is not *retained* across turns and that cold blocks cost one index
  line each.
- Archival keeps the session file intact but hides old tool output from the
  model until expanded — intentional, and reversible with `/hash archive off`
  or by expanding the ref.
- Extension state resets on `/reload`; residency is reloaded from disk.
