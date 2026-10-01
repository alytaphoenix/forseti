#!/bin/sh
# pi-ask: prompt the live forseti pi agent (P7-5 helper).
# Called by lazygit customCommands (P7-4) and reusable from anywhere:
#   pi-ask.sh "Review the selected file"          # plain prompt
#   pi-ask.sh --file "{{.SelectedFile.Name}}" ...  # file context prelude
# Resolves the live pi agent the same way open.sh does (first pi agent,
# FORSETI_AGENT_NAME override). No-op with a clear message when none is live.
set -eu

HERDR="${HERDR_BIN_PATH:-herdr}"
AGENT_NAME="${FORSETI_AGENT_NAME:-coder}"

text=""
if [ "${1:-}" = "--file" ]; then
  file="${2:-}"
  shift 2 || true
  text="[lazygit] File context: $file${1:+ — }$*"
else
  text="$*"
fi
[ -n "$text" ] || { printf 'pi-ask: no text\n' >&2; exit 2; }

# resolve a live agent: prefer the named one, else the first pi agent
if ! "$HERDR" agent list 2>/dev/null | grep -q "\"name\"[[:space:]]*:[[:space:]]*\"$AGENT_NAME\""; then
  resolved=$("$HERDR" agent list 2>/dev/null | python3 -c "
import json, sys
try:
    d = json.load(sys.stdin)
except Exception:
    sys.exit(0)
for a in (d.get('result', {}) or {}).get('agents', []):
    if a.get('agent') == 'pi':
        print(a.get('name', ''))
        break
" 2>/dev/null || true)
  [ -n "$resolved" ] && AGENT_NAME="$resolved"
fi

"$HERDR" agent prompt "$AGENT_NAME" "$text"
printf '{"pi_ask":"sent","agent":"%s"}\n' "$AGENT_NAME"
