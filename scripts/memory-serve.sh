#!/bin/sh
# memory-serve: start/stop/status the forseti-memory service (P10-1).
#   scripts/memory-serve.sh start|stop|status|restart
# Same contract as laya-serve.sh: binds 127.0.0.1 only, pidfile idempotent.
#   POST /write {agent, text, tags?, source?, run?}
#   POST /recall {query, agent?, k?, since?}
#   GET  /health
set -eu

PORT="${FORSETI_MEMORY_PORT:-8752}"
VENV="${FORSETI_LAYA_VENV:-$HOME/.config/forseti/laya-venv}"
PIDFILE="${TMPDIR:-/tmp}/forseti-memory-serve.pid"
LOG="${TMPDIR:-/tmp}/forseti-memory-serve.log"
URL="http://127.0.0.1:$PORT"
SCRIPT="$(cd "$(dirname "$0")/.." && pwd)/crew/memory/memory_serve.py"

alive() { [ -f "$PIDFILE" ] && kill -0 "$(cat "$PIDFILE")" 2>/dev/null; }
healthy() { curl -s -m 3 "$URL/health" 2>/dev/null | grep -q '"status":"ok"'; }

# stop_owned kills the pidfile'd process and WAITS for the port to close —
# a start immediately after must not meet a dying process still bound to the
# port (hit live 2026-10-01: restart raced the old process, start then said
# "already_up" against the zombie).
stop_owned() {
  if alive; then
    kill "$(cat "$PIDFILE")" 2>/dev/null || true
  fi
  rm -f "$PIDFILE"
  i=0
  while healthy && [ $i -lt 5 ]; do
    sleep 1
    i=$((i+1))
  done
}

case "${1:-status}" in
  start)
    if healthy && ! alive; then
      # healthy but unowned: adopt nothing — refuse (a foreign process owns
      # the port; the pidfile would lie). Documented, not auto-killed.
      echo "{\"memory\":\"port_busy_foreign\",\"url\":\"$URL\"}" >&2
      exit 3
    fi
    if healthy; then
      echo "{\"memory\":\"already_up\",\"url\":\"$URL\"}"
      exit 0
    fi
    stop_owned
    [ -x "$VENV/bin/python" ] || { echo '{"memory":"missing_runtime — run scripts/laya-setup.sh"}'; exit 2; }
    [ -f "$SCRIPT" ] || { echo "{\"memory\":\"missing script: $SCRIPT\"}"; exit 2; }
    FORSETI_MEMORY_PORT="$PORT" nohup "$VENV/bin/python" "$SCRIPT" > "$LOG" 2>&1 &
    echo $! > "$PIDFILE"
    i=0
    while [ $i -lt 60 ]; do
      if healthy; then
        echo "{\"memory\":\"started\",\"url\":\"$URL\",\"pid\":\"$(cat "$PIDFILE")\"}"
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
      echo '{"memory":"down","url":"'"$URL"'"}'
    fi
    ;;
  restart)
    stop_owned
    exec "$0" start
    ;;
  *)
    echo "usage: memory-serve.sh start|stop|status|restart" >&2
    exit 2
    ;;
esac
