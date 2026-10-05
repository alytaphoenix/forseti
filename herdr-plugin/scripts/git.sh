#!/bin/sh
# Forseti git surface: open/focus lazygit on the current checkout.
# Runs HEADLESS (no PTY) like open.sh — lazygit is launched INTO a herdr pane
# by that pane's own shell (`pane run` types the command); this action only
# resolves layout, detects an existing lazygit process, and focuses.
# Contract (docs/spikes.md S1): context in HERDR_PLUGIN_CONTEXT_JSON,
# herdr binary in HERDR_BIN_PATH, CWD is the plugin root.
# S16 facts: `lazygit -p <repo>` renders in a pane; `q` quits cleanly; config
# lives at $XDG_CONFIG_HOME/lazygit/config.yml (set) or the macOS
# Application Support path (not set).
set -eu

HERDR="${HERDR_BIN_PATH:-herdr}"
EDITOR_LABEL="${FORSETI_EDITOR_LABEL:-forseti}"

# --- helpers (mirrors open.sh; no jq dependency) ---------------------------

# P13-B15: quote paths for the command TYPED into a pane (pane run is not
# exec — the pane's shell re-parses it; a repo path with spaces desugared)
squote() { printf "'%s'" "$(printf '%s' "$1" | sed "s/'/'\\\\''/g")"; }
# P13-B15: minimal JSON string escaping for emitted summaries
json_escape() { printf '%s' "$1" | sed 's/\\/\\\\/g; s/"/\\"/g'; }

json_field() {
  printf '%s' "$1" \
    | grep -o "\"$2\"[[:space:]]*:[[:space:]]*\"[^\"]*\"" \
    | head -1 \
    | sed "s/.*\"$2\"[[:space:]]*:[[:space:]]*\"//" \
    | sed 's/"$//'
}

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

# --- 1. resolve target directory (same order as open.sh) -------------------

dir=""
if [ -n "${HERDR_PLUGIN_CONTEXT_JSON:-}" ]; then
  ctx="$HERDR_PLUGIN_CONTEXT_JSON"
  dir=$(json_field "$ctx" checkout_path)
  [ -z "$dir" ] && dir=$(json_field "$ctx" focused_pane_cwd)
  [ -z "$dir" ] && dir=$(json_field "$ctx" workspace_cwd)
fi
dir="${TTT_TARGET_DIR:-${dir:-$(pwd)}}"
case "$dir" in /*) ;; *) dir="$(cd "$dir" && pwd)" ;; esac

# --- 2. find the forseti tab ----------------------------------------------

tabs=$(herdr_json tab list)
tab_id=""
_tab_line=$(printf '%s' "$tabs" | grep -o "{[^{}]*\"label\"[[:space:]]*:[[:space:]]*\"$EDITOR_LABEL\"[^{}]*}" | head -1 || true)
[ -n "$_tab_line" ] && tab_id=$(json_field "$_tab_line" tab_id)
if [ -z "$tab_id" ]; then
  printf '{"forseti":"no forseti tab (run forseti:open first)"}\n'
  exit 1
fi

# --- 3. lazygit customCommands (P7-4, S17) -----------------------------------
# Appends a marked forseti block to lazygit's config: Ctrl+G opens the selected
# file in the LIVE ttt (jump handoff via git-jump.sh), Ctrl+Y prompts the live
# pi agent about it (pi-ask.sh). Keys avoid inbuilt collisions (Ctrl+O =
# universal copy-to-clipboard in 0.65.x).
install_lazygit_config() {
  _lg_cfg=""
  if [ -n "${XDG_CONFIG_HOME:-}" ]; then
    _lg_cfg="$XDG_CONFIG_HOME/lazygit/config.yml"
  elif [ "$(uname)" = "Darwin" ]; then
    _lg_cfg="$HOME/Library/Application Support/lazygit/config.yml"
  else
    _lg_cfg="$HOME/.config/lazygit/config.yml"
  fi
  [ -f "$_lg_cfg" ] || mkdir -p "$(dirname "$_lg_cfg")" 2>/dev/null || true
  [ -f "$_lg_cfg" ] || : > "$_lg_cfg"
  grep -q "# >>> forseti >>>" "$_lg_cfg" 2>/dev/null && return 0

  _scripts_abs="$(cd "$(dirname "$0")" && pwd)"
  cat >> "$_lg_cfg" << EOF

# >>> forseti >>> managed block (installed by forseti git.sh; keys: Ctrl+G / Ctrl+Y)
customCommands:
  - key: '<c-g>'
    context: 'files'
    description: 'Open in forseti (ttt)'
    loadingText: 'Opening in ttt…'
    command: '$_scripts_abs/git-jump.sh {{.SelectedFile.Name | quote}}'
  - key: '<c-y>'
    context: 'files'
    description: 'Ask pi about this file'
    loadingText: 'Asking pi…'
    command: '$_scripts_abs/pi-ask.sh --file {{.SelectedFile.Name | quote}} "review this file in the current working tree"'
# <<< forseti <<<
EOF
}

# --- 4. find or create the lazygit pane ------------------------------------
# S16/S17: idempotency keys off pane.process_info — a pane whose FOREGROUND
# process is lazygit is the git pane (relaunch-safe: a quit lazygit leaves the
# shell, not a process). A bare-shell pane is a host candidate; else we split
# a new pane off the first pane of the tab.

panes=$(herdr_json pane list)
pane_ids=$(printf '%s' "$panes" | python3 -c "
import json, sys
try:
    d = json.load(sys.stdin)
except Exception:
    sys.exit(0)
for p in (d.get('result', {}) or {}).get('panes', []):
    if p.get('tab_id') == sys.argv[1]:
        print(p.get('pane_id', ''))
" "$tab_id" 2>/dev/null || true)
[ -n "$pane_ids" ] || { printf '{"forseti":"no panes in tab %s"}\n' "$tab_id"; exit 1; }

# P7-4: make sure the lazygit→forseti custom commands exist (idempotent,
# marker-guarded) — do this on every invoke so an edited config self-heals.
install_lazygit_config

git_pane=""
host_pane=""
first_pane=""
i=0
for pid in $pane_ids; do
  i=$((i+1))
  [ $i -eq 1 ] && first_pane="$pid"
  fg_names=$(herdr_json pane process-info --pane "$pid" 2>/dev/null \
    | grep -o '"name"[[:space:]]*:[[:space:]]*"[^"]*"' \
    | sed 's/.*: *"//; s/"//' || true)
  case "$fg_names" in
    *lazygit*) git_pane="$pid" ;;
    *zsh*|*sh*|*bash*) [ -z "$host_pane" ] && host_pane="$pid" ;;
  esac
done

if [ -n "$git_pane" ]; then
  herdr_json tab focus "$tab_id" >/dev/null
  printf '{"forseti":"focused","git_pane":"%s","tab":"%s"}\n' "$git_pane" "$tab_id"
  exit 0
fi

# never type into a running TUI (ttt/pi occupy their panes): host on a shell
# pane when one exists, otherwise split a fresh pane off the first pane.
if [ -n "$host_pane" ]; then
  target="$host_pane"
else
  split=$(herdr_json pane split --pane "$first_pane" --direction right --no-focus)
  target=$(json_field "$split" pane_id)
  [ -n "$target" ] || { printf '{"forseti":"pane split failed: %s"}\n' "$split"; exit 1; }
fi

herdr_json pane run "$target" "lazygit -p $(squote "$dir")" >/dev/null
herdr_json tab focus "$tab_id" >/dev/null
printf '{"forseti":"created","git_pane":"%s","tab":"%s","cwd":"%s"}\n' "$target" "$tab_id" "$(json_escape "$dir")"
