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
| `git init` + GitHub remote `alytaphoenix/forseti` (public since 2026-09-30) | ✅ done |

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

## Phase 2c — real ask + IDE-awareness — **planned → implementing** (2026-09-30)

| Task | Design |
|---|---|
| 2c-1 `forseti.ask` takes a real question | Sidebar panel gains an input widget (`panel:input{on_submit}`); submit sends `path + loc + selection + user question`. `ctrl+k a` remains the quick-ask with the fixed template. |
| 2c-2 auto-focus round trip | After ask submits, `herdr agent focus <name>` so the answer streams in the pi pane (toggle command "Forseti: Toggle focus pi on ask", default ON). |
| 2c-3 editor context into every pi prompt | Lua writes `context.json` (plugin dir) on `cursor.change`/`file.save`/`file.open` (throttled): path/line/col/selection. pi ext transforms `pi.on("input")` (`{action:"transform"}`) to prepend `[Forseti editor context] …`; slash commands exempt; `/ttt context on|off` (default ON). |

## Phase 2d — model-driven editor + review mode — **planned → implementing**

| Task | Design |
|---|---|
| 2d-1 pi tools | `pi.registerTool`: `ttt_open(path, line?, end_line?)` and `ttt_diff()` — the model navigates ttt itself (same jump hand-off). Plus `ttt_read_context` (reads context.json). | ✅ **verified live** (2026-09-30): `ttt_open` by the model opened `README.md:5` |
| 2d-2 review mode | `/ttt review on|off` — collect edits across a turn (`tool_execution_*` pairing), on `turn_end` write `review.json` + `exec "Forseti: Review"` → Lua renders a review tab (custom `open_tab` listing files+hunks, opening the first change). Replaces per-edit jumping when both enabled. | ✅ **verified live** (2026-09-30): review.json listed both files; "Forseti: Review" palette cmd opened the summary tab (registered in `forseti.review`). Root cause of the break: command registration omitted from register table on first write (caught via `exec` 400 + screen check). NOTE: ttt caches registered commands at load — init.lua changes need restart/reload, and a plugin reload after a *failed* one latches stale state (verified 18:05); fresh ttt start is the clean dial. |
| 2d-3 `/ttt diff` E2E | Visual verify Changes view. | ✅ **verified live** (2026-09-30): `exec "Git: Open Changes"` → Changes view screenshot-verified. |
| 2d-4 `parse_agents` hardening | Single-pattern kind-first parse (removes the known `raw:match` over-selection wart); reversed-order fallback kept. |

Blocked/rule notes: manifest gains `fs.write`, `events.editor`, `events.file` → approval dialog re-runs (handled via /exec coordinate click, as before). Follow-mode-vs-review precedence: review wins when on. All state files stay in the plugin dir (fs sandbox).

## Phase 3 — Sync, polish, shipping — **in progress** (2026-09-30)

| Task | Status |
|---|---|
| Round-trip smoke test (`scripts/smoke.sh`): agent live + ttt reachable + jump hand-off | ✅ done & passing |
| Jump accuracy: exact changed line via pi's `firstChangedLine` | ✅ done (Phase 2b) |
| Notifications on `done`/`blocked` transitions | ✅ done — sidebar poll sets a right status-bar badge on `working → settled` transitions (`coder ●` / `! name needs you`); second run of the local herdr notification surface proved `disabled` on this setup, so the ttt status bar is the channel |
| Event-driven status if S4 finds a surface; else tuned polling visibility | ✅ resolved as polling (7 ms per `agent list` call — negligible; 3 s cadence) |
| Packaging: `herdr plugin install alytaphoenix/forseti/herdr-plugin` slug pattern verified (`OWNER/REPO[/SUBDIR]`); ttt plugin manual install documented; pi package shape added (`package.json` + `pi.extensions`), verified loadable via `pi -e`. Repo has been **public** since 2026-09-30 — hint: don't run `plugin install` while the local `plugin link` for the same id (`forseti`) is active — it would create a duplicate instance. | ✅ done |
| Post-upgrade smoke run (after herdr/ttt upgrades) | ⬜ continuous |

## Parallel / housekeeping

- **U1**: upstream issue to ttt — request an `open FILE[:LINE[:COL]]` exec command
  (kills the palette+state-file dance; maintainer already ships a herdr plugin).
- ~~Optional: `git init` + first commit~~ → done (GitHub, `alytaphoenix/forseti` — repo flipped public 2026-09-30).
- ~~Run S3 spike at the next live herdr session~~ → **done**.

## Test infrastructure (added 2026-09-30)

- pi's default model in this repo is **OpenCode Go `glm-5.3-flash`** (project
  `.pi/settings.json`), with the halogen server (`halogen-qwen3.8-flash-next`) as a
  free secondary. Phase 1+ test runs launch `herdr agent start --kind pi` sessions
  against these. Both verified with live `pi -p` calls (2026-09-30).
- Go API key stored at `~/.config/forseti/opencode-go.key` (0600, outside the repo);
  referenced by pi via `!cat` in `~/.pi/agent/models.json`.

## Implementation complete — status 2026-09-30

All four phases executed and verified live (see per-phase tables above; spikes in
`docs/spikes.md`). Working loop proven end-to-end:

```
forseti.open            → herdr tab: ttt (--listen) + pi agent
ttt ctrl+k a / palette  → selection + line → herdr agent prompt
pi replies / edits code → follow mode jumps ttt to the exact changed line
sidebar + status bar    → live agent lifecycle (idle/working/blocked/done)
smoke gate: scripts/smoke.sh → PASS
```

Only remaining (non-blocking) items: repo visibility decision (packaging for
public distribution), U1 upstream issue, and re-running the smoke gate after
herdr/ttt upgrades.
