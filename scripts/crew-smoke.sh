#!/usr/bin/env bash
# crew-smoke.sh — Phase 5 gate: 2-agent headless crew run E2E.
# Requires: herdr server running, pi configured (opencode-go), crew built.
set -euo pipefail
cd "$(dirname "$0")/.."

BIN="crew/bin/forseti-crew"
CREWFILE="crew/examples/crew.yaml"
PROBE="HELLO_CREW.md"

echo "crew-smoke: building…"
( cd crew && go build -o bin/forseti-crew ./cmd/forseti-crew )

echo "crew-smoke: validating example…"
"$BIN" validate -f "$CREWFILE" >/dev/null

echo "crew-smoke: running headless crew (this takes a few minutes)…"
rm -f "$PROBE"
if ! "$BIN" run --headless -f "$CREWFILE" > /tmp/crew-smoke-events.jsonl 2>&1; then
  echo "crew-smoke: FAIL — run exited nonzero"
  tail -5 /tmp/crew-smoke-events.jsonl
  exit 1
fi

if ! grep -q '"type":"run_end"' /tmp/crew-smoke-events.jsonl; then
  echo "crew-smoke: FAIL — no run_end event"
  exit 1
fi
if grep -qE '"type":"(node_failed|node_blocked)"' /tmp/crew-smoke-events.jsonl; then
  echo "crew-smoke: FAIL — node failed/blocked"
  grep -E '"type":"(node_failed|node_blocked)"' /tmp/crew-smoke-events.jsonl
  exit 1
fi
if [ ! -f "$PROBE" ] || ! grep -q "crew pipeline works" "$PROBE"; then
  echo "crew-smoke: FAIL — $PROBE missing or wrong content (planner→coder handoff broken)"
  exit 1
fi
if [ ! -s ".forseti/bus/planner.md" ] || [ ! -s ".forseti/bus/builder.md" ]; then
  echo "crew-smoke: FAIL — bus files empty"
  exit 1
fi

rm -f "$PROBE"
echo "crew: PASS — planner→coder handoff, bus files, run log all good"
