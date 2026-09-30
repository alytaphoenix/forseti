#!/bin/sh
# Forseti: notes workspace variant (Phase 4-5).
# Targets the evergreen vault at ~/forseti (or $FORSETI_VAULT); reuses the
# same idempotent bring-up machinery in open.sh.
set -eu
export FORSETI_VAULT="${FORSETI_VAULT:-$HOME/forseti}"
export FORSETI_EDITOR_LABEL="${FORSETI_EDITOR_LABEL:-forseti-notes}"
exec sh "$(dirname "$0")/open.sh"
