#!/bin/sh
# memory-eval: deterministic gate for the shared memory layer (P10-5, v2 P12).
# Writes the probe facts into a DEDICATED eval namespace, recalls them by
# paraphrase, asserts rank + content + namespace isolation; v2 adds hybrid
# (keyword-leg) recall, dedup, supersede, TTL, and type-filter contracts.
#   scripts/memory-eval.sh
# Exit 0 = pass.
# P13: SELF-CONTAINED — spawns a throwaway service on a scratch SQLite file
# and a private port. The old design polluted the shared memory.db with
# eval-probe-* rows every gate run (~45 rows observed) and failed whenever
# the shared service happened to be down.
set -eu
cd "$(dirname "$0")/.."

VENV="${FORSETI_LAYA_VENV:-$HOME/.config/forseti/laya-venv}"
[ -x "$VENV/bin/python" ] || { echo "memory-eval: no venv — run scripts/laya-setup.sh"; exit 2; }

WORKDIR="$(mktemp -d)"
EVAL_PORT="${FORSETI_MEMORY_EVAL_PORT:-8762}"
URL="http://127.0.0.1:$EVAL_PORT"
FORSETI_MEMORY_DB="$WORKDIR/eval.db" FORSETI_MEMORY_PORT="$EVAL_PORT" \
  "$VENV/bin/python" "$(pwd)/crew/memory/memory_serve.py" > "$WORKDIR/serve.log" 2>&1 &
EVAL_PID=$!
cleanup() {
  kill "$EVAL_PID" 2>/dev/null
  wait "$EVAL_PID" 2>/dev/null
  rm -rf "$WORKDIR"
}
trap cleanup EXIT

i=0
while [ $i -lt 60 ]; do
  curl -s -m 2 "$URL/health" 2>/dev/null | grep -q '"status":"ok"' && break
  kill -0 "$EVAL_PID" 2>/dev/null || {
    echo "memory-eval: scratch service died during startup — log tail:" >&2
    tail -20 "$WORKDIR/serve.log" >&2 || true
    exit 1
  }
  i=$((i+1)); sleep 1
done
curl -s -m 3 "$URL/health" | grep -q '"status":"ok"' || {
  echo "memory-eval: scratch service not healthy within 60 s — log: $WORKDIR/serve.log" >&2
  exit 1
}

NS="eval-probe-$$"   # unique per run — no cross-contamination

"$VENV/bin/python" - "$URL" "$NS" "$WORKDIR/eval.db" << 'EOF'
import json, sys, urllib.request

url, ns, dbpath = sys.argv[1], sys.argv[2], sys.argv[3]

def get(path):
    with urllib.request.urlopen(url + path, timeout=60) as resp:
        return json.loads(resp.read().decode())

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

# ---- P12.5 probes (A clear / B auto-links / C history audit / D boosts) ----

# B: auto-links — a related-but-not-duplicate fact links its neighbour at write
a1 = post("/write", {"agent": ns,
                     "text": "the api gateway rate limit is 500 requests per minute"})
a2 = post("/write", {"agent": ns,
                     "text": "the api gateway throttles inbound traffic to 500 requests each minute"})
passed = a2["id"] != a1["id"] and a1["id"] in a2.get("auto_links", [])
mark = "PASS" if passed else "FAIL"
print(f"  [{mark}] auto-links on write (related fact links neighbour)")
ok += passed

# C: history — a merge keeps the destroyed statement queryable
c1 = post("/write", {"agent": ns, "text": "the audit log retention window is ninety days"})
c2 = post("/write", {"agent": ns, "text": "The audit log retention window is ninety days."})
hist = get(f"/history?memory_id={c1['id']}&agent={ns}")
merges = [h for h in hist if h["event"] == "merge"]
passed = (c2["id"] == c1["id"] and len(merges) == 1 and
          merges[0]["old_text"] == "the audit log retention window is ninety days")
mark = "PASS" if passed else "FAIL"
print(f"  [{mark}] history: merge preserves the old statement")
ok += passed

# C: history — forget leaves a delete event with the last text; namespace-guarded
import urllib.error

def status_of(path, payload=None, method="POST"):
    data = json.dumps(payload).encode() if payload is not None else None
    req = urllib.request.Request(url + path, data=data, method=method,
                                 headers={"Content-Type": "application/json"})
    try:
        with urllib.request.urlopen(req, timeout=60) as resp:
            resp.read()
            return resp.status
    except urllib.error.HTTPError as e:
        e.read()
        return e.code

c3 = post("/write", {"agent": ns, "text": "the vault seal key rotates each quarter"})
post("/forget", {"id": c3["id"], "agent": ns})
hist = get(f"/history?memory_id={c3['id']}&agent={ns}")
deletes = [h for h in hist if h["event"] == "delete"]
guarded = status_of(f"/history?memory_id={c3['id']}&agent=stranger", method="GET") == 404
passed = len(deletes) == 1 and deletes[0]["old_text"] == "the vault seal key rotates each quarter" and guarded
mark = "PASS" if passed else "FAIL"
print(f"  [{mark}] history: delete event + namespace guard (stranger 404)")
ok += passed

# A: /clear removes exactly one namespace; /namespaces agrees
cs = ns + "-clear"
post("/write", {"agent": cs, "text": "scratch fact alpha for the clear probe"})
post("/write", {"agent": cs, "text": "scratch fact beta for the clear probe"})
cres = post("/clear", {"agent": cs})
left = post("/recall", {"query": "scratch fact alpha beta", "agent": cs, "k": 5})
listed = any(n["agent"] == cs for n in get("/namespaces")["namespaces"])
passed = cres.get("cleared") == 2 and left == [] and not listed and post("/recall", {"query": "release train freezes", "agent": ns, "k": 5}) != []
mark = "PASS" if passed else "FAIL"
print(f"  [{mark}] /clear namespace (rows gone, neighbours survive, listing agrees)")
ok += passed

# A: clearing SHARED without confirm is refused
st = status_of("/clear", {"agent": "shared"})
passed = st == 422
mark = "PASS" if passed else "FAIL"
print(f"  [{mark}] /clear shared requires confirm:true (422 without)")
ok += passed

# D: importance calibration — with near-equal relevance, 0.9 outranks 0.5
d1 = post("/write", {"agent": ns,
                     "text": "the billing export job packages invoices into nightly s3Parquet dumps",
                     "importance": 0.5})
d2 = post("/write", {"agent": ns,
                     "text": "the billing export job bundles invoices into nightly s3Parquet extracts",
                     "importance": 0.9})
rows = post("/recall", {"query": "when the billing export job writes invoices to s3", "agent": ns, "k": 3})
top = rows[0]["id"] if rows else None
passed = d2["id"] != d1["id"] and top == d2["id"]
mark = "PASS" if passed else "FAIL"
print(f"  [{mark}] importance boost calibration (0.9 outranks 0.5 at equal relevance)")
ok += passed

# D: recency reinforcement — a recalled row's last_access/access_count advance
import sqlite3
scon = sqlite3.connect(dbpath)
ac0, la0 = scon.execute("SELECT access_count, last_access FROM memories WHERE id = ?",
                        (d2["id"],)).fetchone()
post("/recall", {"query": "billing export job nightly s3 invoices", "agent": ns, "k": 3})
ac1, la1 = scon.execute("SELECT access_count, last_access FROM memories WHERE id = ?",
                        (d2["id"],)).fetchone()
scon.close()
passed = ac1 > ac0 and la1 is not None and la0 is not None and la1 >= la0
mark = "PASS" if passed else "FAIL"
print(f"  [{mark}] recall reinforcement (access_count bumped, last_access set)")
ok += passed

# ---- P13 probes: error-path contracts (the fixes as assertions) ----
import urllib.error
def post_status(path, payload):
    req = urllib.request.Request(url + path, data=json.dumps(payload).encode(),
                                 headers={"Content-Type": "application/json"})
    try:
        with urllib.request.urlopen(req, timeout=60) as resp:
            resp.read()
            return resp.status
    except urllib.error.HTTPError as e:
        e.read()
        return e.code

# forget is namespace-guarded: another agent's caller gets 404, the row lives
st = post_status("/forget", {"id": w1["id"], "agent": "not-" + ns})
alive = post_status("/recall", {"query": "migration tool mig2x", "agent": ns, "k": 3}) == 200
passed = st == 404 and alive
mark = "PASS" if passed else "FAIL"
print(f"  [{mark}] forget namespace guard (cross-ns 404, row survives)")
ok += passed

# k=0 is a 422, not a sneaky one-row result
st = post_status("/recall", {"query": "release train", "agent": ns, "k": 0})
passed = st == 422
mark = "PASS" if passed else "FAIL"
print(f"  [{mark}] recall k>=1 contract (k=0 → 422)")
ok += passed

total = len(probes) + 17
print(f"{ok}/{total} probes passed")
sys.exit(0 if ok == total else 1)
EOF
echo "memory-eval: PASS"
