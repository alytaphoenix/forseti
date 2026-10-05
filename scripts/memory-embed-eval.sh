#!/bin/sh
# memory-embed-eval: bounded embedding-model comparison for the memory service
# (P12-6). Ranks the SAME probe set (paraphrase + exact-identifier queries)
# under each 384-d candidate and prints the tally. Swap only if a candidate
# beats the incumbent on this probe set — all published constants were tuned
# on 1024-d embeddings (S23), so our own probes are the only trustworthy dial.
#   scripts/memory-embed-eval.sh
# Exit 0 = pass (all models produce sane rankings; incumbent not necessarily best).
set -eu
cd "$(dirname "$0")/.."

VENV="${FORSETI_LAYA_VENV:-$HOME/.config/forseti/laya-venv}"
[ -x "$VENV/bin/python" ] || { echo "memory-embed-eval: no venv — run scripts/laya-setup.sh"; exit 2; }

"$VENV/bin/python" << 'EOF'
import sys
from sentence_transformers import SentenceTransformer

facts = [
    "the release train freezes every second Thursday at noon UTC",
    "the primary database is named orion-primary in the private cloud",
    "new services must register in the service catalog within one week",
    "the legacy migration tool is called mig2x",
    "always run the smoke gate before pushing to main",
]
queries = [  # (query, expected fact index, kind)
    ("when does the release train freeze?", 0, "paraphrase"),
    ("what is the primary database called?", 1, "paraphrase"),
    ("how soon must services be catalogued?", 2, "paraphrase"),
    ("mig2x", 3, "exact-identifier"),
    ("how do we push safely to main?", 4, "paraphrase"),
]
# e5-family models need asymmetric prefixes
def embed(m, texts, is_query):
    if "e5" in m:
        pref = "query: " if is_query else "passage: "
        return m.encode([pref + t for t in texts], normalize_embeddings=True)
    return m.encode(texts, normalize_embeddings=True)

models = ["all-MiniLM-L6-v2", "BAAI/bge-small-en-v1.5", "thenlper/gte-small", "intfloat/e5-small-v2"]
results = {}
for name in models:
    m = SentenceTransformer(name)
    f = embed(m, facts, False)
    ok = 0
    rows = []
    for q, expected, kind in queries:
        qv = embed(m, [q], True)[0]
        sims = (f @ qv)
        top = int(max(range(len(facts)), key=lambda i: sims[i]))
        passed = top == expected
        ok += passed
        rows.append(f"    {'PASS' if passed else 'FAIL'} [{kind:16}] {q[:38]:40} → #{top}")
    results[name] = (ok, rows)
    del m

print("embedding-model comparison (5 probes each):")
for name, (ok, rows) in results.items():
    print(f"  {name}: {ok}/5")
    for r in rows:
        print(r)

# P13-M21b: the tuple is (ok, rows) — the old unpack bound the ROWS to
# inc_ok, masked only because the swap branch short-circuited; the new
# floor check below exposed it (list < int TypeError).
inc_ok, _inc_rows = results["all-MiniLM-L6-v2"]
best_name = max(results, key=lambda n: results[n][0])
best_ok = results[best_name][0]
print()
if best_name != "all-MiniLM-L6-v2" and best_ok > inc_ok:
    print(f"RECOMMEND SWAP: {best_name} ({best_ok}/5) beats the incumbent ({inc_ok}/5).")
    print("Swap = FORSETI_MEMORY_MODEL env (models.json/venv already present) + re-embed migration.")
else:
    print("KEEP INCUMBENT: no candidate beats all-MiniLM-L6-v2 on this probe set.")

# P13-M21: this script ALWAYS exited 0 — useless as a signal. The exit code
# now gates the INCUMBENT's sanity (floor 4/5): a silent venv/model regression
# must fail the gate, while candidate tallies stay informational comparison.
MIN_INCUMBENT = 4
if inc_ok < MIN_INCUMBENT:
    print(f"GATE FAIL: incumbent {inc_ok}/5 is below the {MIN_INCUMBENT}/5 sanity floor.")
    sys.exit(1)
print(f"incumbent sanity floor met ({inc_ok}/5 >= {MIN_INCUMBENT}/5)")
sys.exit(0)
EOF
echo "memory-embed-eval: done"