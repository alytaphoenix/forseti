#!/bin/sh
# S3 spike: verify `herdr agent start --kind pi -- <args>` passes args to pi.
#
# Three runs, each on a FRESH scratch tab (an agent name is bound to its pane;
# after `agent start` the pane is no longer an available shell pane):
#   1. control  : no args         -> pi banner / prompt visible
#   2. positive : -- --version    -> pi prints its version, no prompt
#   3. negative : -- --bogus-flag -> pi exits with "unknown option" error
#
# Needs a RUNNING herdr server (`herdr status`). Safe: only touches tabs it
# created, cleans up after itself.
set -eu

NAME_BASE="forseti-s3"
HERDR="${HERDR_BIN_PATH:-herdr}"

die() { printf 'S3-FATAL: %s\n' "$1" >&2; exit 1; }
command -v "$HERDR" >/dev/null || die "herdr not on PATH"
command -v pi >/dev/null || die "pi (pi-coding-agent) not on PATH"

herdr status 2>&1 | grep -q "status: not running" \
  && die "herdr server not running — start it with \`herdr\` first"

jget() { # jget <json> <key> — first match on flat unique leaf keys
  printf '%s' "$1" | grep -o "\"$2\"[[:space:]]*:[[:space:]]*\"[^\"]*\"" | head -1 \
    | sed "s/.*\"$2\"[[:space:]]*:[[:space:]]*\"//" | sed 's/"$//'
}
hr() { # herdr <args...> -> stdout json, die on stderr-failure
  _o=$("$HERDR" "$@" 2>"$ERRF") || die "herdr $* failed: $(cat "$ERRF")"
  printf '%s' "$_o"
}

new_tab_and_start() { # $1=name suffix, $2...=agent args after --
  sf="$1"; shift
  # CLI runs outside herdr have no implicit workspace -> create a scratch one.
  ws_created=$(hr workspace create --cwd "$PWD" --label "$NAME_BASE-$sf" --no-focus)
  WS=$(jget "$ws_created" workspace_id)
  [ -n "$WS" ] || die "unparsed workspace create: $ws_created"
  created=$(hr tab create --workspace "$WS" --label "$NAME_BASE-$sf" --no-focus)
  TAB=$(jget "$created" tab_id)
  PANE=$(jget "$created" pane_id)
  [ -n "$TAB" ] && [ -n "$PANE" ] || die "unparsed tab create: $created"
  # shellcheck disable=SC2086
  if ! "$HERDR" agent start "s3-$sf" --kind pi --pane "$PANE" -- "$@" 2>"$ERRF"; then
    printf 'run %s: agent start FAILED: %s\n' "$sf" "$(cat "$ERRF")"
    return 1
  fi
}

observe() { # $1=label
  printf '\n===== run %s: first 25 lines of pi pane =====\n' "$1"
  "$HERDR" agent read "s3-$1" --source recent-unwrapped --lines 25 2>&1 || \
    "$HERDR" pane read "$PANE" --source recent-unwrapped --lines 25 2>&1 || true
}

cleanup() { # workspace close removes its tab, panes, and the agent runnning in them
  "$HERDR" workspace close "$WS" >/dev/null 2>&1 || true
}

ERRF=$(mktemp)
summary=""

# --- run 1: control (no args) --------------------------------------------
if new_tab_and_start control; then
  sleep 3
  observe control
  summary="$summary control:OK"
  cleanup
else
  summary="$summary control:FAILED"
  cleanup
fi

# --- run 2: positive (-- --version) --------------------------------------
if new_tab_and_start version --version; then
  sleep 3
  observe version
  summary="$summary version:OK"
  cleanup
else
  summary="$summary version:FAILED"
  cleanup
fi

# --- run 3: negative (-- --bogus-flag) -----------------------------------
if new_tab_and_start bogus --bogus-flag; then
  sleep 3
  observe bogus
  summary="$summary bogus:UNEXPECTED-START-OK"
  cleanup
else
  printf 'run bogus: agent start rejected args as expected (agent_not_ready or similar)\n'
  summary="$summary bogus:rejected"
  cleanup
fi

printf '\n===== S3 SUMMARY: %s =====\n' "$summary"
printf 'Interpretation: version run shows a version string => args reach pi.\n'
printf 'bogus run failing to start => herdr/_SURFACE passes args verbatim.\n'
