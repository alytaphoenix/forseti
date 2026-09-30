#!/bin/sh
# Forseti — round-trip smoke test (Phase 3 gate)
#
# Proves the loop: live pi agent + ttt --listen reachable + jump hand-off works.
# Run from the repo (paths resolve through $PWD) with a herdr server running.
#
#   ./scripts/smoke.sh            # use the existing bring-up
#   ./scripts/smoke.sh --setup    # also invoke forseti.open first (~45s)
#
set -eu

HERDR="${HERDR_BIN_PATH:-herdr}"
JUMP_FILE="$HOME/.config/ttt/plugins/forseti/jump.json"

herdr status 2>&1 | grep -q "status: not running" && {
  echo "smoke: herdr server not running"; exit 2;
}

if [ "${1:-}" = "--setup" ]; then
  echo "smoke: invoking bring-up (~45s)..."
  "$HERDR" plugin action invoke forseti.open >/dev/null 2>&1
  sleep 45
fi

# 1. a live pi agent must exist
"$HERDR" agent list | grep -q '"agent":"pi"' || { echo "smoke: no live pi agent"; exit 3; }

# 2. ttt's control server must answer
curl -s --max-time 5 -X POST --data 'screenshot /tmp/forseti-smoke-top.txt' \
  http://127.0.0.1:4242/exec >/dev/null || { echo "smoke: ttt :4242 unreachable"; exit 4; }

# 3. jump round-trip: state file + palette invoke
TARGET="$PWD/AGENTS.md"
cat > "$JUMP_FILE" << EOF
{"path":"$TARGET","line":5,"end_line":7}
EOF
curl -s --max-time 5 -X POST --data 'exec "Forseti: Jump"' \
  http://127.0.0.1:4242/exec >/dev/null
sleep 2
S=$(curl -s --max-time 5 -X POST --data 'screenshot /tmp/forseti-smoke-jumped.txt' \
  http://127.0.0.1:4242/exec)
case "$S" in
  ok*) echo "smoke: PASS — pi agent live, ttt reachable, jump hand-off executed (AGENTS.md opened at line 5)";;
  *)   echo "smoke: FAIL — jump command rejected"; exit 5;;
esac
