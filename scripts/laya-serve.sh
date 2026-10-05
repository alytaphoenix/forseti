#!/usr/bin/env bash
# laya-serve: start/stop/status the local Laya decision endpoint (S18).
#   scripts/laya-serve.sh start|stop|status|restart
# Endpoint: POST /v1/systemone {state, questions} → {model, answers, usage, routing}
#           GET  /health
# Binds 127.0.0.1 only (local decisions; no remote surface). pid file keeps
# start idempotent. Config via env: FORSETI_LAYA_PORT (8751),
# FORSETI_LAYA_VENV (~/.config/forseti/laya-venv).
# P13 (review B9) hardening, ported from memory-serve.sh: stop waits for the
# PID to die (not just health to fail); alive() verifies pid identity; start
# fails fast with the log tail when the child dies. (No boot nonce: /health
# is upstream's surface — a dying old server can still answer the poll, so
# stop_owned's pid-death wait is the guard.)
set -eu

PORT="${FORSETI_LAYA_PORT:-8751}"
VENV="${FORSETI_LAYA_VENV:-$HOME/.config/forseti/laya-venv}"
PIDFILE="${TMPDIR:-/tmp}/forseti-laya-serve.pid"
LOG="${TMPDIR:-/tmp}/forseti-laya-serve.log"
URL="http://127.0.0.1:$PORT"
SELF="$(cd "$(dirname "$0")" && pwd)/laya-serve.sh"

pid() { cat "$PIDFILE" 2>/dev/null; }

# alive: pidfile exists, pid lives, AND the pid is actually our server
# (P13: a recycled pid answering kill -0 was an innocent bystander).
alive() {
  [ -f "$PIDFILE" ] || return 1
  local p
  p="$(pid)" || return 1
  [ -n "$p" ] || return 1
  kill -0 "$p" 2>/dev/null || return 1
  ps -p "$p" -o command= 2>/dev/null | grep -q "laya-serve"
}

healthy() {
  curl -s -m 3 "$URL/health" 2>/dev/null | grep -q '"status":"ok"'
}

# adopt: the port's owner is verifiably OUR server (unique pgrep on the exact
# venv path) but the pidfile is gone/stale — adopt it so stop/restart can
# manage it. Refuses anything ambiguous (P13: lost pidfiles were the old
# script's bug class; a wrong adoption kills an innocent process).
adopt() {
  local pids p
  pids="$(pgrep -f "$VENV/bin/laya-serve$" 2>/dev/null)" || true
  p="$(printf '%s\n' "$pids" | head -1)"
  if [ -n "$p" ] && [ "$(printf '%s\n' "$pids" | grep -c .)" -eq 1 ] \
     && ps -p "$p" -o command= 2>/dev/null | grep -q "laya-serve"; then
    echo "$p" > "$PIDFILE"
    echo "{\"laya\":\"adopted\",\"pid\":\"$p\"}"
    return 0
  fi
  return 1
}

# stop_owned kills the pidfile'd process and WAITS FOR THE PID TO DIE.
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

case "${1:-status}" in
  start)
    if healthy && ! alive; then
      # healthy but unowned: adopt only if the owner is verifiably our server
      # (unique pgrep on the exact venv path); else refuse — a foreign process
      # owns the port and the pidfile must not lie about it.
      adopt || { echo "{\"laya\":\"port_busy_foreign\",\"url\":\"$URL\"}" >&2; exit 3; }
    fi
    if healthy; then
      echo "{\"laya\":\"already_up\",\"url\":\"$URL\",\"pid\":\"$(pid)\"}"
      exit 0
    fi
    stop_owned
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
      if ! kill -0 "$(pid)" 2>/dev/null; then
        echo "laya-serve: child died during startup — log tail ($LOG):" >&2
        tail -20 "$LOG" >&2 || true
        rm -f "$PIDFILE"
        exit 1
      fi
      if healthy; then
        echo "{\"laya\":\"started\",\"url\":\"$URL\",\"pid\":\"$(pid)\"}"
        exit 0
      fi
      i=$((i+1)); sleep 1
    done
    echo "laya-serve: not healthy after 40 s — log: $LOG" >&2
    exit 1
    ;;
  stop)
    if alive; then
      stop_owned
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
      echo "{\"laya\":\"down\",\"url\":\"'"$URL"'\"}"
    fi
    ;;
  restart)
    stop_owned
    exec "$SELF" start
    ;;
  *)
    echo "usage: laya-serve.sh start|stop|status|restart" >&2
    exit 2
    ;;
esac
