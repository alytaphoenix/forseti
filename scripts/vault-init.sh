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

cat << 'TPL' | write_if_absent "$VAULT/_templates/daily.md"
---
title: PLACEHOLDERDATE
created: PLACEHOLDERDATE
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

# stamp today's date into fresh templates
for f in "$VAULT/_templates/evergreen.md" "$VAULT/_templates/daily.md"; do
  [ -f "$f" ] && sed -i '' "s/PLACEHOLDERDATE/$TODAY/g" "$f"
done
[ -f "$VAULT/index.md" ] && sed -i '' "s/PLACEHOLDERDATE/$TODAY/" "$VAULT/index.md" 2>/dev/null || true

if [ ! -d "$VAULT/.git" ]; then
  (cd "$VAULT" && git init -q && git add -A && git commit -qm "forseti vault scaffold" ) \
    && printf '  git: initialized\n' || printf '  git: held off\n'
fi

printf 'forseti vault ready: open ~/forseti as a workspace in ttt, an Obsidian vault, or pi cwd\n'
