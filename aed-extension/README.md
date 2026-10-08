# Forseti aed pi extension

Agent-side component: let a **pi** session make safe, verified file edits by
delegating to [agentic-edit](https://github.com/alytathoenix/agentic-edit)
(`aed`). This extension never edits code itself — it shells out to `aed`, which
previews the diff, writes a `.bak` backup, runs a safety denylist, and (by
default) verifies the change landed.

```
/aed edit  <action> --files a.go b.go [--apply true|false] [--verify true|false]
/aed grep  <pattern> --files *.go [-E]
```

Model-facing tools (auto-discovered by pi):

- `aed_edit` — make an edit. `action` is a natural-language instruction
  (e.g. `replace "v1" with "v3"`); `files` is one or more target files; `apply`
  defaults to `true` (apply + verify); set it `false` for a dry-run preview;
  `verify` defaults to `true` (the post-apply guard).
- `aed_grep` — search files; returns 0 even when nothing matches.

`aed` is resolved from `AED_BIN` (default `aed` on `PATH`). If it is missing the
tool returns an install hint.

## Install

**User-level (recommended)** — every pi session gets it:

```sh
cp aed-extension/index.ts ~/.pi/agent/extensions/forseti-aed.ts
```

**As a package** (shareable):

```sh
pi install /path/to/forseti/aed-extension          # local source package
pi install git:github.com/alytathoenix/forseti@v0.1.0   # pinned git ref
```

The directory qualifies as a pi package (`package.json` with `pi.extensions`;
verified loadable via `pi -e ./aed-extension`).

**Try for one invocation:** `pi -e ./aed-extension`

## Prerequisites

Install agentic-edit once so `aed` is on PATH:

```sh
git clone https://github.com/alytathoenix/agentic-edit.git && cd agentic-edit
go build -o aed ./cmd/aed
mv aed "$(go env GOPATH)/bin"   # or any dir on PATH
```

Point at a custom binary with `AED_BIN=/abs/path/to/aed` (e.g. via
`~/.pi/agent/models.json` env or the shell that starts pi).

## Notes

- Read-only from pi's side: the extension only ever asks `aed` to do the work.
- The `aed edit` guard exits `3` on a verification failure; that surfaces here as
  `isError: true` with `aed`'s error diff in the text.
- Extension state resets on `/reload`.
