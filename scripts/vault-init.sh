#!/bin/sh
# forseti — vault-init.sh
# Scaffold the evergreen vault at ~/forseti (format v1, see docs/implementation-plan.md Phase 4).
# Idempotent: safe to re-run; never overwrites existing files.
set -eu

VAULT="${FORSETI_VAULT:-$HOME/forseti}"
TODAY="$(date +%F)"

mkdir -p "$VAULT/notes" "$VAULT/daily" "$VAULT/_templates" "$VAULT/assets"

write_if_absent() { # write_if_absent <path> <content-from-stdin>
  if [ ! -e "$1" ]; then
    cat > "$1"
    printf '  created %s\n' "${1#$VAULT/}"
  else
    printf '  exists  %s\n' "${1#$VAULT/}"
  fi
}

# P13-B17: the date stamp below must not touch pre-existing files (a re-run
# used to overwrite every vault file's created:). Existence must be probed
# BEFORE the heredocs — tracking inside write_if_absent can't work because
# `cat | write_if_absent` runs it in a subshell.
_stamp_ever=""; [ -e "$VAULT/_templates/evergreen.md" ] || _stamp_ever=1
_stamp_index=""; [ -e "$VAULT/index.md" ] || _stamp_index=1

printf 'forseti vault: %s\n' "$VAULT"

cat << 'TPL' | write_if_absent "$VAULT/_templates/evergreen.md"
---
title:
created: PLACEHOLDERDATE
type: evergreen
tags: []
---

# %%TITLE%%

<!-- evergreen note: one idea, statement-shaped title, dense [[links]] -->
TPL

# The daily template keeps the __TODAY__ token (NOT stamped): the ttt Lua
# daily_note() renderer substitutes it when the note is created. The old
# PLACEHOLDERDATE form meant every daily note got the scaffold date — or the
# literal token — because Lua's gsub looked for __TODAY__ (P13-B17).
cat << 'TPL' | write_if_absent "$VAULT/_templates/daily.md"
---
title: __TODAY__
created: __TODAY__
type: daily
---

## Log

TPL

cat << 'EOF' | write_if_absent "$VAULT/index.md"
---
title: Map of Content
created: PLACEHOLDERDATE
type: project
---

# Index

Start points live here. Evergreen notes live flat in [[notes]] — this index is
the only curated hub.

- Start: ( add your first [[evergreen-notes]] )
EOF

cat << 'EOF' | write_if_absent "$VAULT/.gitignore"
.trash/
EOF

# Stamp today's date into FRESHLY CREATED files only (P13-B17), with a
# portable in-place edit (P13-B18: `sed -i ''` is BSD-only).
stamp() { # stamp <path>
  sed "s/PLACEHOLDERDATE/$TODAY/g" "$1" > "$1.tmp" && mv "$1.tmp" "$1"
}
[ -n "$_stamp_ever" ] && stamp "$VAULT/_templates/evergreen.md"
[ -n "$_stamp_index" ] && stamp "$VAULT/index.md"
# Legacy migration (P13-B17): vaults scaffolded before this fix carry a
# PLACEHOLDERDATE the Lua side never substitutes (it looks for __TODAY__).
# Retargeting that token is not a user-content overwrite — nothing the user
# writes contains it.
_daily_tpl="$VAULT/_templates/daily.md"
if [ -f "$_daily_tpl" ] && grep -q PLACEHOLDERDATE "$_daily_tpl"; then
  sed "s/PLACEHOLDERDATE/__TODAY__/g" "$_daily_tpl" > "$_daily_tpl.tmp" && mv "$_daily_tpl.tmp" "$_daily_tpl"
  printf '  migrated daily template → __TODAY__\n'
fi

if [ ! -d "$VAULT/.git" ]; then
  (cd "$VAULT" && git init -q && git add -A && git commit -qm "forseti vault scaffold" ) \
    && printf '  git: initialized\n' || printf '  git: held off\n'
fi

printf 'forseti vault ready: open ~/forseti as a workspace in ttt, an Obsidian vault, or pi cwd\n'
