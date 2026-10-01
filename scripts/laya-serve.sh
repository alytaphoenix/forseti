#!/bin/sh
# laya-serve: start/stop/status the local Laya decision endpoint (S18).
#   scripts/laya-serve.sh start|stop|status|restart
# Endpoint: POST /v1/systemone {state, questions} → {model, answers, usage, routing}
#           GET  /health
# Binds 127.0.0.1 only (local decisions; no remote surface). pid file keeps
# start idempotent. Config via env: FORSETI_LAYA_PORT (8751),
# FORSETI_LAYA_VENV (~/.config/forseti/laya-venv).
set -eu

PORT="${FORSETI_LAYA_PORT:-8751}"
VENV="${FORSETI_LAYA_VENV:-$HOME/.config/forseti/laya-venv}"
PIDFILE="${TMPDIR:-/tmp}/forseti-laya-serve.pid"
LOG="${TMPDIR:-/tmp}/forseti-laya-serve.log"
URL="http://127.0.0.1:$PORT"

alive() {
  [ -f "$PIDFILE" ] && kill -0 "$(cat "$PIDFILE")" 2>/dev/null
}

healthy() {
  curl -s -m 3 "$URL/health" 2>/dev/null | grep -q '"status":"ok"'
}

case "${1:-status}" in
  start)
    if healthy; then
      echo "{\"laya\":\"already_up\",\"url\":\"$URL\",\"pid\":\"$(cat "$PIDFILE" 2>/dev/null || true)\"}"
      exit 0
    fi
    if alive; then
      # pid alive but unhealthy: stop it first
      kill "$(cat "$PIDFILE")" 2>/dev/null || true
      sleep 1
    fi
    [ -x "$VENV/bin/laya-serve" ] || {
      echo '{"laya":"missing_runtime"} — run scripts/laya-setup.sh first' >&2
      exit 2
    }
    LAYA_HOST=127.0.0.1 LAYA_PORT="$PORT" LAYA_MODELS=english LAYA_LOG_LEVEL=warning \
      nohup "$VENV/bin/laya-serve" > "$LOG" 2>&1 &
    echo $! > "$PIDFILE"
    echo "laya-serve: waiting for /health…"
    i=0
    while [ $i -lt 40 ]; do
      if healthy; then
        echo "{\"laya\":\"started\",\"url\":\"$URL\",\"pid\":\"$(cat "$PIDFILE")\"}"
        exit 0
      fi
      i=$((i+1)); sleep 1
    done
    echo "laya-serve: not healthy after 40 s — log: $LOG" >&2
    exit 1
    ;;
  stop)
    if alive; then
      kill "$(cat "$PIDFILE")" 2>/dev/null || true
      rm -f "$PIDFILE"
      echo '{"laya":"stopped"}'
    else
      rm -f "$PIDFILE"
      echo '{"laya":"not_running"}'
    fi
    ;;
  status)
    if healthy; then
      curl -s -m 3 "$URL/health"
    elif alive; then
      echo "{\"laya\":\"starting\",\"url\":\"$URL\"}"
    else
      echo '{"laya":"down","url":"'"$URL"'"}'
    fi
    ;;
  restart)
    "$0" stop >/dev/null 2>&1 || true
    exec "$0" start
    ;;
  *)
    echo "usage: laya-serve.sh start|stop|status|restart" >&2
    exit 2
    ;;
esac
