# Forseti — Implementation Plan

Companion to `docs/design.md`. Update the status of each task as work lands.
Rule of thumb: every claim recorded here must have been executed or observed at
least once, else it stays marked unverified.

## Phase 0 — Foundation & remaining spike — ✅ **complete** (2026-09-30)

| Task | Status |
|---|---|
| Verify herdr CLI flags for bring-up (`tab create --cwd/--env`, `tab focus`, `pane split/run`) | ✅ done |
| Refresh `AGENTS.md` with verified tool/CLI/plugin facts | ✅ done |
| Record S1/S2 findings in `docs/spikes.md` (plugin contract, exec vocabulary, port caveat) | ✅ done |
| Repo skeleton (`herdr-plugin/`, `ttt-plugin/`, `pi-extension/`, `scripts/`, README) | ✅ done |
| S3: verify `agent start --kind pi -- <args>` passthrough — `scripts/spike-s3.sh` | ✅ done — args pass verbatim; non-interactive args time out on readiness wait (expected); see `docs/spikes.md` |
| S4 (optional): hunt a CLI/event surface for status push instead of polling | ⬜ deferred |
| Test stack: pi wired to OpenCode Go `glm-5.3-flash` (default) + halogen; both live-verified | ✅ done |
| `git init` + GitHub private remote (`alytaphoenix/forseti`) | ✅ done |

Exit criteria: skeleton in place; S3 answered; AGENTS.md current.

## Phase 1 — herdr plugin: idempotent bring-up (ttt + pi in a dedicated tab) — **partially verified** (2026-09-30)

Live result: bring-up action succeeded end-to-end on first run:
tab `w1:t3` (label `forseti`) created with 2 panes — ttt editor + pi agent pane;
`agent start --kind pi` reached `idle`/`interactive_ready`; ttt's `--listen` HTTP
server confirmed listening on 4242; `POST /exec` verified (screenshot test, HTTP 200).
Two parsing bugs fixed along the way: (a) `agent list` takes NO `--json` flag (JSON is
default — the first live run failed on that), (b) `tab create` keys are nested
(`.result.tab.tab_id`, `.result.root_pane.pane_id`), so flat `json_field` lookups
work for leaf keys only.

**Open bug P1-1**: the launched ttt instance shows `untitled` rather than the forseti
project dir — `herdr pane run` apparently runs the command in the plugin script's
cwd (plugin root), not the tab's `--cwd`. Fix candidate: use `pane run 'ttt <dir>'`
(open dir argument) instead of relying on shell cwd.

**Open bug P1-2**: `exec "Forseti: Jump"` returns 400 "not found" — correct, since
the Phase 2a Lua plugin (which registers that palette command) isn't installed yet.
This is the expected failure until 2a lands; document and proceed.

| Task | Status |
|---|---|
| Fix `agent list --json` flag bug (agent list takes no flags) | ✅ done |
| Fix nested response parsing (tab_id/pane_id via leaf keys) | ✅ done |
| plugin linked (`herdr plugin link`) and enabled, manifest validated by herdr | ✅ done |
| First live `forseti.open` → dedicated tab + ttt + pi(`coder`) `idle` | ✅ done |
| ttt `--listen` HTTP control surface verified (`POST /exec` 200) | ✅ done |
| P1-1: ttt opens without target dir (untitled) | ✅ resolved — misdiagnosis: `untitled` is ttt's default empty editor-tab label; the Explore panel was correctly rooted at the workspace dir from the very first run. The dir-argument added in open.sh is kept (more robust). |
| Idempotency check (second invoke → reuse/`agent focus`) | ✅ verified live: second `forseti.open` returned `{"forseti":"reused","agent":"coder"}`, exit 0; agent count stayed 1; no new tab. |
| README keybinding recipe validation (manual, in-TUI) | ⬜ deferred |

**Acceptance: met.** One tab, one ttt (listening on 4242), one pi — and repeated
invocations reuse rather than duplicate.

## Phase 2a — ttt plugin: ask, status sidebar, `Forseti: Jump` — ✅ **verified live** (2026-09-30)

Installed into `~/.config/ttt/plugins/forseti/` (copy — **symlinks are not picked up**
by ttt's plugin loader, and new plugins load at startup only via "Plugins: Reload
All"); first-load permission dialog approved (approvals persist in
`~/.config/ttt/plugins.ttt.json`).

Live verification:

| Feature | Result |
|---|---|
| `Forseti: Jump` (state file in plugin dir + `/exec` palette call) | ✅ opened `README.md` in a real editable buffer at the right line with the changed-range selection applied — via `ttt.open_file(path, line)` + `editor.set_selection`. No special permission needed. |
| `forseti.ask` (selection/current-line → `herdr agent prompt`) | ✅ prompt delivered to the live pi pane (code-block context built from the active buffer); pi lifecycle observed `idle → working → idle` |
| Sidebar "Forseti" panel | ✅ registered (`plugin.forseti` in sidebar debug dump); polls `herdr agent list` every 3 s |
| `set_status_item` | ✅ correct signature is `(side, id, text)` — found via live error, fixed |

Open/known items:

- Go's upstream had transient `server_error: upstream service timeout` on one turn
  (pi auto-retried); manual re-test replied instantly. Model flakiness, not wiring.
- FS sandbox note (recorded in design §2): jump state went to the plugin's own dir —
  `$SANDBOX` blocks reads outside workspace+plugin-dir, so `/tmp` was unusable.
- Sidebar tab visibility in the running pane (it exists in the panel list; verifying
  the visible tab strip renders it is a manual check).

## Phase 2b — pi extension: `/ttt` commands, follow mode — ✅ **verified live** (2026-09-30)

Installed at `~/.pi/agent/extensions/forseti.ts` (user-level — every pi instance
picks it up; repo copy is the source). Live E2E through a scratch herdr pi agent:

| Feature | Result |
|---|---|
| `/ttt jump <path> <line>` via slash command through `herdr agent prompt` | ✅ jump file written, ttt opened `docs/design.md` at the line |
| `/ttt follow on` + pi `edit` (append at line 2) | ✅ jump pushed with `firstChangedLine` → ttt opened `pi-follow-test.md` at the exact changed line |
| `/ttt follow` status | ✅ (state resets on `/reload` — re-toggle after reloads) |
| `/herd agents` (read-only) | ✅ |
| `/ttt diff` → `exec "Git: Open Changes"` | ✅ wired (verified palette title in ttt source; the command POSTs ok — visual confirmation of the Changes tab pending a manual look, non-blocking) |

Fix along the way: `ToolExecutionEndEvent` carries **no `args`** — follow mode pairs
`tool_execution_start` (has `args.path`) with end events via `toolCallId` to get the
path + `result.details.firstChangedLine`. pi's edit tool details literally document
`firstChangedLine` as "for editor navigation".

## Phase 3 — Sync, polish, shipping — ⬜ not started

- Event-driven status if S4 finds a surface; else tuned polling visibility rules.
- Notifications on `done`/`blocked` transitions (herdr notification surface).
- Jump accuracy: exact hunk from pi edit payloads; optional diagnostics follow.
- Packaging: `herdr plugin install` slug, ttt plugin zip/dir install, `pi install`
  package; per-component READMEs documenting trust + single-ttt constraint.
- Post-upgrade smoke script covering bring-up → ask → jump roundtrip.

## Parallel / housekeeping

- **U1**: upstream issue to ttt — request an `open FILE[:LINE[:COL]]` exec command
  (kills the palette+state-file dance; maintainer already ships a herdr plugin).
- Optional: `git init` + first commit (repo currently has no VCS).
- ~~Run S3 spike at the next live herdr session~~ → **done** (see below).

## Test infrastructure (added 2026-09-30)

- pi's default model in this repo is **OpenCode Go `glm-5.3-flash`** (project
  `.pi/settings.json`), with the halogen server (`halogen-qwen3.8-flash-next`) as a
  free secondary. Phase 1+ test runs launch `herdr agent start --kind pi` sessions
  against these. Both verified with live `pi -p` calls (2026-09-30).
- Go API key stored at `~/.config/forseti/opencode-go.key` (0600, outside the repo);
  referenced by pi via `!cat` in `~/.pi/agent/models.json`.
