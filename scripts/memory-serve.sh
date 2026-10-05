#!/usr/bin/env bash
# memory-serve: start/stop/status the forseti-memory service (P10-1).
#   scripts/memory-serve.sh start|stop|status|restart
# Same contract as laya-serve.sh: binds 127.0.0.1 only, pidfile idempotent.
#   POST /write {agent, text, tags?, type?, importance?, supersedes?, ...}
#   POST /recall {query, agent?, k?, since?, type?}
#   GET  /health   POST /export   POST /import
# P13 (review M2/M3) hardening:
#   - stop waits for the PROCESS to die (not just /health to fail — a dying
#     server kept answering past the window and restart spawned a corpse)
#   - alive() verifies pid identity via ps (a recycled pid must not get killed)
#   - start polls with a BOOT NONCE echoed by /health (only OUR process counts
#     as healthy) and fails fast with the log tail when the child dies
set -eu

PORT="${FORSETI_MEMORY_PORT:-8752}"
VENV="${FORSETI_LAYA_VENV:-$HOME/.config/forseti/laya-venv}"
PIDFILE="${TMPDIR:-/tmp}/forseti-memory-serve.pid"
LOG="${TMPDIR:-/tmp}/forseti-memory-serve.log"
URL="http://127.0.0.1:$PORT"
SCRIPT="$(cd "$(dirname "$0")/.." && pwd)/crew/memory/memory_serve.py"

pid() { cat "$PIDFILE" 2>/dev/null; }

# alive: pidfile exists, pid lives, AND the pid is actually our server
# (P13-M3: a recycled pid answering kill -0 was an innocent bystander).
alive() {
  [ -f "$PIDFILE" ] || return 1
  local p
  p="$(pid)" || return 1
  [ -n "$p" ] || return 1
  kill -0 "$p" 2>/dev/null || return 1
  ps -p "$p" -o command= 2>/dev/null | grep -q "memory_serve.py"
}

healthy() { curl -s -m 3 "$URL/health" 2>/dev/null | grep -q '"status":"ok"'; }

# adopt: the port's owner is verifiably OUR server (unique pgrep on the exact
# script path) but the pidfile is gone/stale — adopt it so stop/restart can
# manage it. Refuses anything ambiguous (P13: same trap as laya-serve).
adopt() {
  local pids p
  pids="$(pgrep -f "$SCRIPT" 2>/dev/null)" || true
  p="$(printf '%s\n' "$pids" | head -1)"
  if [ -n "$p" ] && [ "$(printf '%s\n' "$pids" | grep -c .)" -eq 1 ] \
     && ps -p "$p" -o command= 2>/dev/null | grep -q "memory_serve.py"; then
    echo "$p" > "$PIDFILE"
    echo "{\"memory\":\"adopted\",\"pid\":\"$p\"}"
    return 0
  fi
  return 1
}

# ours_healthy: /health answers with OUR boot nonce — a foreign or dying
# process on the port does not count (P13-M2b).
ours_healthy() {
  [ -n "$BOOT_ID" ] || { healthy; return; }
  curl -s -m 3 "$URL/health" 2>/dev/null | grep -q "\"boot_id\":\"$BOOT_ID\""
}

# stop_owned kills the pidfile'd process and WAITS FOR THE PID TO DIE
# (P13-M2a: waiting for /health to fail let a dying server keep serving past
# the window — the restart race that spawned dead services hit live twice).
stop_owned() {
  if alive; then
    local p
    p="$(pid)"
    kill "$p" 2>/dev/null || true
    local i=0
    while kill -0 "$p" 2>/dev/null && [ $i -lt 15 ]; do
      sleep 1
      i=$((i+1))
    done
  fi
  rm -f "$PIDFILE"
}

BOOT_ID="boot-$$-$(date +%s)"
export BOOT_ID

case "${1:-status}" in
  start)
    if healthy && ! alive; then
      # healthy but unowned: adopt only if the owner is verifiably our server;
      # else refuse (a foreign process owns the port; the pidfile would lie).
      adopt || { echo "{\"memory\":\"port_busy_foreign\",\"url\":\"$URL\"}" >&2; exit 3; }
    fi
    if healthy; then
      echo "{\"memory\":\"already_up\",\"url\":\"$URL\"}"
      exit 0
    fi
    stop_owned
    [ -x "$VENV/bin/python" ] || { echo '{"memory":"missing_runtime — run scripts/laya-setup.sh"}'; exit 2; }
    [ -f "$SCRIPT" ] || { echo "{\"memory\":\"missing script: $SCRIPT\"}"; exit 2; }
    FORSETI_MEMORY_PORT="$PORT" FORSETI_MEMORY_BOOT_ID="$BOOT_ID" \
      nohup "$VENV/bin/python" "$SCRIPT" > "$LOG" 2>&1 &
    echo $! > "$PIDFILE"
    # P13-M2c: poll OUR nonce; fail fast with the log tail if the child dies
    i=0
    while [ $i -lt 60 ]; do
      if ! kill -0 "$(pid)" 2>/dev/null; then
        echo "memory-serve: child died during startup — log tail ($LOG):" >&2
        tail -20 "$LOG" >&2 || true
        rm -f "$PIDFILE"
        exit 1
      fi
      if ours_healthy; then
        echo "{\"memory\":\"started\",\"url\":\"$URL\",\"pid\":\"$(pid)\"}"
        exit 0
      fi
      i=$((i+1)); sleep 1
    done
    echo "memory-serve: not healthy after 60 s — log: $LOG" >&2
    exit 1
    ;;
  stop)
    if alive; then
      stop_owned
      echo '{"memory":"stopped"}'
    else
      rm -f "$PIDFILE"
      echo '{"memory":"not_running"}'
    fi
    ;;
  status)
    if healthy; then
      curl -s -m 3 "$URL/health"
    elif alive; then
      echo "{\"memory\":\"starting\",\"url\":\"$URL\"}"
    else
      echo "{\"memory\":\"down\",\"url\":\"'"$URL"'\"}"
    fi
    ;;
  restart)
    stop_owned
    exec "$(cd "$(dirname "$0")" && pwd)/memory-serve.sh" start
    ;;
  *)
    echo "usage: memory-serve.sh start|stop|status|restart" >&2
    exit 2
    ;;
esac
