#!/bin/sh
# memory-eval: deterministic gate for the shared memory layer (P10-5).
# Writes the probe facts into a DEDICATED eval namespace, recalls them by
# paraphrase, asserts rank + content + namespace isolation.
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

total = len(probes) + 1
print(f"{ok}/{total} probes passed")
sys.exit(0 if ok == total else 1)
EOF
echo "memory-eval: PASS"
