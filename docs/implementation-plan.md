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
| Test stack: pi wired to OpenCode Go `glm-5.3-flash` (default) + halogen; both live-verified — **refreshed 2026-10-17: free Go ids `beam-free`/`longcat-2.5-preview-free`** | ✅ done |
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

## Test infrastructure (updated 2026-10-17): free OpenCode Go ids

- **Model discipline**: E2E/test crews and the smoke gate now pin
  **`opencode-go/beam-free`** — a zero-cost OpenCode Go id. The LAN box
  (halogen→valhalla) is reserved for the user's clustering work, and the paid
  Go ids (`glm-5.3-flash`, `space-bunny`) return **402 Insufficient funds**.
  `crew/examples/*.yaml` all pin the free id; `.pi/settings.json` follows.
- The free tier is the whole test path now: `beam-free` (efficient) +
  `longcat-2.5-preview-free` (capable) in the routed examples, so a routing
  demo needs neither the LAN box nor a funded key. Both ids are verified live
  (chat, tool-calling, a 64k `max_tokens` request, a ~180k-token input).
- **Reasoning-model empty-content guard**: both free ids reason, so they share
  the halogen/valhalla failure mode (a small `max_tokens` is spent on reasoning
  and the answer comes back empty). The one-shot empty-settle retry is now
  keyed by `runner.emptySettleProne` (halogen|valhalla|beam-free|longcat)
  instead of the LAN box alone; healthy non-reasoning providers still skip it,
  so a short answer is never paid for twice.
  `runner_test.go: TestEmptySettleProneMatchesFreeReasoningIds`.
- Free ids are rate-limited/occasionally unavailable (a 429 `Endpoint is
  unavailable` was observed mid-probe); the one-shot re-prompt absorbs a
  transient empty settle, and `FORSETI_CREW_MODEL` stays the per-run escape
  hatch (`--model` for the CLI).
- Go API key stored at `~/.config/forseti/opencode-go.key` (0600, outside the repo);
  referenced by pi via `!cat` in `~/.pi/agent/models.json` (the switchyard
  proxy resolves the same way when generating its TOML — keys pass through env
  vars, never the file).
- `switchyard-server 0.2.0` (crates.io) installed; crew spawns a per-run proxy
  only when the crew file declares `routes:`.

## Phase 6 — observability + agent sandbox/E2E — ✅ **implemented + verified live** (2026-10-01)

Spikes S11–S15 resolved live (see `docs/spikes.md`). Shipped:

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

## Phase 14 — MemTree hierarchy + crew memory integration — ✅ **implemented + gates green** (2026-10-07)

| item | status |
|---|---|
| 14-1 MemTree port | memtree.py: theta(d) descent insert, collapsed-tree recall resolving internal hits to descendant leaves, living-children recompute on merge/forget (paper facts + deviations: spikes S26) |
| 14-2 schema v5 | tree_nodes + memories.parent_id + tree_vec (vec0), in-txn backfill of pre-v5 rows, GET /tree, export _tree lines; kill switch FORSETI_MEMTREE=0 |
| 14-3 aggregation | heuristic default; FORSETI_MEMTREE_AGG=llm advisory valhalla fold (8 s cap; any failure falls back; writes never block) |
| 14-4 crew wiring | memory: block (namespace/auto_recall/recall_k/auto_write/write_max_chars) + per-agent memory: off / memory_query; auto-recall at dispatch, auto-write on node_done (event memory_write); .forseti/memory-crew + FORSETI_MEMORY_CREW |
| 14-5 gates | memory-eval 28/28 (5 new P14 rows); Go suite green (schema memory tests + MemoryClient payload tests); example crew/examples/crew-memory.yaml validates |

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

## Phase 15 — focus bridge: ttt ⇄ herdr agent switcher — ✅ **implemented + gates green** (2026-10-09)

Orchestration stays in herdr (input addressing, status events — S27); ttt
becomes the human's switching surface. Design rationale: the trade-off
analysis that became docs/ttt-terminal-api.md.

| item | status |
|---|---|
| 15-1 runner bridge extension | crew-status.json gains nodes:[{name,status}] (run state, crew order) + tab (crew tab id); unit test pins the contract incl. the finished-run freeze | ✅ live (mid-run probe) |
| 15-2 sidebar switcher | agent rows are a panel:list — up/down + enter → `herdr agent focus` (verified `focused:true`, S27); crew members get a `crew` badge | ✅ code; ttt-reload check = user |
| 15-3 clickable crew badge | `set_status_item … {on_click}` → `herdr tab focus <tab>`; unknown tab → tab_not_found/exit 1 → fallback focuses first blocked, else running, else done node | ✅ code (fallback path proven live, S27); ttt-reload check = user |
| 15-4 palette | `Forseti: Focus agent pane (cycles)` — keyboard-only path when the sidebar is closed | ✅ code |
| 15-5 gates | go suite green (new status-bridge contract test); `luac -p` clean; live sandbox probes S27 (agent list ids, agent focus, tab focus, sidebar parser, mid-run status file) | ✅ |

Pending (needs a GUI ttt): **Plugins: Reload All** in the live ttt, then select a
row in the Forseti sidebar → the agent's pane takes focus; click the crew badge
during a run → lands on the crew tab. Only claim what's verified — that row is
the user's to mark.

## Phase 16-proposed — ttt terminal API (fork/upstream) — 📋 **planned, not started** (2026-10-09)

Run pi as a *named terminal tab inside ttt* (single-window crew). Blocked on
fact today: ttt v1.6.0 has no Lua terminal API and /exec cannot address a
non-focused tab (S27). Patch design, integration bridge, step plan and
abandon criteria: **docs/ttt-terminal-api.md**. Phase 15 is the hedge that
ships now.
## Phase 18 — switchyard 0.3.0: full headers + full routing on an embedded Rust host — 🚧 **in progress** (2026-10-15)

Direction (agreed): upgrade to switchyard 0.3.0, implement the **full header and
routing surface**, and move toward a **Rust host that embeds switchyard in-process**
(no separate `switchyard-server`). Python layers (laya/memory) stay isolated
127.0.0.1 services — PyO3 embedding rejected on evidence (1.3 GB venv, libpython/GIL
coupling, loses crash isolation; S28).

### 18A — full headers ✅ **implemented + verified live** (2026-10-15)
- `pi-extension/switchyard.ts` (new, independent of the ttt bridge `index.ts`):
  `before_provider_headers` injects `x-switchyard-session-id` + legacy
  `proxy_x_session_id` (pi's session id), `x-switchyard-origin`,
  `x-switchyard-request-id`, opt-in `x-switchyard-agent-id`; gated to the
  `switchyard` provider. `after_provider_response` → `/switchyard` reports the
  served target (`x-model-router-selected-model`).
- Live E2E: pi → `forseti-host` (embedded switchyard) → `valhalla-flash-next`;
  routing log `origin` tracks `FORSETI_SWITCHYARD_ORIGIN` (S29).
- `pi-extension/package.json` ships both extensions; README + AGENTS + S29 updated.

### 18B — embedded Rust host ✅ **implemented + verified live** (2026-10-15)
- `host/` crate `forseti-host` embeds `BoundServer` (`switchyard_server` lib),
  loads route TOML, prints `FORSETI_HOST_ADDR/MODELS/READY`, supports
  `--routing-log` and `--dry-run` (S28 live proof).
- The crew's supervisor now resolves OUR host first (`ResolveRouter`:
  `FORSETI_CREW_ROUTER_BIN` → `host/target/{release,debug}/forseti-host` →
  `$PATH` → legacy `switchyard-server`), and `Router.Args` keeps both flag sets
  working. `--dry-run` runs as a pre-flight before a port is bound, so a bad
  routes file fails with the router's own message.
- The routing log now carries real attribution (route id, algorithm, tier,
  session, origin, token split) in `route_decision` events — the S29 era shim
  and the "per-node attribution impossible" note are gone (S30).
- Live gate: `FORSETI_LIVE_ROUTER=1 go test ./internal/switchyard/` (20.5 s) —
  Start → dry-run → bind → `/health` → a real routed completion → routing record.
  Needs the free LAN model; unit tests stay offline.

### 18C — full routing surface ✅ **implemented + verified live** (2026-10-15)
- `schema.Route` covers the whole 0.3.0 `AlgorithmSpec` surface: tier types
  derive `capable_target`/`efficient_target` (or `strong_target`/`weak_target`),
  `passthrough`/`random`/`noop` map directly, a new `judge:` mints `<id>_judge`
  and wires `classifier_target`, and `options:` passes any other route key
  through verbatim (TOML inline tables for nested shapes) — so a router release
  with new keys needs no forseti change.
- Reserved-key collisions, tier requirements, and type validity are rejected at
  load; the router's `--dry-run` remains the authority on semantics, and
  `forseti-crew validate` now runs it (`crew/examples/crew-routes.yaml`
  exercises stage_router/auto/plan_execute/llm_classifier+judge/advisor).

### 18D — runner/TUI on the host (staged Rust port) ✅ **18D-1 + 18D-2 + 18D-3 implemented** (2026-10-18)

Direction (agreed): `forseti-host` grows from "embedded router" into the crew
**engine** (herdr client, scheduler, runner, TUI) while the Go `forseti-crew`
stays the **compatibility surface**. Every stage is gated by byte-identical
output against the Go engine, so the port can be finished incrementally without
freezing either engine: `scripts/crew-host-oracle.py` captures goldens from
`forseti-crew validate -f FILE --explain` and the Rust engine must reproduce
them exactly (stdout, stderr, exit code).

1. **18D-1 ✅ done** — the crew model + YAML load + validation + `explain` in
   Rust, exposed as `forseti-host crew validate -f FILE [--explain]`. Scope = the
   base graph (agents/edges/phases/checks/watch/memory/routes). `roles:`/
   `patterns:`/an agent's `role:` parse but are **rejected** with the constant
   `validate::STAGE_2_UNSUPPORTED` rather than silently ignored — the divergence
   is visible, never a wrong "valid".
   - Crate layout: `host/` gained `[lib] forseti_host`; the crew port lives in
   `host/src/crew/{mod,model,routes,validate,explain,command}.rs`. Deps added:
   `serde_norway` 0.9 (maintained serde_yaml fork), `regex`; dev-dep `serde_json`.
   The router path (`--config/--addr/--routing-log/--dry-run` → `BoundServer`) is
   untouched, so the Go runner keeps spawning this binary as its router.
   - Oracle: `scripts/crew-host-oracle.py [--check]` captures
     `forseti-crew validate -f FILE --explain` per crew into
     `host/tests/golden/<kind>/<crew>.json` (`{file, argv, exit, stdout, stderr}`)
     and `host/tests/crew_oracle.rs` replays every record through the Rust engine
     — so `cargo test` needs no Go. `--check` fails on a stale/missing/unexpected
     golden. The corpus is `crew/examples/*.yaml` + `host/tests/fixtures/{valid,invalid}/*.yaml`:
     60 records at 18D-1 (when `crew-sop.yaml` was pinned-skipped), 85 after
     18D-2 (9 examples, 4 valid, 72 one-rule-per-file invalid fixtures) — the
     `STAGE_18D2_SKIP` gate and its `crew-sop.yaml` entry were removed once the
     Phase-19 layer landed. Route dry-run nondeterminism is removed by pointing
     `FORSETI_CREW_ROUTER_BIN` at a `exit 0` stub (the Rust engine has no dry-run
     yet).
   - Documented divergences (outside the corpus): regex-engine error text (Go RE2
     vs the Rust `regex` crate) and YAML parse-error text; both keep Go's prefixes
     (`edge N: bad regex …`, `parse crew.yaml: …`).
   - Unit tests cover what the oracle cannot: the two regex-error rules, every
     valid `when` form + defaults, and the graph helpers (`find_cycles`,
     `back_edges` DFS classification, `derive_subscriptions`, `entry`,
     `writes_type`, the CLI missing-file message). (18D-2 replaced the
     stage-rejection test with the role/pattern/bus suites; the corpus grew to
     85 records — see the 18D-2 gates below.)
2. **18D-2 ✅ done (2026-10-18)** — the Phase-19 authoring layer: `role` (the
   archetype library + `RolePack.Validate`), `pattern` (the four SOP patterns
   compiled into the graph), `bus` (`ParseVerdict`). `Crew::validate` now runs
   `expand_patterns()` then `RolePack::validate` at the top, in the Go engine's
   exact order, and the stage-18D-2 rejection shim is gone. Ported byte-for-byte:
   the synthesized node names (`rl-reviewer`, `ap-critic-2`, `ap-judge`,
   `ex-implement`, `ex-verify`), `then:` chain resolution (a pattern id → its
   ENTRY nodes), the phase join (a synthesized node joins its subject's phase; a
   subject-less executor joins the last), `criteria_note`, and the ~22 role/
   pattern validation strings. `explain` gained the `role=<id>(<Profile>)` node
   tag and the `patterns:` block.
   - **`prompts` parity surface (new)**: `validate --explain` pins the graph but
     not the prompt text the runner will render, so the Go CLI gained a
     `forseti-crew prompts -f FILE` subcommand that dumps every resolved node's
     prompt as JSON. The oracle captures it into each golden record (`prompts`),
     and `crew_oracle.rs` loads the crew through the Rust library and diffs
     name+prompt node-by-node — a one-word pattern/role text drift fails the gate
     (verified by mutation).
3. **18D-3 ✅ done (2026-10-18)** — the herdr socket client,
   `host/src/crew/herdr.rs`, the Rust port of `crew/internal/herdrd`. Scope:
   socket-path resolution (`HERDR_SOCKET_PATH` → `HERDR_SESSION` →
   `$HERDR_CONFIG_DIR|~/.config/herdr`, plus `socket_path_for(name)` for named
   sessions), `Client` with the mutex-guarded `seq` and the
   `crew-<prefix>-<n>` id shape, dial-per-call `call` on NDJSON with the
   empty-id rule and the Go `bad response frame` / `does not match request`
   checks, `subscribe` + a pump thread (60 s idle keep-alive, a pushed
   `events_lost` frame terminal) behind a closable `Subscription`, and every
   typed helper the runner needs: `ping`, `agent_list`, `agent_read`
   (`result.read.text`), `agent_start`, `agent_prompt`, `agent_prompt_wait`
   (prompt + settle in ONE request, the `agent_blocked` → `"blocked"` mapping,
   and **both** settle frames — the event shape and herdr 0.9.3's
   `{"type":"agent_prompted","agent":{…}}`), `agent_focus`, `agent_wait`,
   `focused_workspace`, `notification_show`, `agent_view_set`/`clear`,
   `pane_wait_for_output`, `worktree_create`/`remove`.
   - `serde_json` moved from a dev-dep to a normal dep (the wire is
     `serde_json::Value`; 18D-5's run log is JSONL too).
   - Three deliberate divergences from the Go client, documented in the module
     header: dialing is a plain `connect` (a UDS connect does not block), the
     subscribe ack is read byte-wise (no look-ahead can swallow the first
     pushed event), and `Subscription` owns its reader so a partial frame
     survives the 60 s idle deadline (Go's `bufio` discards it).
   - Not an oracle surface (`herdrd` talks to a live server, no goldens):
     coverage is unit tests over a **real** `UnixListener` in a temp dir plus an
     `#[ignore]`d live round-trip (`ping` + `agent.list`).
4. **18D-4 ✅ done (2026-10-18)** — the scheduler,
   `host/src/crew/scheduler.rs`: the P13/P19 wave scheduler as *decisions only*
   (no herdr, no threads, no panes), so it is deterministic and unit-testable.
   The runner (18D-5) drives it wave by wave.
   - Ported 1:1: `NodeState`/`Status` (the Go strings, as an enum so a terminal
     set cannot drift), `PhaseState`, `EdgeState` (`visited`/`ok`/collected
     skips), `fire_edge` (re-arm a settled target; `max_visits` cap → skip),
     `miss_edge` (permanently skip a still-pending target), `node_ready`
     (fan-in AND, back edges ignored for entry, phase barrier), `phase_unlocked`
     / `emit_phase_starts` / `update_phase_progress` / `current_phase`,
     `run_due_checks` (visit-keyed `check:<i>#<visits>`, a later pass clears
     `_failed`, `check_skip` for blocked/skipped after-nodes, `check_fail` only
     for a failed one), `settle_unreachable` (pending → skipped, blocked keeps
     tab+worktree), `snapshot` value copies, `edge_satisfied`
     (idle/regex/verdict) with the Go `compileCached` regex cache, and
     `settle_edges` (the wave loop's inner pass, including the exact
     `when "re:X" not matched on a output` skip text).
   - `run_check(shell, dir)` executes a check with the Go engine's safety
     properties: a 5-minute budget, **own process group** + group SIGKILL so
     grandchildren cannot hold the pipes past the deadline (P13 BUG-08), a
     timeout reported as a failure even when the direct child exited 0 (Go's
     `WaitDelay`), a 240-byte tail, and `exit status N` / `signal: N` text.
   - Events are **collected** (`emit` timestamps + queues; `drain_events`), not
     streamed: the run log / status bridge / TUI are 18D-5's sinks. Timestamps
     are RFC3339 UTC millis (Go's `time.Time` JSON shape) from a dependency-free
     civil-from-days formatter; `run_id` is Go's `20060102-150405` UTC stamp.
   - `Crew` gained `in_edges`/`phase_of` (the Go `incoming` + `PhaseOf`).
   - Tests: the Go scheduler suite ported one-for-one (edge forms, verdicts,
     the phase barrier incl. blocked-holds and skipped-releases, re-arm +
     `max_visits`, the uncapped loop body, the capped guard, fan-in AND,
     miss-skips, cycle-body readiness, per-visit checks) plus a test-only
     `drive()` that runs the Go wave loop to quiescence — proving a
     reviewer loop converges LBTM→LBTM→LGTM in 7 dispatches and that a phase
     barrier actually delays the next wave.
5. **18D-5** — the runner: pane dispatch, prompt+settle (`agent.prompt.wait`, the
   reasoning-model empty-settle guard), templates (`{{ .upstream }}`,
   `{{ node }}`, `{{ artifact }}`, `{{ memory }}`), run log, memory/bus.
   Design note (2026-10-18, user direction): pi should also be launchable
   **inside ttt** (S38), so the runner should sit behind a `PaneBackend` trait
   (spawn/prompt/prompt-wait/read/focus/status) with the herdr impl first —
   the Go engine hardcodes `*herdrd.Client`, and that is the one place a Rust
   port can improve on it without diverging.
6. **18D-6** — the TUI and an `--engine go|rust` switch so a live run can be
   compared side by side; the Go engine retires only once the Rust engine passes
   the smoke gate unsupervised.

### Gates (18D-1)
- `cargo test` in `host/` — 14 unit tests + the oracle integration test
  (`host/tests/crew_oracle.rs`) replaying all **60 goldens** green; `cargo clippy
  --all-targets` and `cargo fmt --check` clean. (The corpus has since grown to 85
  and the unit suite to 27 — see 18D-2 below.)
- `scripts/crew-host-oracle.py --check` → `goldens current (60 records)`, i.e. the
  committed goldens equal a fresh capture from the Go engine.
- The Go side is unchanged and green: `gofmt -l .` empty, `go test ./...` in
  `crew/` ok; `forseti-crew validate -f crew/examples/crew-routes.yaml` still
  dry-runs the routes against the real `forseti-host` (RouterHost) — verified by
  pointing `FORSETI_CREW_ROUTER_BIN` at a failing stub, which turns the run into
  `INVALID (routes): forseti-host --dry-run: …`.
- `scripts/crew-smoke.sh` runs `crew-host-oracle.py --check` before the live leg.
- Spot check (8 base examples): `forseti-host crew validate -f FILE --explain`
  is byte-identical to `forseti-crew validate -f FILE --explain`, including the
  `← <from> [when]` arrow lines and every default (`kind: pi`, `when: idle`,
  check names, memory recall_k/write_max_chars, route picker/confidence).

### Gates (18D-2)
- `cargo test` in `host/` — **28 tests** (27 unit + the oracle): `role` (7:
  prefix scaffold, tier resolution + subscriptions, explicit-model-wins,
  tierless-crew rejection, unknown-role list, watch/writes-without-role, the
  error strings), `pattern` (6: reviewer-loop synthesis + back edge, panel +
  executor roster/checks, `then:`→entry-nodes, phase join, declared-member reuse,
  `criteria` rendering), `bus` (the 11-row Go `TestParseVerdict` table), plus the
  oracle replay of **85 goldens** including `prompts` name+prompt parity.
- `scripts/crew-host-oracle.py --check` → `goldens current (85 records)`.
- Corpus additions: `crew/examples/crew-sop.yaml` (the four-pattern SOP crew) +
  valid `sop-roles.yaml` / `sop-patterns.yaml` + 22 invalid role/pattern
  one-rule-per-file fixtures.
- `cargo fmt --check` / `cargo clippy --all-targets` clean; `gofmt -l .` empty;
  `go vet ./...` + `go test ./...` green in `crew/` (the new `prompts`
  subcommand included).
- Mutation check: a one-word change to a synthesized prompt in `pattern.rs`
  fails two goldens (`examples/crew-sop`, `valid/sop-patterns`) — the prompt
  leg is not vacuous.

### Gates (18D-3)
- `cargo test` in `host/` — **40 unit tests + 1 oracle** (+1 ignored): the 13
  new `herdr` tests run the client against a real `UnixListener` in the temp
  dir and assert NDJSON framing, the per-call id shape
  (`crew-agent.list-1`, `…-2`), a mismatched reply id rejected,
  `{"error":…}` → `ApiError` (`"agent_blocked: waiting"`), `agent.prompt.wait`
  → `"blocked"` / the event-status frame / the `{"type":"agent_prompted"}`
  frame / bare `wait_matched` → `idle` / an unrecognised shape is an error,
  `agent.read`'s `read.text` nesting, `agent.list` field decoding, the
  `not present (server running?)` stat message, `socket_path_for` naming the
  session dir, a subscription (ack → two events → close), a rejected ack, and
  `events_lost` terminating the subscription.
- **Live**: `cargo test --lib -- --ignored` runs the ignored round-trip against
  the running herdr server on this box — `ping` + `agent.list` green (the wire
  is right, not just the fake).
- `cargo fmt --check` and `cargo clippy --all-targets` clean.

### Gates (18D-4)
- `cargo test` in `host/` — **59 unit tests + 1 oracle** (+1 ignored): the 19
  new `scheduler` tests cover the Go `runner_test.go` scheduler suite
  one-for-one (`TestEdgeSatisfied`, `TestEdgeSatisfiedVerdict`,
  `TestPhaseUnlocked`, `TestPhaseProgressEvents`,
  `TestFireEdgeReArmsSettledTarget`, `TestUncappedEdgeRefiresLoopBody`,
  `TestCappedEdgeStillStops`, `TestNodeReadyFanInAND`,
  `TestNodeReadyCycleBodyWaitsForItsGate`,
  `TestChecksRerunWhenAfterNodeIsRevisited`) plus the blocked/failed
  after-node check paths, the skip text, RFC3339/civil-date helpers, the
  unreachable sweep, and the two end-to-end `drive()` runs.
- `run_check` is exercised for real (`true`, `exit 3` + stderr tail) and for
  the BUG-08 property: `sh -c "sleep 30 & exit 0"` returns a timeout failure in
  under 10 s because the group kill takes the grandchild that holds the pipes.
- `cargo fmt --check` / `cargo clippy --all-targets` clean; the oracle is
  untouched (85 records) — the scheduler is not a validate surface.

## Phase 19 — roles + SOP patterns: MetaGPT-style archetypes and declarative multi-agent flows — ✅ **implemented + verified end to end LIVE** (2026-10-17)

Direction (user): move the crew layer from ad-hoc prompts to MetaGPT-style
roles/actions/SOPs — a **role archetype library** plus **declarative
multi-agent patterns** (reviewer loop, security review, adversarial panel +
judge, executor) compiled into the existing verified graph. MetaGPT (MIT) is
inspiration/spec + attribution only; the SOP text is forseti's own and nothing
is vendored. `opsctl` was dropped at the user's request. S31 records the
source-verified MetaGPT contracts.

Design rule: **patterns compile, they do not interpret.** Every pattern expands
to ordinary agents/edges at `Validate`, so the P13 scheduler (fan-in AND,
bounded back edges, `max_visits`), phases, checks, run log, watch TUI and the
Phase 18 routing layer are reused untouched. A crew with no `roles:`/`patterns:`
is byte-for-byte the old behavior.

### 19A — role archetypes ✅
- `schema/role.go`: `Action`/`Role` + `Role.Prefix(name)` (the MetaGPT prefix
  template, verbatim). Built-ins: `planner`, `researcher` (BY_ORDER search →
  gather → synthesize), `engineer`, `reviewer` (lgtm|lbtm),
  `security_reviewer` (pass|fail), `adversarial_reviewer`, `judge`
  (accept|revise), `verifier` (pass|fail). Crew `roles:` re-declares a name to
  override/add.
- Tier indirection: a crew-level `models: {efficient, capable}` map; an
  archetype names a tier, never a model id (model ids churn — see AGENTS.md).
  Explicit `model:`/`route:` on the node wins; `tier` without `models:` fails
  validation.
- The runner prepends `RolePrompt(node, prompt)` at dispatch (role-less nodes
  unchanged); a role `instruction` fills an empty prompt.

### 19B — verdicts ✅
- `schema/bus.go`: `ParseVerdict` (LAST `VERDICT: <label>` line wins — panes
  echo the prompt; bare `LGTM`/`LBTM` accepted) and the `verdict:<label>` edge
  gate in the runner's `edgeSatisfied`.
- `node_done` emits a `verdict` event when a node's output carries one; watch
  renders it per row (✓/✗).

### 19C — patterns ✅
- `schema/pattern.go`: `reviewer_loop`, `security_review`, `adversarial_panel`
  (`critics` 2–7 × `lenses` → judge), `executor` (implementer → verifier + real
  shell checks, `max_fixes`). Params validated per kind; `then:` chains
  pattern-to-pattern (or to a node) — a pattern id resolves to that pattern's
  ENTRY nodes (every critic of a panel, the implementer of an executor), so the
  next stage's entries all wait on the verdict edge (fan-in AND); `members:`
  reuses declared nodes; a synthesized node joins its subject's phase.
- `criteria:` (added 2026-10-17 after a live non-convergence, S33c): every
  verdict-gated pattern accepts an acceptance standard that renders into the
  reviewer/critic/judge prompt. Without it a generic "list defects" reviewer
  always finds something, the bounded loop exhausts its revisions, and the
  positive-verdict edge skips every downstream stage — observed live, twice.
  `pattern_test.go: TestPatternCriteriaLandsInEveryVerdictPrompt`.
- `validate -explain` renders the resolved graph; `crew/examples/crew-sop.yaml`
  exercises all four patterns end to end (validate-clean). The example names a
  concrete request on its entry node — a `plan` node with no prompt leaves the
  planner archetype's "write the plan for the request below" pointing at
  nothing, and the live run then produced model-invented work (S32) — and its
  executor `checks:` are shell commands runnable from the **crew cwd** (the
  repo root, or the worktree checkout with `--worktree`), which is where checks
  run; `go build ./...` could only FAIL there (this repo's Go module is
  `crew/`).
- **Checks re-run per visit** (2026-10-17, fixed the P13-era first-settle-only
  behavior): the done-flag is keyed on the after-node's visit
  (`check:<i>#<visits>`) and a passing re-run clears the `_failed` marker, so an
  executor `max_fixes` round re-runs its shell checks and the exit code follows
  the LATEST iteration. Same-visit re-checks stay no-ops.
  `runner_test.go: TestChecksRerunWhenAfterNodeIsRevisited`.

### 19D — typed pub/sub ✅
- `watch:`/`writes:` derive `producer → watcher` edges; `{{ artifact "type" }}`
  resolves the latest published artifact (in-run producer, else
  `.forseti/bus/typed/<type>.md`). Missing producer = validation error.

### 19E — source-form serialization ✅
- `Crew.MarshalSource()` writes the SOURCE (declared agents/edges, patterns,
  roles) and drops everything `Validate` synthesized — pattern nodes/edges, the
  materialised checks, pub/sub-derived edges, and the values a role filled
  (tier model, writes, watch). `Load(MarshalSource())` is a fixed point of the
  resolved graph, so a TUI save/run of a role/pattern crew never forges a
  second copy of the synthesized nodes. `saveCrew` and `startRun` use it;
  `marshal_test.go` pins the round trip.

### 19F — prompt binding for synthesized prompts ✅
- The FIRST live SOP run failed at `rev-reviewer` before any model call:
  `prompt template: template: rev-reviewer:5: function "design" not defined`
  (S32). Three findings:
  1. a dot-less action is a FUNCTION call, so the generated reference for an
     upstream output must be `{{ .name }}` (the upstream map is keyed by node
     name) — the generated prompts were missing the dot;
  2. hyphens make field access unwritable (`{{ .ap-critic-1 }}` = `bad character
     U+002D`), and EVERY synthesized node is `<pattern>-<part>`;
  3. the panel's judge renders the artifact under judgement but edges only from
     the critics, so the subject is not an incoming edge — there was nothing to
     bind even with a valid name.
- Fix: a runner template function `{{ node "name" }}` (string-typed name → any
  node's latest text, `""` until it finishes), used by all six generated
  references. `{{ .<node> }}` (identifier-safe names) and
  `{{ artifact "<type>" }}` are unchanged.
- Guard: `schema.TestPatternPromptsRender` parses+renders every node of a crew
  with all four patterns under the runner's binding rules, and `pattern_test.go`
  asserts the function form — the smoke gate was green throughout, because
  template PARSE happens at dispatch (`runNode`), not at `Validate`.

### 19G — TUI authoring of roles/patterns ✅ (2026-10-17)
- Build mode gains `l` (role form: name/profile/goal/constraints/tier/
  instruction/writes) and `g` (pattern form: id/kind/subject/criteria/then);
  the agent form gains a fifth `role` field. `buildRole`/`buildPattern` are
  pure submit validators (name rules, the kind/tier enums, profile+goal
  required, duplicate handling: re-declaring a role name OVERRIDES it, a
  duplicate pattern id is refused), so the rules are unit-tested
  (`cmd/forseti-crew/tui_test.go`: form opening, an end-to-end
  walk-and-submit through `handleKey`, the build-panel rendering).
- Only the common dials are fields; `members`, `lenses`, `critics`,
  `max_revisions`, `max_fixes`, `checks` stay YAML-only by design (a pattern's
  `params` is open-ended).
- Saving/`r`-running a crew edited this way goes through `MarshalSource`
  (19E), so a pattern added to an already-expanded crew re-expands exactly
  once — the round-trip test and the live drive both assert no synthesized
  node is ever written.
- Live verification (S33e): `scripts/crew-tui-drive-roles.py` drives both forms
  + the role field in a REAL herdr sandbox pane and asserts on the saved file
  (`roles:`, `patterns:`, `role: architect`, no `rev-reviewer`) plus
  `validate -f` exit 0. One integration rule came out of the drive: readiness
  must key on TUI-only output (the crew name also appears in the shell's echo of
  the command line, and a TUI drops keystrokes queued before it installs its
  input reader). `pane.send_text`/`send_keys` were probed and do NOT need a
  focused pane (the first hypothesis, retracted in S33e).

### Engine refinement (needed by the loops) ✅
- `BackEdges` is now **DFS-classified** from the entry nodes (plus an explicit
  `loop: true` marker for ambiguous cycles) instead of "target reaches source".
  The old rule marked every edge of a cycle, so a loop body dispatched at t0;
  `x→a→b→a` now runs a → b → a. The no-entry rejection is preserved for
  all-cycle crews. See S31 + `schema/cycle_test.go`.
- **Edge firing cap**: `max_visits` unset now means UNCAPPED — an edge fires
  every time its source settles (before, the implicit cap was 1, so only the
  loop-back edge could re-fire and a multi-edge loop body stalled after one
  pass, never re-running the reviewer). Termination still holds because schema
  validation requires every cycle to carry at least one `max_visits` edge and
  a re-dispatch only happens when an edge fires. Acyclic crews are unaffected
  (a node settles once). `runner_test.go`: `TestUncappedEdgeRefiresLoopBody`,
  `TestCappedEdgeStillStops`.

### Gates
- `go test ./...` in `crew/` green (role/pattern/bus/cycle/verdict tests added).
- `forseti-crew validate -f crew/examples/crew-sop.yaml -explain` — the four
  patterns expand, one ENTRY node, verdict gates present.
- `scripts/crew-smoke.sh` asserts the expansion (no herdr needed for the
  validate leg).
- Live gate — **FULL live SOP run, all four patterns** (2026-10-17,
  `opencode-go/beam-free`, 45 s wall):
  `forseti-crew run --headless -f crew/examples/crew-sop.yaml --session sandbox
  --worktree sop-free` → `run_end done=10 cost=$0.0000` (exit 0). Every leg
  verified live: `plan` → `design` → `rev-reviewer` **LGTM** (its `verdict:lbtm`
  loop-back edge skipped, correct) → `sec-security_reviewer` **PASS** →
  three critics dispatched in parallel → `adv-judge` **accept** → executor
  `run-implement` → **both shell checks `check_pass`** → `run-verify`
  **PASS** (its fix edge skipped) → teardown removed the worktree. The
  criteria-less first attempt (design → `rev-reviewer` LBTM, revision → LBTM
  again, then the `verdict:lgtm` edge skipped and every downstream stage with
  it) is the S33c finding that produced `criteria:`.
  One `node_retry` live: `run-verify` settled with a negative pane delta and
  the extended empty-settle guard re-prompted once — the retry is what produced
  a clean verdict, so the guard is load-bearing on the free reasoning ids too.
  Run log `.forseti/runs/202610-112306…-crew-sop.jsonl` (10 `node_start`, all
  `"model":"opencode-go/beam-free"`; 4 `edge_skip` = the four correct
  verdict-edge non-firings; 2 `check_pass`; 1 `node_retry`).
- Live gate (**2026-10-17, current model**): `bash scripts/crew-smoke.sh` — PASS
  with NO override, on `opencode-go/beam-free` (the script defaults
  `FORSETI_CREW_MODEL` to the free id). Checked crew in the sandbox session
  (bus files, watch match, probe) + phased crew barrier held; run logs
  `.forseti/runs/202610-111455…demo-planner-builder-checked.jsonl` and
  `…111506…demo-phased-planner-builder.jsonl`, every `node_start`
  `"model":"opencode-go/beam-free"`. Zero cost, no LAN dependency.
- Live gate (2026-10-16, LAN era): `FORSETI_CREW_MODEL=valhalla/valhalla-flash-next
  bash scripts/crew-smoke.sh` — PASS; run logs
  `.forseti/runs/202610-090415…demo-planner-builder-checked.jsonl` and
  `…090554…demo-phased-planner-builder.jsonl`.
- SOP live run — **PARTIAL** (2026-10-16):
  `forseti-crew run -f crew/examples/crew-sop.yaml --session sandbox
  --worktree sop-demo`. Verified live: the graph expanded at run time (10
  agents / 20 edges, every dispatch on valhalla) and the reviewer loop ran for
  real — `rev-reviewer` settled `VERDICT: LBTM`, its `verdict:lgtm` edge to
  `sec-security_reviewer` was **skipped**, and `design` re-dispatched twice:
  the P19 back-edge + uncapped-edge behavior exercised against a live model
  (unit-tested only before this). NOT verified live: the security/panel/
  executor legs. The run was interrupted on purpose while the LAN box was
  loaded by unrelated clustering tests (a `design` settle took ~2.5 min, the
  reviewer's on a ~52k-char subject ~7 min). Also the first attempts died
  before any model call on the S32 template bug and on a 4-minute node timeout
  (`agent.prompt+wait: timed out waiting for agent status`), which is how the
  example's missing request and its unrunnable `checks:` were found.
