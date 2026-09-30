#!/bin/sh
# Forseti bring-up: one action -> dedicated tab with ttt (--listen) + pi agent.
# Runs HEADLESS (no PTY). Only ever spawns `herdr` CLI subcommands; never a TUI.
# Contract (docs/spikes.md S1): context in HERDR_PLUGIN_CONTEXT_JSON,
# herdr binary in HERDR_BIN_PATH, CWD is the plugin root (do not pass --cwd to
# this script's invocation; cd explicitly where needed).
set -eu

HERDR="${HERDR_BIN_PATH:-herdr}"
AGENT_NAME="${FORSETI_AGENT_NAME:-coder}"
LISTEN_PORT="${FORSETI_TTT_PORT:-4242}"
EDITOR_LABEL="${FORSETI_EDITOR_LABEL:-forseti}"

# --- helpers -------------------------------------------------------------

json_field() {
  # grep/sed JSON extraction, mirroring ttt.editor's parser (no jq dependency).
  printf '%s' "$1" \
    | grep -o "\"$2\"[[:space:]]*:[[:space:]]*\"[^\"]*\"" \
    | head -1 \
    | sed "s/.*\"$2\"[[:space:]]*:[[:space:]]*\"//" \
    | sed 's/"$//'
}

# herdr prints JSON on stdout, errors as JSON on stderr (exit 1).
# Capture both; on failure emit the server error verbatim and die.
herdr_json() {
  _out=$("$_HERDR" "$@" 2>"$_FORSETI_ERR") || {
    printf 'forseti: herdr %s failed:\n%s\n' "$1" "$(cat "$_FORSETI_ERR")" >&2
    exit 1
  }
  printf '%s' "$_out"
}
_HERDR="$HERDR"
_FORSETI_ERR="${TMPDIR:-/tmp}/forseti-herdr-err.$$"
trap 'rm -f "$_FORSETI_ERR"' EXIT

# --- 1. resolve target directory (ttt.editor order) ----------------------

dir=""
if [ -n "${HERDR_PLUGIN_CONTEXT_JSON:-}" ]; then
  ctx="$HERDR_PLUGIN_CONTEXT_JSON"
  dir=$(json_field "$ctx" checkout_path)
  [ -z "$dir" ] && dir=$(json_field "$ctx" focused_pane_cwd)
  [ -z "$dir" ] && dir=$(json_field "$ctx" workspace_cwd)
fi
dir="${TTT_TARGET_DIR:-${dir:-.}}"

# --- 2. idempotency: reuse a live pi agent instead of double-spawning ----

agents=$(herdr_json agent list --json)
live=$(printf '%s' "$agents" \
  | grep -o "\"name\"[[:space:]]*:[[:space:]]*\"$AGENT_NAME\"" | head -1 || true)
if [ -n "$live" ]; then
  herdr_json agent focus "$AGENT_NAME" >/dev/null 2>&1 || true
  printf '{"forseti":"reused","agent":"%s"}\n' "$AGENT_NAME"
  exit 0
fi

# --- 3. port probe: is another ttt --listen already bound? ---------------

listen_args="--listen"
if command -v nc >/dev/null 2>&1 \
  && nc -z -w 1 127.0.0.1 "$LISTEN_PORT" >/dev/null 2>&1; then
  listen_args=""
  printf 'forseti: port %s busy — launching ttt WITHOUT --listen; jump/follow pushes will no-op until that instance exits\n' "$LISTEN_PORT" >&2
fi

# --- 4. dedicated tab: ttt in the root pane ------------------------------

created=$(herdr_json tab create --cwd "$dir" --label "$EDITOR_LABEL" --no-focus)
tab_id=$(json_field "$created" tab_id)
root_pane=$(json_field "$created" pane_id)
if [ -z "$root_pane" ]; then
  # Fallback: list panes in the new tab and take the last pane_id on the line
  # (tab create's root pane is the only pane at this point).
  root_pane=$(herdr_json pane list --workspace "$(json_field "$created" workspace_id)" 2>/dev/null \
    | sed -n 's/.*"pane_id"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | tail -1)
fi
[ -n "$tab_id" ] && [ -n "$root_pane" ] || {
  printf 'forseti: could not parse tab create response: %s\n' "$created" >&2
  exit 1
}

# `pane run` types "ttt --listen" + Enter into the fresh shell pane.
herdr_json pane run "$root_pane" "ttt $listen_args" >/dev/null

# --- 5. pi pane: split right, then let herdr launch+ready pi itself ------

split=$(herdr_json pane split --pane "$root_pane" --direction right --no-focus)
pi_pane=$(json_field "$split" pane_id)
if [ -n "$pi_pane" ] && [ "$pi_pane" = "$root_pane" ]; then
  # grep-style fallback can latch onto the wrong pane; disambiguate by taking
  # the LAST pane_id seen, or bail loudly with the raw response.
  pi_pane=$(printf '%s' "$split" | sed -n 's/.*"pane_id"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | tail -1)
fi
[ -n "$pi_pane" ] || {
  printf 'forseti: could not parse pane split response: %s\n' "$split" >&2
  exit 1
}

# agent start returns only after herdr detects pi ready in that pane
# (~30 s default). Pi args may follow -- (S3: passed verbatim).
herdr_json agent start "$AGENT_NAME" --kind pi --pane "$pi_pane" >/dev/null

# --- 6. summarize --------------------------------------------------------

herdr_json tab focus "$tab_id" >/dev/null || true
printf '{"forseti":"created","tab":"%s","editor_pane":"%s","agent_pane":"%s","agent":"%s"}\n' \
  "$tab_id" "$root_pane" "$pi_pane" "$AGENT_NAME"
