#!/usr/bin/env bash
# crew-smoke.sh — Phase 6 gate: check-node crew run in a sandbox session on halogen.
# The harness proves itself: assertions live in the crew file (6A-1), the run is
# hermetic (6A-2 --session sandbox), and the model is the free LAN model (6A-3).
# Requires: herdr server running, pi configured (halogen reachable), crew built.
set -euo pipefail
cd "$(dirname "$0")/.."

BIN="crew/bin/forseti-crew"
CREWFILE="crew/examples/crew-checks.yaml"
PROBE="HELLO_CREW.md"

echo "crew-smoke: building…"
( cd crew && go build -o bin/forseti-crew ./cmd/forseti-crew )

echo "crew-smoke: validating examples…"
"$BIN" validate -f "crew/examples/crew.yaml" >/dev/null
"$BIN" validate -f "$CREWFILE" >/dev/null

echo "crew-smoke: running checked crew headless in sandbox session (halogen; a few minutes)…"
rm -f "$PROBE"
if ! "$BIN" run --headless -f "$CREWFILE" --session sandbox > /tmp/crew-smoke-events.jsonl 2>&1; then
  echo "crew-smoke: FAIL — run exited nonzero"
  tail -8 /tmp/crew-smoke-events.jsonl
  exit 1
fi

if ! grep -q '"type":"run_end"' /tmp/crew-smoke-events.jsonl; then
  echo "crew-smoke: FAIL — no run_end event"
  exit 1
fi
if ! grep -q '"type":"check_pass"' /tmp/crew-smoke-events.jsonl; then
  echo "crew-smoke: FAIL — no check_pass event (checks did not execute)"
  grep -E '"type":"check_' /tmp/crew-smoke-events.jsonl || true
  exit 1
fi
if ! grep -q '"type":"pattern_matched","node":"builder"' /tmp/crew-smoke-events.jsonl; then
  echo "crew-smoke: FAIL — builder watch never matched DONE"
  exit 1
fi
if [ ! -f "$PROBE" ] || ! grep -q "crew pipeline works" "$PROBE"; then
  echo "crew-smoke: FAIL — $PROBE missing or wrong content (planner→builder handoff broken)"
  exit 1
fi
if [ ! -s ".forseti/bus/planner.md" ] || [ ! -s ".forseti/bus/builder.md" ]; then
  echo "crew-smoke: FAIL — bus files empty"
  exit 1
fi

# hermeticity: the live (default) session must show no crew agents
if herdr agent list 2>/dev/null | grep -qE 'planner|builder'; then
  echo "crew-smoke: FAIL — crew agents leaked into the live session"
  exit 1
fi

rm -f "$PROBE"
echo "crew: PASS — checked crew in sandbox session, halogen, bus files, watch event all good"
