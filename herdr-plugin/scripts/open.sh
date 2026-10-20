#!/bin/sh
# Forseti bring-up: the herdr plugin action (Open Forseti) delegates to
# `forseti start`, so the plugin and the CLI share ONE reconcile path — the IDE
# tab is found by its recorded identity, a tab that herdr restored as bare
# shells is revived in place, and dead duplicates are pruned.
#
# Runs HEADLESS (no PTY). Context arrives in HERDR_PLUGIN_CONTEXT_JSON; CWD is
# the plugin root, so the target directory is resolved here and passed --dir.
# Falls back to the legacy sh bring-up when the forseti binary is absent.
set -eu

here=$(cd "$(dirname "$0")" && pwd)
BIN="${FORSETI_BIN:-forseti}"

json_field() {
  # grep/sed JSON extraction, mirroring ttt.editor's parser (no jq dependency).
  printf '%s' "$1" \
    | grep -o "\"$2\"[[:space:]]*:[[:space:]]*\"[^\"]*\"" \
    | head -1 \
    | sed "s/.*\"$2\"[[:space:]]*:[[:space:]]*\"//" \
    | sed 's/"$//'
}

# ttt.editor resolution order: checkout_path -> focused_pane_cwd -> workspace_cwd.
dir=""
if [ -n "${HERDR_PLUGIN_CONTEXT_JSON:-}" ]; then
  ctx="$HERDR_PLUGIN_CONTEXT_JSON"
  dir=$(json_field "$ctx" checkout_path)
  [ -z "$dir" ] && dir=$(json_field "$ctx" focused_pane_cwd)
  [ -z "$dir" ] && dir=$(json_field "$ctx" workspace_cwd)
fi
dir="${TTT_TARGET_DIR:-${dir:-$PWD}}"
[ -d "$dir" ] || dir=$PWD
# Never hand a relative path to ttt (a literal "." opened the wrong root).
dir=$(cd "$dir" && pwd)

if command -v "$BIN" >/dev/null 2>&1; then
  if [ -n "${FORSETI_EDITOR_LABEL:-}" ]; then
    exec "$BIN" start --dir "$dir" --label "$FORSETI_EDITOR_LABEL" --no-sync --no-services --json
  fi
  exec "$BIN" start --dir "$dir" --no-sync --no-services --json
fi

printf 'forseti: %s not on PATH — using the legacy sh bring-up (install it with scripts/install-cli.sh)\n' "$BIN" >&2
exec sh "$here/open-legacy.sh"
