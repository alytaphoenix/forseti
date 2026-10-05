#!/usr/bin/env bash
# crew-smoke.sh — Phase 6 gate: check-node crew run in a sandbox session on halogen.
# The harness proves itself: assertions live in the crew file (6A-1), the run is
# hermetic (6A-2 --session sandbox), and the model is the free LAN model (6A-3).
# P11 extension: a second, PHASED crew proves the strict barrier — the build
# phase may only dispatch after the research phase fully settles (phase events
# in the run log are the evidence).
# Requires: herdr server running, pi configured (halogen reachable), crew built.
set -euo pipefail
cd "$(dirname "$0")/.."

BIN="crew/bin/forseti-crew"
CREWFILE="crew/examples/crew-checks.yaml"
PHASEFILE="crew/examples/crew-phases.yaml"
PROBE="HELLO_CREW.md"

echo "crew-smoke: building…"
( cd crew && go build -o bin/forseti-crew ./cmd/forseti-crew )

echo "crew-smoke: validating examples…"
"$BIN" validate -f "crew/examples/crew.yaml" >/dev/null
"$BIN" validate -f "$CREWFILE" >/dev/null
"$BIN" validate -f "$PHASEFILE" >/dev/null

# Model override (outage): FORSETI_CREW_MODEL=opencode-go/glm-5.3-flash swaps
# direct-model nodes without touching the halogen-pinned crew files.
echo "crew-smoke: running checked crew headless in sandbox session (a few minutes)…"
echo "crew-smoke: model: ${FORSETI_CREW_MODEL:-halogen (pinned in crew file)}"
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

echo "crew-smoke: running PHASED crew (P11: strict barrier between phases)…"
rm -f "$PROBE"
if ! "$BIN" run --headless -f "$PHASEFILE" --session sandbox > /tmp/crew-smoke-phases.jsonl 2>&1; then
  echo "crew-smoke: FAIL — phased run exited nonzero"
  tail -8 /tmp/crew-smoke-phases.jsonl
  exit 1
fi

# P11 barrier evidence in the run log: research starts, fully settles, and
# only THEN does the build phase start. Line numbers prove the ordering.
s1=$(grep -n '"type":"phase_start","phase":"research"' /tmp/crew-smoke-phases.jsonl | head -1 | cut -d: -f1 || true)
d1=$(grep -n '"type":"phase_done","phase":"research"' /tmp/crew-smoke-phases.jsonl | head -1 | cut -d: -f1 || true)
s2=$(grep -n '"type":"phase_start","phase":"build"' /tmp/crew-smoke-phases.jsonl | head -1 | cut -d: -f1 || true)
if [ -z "$s1" ] || [ -z "$d1" ] || [ -z "$s2" ]; then
  echo "crew-smoke: FAIL — missing phase events (start/done for research, start for build)"
  grep -E '"type":"phase_' /tmp/crew-smoke-phases.jsonl || true
  exit 1
fi
if [ "$d1" -lt "$s2" ]; then
  : # research fully settled before build started — barrier held
else
  echo "crew-smoke: FAIL — build phase started before research settled (barrier broken)"
  exit 1
fi

if [ ! -f "$PROBE" ] || ! grep -q "crew pipeline works" "$PROBE"; then
  echo "crew-smoke: FAIL — phased run probe $PROBE missing or wrong content"
  exit 1
fi
rm -f "$PROBE"

echo "crew: PASS — checked crew in sandbox session, halogen, bus files, watch event, phased barrier all good"
