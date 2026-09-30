---
title: forseti vault format
description: The user's evergreen secondbrain lives at ~/forseti — format v1 rules pi must follow when touching notes
---

# Forseti vault (evergreen secondbrain)

The user's notes vault is `~/forseti/` (override with `FORSETI_VAULT`). It is
plain markdown in a git repo. Use the `vault_*` tools instead of raw file
writes wherever possible — they enforce the format (slugs, frontmatter,
uniqueness, daily append-only).

## Format rules

- Evergreen notes live flat in `notes/`. One idea per note; the title is the
  filename slug in kebab-case and reads as a statement ("agents should verify
  against live systems"), not a topic ("agents").
- Frontmatter: `title`, `created`, `type` (evergreen|daily|reference|project),
  `tags`. Never invent ids.
- Links are `[[slug]]` wikilinks resolved by unique basename. When you create a
  note, link the existing notes it relates to — dense linking is the point.
- `daily/YYYY-MM-DD.md` is append-only under `## Log` (via `vault_daily`).
- `_templates/` and `assets/` are treated as infrastructure; don't edit templates.
- Mention notes by writing the `[[wikilink]]` inline — let the vault be the
  bridge between conversations.
