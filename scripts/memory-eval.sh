#!/bin/sh
# memory-eval: deterministic gate for the shared memory layer (P10-5, v2 P12).
# Writes the probe facts into a DEDICATED eval namespace, recalls them by
# paraphrase, asserts rank + content + namespace isolation; v2 adds hybrid
# (keyword-leg) recall, dedup, supersede, TTL, and type-filter contracts.
#   scripts/memory-eval.sh
# Exit 0 = pass. Requires the endpoint: scripts/memory-serve.sh start
set -eu
cd "$(dirname "$0")/.."

URL="${FORSETI_MEMORY_URL:-http://127.0.0.1:8752}"
NS="eval-probe-$$"   # unique per run — no cross-contamination

curl -s -m 3 "$URL/health" | grep -q '"status":"ok"' || {
  echo "memory-eval: endpoint down at $URL — run scripts/memory-serve.sh start"
  exit 2
}

VENV="${FORSETI_LAYA_VENV:-$HOME/.config/forseti/laya-venv}"
[ -x "$VENV/bin/python" ] || { echo "memory-eval: no venv — run scripts/laya-setup.sh"; exit 2; }

"$VENV/bin/python" - "$URL" "$NS" << 'EOF'
import json, sys, urllib.request

url, ns = sys.argv[1], sys.argv[2]

def post(path, payload):
    req = urllib.request.Request(url + path, data=json.dumps(payload).encode(),
                                 headers={"Content-Type": "application/json"})
    with urllib.request.urlopen(req, timeout=60) as resp:
        return json.load(resp)

facts = [
    ("the release train freezes every second Thursday at noon UTC", ["release"]),
    ("the primary database is named orion-primary in the private cloud", ["infra"]),
    ("new services must register in the service catalog within one week", ["policy"]),
]
for text, tags in facts:
    post("/write", {"agent": ns, "text": text, "tags": tags})
# an isolation probe: a private fact from another agent
post("/write", {"agent": "eval-other-" + ns, "text": "SECRET FACT that must never leave its namespace"})

ok = 0
probes = [
    ("when does the release train freeze?", facts[0][0]),
    ("what is the primary database called?", facts[1][0]),
    ("how soon must services be catalogued?", facts[2][0]),
]
for q, expected in probes:
    rows = post("/recall", {"query": q, "agent": ns, "k": 3})
    top = rows[0] if rows else None
    passed = bool(top) and top["text"] == expected
    mark = "PASS" if passed else "FAIL"
    detail = f"{(top or {}).get('text', 'NO RESULTS')[:60]}" if top else "no results"
    print(f"  [{mark}] {q[:45]:47} → {detail}")
    ok += passed

# namespace isolation: the other agent's private fact must be invisible
rows = post("/recall", {"query": "SECRET FACT", "agent": ns, "k": 5})
leaked = any("SECRET" in r["text"] for r in rows)
mark = "PASS" if not leaked else "FAIL"
print(f"  [{mark}] namespace isolation (private fact hidden)")
ok += not leaked

# ---- v2 probes (P12: hybrid retrieval, dedup, supersede, TTL, type) ----

# exact dedup: rewriting the same fact returns the same id
w1 = post("/write", {"agent": ns, "text": "the legacy migration tool is called mig2x"})
w2 = post("/write", {"agent": ns, "text": "the legacy migration tool is called mig2x"})
passed = w1["id"] == w2["id"]
mark = "PASS" if passed else "FAIL"
print(f"  [{mark}] exact dedup (same fact twice → same id)")
ok += passed

# near-dup merge: trivial rewording merges into the existing row
w3 = post("/write", {"agent": ns, "text": "The legacy migration tool is called mig2x."})
passed = w3["id"] == w1["id"]
mark = "PASS" if passed else "FAIL"
print(f"  [{mark}] near-dup merge (rewording → same id)")
ok += passed

# hybrid: bare-identifier query (lexical hit; vector-only recall ranks noise)
# NOTE: the row's text may be either dedup variant (near-dup merge rewrote it) —
# the contract is "the migration row is recalled", asserted by id.
rows = post("/recall", {"query": "mig2x", "agent": ns, "k": 3})
top = rows[0] if rows else None
passed = bool(top) and top["id"] == w1["id"]
mark = "PASS" if passed else "FAIL"
detail = f"#{(top or {}).get('id')} {(top or {}).get('text', 'NO RESULTS')[:55]}" if top else "no results"
print(f"  [{mark}] hybrid keyword leg (bare identifier query) → {detail}")
ok += passed

# supersede: a contradicting fact hides the old one by default
w4 = post("/write", {"agent": ns, "text": "the deploy cutoff is 14:00 UTC"})
w5 = post("/write", {"agent": ns, "text": "the deploy cutoff is 15:00 UTC", "supersedes": w4["id"]})
rows = post("/recall", {"query": "what is the deploy cutoff", "agent": ns, "k": 5})
texts = [r["text"] for r in rows]
passed = "the deploy cutoff is 15:00 UTC" in texts and "the deploy cutoff is 14:00 UTC" not in texts
mark = "PASS" if passed else "FAIL"
print(f"  [{mark}] supersede (old fact hidden, history kept) → {texts[:2]}")
ok += passed
# ...and visible with include_superseded
rows = post("/recall", {"query": "what is the deploy cutoff", "agent": ns, "k": 5, "include_superseded": True})
passed = len([r for r in rows if "deploy cutoff" in r["text"]]) == 2
mark = "PASS" if passed else "FAIL"
print(f"  [{mark}] superseded history still queryable (include_superseded)")
ok += passed

# TTL: an expired fact no longer recalls
w6 = post("/write", {"agent": ns, "text": "the incident bridge call starts now",
                     "expires_at": "2020-01-01T00:00:00+00:00"})
rows = post("/recall", {"query": "when does the incident bridge call start", "agent": ns, "k": 5})
passed = not any(r["id"] == w6["id"] for r in rows)
mark = "PASS" if passed else "FAIL"
print(f"  [{mark}] TTL (expired fact hidden)")
ok += passed

# type filter: a procedure recall excludes facts/episodes
w7 = post("/write", {"agent": ns, "text": "always run the smoke gate before pushing to main",
                     "type": "procedure", "importance": 0.9})
rows = post("/recall", {"query": "smoke gate before pushing", "agent": ns, "k": 5, "type": "procedure"})
passed = len(rows) == 1 and rows[0]["id"] == w7["id"] and rows[0]["importance"] == 0.9
mark = "PASS" if passed else "FAIL"
print(f"  [{mark}] type filter + importance (procedure-only recall)")
ok += passed

total = len(probes) + 8
print(f"{ok}/{total} probes passed")
sys.exit(0 if ok == total else 1)
EOF
echo "memory-eval: PASS"
