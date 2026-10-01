#!/bin/sh
# git-jump: lazygit (P7-4) → live ttt. Resolves the selected file against the
# repo root, writes the jump handoff, and re-dispatches into the running ttt.
#   git-jump.sh "{{.SelectedFile.Name}}"
set -eu

file="${1:-}"
[ -n "$file" ] || { printf 'git-jump: no file\n' >&2; exit 2; }

root="$(git rev-parse --show-toplevel 2>/dev/null || pwd)"
case "$file" in
  /*) abs="$file" ;;
  *)  abs="$root/$file" ;;
esac

state_dir="${FORSETI_TTT_STATE:-$HOME/.config/ttt/plugins/forseti}"
mkdir -p "$state_dir" 2>/dev/null || true
printf '{"path":"%s","line":1}\n' "$abs" > "$state_dir/jump.json"

curl -s -m 5 -X POST --data 'exec "Forseti: Jump"' http://127.0.0.1:4242/exec >/dev/null 2>&1 || true
