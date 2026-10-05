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
| S4 (optional): hunt a CLI/event surface for status push instead of polling | ⬜ deferred → **partially answered 2026-09-30**: socket-level subscribe/read surface found (spikes.md S4); framing folded into Phase 5 spike S6 |
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

## Phase 2c — real ask + IDE-awareness — ✅ **verified live** (2026-09-30)

| Task | Design | Status |
|---|---|---|
| 2c-1 `forseti.ask` takes a real question | Sidebar panel gains an input widget (`panel:input{on_submit}`); submit sends `path + loc + selection + user question`. `ctrl+k a` remains the quick-ask with the fixed template. | ✅ shipped — submit path is the same verified `submit_prompt` used by the palette ask; typed-input E2E pending manual focus check (headless focus limits) |
| 2c-2 auto-focus round trip | After ask submits, `herdr agent focus <name>` so the answer streams in the pi pane (toggle command "Forseti: Toggle focus pi on ask", default ON). | ✅ implemented (submit verified; focus toggle command registered) |
| 2c-3 editor context into every pi prompt | Lua writes `context.json` (plugin dir) on `cursor.change`/`file.save`/`file.open` (throttled): path/line/col/selection. pi ext transforms `pi.on("input")` (`{action:"transform"}`) to prepend `[Forseti editor context] …`; slash commands exempt; `/ttt context on|off` (default ON). | ✅ **verified live** — context.json updated on file open + jump-with-selection; coder (glm-5.3-flash) answered "AGENTS.md, lines 12–13" purely from the injected context |

## Phase 2d — model-driven editor + review mode — ✅ **verified live** (2026-09-30)

| Task | Design |
|---|---|
| 2d-1 pi tools | `pi.registerTool`: `ttt_open(path, line?, end_line?)` and `ttt_diff()` — the model navigates ttt itself (same jump hand-off). Plus `ttt_read_context` (reads context.json). | ✅ **verified live** (2026-09-30): `ttt_open` by the model opened `README.md:5` |
| 2d-2 review mode | `/ttt review on|off` — collect edits across a turn (`tool_execution_*` pairing), on `turn_end` write `review.json` + `exec "Forseti: Review"` → Lua renders a review tab (custom `open_tab` listing files+hunks, opening the first change). Replaces per-edit jumping when both enabled. | ✅ **verified live** (2026-09-30): review.json listed both files; "Forseti: Review" palette cmd opened the summary tab (registered in `forseti.review`). Root cause of the break: command registration omitted from register table on first write (caught via `exec` 400 + screen check). NOTE: ttt caches registered commands at load — init.lua changes need restart/reload, and a plugin reload after a *failed* one latches stale state (verified 18:05); fresh ttt start is the clean dial. |
| 2d-3 `/ttt diff` E2E | Visual verify Changes view. | ✅ **verified live** (2026-09-30): `exec "Git: Open Changes"` → Changes view screenshot-verified. |
| 2d-4 `parse_agents` hardening | Single-pattern kind-first parse (removes the known `raw:match` over-selection wart); reversed-order fallback kept. |

Blocked/rule notes: manifest gains `fs.write`, `events.editor`, `events.file` → approval dialog re-runs (handled via /exec coordinate click, as before). Follow-mode-vs-review precedence: review wins when on. All state files stay in the plugin dir (fs sandbox).

## Phase 4 — vault layer: evergreen secondbrain — ✅ **implemented + verified** (2026-09-30)

User decisions: vault at `~/forseti/` (top-level home dir, separate from this repo);
wikilink-based (`[[slug]]`, unique-basename resolution); evergreen note style
(flat namespace, statement-shaped titles, minimal frontmatter). No format exists
yet — this phase defines it. No daemon, no Obsidian API required; Obsidian is an
optional viewer over the same directory (watcher picks up changes natively).

### Vault format v1 (spec)

- **Root** `~/forseti/`, git-backed:
  `index.md` (hub) · `notes/` (flat) · `daily/` (append-only logs) ·
  `_templates/` · `assets/` · `.obsidian/` (Obsidian-generated if opened).
- **Filename = title slug** (`kebab-case`, statement-shaped evergreen titles),
  slug uniqueness enforced at creation (links stable, rename-safe).
- **Frontmatter** (YAML, minimal): `title`, `created` (`YYYY-MM-DD`),
  `type`: evergreen|daily|reference|project, `tags: [...]`.
- **Links**: `[[slug]]` resolved by unique basename; renaming migrations happen
  by grep-replacing the slug.
- **Daily**: `daily/YYYY-MM-DD.md` from `_templates/daily.md` (has `## Log`);
  entries append-only.

### Tasks

| Task | Design | Status |
|---|---|---|
| 4-1 `scripts/vault-init.sh` | Scaffold root, dirs, `_templates/{daily,evergreen}.md`, `index.md`, git init, `.gitignore` (skips nothing critical; tracks `.obsidian/`). Idempotent — safe to re-run. | ✅ **verified** — `~/forseti/` scaffolded, templates date-stamped, git initialized |
| 4-2 pi tools | `vault_search(query)` (rg over vault: file+snippet list), `vault_open(name)` (resolve slug → ttt jump), `vault_note(title, body?)` (slugify, uniqueness check → error with existing-path hint, frontmatter stamp, create), `vault_daily(text)` (append under `## Log` of today's file, create from template when missing). Tools scope paths to the vault root. | ✅ **verified live** (2026-09-30) via the coder agent: `vault_note` created `forseti-vault-format-supports-evergreen-notes.md` (frontmatter + [[daily-logs]] wikilink in body) |
| 4-3 vault skill | `skills/vault.md` — teaches pi the format (structure, conventions, slug rules, daily flow) so "put this in my notes" works unprompted; wired via project settings. | ✅ shipped — `skills/vault.md`, wired via `.pi/settings.json` `"skills"` (project settings resolve paths from the `.pi` dir) |
| 4-4 ttt commands | `Forseti: Daily Note` (create/open today's file), `Forseti: Open in Obsidian` (`open obsidian://open?vault=…&file=…`), backlinks panel (scan vault for refs to current file), wikilink jump (cursor inside `[[…]]` → resolve unique-slug → `open_file`). | ✅ shipped — two live digs root-caused: `sys.env(HOME)` unreliable (→ vault.json state file written by bring-up), and `Plugin.Filesystem` needs `WirePlugin` (→ present after a clean load; retry-till-loaded hedge added). Daily/backlinks/wikilink/Obsidian paths live in init.lua; screen tests land in the verify row |
| 4-5 herdr action | `forseti.notes` — bring-up variant with cwd = vault root (same idempotency/port-probe logic; `[[actions]]` entry runs `sh scripts/notes.sh` wrapping open.sh with `FORSETI_TARGET_DIR`). | ✅ shipped — `open-notes` action listed in `herdr plugin action list`; `scripts/notes.sh` added |
| 4-6 verify | vault scaffold → `pi -p` smoke of the four tools → ttt commands verified via `/exec` screenshots (same pattern as Jump/Review) → docs/AGENTS.md updated. | ✅ **verified live** (2026-09-30): Daily Note opens `daily/2026-09-30.md`; Backlinks scan opens the review tab; wikilink resolution works for vault notes. One upstream ttt bug found while verifying (LoadAll misses `wireAPIs` → startup-loaded plugins get nil Filesystem; hedge: retry pattern + S5 spike; U2 filed in docs) |

Key live-discovered facts this phase:
- Vault fs access from the ttt Lua sandbox requires the vault to be a **ttt workspace root** — bring-up now launches `ttt --listen <dir> <vault>` (multi-root) and records `vault.json` in the plugin dir (bring-up ↔ Lua state-file channel; `sys.env` proved unreliable for arbitrary vars in the sandbox).
- `herdr plugin action list` picks up manifest changes after re-link (`unlink` needs the id, not the path — `invalid_plugin_id` on path; the re-link of the same path re-registered actions anyway).

Non-goals for this phase: index-graph UI in ttt (backlinks panel covers v1),
Obsidian community plugin, Local REST API upgrade (rg baseline is sufficient).

## Phase 3 — Sync, polish, shipping — **in progress** (2026-09-30)

| Task | Status |
|---|---|
| Round-trip smoke test (`scripts/smoke.sh`): agent live + ttt reachable + jump hand-off | ✅ done & passing |
| Jump accuracy: exact changed line via pi's `firstChangedLine` | ✅ done (Phase 2b) |
| Notifications on `done`/`blocked` transitions | ✅ done — sidebar poll sets a right status-bar badge on `working → settled` transitions (`coder ●` / `! name needs you`); second run of the local herdr notification surface proved `disabled` on this setup, so the ttt status bar is the channel |
| Event-driven status if S4 finds a surface; else tuned polling visibility | ✅ resolved as polling (7 ms per `agent list` call — negligible; 3 s cadence) |
| Packaging: `herdr plugin install alytaphoenix/forseti/herdr-plugin` slug pattern verified (`OWNER/REPO[/SUBDIR]`); ttt plugin manual install documented; pi package shape added (`package.json` + `pi.extensions`), verified loadable via `pi -e`. Repo has been **public** since 2026-09-30 — hint: don't run `plugin install` while the local `plugin link` for the same id (`forseti`) is active — it would create a duplicate instance. | ✅ done |
| Post-upgrade smoke run (after herdr/ttt upgrades) | ⬜ continuous |

## Phase 5 — crew layer: agent graph builder + runner — ✅ **implemented + verified** (2026-09-30)

User decisions: deterministic runner (no LLM routing); full interactive
form/list builder (no canvas); Go + Bubble Tea; lives in this repo (`crew/`
component, `forseti-crew` binary). Monitor pane is a persistent split showing
the selected node's live pane tail. Reverses two v1 non-goals — recorded in
design.md §Phase 5.

### Spikes (verify before building — repo rule; formally resolve S4)

| # | Question | Status |
|---|---|---|
| S6 | Socket framing (NDJSON? handshake?) + `events.subscribe`/`events.wait` semantics — one-shot vs stream | ✅ resolved — NDJSON, no handshake; one-shot calls close after response; subscribe = persistent stream (`subscription_started` ack, `{"event","data"}` pushes) |
| S7 | `agent.read` behavior (`source` enum, scrollback vs screen, revision) + `pane_output_changed` event rate on a busy pi pane → pick monitor debounce from data | ✅ resolved — `result.read.text`, all 4 sources work; **no push surface for output** (`events.wait` status-only, `pane_output_changed` unsubscribable) → monitor = status stream + debounced `agent.read` |
| S8 | `layout.apply`/`layout.export` — declarative N-pane crew tab? | ✅ resolved — full BSP round-trip; walk `result.layout.root` for pane ids |
| S9 | Two concurrent pi agents with different `--model` args (per-node models) | ✅ resolved — both models live side by side, own status lines; wait `idle` before first prompt |
| S10 | `agent.view.set` semantics (output filtering? simplifies capture?) | ✅ resolved — UI-only sidebar projection with source ownership; not a control surface |

Driver kept: `scripts/spike-socket.py` (re-runnable, transcript to /tmp).

### Tasks

| Task | Design | Status |
|---|---|---|
| 5-0 spikes | S6–S10 above → `docs/spikes.md` | ✅ **all resolved live** — driver kept at `scripts/spike-socket.py` |
| 5-1 schema + loader | `crew.yaml` v1 (agents/edges/entry), validator (herdr name rule, edge endpoints, entry exists, loops need `max_visits`), example 2-agent planner→coder | ✅ shipped — `crew/internal/schema` + 7 unit tests passing; entry derived (no `entry:` key); example `crew/examples/crew.yaml` |
| 5-2 Go socket client | framing per S6, id-correlated request/response, subscription stream | ✅ shipped — `crew/internal/herdrd`: per-call dial (server closes one-shot conns), persistent `Subscribe()` with `subscription_started` ack, typed helpers (`AgentPromptWait` race-free dispatch) |
| 5-3 runner core | headless Go package: sequential + parallel fan-out/fan-in, `when: status[/regex]` edges, `{{ nodes.X.output }}` templating, bus-file handoff (`.forseti/bus/<node>.md`) for large outputs, timeouts, JSONL run log (`.forseti/runs/`); `forseti-crew run --headless` CLI | ✅ **verified live** (22:38) — planner→builder run created `HELLO_CREW.md` via `re:PLAN_READY` edge; bus files + JSONL run log confirmed; wave scheduler with parallel fan-out, `max_visits` cycle bounds |
| 5-4 app shell + monitor | Bubble Tea split layout: left graph/builder, right persistent monitor pane (live tail via `pane_output_changed` + `agent.read`, debounced per S7, visible target only), bottom event-log strip, focus-pane keybinding (read-only monitor; no nested-terminal interaction in v1) | ✅ shipped, UI verified in-pane (22:40) — monitor is debounced `agent.read` (700 ms, running-selected only) since S7 proved no output-push surface; event strip = last runner event |
| 5-5 builder | agent CRUD forms (name validated `[a-z][a-z0-9_-]{0,31}`, kind, args, prompt), edge forms (from/to/when), validation-on-save → `crew.yaml`, adopt-live-agent import | ✅ shipped, verified in-pane — add-agent/edge forms step through, adopt-live picked up `coder`, save validates before write |
| 5-6 integration | `scripts/crew-smoke.sh` (2-agent headless run E2E), README section, final design.md/AGENTS.md status update | ✅ **done** — smoke PASS, README+AGENTS updated, pushed 64ca798; late addition: `f` focus-pane keybinding verified live (TUI → agent.focus → herdr focus jumps to real pane) |

Boundaries: crew needs herdr+pi only (ttt optional; members may carry the forseti
pi extension); dedicated crew tab, teardown closes only what it created; `blocked`
surfaced, never auto-answered; one active run in v1; no LLM-routed edges in v1.

## Parallel / housekeeping

- **U1**: upstream issue to ttt — request an `open FILE[:LINE[:COL]]` exec command
  (kills the palette+state-file dance; maintainer already ships a herdr plugin).
- ~~Optional: `git init` + first commit~~ → done (GitHub, `alytaphoenix/forseti` — repo flipped public 2026-09-30).
- ~~Run S3 spike at the next live herdr session~~ → **done**.

## Test infrastructure (updated 2026-10-01)

- **Model discipline (user-confirmed)**: E2E/test crews and smoke gates pin
  **halogen** (`halogen/halogen-qwen3.8-flash-next`, LAN, free). The interactive
  bring-up agent keeps `opencode-go/glm-5.3-flash` (paid, low-volume).
  `crew/examples/*.yaml` all pin halogen; `.pi/settings.json` unchanged.
- Halogen quirks (AGENTS.md): emits `reasoning_content`, can return empty
  content when `max_tokens` is small (runner: one empty-settle retry, 6A-3),
  and is a **flaky LAN dependency** — it went down mid-session (connection
  refused); pi then stalls prompts (herdr `agent_prompt_stalled` after its 5 s
  detection window). E2E failing on a down LAN server is by design.
- Go API key stored at `~/.config/forseti/opencode-go.key` (0600, outside the repo);
  referenced by pi via `!cat` in `~/.pi/agent/models.json` (the switchyard
  proxy resolves the same way when generating its TOML — keys pass through env
  vars, never the file).
- `switchyard-server 0.2.0` (crates.io) installed; crew spawns a per-run proxy
  only when the crew file declares `routes:`.

## Phase 6 — observability + agent sandbox/E2E — ✅ **implemented + verified live** (2026-10-01)Spikes S11–S15 resolved live (see `docs/spikes.md`). Shipped:

| Task | Status |
|---|---|
| 6A-1 `check` nodes | ✅ checked crew ran: both `check_pass`, failing check → `check_fail` + process exit 1 |
| 6A-2 `--session` sandbox | ✅ sandbox session bootstrapped from a temp live-session pane (S11), run fully hermetic (`herdr agent list` in live session shows no crew agents) |
| 6A-3 halogen pin + empty-content guard | ✅ all example crews halogen; one empty-settle retry wired (untriggered live — halogen healthy on those runs) |
| 6A-4 `--worktree` | ✅ worktree + workspace created, panes/checks run inside, main checkout untouched, husk sweep + branch cleanup hardened; `-a` auto-trust for the fresh dir (pi trust dialog would stall every prompt — verified) |
| 6B-1 `forseti-crew watch` | ✅ post-hoc render: nodes + durations, checks, summary |
| 6B-2 status stream | ✅ per-pane `pane.agent_status_changed` subs → `node_status working/done` events in <1 s |
| 6B-3 blocked alerts | ✅ best-effort `notification.show --sound request` (returns `disabled` on this setup — expected); reliable path = 6B-5 badge |
| 6B-4 `agent.view.set` | ✅ set at run start (pane-id filter, attention sort), cleared at teardown; zero projection-failure events across all live runs (S10 verified the ownership semantics; the herdr snapshot does not expose active views, so visual confirmation is the human's Agents sidebar) |
| 6B-5 ttt status bridge | ✅ live: `crew demo 1/3 !` badge in ttt status bar while a run writes the file, cleared at teardown |
| 6C-1 output watchers | ✅ `watch:` regex armed on running panes, deduped `pattern_matched` events |
| 6C-2 monitor meta tab | ✅ `tab` key: pane id, model, status(+herdr), started/duration, visits, retries, output size, cost/ctx |
| 6C-3 cost capture | ✅ `ctx_pct` parsed live from pi's status line; run summary totals cost (halogen = $0.0000) |
| 6D-1/2/4/6 switchyard routing | ✅ routed-pool crew E2E: per-run proxy on a dynamic port, two agents through `switchyard/auto-pool`, routing JSONL tailed into `route_decision` events, provider entry materialized + restored at teardown |
| 6D-3 schema | ✅ routes + model/route exclusivity validated (unit tests) |
| 6D-5 TUI route forms | ✅ driven live in-pane: route form (id/efficient/capable/picker/confidence with defaults) + agent form route field, saved file passes `validate` |

Verification: `scripts/crew-smoke.sh` = **checked crew in `--session sandbox`
on halogen** — PASS (halogen + glm override paths both green). Watchers,
routing (halogen direct + glm fallback during the outage), worktree success
case, and the TUI route forms all verified in additional live runs.
Halogen's llama-swap dropped the pinned model on 2026-10-01 (registry swapped);
until it returns, E2E runs use `FORSETI_CREW_MODEL=opencode-go/glm-5.3-flash` —
the pinned files and the gate are unchanged.

Boundaries: crew still needs herdr+pi only; teardown closes only its own tab;
`blocked` surfaced, never auto-answered; one active run in v1. **Edges stay
deterministic** — switchyard routes model calls *within* nodes, never crew edges.

## Phase 7 — lazygit integration (git surface) — ✅ **implemented + verified live** (2026-10-01)

Spikes S16–S17 resolved live (see `docs/spikes.md`). Shipped:

| Task | Status |
|---|---|
| P7-1 `forseti.git` action | ✅ focus-or-create lazygit pane in the forseti tab; process-info idempotency (second invoke focuses, never duplicates; user-quit relaunches safely); never types into running TUIs |
| P7-2 ttt palette `Forseti: Git (lazygit pane)` | ✅ re-dispatch via herdr action; result surfaced as status item |
| P7-3 crew `--review` | ✅ implies keep-worktree+keep-tab; lazygit pane on the worktree at run end; merge recipe in `run_end`; `crew/examples/crew-worktree.yaml` with git-state checks |
| P7-4 lazygit customCommands loop | ✅ marked marker-guarded config block; Ctrl+G → live ttt (opened the selected file), Ctrl+Y → live pi (coder answered) |
| P7-5 ttt `Forseti: Ask pi about uncommitted changes` | ✅ porcelain status → capped diff excerpt → coder prompt; clean-tree no-op message |

Verification: git.sh invoked twice live (created → focused); full worktree
review E2E green on glm (halogen's model still absent from the LAN registry);
lazygit Ctrl+G/Ctrl+Y live drives both closed the loop. Load-order lesson
recorded: handlers must be defined above `ttt.register`.

## Phase 8 — Laya decision layer — ✅ **implemented + verified live** (2026-10-01)

Spikes S18–S19 resolved live (see `docs/spikes.md`). Shipped:

| Task | Status |
|---|---|
| P8-1 runtime + serve | ✅ pinned venv (`laya==0.3.22`), `scripts/laya-setup.sh` + `laya-serve.sh` (start/stop/status/restart, /health-polled, pidfile idempotent); endpoint on 127.0.0.1:8751, mps, warm ~21 ms |
| P8-2 `when: laya:choice` edges | ✅ schema (min_confidence + state_file) + grouped runner gate; E2E green (opsfix conf 0.675 / 119 ms); abstention path verified live (0.431 < 0.45 → skip) |
| P8-3 pi tool `forseti_decide` | ✅ registered + deployed; live probe: bounded decision with distribution, correct hedge on ambiguous input |
| P8-4 calibration + abstention | ✅ `scripts/laya-eval.sh` + 9-probe set (choice/noul/score/abstain contracts) — 9/9 PASS |
| P8-5 docs + demo | ✅ design.md Phase 8 as-built, this table, spikes S18/S19, `crew/examples/crew-laya.yaml` |

State-quality lesson recorded: laya gates should judge a file artifact, not
terminal scrollback (encoder truncation + prompt echo, hit live).

## Phase 10 — shared agent memory — ✅ **implemented + verified live** (2026-10-01)

Spikes S22a/S22b resolved (see `docs/spikes.md`; Opik = documented no-go).

| Task | Status |
|---|---|
| P10-1 memory service | ✅ FastAPI in the laya venv, port 8752, SQLite+vec0 (cosine), all-MiniLM-L6-v2; /write /recall /forget /health; namespace isolation verified both ways |
| P10-2 pi tools + crew helper | ✅ `memory_write`/`memory_recall` live (agent answered strictly from recall); `{{ memory "q" }}` renders recalled entries into node prompts (failure → "") |
| P10-3 serve script | ✅ `scripts/memory-serve.sh` (hardened stop: waits for the port; restart race hit live) |
| P10-4 opik exporter | ⏸ deferred (S22b no-go: no hosted key, docker down; exporter designed, zero runner deps) |
| P10-5 eval gate | ✅ `scripts/memory-eval.sh` — 4/4 (paraphrase recall ×3 + secret-namespace isolation) |

## Phase 11 — crew phases: planner work assignment — ✅ **implemented + verified live** (2026-10-05)

User decisions: the planner = the crew builder TUI; strict barrier (all of
phase N terminal before N+1 dispatches); failure/skip releases the barrier
(`blocked` holds it — surfaced, never auto-answered); full vertical in one
pass. No spikes needed — no new herdr/ttt/pi surface, pure crew work.

| Task | Status |
|---|---|
| 11-1 schema: `Phase{Name, Instructions, Agents}` + `Crew.Phases` + validator (name rule, duplicate, unknown/double/unphased member, backward-phase edge) + `PhaseOf`/`Phase` helpers | ✅ schema tests green |
| 11-2 runner: barrier gate in the wave scheduler (`phaseUnlocked`, terminal = done/failed/skipped; blocked holds), instruction injection (`instructions + "\n\n" + prompt` as ONE template render), `phase_start`/`phase_done` events, `phase` on `node_start` | ✅ unit tests + live |
| 11-3 observability: `watch` groups rows under phase headers; `crew-status.json` gains `phase`; ttt badge renders `crew <name> [<phase>] <done>/<total>` | ✅ live (badge field additive) |
| 11-4 TUI: `p` phase form (name/instructions/members), graph view grouped under `— phase N <name>`, meta tab phase line, save round-trips validation | ✅ driven in-pane via `scripts/crew-tui-drive.py` (incl. live rejection of an unphased agent) |
| 11-5 example: `crew/examples/crew-phases.yaml` (halogen-pinned, 2 phases + checks) | ✅ validates; ran live |
| 11-6 gate: `crew-smoke.sh` runs the phased crew as a second sandbox run + asserts barrier ordering from the JSONL (research phase_done before build phase_start) | ✅ **PASS** |
| 11-7 docs: design.md Phase 11 as-built, this table, AGENTS.md crew bullet | ✅ |

Boundaries (future items): phase-boundary checks (`after: phase:<name>`),
agents in multiple phases, cross-phase loop-backs.

## Phase 12 — memory system v2 — ✅ **implemented + verified live** (2026-10-05)

User decisions: survey-only spike; all eight improvement areas; **RRF k=60**
default fusion; associative-link expansion **default-on bounded**. Spike S23
resolved (primary sources: arXiv full texts, Mem0 OSS source, official docs —
`docs/spikes.md`).

| Task | Status |
|---|---|
| 12-1 spike S23 | ✅ resolved (survey) — fusion=RRF k=60; supersede-not-delete (Zep); Mem0's published recall scoring + write-quality rules; A-MEM concat-embeddings + link propagation; Letta tiering-by-context; explicit overkill list with receipts |
| 12-2 v2 design | ✅ user-confirmed before build (RRF; expansion default-on) |
| 12-3 service v2 | ✅ `memory_serve.py` rewritten: FTS5 external-content index + triggers + rebuild backfill; hybrid RRF recall with semantic pre-gate; near-dup merge; explicit supersedes (fixed live: inverted semantics + heuristic swallowing conflicts); access reinforcement; schema migrated on start (`PRAGMA user_version` 1→2, 8 pre-existing rows preserved — verified live) |
| 12-4 consumers | ✅ pi tools gain optional params (type/importance/supersedes/expires_at/links on write; type + include_superseded on recall); deployed to `~/.pi/agent/extensions/forseti.ts`; crew helper unchanged |
| 12-5 eval gate | ✅ **11/11 PASS** — P10's four contracts + exact dedup, near-dup merge, hybrid keyword-leg recall, supersede default + history queryable, TTL, type filter + importance |
| 12-6 embedding check | ✅ four 384-d candidates compared on the probe set — 5/5 ties → **KEEP INCUMBENT** (`scripts/memory-embed-eval.sh`, re-runnable) |
| 12-7 docs | ✅ design.md Phase 12 as-built, this table, AGENTS.md memory bullet, spikes S23 |

Boundaries (future items): write-time extraction pipeline; entity/graph
store; history table for merged facts; memory remains advisory context.

## Phase 13 — full-repo bug review — ✅ **implemented + gates green** (2026-10-05)

User instruction: review the whole repo, fix ALL confirmed bugs, then commit+push.
Four read-only review agents (memory service; Go runner+schema; pi/ttt/shell;
Go cmd+clients); every finding adjudicated, fixed, re-verified. Record:
docs/spikes.md S24; semantics as-built: docs/design.md Phase 13.

| Task | Status |
|---|---|
| 13-1 scheduler | ✅ fan-in **AND** + edge fire/miss bookkeeping + back-edge re-dispatch (`schema.BackEdges`); no emit under lock; µs run-log names; Snapshot value copies; unit tests incl. `TestNodeReadyFanInAND`/`TestFireEdgeReArmsSettledTarget` |
| 13-2 schema | ✅ rejects entry-less crews, duplicate (from,to) edges, invalid crew names (`ValidName`); `inCycle` consecutive-endpoints fix; self-loop allowed only with `max_visits` |
| 13-3 runner contract | ✅ `check_skip` + `teardown` events; blocked at run end → KeepTab+KeepWorktree + nonzero verdict (TUI ≡ watch); empty-settle retry box-only (halogen/valhalla) |
| 13-4 clients | ✅ herdrd: `agent_prompted` live shape + unknown-shape error, reply-ID correlation, subscription mutex; switchyard: 2 s client, key-delete restore w/ ownership check, stale-pre-entry sweep, quoted TOML keys |
| 13-5 forseti-tools | ✅ isError on executor failure, http-probe body match, `ValidateArgs` serve+cli, unquoted-`{{ .param }}` lint, dup tool names, `in.Err()` on stdin |
| 13-6 cmd/forseti-crew | ✅ event-based exit verdict (blocked+checks), saveCrew writes -f path, watch check-reset per replay, runewidth pad/height clamp, display order + phase headers, ctrl+c/shift+tab, tool probe off the event loop, phase-form validation, NArg/timeout arg guards |
| 13-7 memory service | ✅ M0–M22 (see S24): forget namespace guard, BEGIN IMMEDIATE dedup, gate applies to FTS-only ids, extra=forbid, un-supersede on forget, export `_meta` + import validation, DIM/model lock, schema v3 (FTS `AFTER UPDATE OF text`) — memory-eval **13/13** |
| 13-8 serve scripts | ✅ stop=pid-death + identity check + boot nonce + guarded `adopt()` (live-verified restarts + laya adoption; laya-eval 9/9) |
| 13-9 pi extension | ✅ fetch timeouts everywhere, `Type.Integer` ids/lines/k, JSON-encoded frontmatter + `notes/` mkdir, `pendingEdits.clear()` per turn, dead code out; deployed to `~/.pi/agent/extensions/forseti.ts` |
| 13-10 ttt plugin | ✅ wikilink span scan, backlinks literal find, per-object pi-kind agent filter, throttle cursor-only, obsidian nil-vault/URI/exec-result guards, manifest `open`+`xdg-open`; duplicate `review()` removed; deployed by COPY |
| 13-11 herdr-plugin scripts | ✅ `squote`/`json_escape` guards (pane-run commands, state files, jump handoff), repo/vault state written BEFORE reuse-exit, lazygit templates use sprig `quote` |
| 13-12 dev scripts | ✅ crew-smoke: mktemp scratch + trap, failure diagnostics; crew-tui-drive: repo-relative BIN, saved-<path> match, rmtree on PASS; vault-init: `__TODAY__` daily template + fresh-only stamp + portable sed + legacy migration (live vault repaired) |
| 13-13 gates self-contained | ✅ memory-eval spawns its own service (scratch DB, private port) — no shared-DB pollution; embed-eval exit code gates incumbent floor (4/5) |
| 13-14 final gates | ✅ go build/vet/test; memory-eval 13/13; laya-eval 9/9; embed-eval 5/5+floor; **crew-smoke PASS** (checked+phased, `FORSETI_CREW_MODEL=valhalla/valhalla-flash-next`) |

Deliberate skip: memory-service `closing()` refactor (CPython refcounts already
close per-request conns; churn without observed failure).

## Phase 12.5 — memory hardening — ✅ **implemented + verified live** (2026-10-05)

| item | status |
|---|---|
| 12.5-A `/clear` + `/namespaces` | ✅ `shared` needs `confirm:true`; rows whose superseder was cleared are REVIVED; one-off shared-DB cleanup 106→4 rows (all probe debris gone, real memories kept) |
| 12.5-B auto-links on write | ✅ top-10 visible candidates, cosine [0.7, 0.95), every INSERT path; shared rows never link private ones (existence leak) |
| 12.5-C history audit (schema v4) | ✅ add/merge/supersede/delete/revive commit INSIDE the mutation txn; `GET /history?memory_id=` namespace-guarded; pre-v4 rows backfilled with `add` |
| 12.5-D boost calibration | ✅ probes FOUND a real bug: recall truncated at k in RRF order BEFORE boost-ranking (importance/recency were decorative, contract said otherwise) — now boost→sort→truncate; +recency-reinforcement probe |

## Phase 12.6 — laya in memory — ✅ **implemented + verified live** (2026-10-05)

| item | status |
|---|---|
| 12.6-1 gray-band conflicts | ✅ binary value-changed? verdict per top-3 in-band candidate, computed OUTSIDE the write txn: `same`→merge, `changed`→auto-supersede (M9-guarded), abstain/down→pure P12 heuristics |
| 12.6-2 type auto-classification | ✅ omitted `type` → fact/episode/procedure/preference (abstain→fact); explicit types unchanged |
| 12.6-3 gate calibration | ✅ gate = P(top label) ≥ 0.65 (`FORSETI_MEMORY_LAYA_PROB`); laya's `confidence` field proven unusable for rubrics (S25: 0.09–0.27 while argmax right) |
| 12.6-4 degradation | ✅ laya-down probe: writes succeed with heuristic behavior preserved (auto-links + 0.95 merge + fact default) |
| 12.6-5 gates | ✅ memory-eval **23/23** (3 laya rows SKIP-not-fail when laya is down, total adapts); laya-eval 9/9; live shared-service smoke: auto-supersede + /history + /clear |

## Phases 0–5 implementation complete — status 2026-09-30

All five phases executed and verified live (see per-phase tables above; spikes in
`docs/spikes.md`). Working loop proven end-to-end:

```
forseti.open            → herdr tab: ttt (--listen) + pi agent
ttt ctrl+k a / palette  → selection + line → herdr agent prompt
pi replies / edits code → follow mode jumps ttt to the exact changed line
sidebar + status bar    → live agent lifecycle (idle/working/blocked/done)
~/forseti vault         → evergreen notes + daily logs via pi tools / ttt commands
forseti-crew run        → deterministic agent graph (planner→coder) in its own tab
smoke gates: scripts/smoke.sh + scripts/crew-smoke.sh → PASS
```

Phase 5 (crew) added: `crew/` Go module (`forseti-crew`), socket client +
runner + Bubble Tea builder/monitor, `crew.yaml` v1 with validator + unit
tests, example planner→coder pipeline E2E (created `HELLO_CREW.md` through
the `re:PLAN_READY` edge), smoke gate.

Only remaining (non-blocking) items: U1/U2 upstream issues to ttt, and
re-running the smoke gates after herdr/ttt upgrades.
