#!/bin/sh
# Send a follow-up prompt into the pi agent pane after ask submission.
# Usage: tell-pi.sh <agent-name> [text]
set -eu
AGENT="${1:-}"
TXT="${2:-}"
[ -n "$AGENT" ] && [ -n "$TXT" ] || { echo "usage: tell-pi.sh <agent> <text>" >&2; exit 2; }
herdr agent prompt "$AGENT" "$TXT" >/dev/null
echo ok
