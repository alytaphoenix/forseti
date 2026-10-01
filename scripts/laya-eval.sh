#!/bin/sh
# laya-eval: the deterministic gate for the Laya decision layer (P8-4).
# Runs crew/eval/laya-probe.jsonl through the local endpoint and checks:
#   choice   → argmax == expected
#   noul_high → P(true) >= 0.7 (and < 0.3 for the low probes)
#   score_high → expected level is the argmax of the score distribution
# Exit 0 = all pass. Also prints a one-line calibration note (confidence vs
# observed correctness — the article's discipline: accuracy ≠ confidence).
# Usage: scripts/laya-eval.sh [--url http://127.0.0.1:8751]
set -eu
cd "$(dirname "$0")/.."

URL="${FORSETI_LAYA_URL:-http://127.0.0.1:8751}"
PROBE="crew/eval/laya-probe.jsonl"

curl -s -m 3 "$URL/health" | grep -q '"status":"ok"' || {
  echo "laya-eval: endpoint down at $URL — run scripts/laya-serve.sh start"
  exit 2
}

VENV="${FORSETI_LAYA_VENV:-$HOME/.config/forseti/laya-venv}"
[ -x "$VENV/bin/python" ] || { echo "laya-eval: no venv — run scripts/laya-setup.sh"; exit 2; }

"$VENV/bin/python" - "$URL" "$PROBE" << 'EOF'
import json, sys, urllib.request

url, probe_path = sys.argv[1], sys.argv[2]
rows = [json.loads(l) for l in open(probe_path) if l.strip()]
ok = 0
notes = []
for i, row in enumerate(rows, 1):
    kind, expected = row["kind"], row["expected"]
    qname = "decide"
    if kind == "choice":
        # criteria come from the probe row (real rubric text — the article's
        # point: criteria quality decides quality)
        criteria = dict(row.get("criteria") or {})
        if not criteria:
            labels = sorted({r["expected"] for r in rows if r["kind"] == "choice"} | {"other"})
            criteria = {l: f"the output fits {l}" for l in labels if l != "other"}
            criteria["other"] = "none of the options fit"
        q = {"type": "choice", "instructions": "Which option best fits this output?", "criteria": criteria}
        state = {"input": row["state"]}
    elif kind == "noul_high":
        state = {"input": row["state"]}
        q = {"type": "noul", "instructions": "Does the state contain an explicit threat to cancel or block progress?"}
        qname = "threat"
    elif kind == "score_high":
        state = {"input": row["state"]}
        q = {"type": "score", "instructions": "How urgent is this?",
             "criteria": ["routine", "soon", "blocking"]}
        qname = "urgency"
    else:
        notes.append(f"probe {i}: unknown kind {kind}"); continue

    body = json.dumps({"state": state, "questions": {qname: q}}).encode()
    req = urllib.request.Request(url + "/v1/systemone", data=body,
                                 headers={"Content-Type": "application/json"})
    with urllib.request.urlopen(req, timeout=30) as resp:
        out = json.load(resp)
    a = out["answers"][qname]

    if kind == "choice":
        probs = a.get("probabilities", {})
        argmax = max(probs, key=probs.get) if probs else a.get("choice")
        if expected == "abstain":
            # confusable rows assert the ABSTENTION policy: the model must
            # not resolve confidently (a threshold gate would abstain)
            passed = a.get("confidence", 1.0) < 0.5
            detail = f"argmax={argmax} p={probs.get(argmax, 0):.3f} conf={a.get('confidence', 0):.3f} (<0.5 required)"
        else:
            passed = argmax == expected
            detail = f"argmax={argmax} p={probs.get(argmax, 0):.3f}"
    elif kind == "noul_high":
        p = a.get("noul", -1)
        passed = (p >= 0.7) if expected == "high" else (p < 0.3)
        detail = f"P(true)={p:.3f}"
    else:  # score_high: rubric levels map by index over the criteria order
        probs = a.get("probabilities", {})
        levels = q["criteria"]
        idx = max(probs, key=probs.get) if probs else None
        try:
            argmax = levels[int(idx)]
        except (TypeError, ValueError, IndexError):
            argmax = idx
        passed = argmax == expected
        detail = f"argmax={argmax} (idx {idx})"
    mark = "PASS" if passed else "FAIL"
    ok += passed
    print(f"  [{mark}] {row['note']}: {detail} (conf {a.get('confidence', 0):.3f})")

print(f"{ok}/{len(rows)} probes passed")
# calibration note: mean confidence on correct vs incorrect answers is not
# computed here (needs a larger set) — the probe set is a smoke gate, not a
# calibration study. Reliability diagrams belong to laya's own eval tooling.
if ok != len(rows):
    sys.exit(1)
EOF
echo "laya-eval: PASS"
