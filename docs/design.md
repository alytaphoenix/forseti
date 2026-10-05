# Forseti — Design

Forseti ties together three locally installed terminal tools:

- **herdr** 0.9.3 — terminal workspace manager (workspaces/tabs/panes, agent lifecycle)
- **pi** (`@earendil-works/pi-coding-agent` 0.99.1) — coding agent CLI
- **ttt** (`github.com/eugenioenko/ttt` v1.6.0) — terminal IDE

Decisions (2026-09-30, user-confirmed):

1. **Scope: full suite, phased.** Phase 1 coordinated bring-up; Phase 2 bidirectional
   ttt↔pi context flow; Phase 3 status sync and polish.
2. **Architecture: native plugins per host.** No daemon. Three small components, each
   on its host's native extension surface, talking through the hosts' existing
   control channels (herdr CLI/socket API, ttt `--listen` HTTP, pi extension API).
3. **Bring-up produces one cohesive unit**: a dedicated herdr tab containing ttt and a
   pi agent pane. Bring-up is idempotent (re-run focuses the existing pair).
4. **Jump handoff**: pi signals "look here" by writing a small state file and invoking
   the ttt palette command `Forseti: Jump` over ttt's exec channel; a keystroke-chain
   is the zero-code fallback. ttt's exec vocabulary has no native `open file:line`.
5. **Follow mode is off by default** (it can steal editor focus mid-typing).

Verified tool facts live in [`AGENTS.md`](../AGENTS.md) and [`spikes.md`](spikes.md);
this doc records design decisions only.

## Non-goals (v1)

- ~~No new long-running daemon or orchestrator process.~~ **Reversed for Phase 5**
  (2026-09-30): the crew runner is a user-invoked process that lives for a
  build/run session — still no always-on daemon.
- ~~No custom UI outside host surfaces (no separate forseti TUI/web).~~ **Reversed
  for Phase 5** (2026-09-30): the crew TUI is a custom UI, but it runs inside a
  herdr pane — herdr remains the host surface.
- No LLM/provider work: pi's providers, auth, and models stay pi's own.
- No remote/SSH (`herdr --machine`) support — local single-machine only.
- No reimplementation of herdr's agent tracking (pi is a native herdr agent kind).
- No auth on ttt's control port (documented trust boundary instead).

## Architecture

```
          herdr server (socket: ~/.config/herdr/herdr.sock)
          ┌───────────────────────────────────────────────────┐
          │ dedicated "forseti" tab                           │
          │  ├─ root pane:  ttt --listen  (exec HTTP :4242)    │
          │  └─ split pane: pi   ← native herdr agent kind     │
          └───────────────────────────────────────────────────┘
   ttt Lua plugin "forseti"                    pi TS extension "forseti"
   ├─ ask: buffer/selection → herdr agent      ├─ /ttt jump <path[:line]> — write
   │   prompt <agent> --wait                    │  FORSETI_JUMP_FILE + POST /exec
   ├─ status sidebar: poll agent list          │  "exec \"Forseti: Jump\""
   └─ Forseti: Jump: read FORSETI_JUMP_FILE    ├─ /ttt follow on|off — tool_result
      → open_tab + cursor + hunk highlight     │  hook → jump push (off by default)
                                               └─ /herd … — read-only herdr CLI pass-through
```

| Component | Host surface | Language | Responsibility |
|---|---|---|---|
| `herdr-plugin/` | `herdr-plugin.toml` `[[actions]]` | sh | Bring-up: dedicated tab, ttt `--listen` in root pane, pi via `agent start --kind pi` |
| `ttt-plugin/` | `plugin.ttt.json` + Lua (`ttt.register`) | Lua 5.1 | Editor side: ask, status sidebar, `Forseti: Jump` |
| `pi-extension/` | default factory module (`ExtensionAPI`) | TypeScript | Agent side: `/ttt` commands, follow mode, `/herd` reads |

Dev/install (all verified): herdr `herdr plugin link <abs path>` (later `herdr plugin
install`); ttt copy/symlink into `~/.config/ttt/plugins/forseti/` + approval dialog +
**Plugins: Reload**; pi `pi --extension ./pi-extension/index.ts` for dev or
`pi install ./pi-extension -l` (needs `-a` to trust project-local files).

## Verified control surfaces (design-relevant)

- **herdr plugin contract** (from ttt's shipped plugin, spike S1): context arrives as
  `HERDR_PLUGIN_CONTEXT_JSON` (resolve `checkout_path` → `focused_pane_cwd` →
  `workspace_cwd`), `HERDR_BIN_PATH` names the herdr binary, actions run **headless**
  (never exec a TUI directly), and herdr spawns plugin commands from the plugin root.
  Forseti's action only spawns `herdr` CLI subcommands, so headless is safe.
- **herdr agent surface**: `agent start <name> --kind pi --pane <id>` launches and
  readies pi itself (pane must sit at an interactive shell prompt; readiness ≈ 30 s
  default). State machine `idle|working|blocked|done|unknown`; `agent prompt … --wait`
  returns on first settled state; names `[a-z][a-z0-9_-]{0,31}`, unique among live
  agents. Server errors: JSON on stderr, exit 1; syntax errors exit 2.
- **ttt exec script** (spike S2, from `internal/app/exec_script.go`): vocabulary is
  `click|rclick|hover|drag`, `key`, `type`, `paste`, `copy`, `exec "Palette Command"`,
  `screenshot`, `debug`, `wait`, `wait-for TEXT`, `panel ID`, `quit|shutdown`. No
  `open file`. `--listen` binds the hardcoded const `127.0.0.1:4242` and is declared a
  *"single-operator debug tool, not a public API"* in ttt source → exactly one
  forseti-enabled ttt per machine; unauthenticated local control is accepted for v1.
- **ttt Lua plugin**: sandboxed Lua 5.1, permission-gated; `ttt.set_interval(ms, fn)`
  runs on the main loop (min 50 ms, auto-cleared on reload — no permission needed);
  `ttt.open_tab(path)` needs `panel.editor`; `editor.set_cursor/set_selection` need
  `editor.write`; context build needs `editor.read`; `fs.read` for the jump file;
  `system.exec: ["herdr"]` scopes exec to herdr only.
- **pi extension**: default-export factory receiving `ExtensionAPI`, TS loaded via jiti
  (no build step). Factory must not start processes/sockets/timers — start/stop in
  `session_start`/`session_shutdown`. `pi.on("tool_call"|"tool_result")` carry
  `toolName` + input; edit tool inputs contain path + old/new strings.

## Component designs

### 1. herdr-plugin — bring-up (Phase 1)

Manifest: id `forseti`, `min_herdr_version = "0.7.0"`, platforms linux/macos, one
`[[actions]] open` (contexts `workspace`) run by `sh scripts/open.sh`. No `[[panes]]`:
the action drives everything through `herdr` CLI subcommands (headless-safe).

`open.sh` algorithm:

1. Resolve target dir from `HERDR_PLUGIN_CONTEXT_JSON` (ttt.editor's order).
2. **Idempotency**: `herdr agent list --json`; if the configured agent name (default
   `coder`) is live → `herdr agent focus <name>` and exit 0. Never double-spawn.
3. **Port probe**: if something answers on 4242 → launch ttt *without* `--listen`,
   warn, and rely on the Lua polling fallback for jumps (design keeps working).
4. `herdr tab create` → dedicated tab; ttt into `.result.tab.root_pane` via
   `herdr pane run <root_pane> "ttt --listen"` (new panes sit at a shell prompt).
5. `herdr pane split --pane <root_pane> --direction right --no-focus` → pi pane id
   from `.result.pane.pane_id`.
6. `herdr agent start coder --kind pi --pane <pi_pane>` — herdr waits for readiness.
7. Focus the new tab (command TBD-2) and print a JSON summary of created IDs.

Failure policy: never close or mutate user-created topology; on failure, leave partial
state and print the created tab/pane IDs in the error message.

### 2. ttt-plugin — editor side (Phase 2a)

Manifest `plugin.ttt.json` name `forseti`; permissions: `panel.sidebar`, `commands`,
`keybindings`, `editor.read`, `editor.write`, `panel.editor`, `fs.read`,
`system.exec: ["herdr"]`.

**Agent resolution (no hardcoded target):** the plugin runs
`herdr agent list --json`, filters `kind == "pi"`, and resolves the agent whose pane
lives in the current workspace (ttt's process env has `HERDR_PANE_ID`). Errors
distinguish "none live — run forseti.open" from "ambiguous — N agents in workspace".

Registers:

- `Forseti: Jump` (palette command) — reads `FORSETI_JUMP_FILE` (default
  `/tmp/forseti-<workspace-ish>.json`), a flat JSON `{path, line, end_line}`; opens the
  file via `ttt.open_tab`, `set_cursor` to `line`, and `set_selection(line → end_line)`
  to highlight the changed hunk. Written to work even without `--listen` (pi can't
  push, but the file handoff + a small poller still lets pi request jumps — solo-ttt
  setups).
- `forseti.ask` (keybinding `ctrl+k a`) — builds prompt from active buffer path +
  cursor + selection, then `herdr agent prompt <resolved> "<prompt>" --wait`.
- Sidebar panel `Forseti` — `set_interval(3000)` poll of `herdr agent list --json`,
  rendered as name/state (`idle|working|blocked|done|unknown`); `blocked` highlighted
  (an approval/question UI is showing). Only polls while data is wanted (panel visible
  check TBD-3); exec subprocess is synchronous on the main loop, so keep the cadence
  coarse and the command cheap.

### 3. pi-extension — agent side (Phase 2b)

TypeScript module respecting pi's lifecycle rules. Registers `/`-commands:

- `/ttt jump <path> [line] [end_line]` — write `FORSETI_JUMP_FILE`, then POST
  `exec "Forseti: Jump"` to `http://127.0.0.1:4242/exec` (fire-and-forget; 4242 closed
  → silent no-op with a one-time notice).
- `/ttt follow on|off` — default off. Hook `pi.on("tool_result")` for pi's edit/write
  tools; compute file + first-changed-line from the tool input (`old_string`/
  `new_string` diff for edits; line 1 + scan for writes), then emit the jump.
- `/ttt diff` — open ttt's working-tree diff view; exact palette command names are
  unverified (TBD-4), fall back to keystroke chain if needed.
- `/herd list` … — read-only herdr CLI pass-throughs (`agent list`, `pane list`)
  when `HERDR_ENV=1` (normal inside the forseti tab). No mutation commands in v1.

## Control & data flows

**Ask from editor (2a):** selection → Lua builds prompt → `herdr agent prompt … --wait`
→ herdr submits as one ordered paste-safe sequence to the pi pane → returns on first
settled state → panel re-renders.

**Jump / follow (2b):** pi tool_result → write jump file → `POST /exec "Forseti:
Jump"` → Lua opens tab, cursor + selection highlight. **Jump accuracy ladder**: file
level always; line level heuristic from edit args (v1); exact hunk highlight (auto
from jump file — already v1 since Lua does the selection); exact *byte* positions
only if a future pi event carries them.

**Status sync (3):** poll 3 s while panel active; if spike S4 finds a usable herdr
event subscription, switch to events.

## Error handling & safety

- Every wait carries `--timeout`. Timeout or `agent_prompt_stalled` ≠ "not delivered":
  re-read `agent get` before resubmitting; never blind-retry prompts.
- `blocked` is surfaced, never Auto-answered, by both Lua and TS components.
- Bring-up never closes user topology; `--no-focus` on splits; focus only the tab we
  create. Idempotent re-runs focus rather than duplicate.
- ttt Lua: if a permission is missing, the corresponding functions simply don't exist
  — degrade (hide the command, show a notice) instead of erroring.
- pi extension exec surface limited to the `herdr` binary + ttt localhost HTTP.
- Port-4242 trust: unauthenticated local control; v1 accepts it, README documents it;
  multiple forseti ttts are unsupported by design.

## Testing strategy

- **herdr plugin:** link into a disposable **named test session**
  (`herdr --session forseti-test`), invoke the action, assert DOM via
  `herdr agent list --json` / `pane list --json` (never sidebar order — parse JSON IDs).
- **ttt plugin:** drive with `ttt --exec "…; wait-for …; screenshot …; quit"` (invalid
  actions exit nonzero — useful as assertions); plugin testing guide in ttt docs.
- **pi extension:** `pi --extension ./pi-extension/index.ts` in a scratch session;
  `pi -p` smoke for follow mode edits.
- **Jump push smoke:** write jump file + `curl -X POST --data 'exec "Forseti: Jump"'
  http://127.0.0.1:4242/exec` → screenshot shows the file open with the hunk selected.

## Risks, upstreams, and unresolved items

| # | Item | Status / plan |
|---|---|---|
| S1 | herdr plugin context contract | **Resolved** (ttt.editor source): `HERDR_PLUGIN_CONTEXT_JSON`, `HERDR_BIN_PATH`, headless actions, plugin-root cwd trap |
| S2 | ttt exec vocabulary | **Resolved**: full list enumerated; no `open` command → palette-command + state-file design |
| S3 | `herdr agent start --kind pi -- <args>` passthrough | ✅ Resolved (2026-09-30): args pass verbatim; readiness timeout on non-interactive args is the expected signature |
| S4 | herdr event subscriptions vs polling | Optional; `herdr api schema --json` in Phase 0/3 |
| TBD-1 | Does `herdr tab create` accept a cwd/`--env`? | ✅ Yes — verified: `--cwd PATH --env KEY=VALUE --[no-]focus` |
| TBD-2 | Tab focus command name | ✅ Verified: `herdr tab focus <tab_id>` |
| TBD-3 | Is "sidebar panel currently visible" queryable from Lua? | Phase 2a verify |
| TBD-4 | Exact ttt palette command titles (diff view) | ✅ Verified from source: `Git: Open Changes` (`changes.openDiff`), `Git: Open Full Diff`, `Git: Next/Previous Changed Hunk` |
| R1 | ttt `--listen` is debug-grade; port/value pinned in source | Accepted v1; re-check after ttt upgrades |
| R2 | Port 4242 collision (second instance binds silently) | Designed: bring-up probes, drops `--listen` with warning |
| R3 | Trust friction: ttt approval dialog, pi `-a` | **Resolved live** — approval dialog verified end-to-end (screen-scraped coordinates via exec `click`), persisted in `plugins.ttt.json` |
| R4 | herdr/ttt/pi upgrades shifting surfaces | `min_herdr_version` pinned; smoke test after every upgrade |
| R5 | Follow mode stealing focus | Off by default; jump, not keystroke spam |
| U1 | Upstream suggestion: `open FILE[:LINE[:COL]]` exec command in ttt | File issue with ttt (maintainer already ships a herdr plugin) |

## Milestones

See [`implementation-plan.md`](implementation-plan.md). Summary: Phase 0 spikes +
skeleton → Phase 1 bring-up → 2a ttt plugin → 2b pi extension → Phase 3 sync/polish
+ shipping → **Phase 4: vault layer** (evergreen secondbrain in `~/forseti/`:
wikilinks `[[slug]]`, flat title-slug notes, daily logs; vault tools for pi, ttt
vault commands, `forseti.notes` bring-up — spec + task table in the plan) →
**Phase 5: crew layer** (below).

## Phase 5 — crew layer: agent graph builder + runner (design locked 2026-09-30)

A LangGraph/CrewAI-style layer over herdr's native agent surface. Decisions
(2026-09-30, user-confirmed):

1. **Orchestration: deterministic runner.** Fixed edges with explicit `when`
   conditions (agent status + optional output regex). LLMs do node work only —
   no LLM routing decisions.
2. **Builder: full interactive, form/list-based.** No drag canvas (terminal-
   inappropriate). `crew.yaml` is the source of truth; the builder reads/writes
   it with validation-on-save.
3. **Stack: Go + Bubble Tea.** Single binary `forseti-crew`; component dir
   `crew/`. Runner core is a headless Go package; the TUI is a view over it
   (`forseti-crew run --headless crew.yaml` is the smoke/CI path).
4. **Monitor pane is persistent** — split layout, live tail of the selected
   node's pane. Read-only: takeover happens in the real herdr pane via a
   focus keybinding, not by embedding an interactive terminal.
5. Lives in this repo as Phase 5; reverses two v1 non-goals (see above).

### Architecture

```
crew.yaml ──source of truth──▶ forseti-crew (Go binary, runs in its own herdr tab)
                                ├─ herdr socket client (protocol 22, id-correlated)
                                │    · agent.start / agent.prompt(wait) / agent.read
                                │    · events.subscribe → status + output events
                                ├─ runner core (pure state machine, no TUI deps)
                                │    · graph walk, conditions, templating, timeouts
                                │    · JSONL run log → .forseti/runs/<ts>.jsonl
                                └─ Bubble Tea front-end (builder + monitor)
```

ttt is **not** required — crew needs herdr + pi only. Crew members are ordinary pi
agents; each may independently carry the forseti pi extension (jump/review/follow
keep working inside crew runs).

### crew.yaml schema v1 (as built)

```yaml
name: demo-planner-coder
agents:
  - name: planner                      # herdr rule [a-z][a-z0-9_-]{0,31}
    kind: pi                           # v1: pi only (default)
    model: opencode-go/glm-5.3-flash   # per-node model → pi --model (S9 verified)
    args: []                           # extra pi argv appended after --model
    prompt: |
      You are the planner. ... End with PLAN_READY.
  - name: builder
    model: opencode-go/glm-5.3-flash
    prompt: |
      Execute this plan exactly:
      {{ .planner }}                   # upstream output templated by node name
edges:
  - { from: planner, to: builder, when: "re:PLAN_READY" }   # gate on output
  # when: idle (default) | re:<regex>; self-loops/cycles rejected unless an
  # edge in the cycle carries max_visits (validator enforces)
```

- Entry nodes are **derived** (no incoming edges) — no explicit `entry:` key.
- Output flow: `{{ .<upstream> }}` templating; every captured output is also
  written to a bus file (`.forseti/bus/<node>.md`) — pi reads files natively;
  big outputs never go through paste.
- Validation (all enforced in `internal/schema`): name rule, duplicate names,
  edge endpoints exist, `when` is a status or compilable `re:`, cycles require
  a `max_visits`-bounded edge.

### Runner semantics (as built, live-verified 2026-09-30)

- **Bring-up**: dedicated crew tab via one `layout.apply` BSP call (S8);
  one pane per node; `agent.start --kind pi` concurrently per node
  (readiness ≈ 30 s each). Never touches user topology; teardown closes only
  the crew tab it created (verified — tab auto-closed after run).
- **Dispatch**: `agent.prompt` **with `wait {until: [idle,done,blocked],
  timeout_ms}`** — one request, race-free. (Live catch 2026-09-30: a separate
  `wait idle` after a prompt matches the *pre-prompt* idle and captures the
  startup screen instead of the answer; and an unfocused agent settles to
  `done`, not `idle` — both statuses must be in `until`.)
- **Capture**: `agent.read recent_unwrapped` at node settle → node `output`
  + bus file + run log.
- **Events**: `pane.agent_status_changed` subscription drives runner/TUI state.
  **No output push exists in 0.9.3** (S7: `events.wait` is status-only;
  `pane_output_changed` unsubscribable) — the monitor tail is a debounced
  `agent.read` (700 ms) against the visible target only.
- **Human-in-the-loop**: `blocked` surfaces in the TUI + event log and is
  never auto-answered.
- **E2E proof**: `crew/examples/crew.yaml` planner→builder run created
  `HELLO_CREW.md` ("crew pipeline works") through the `re:PLAN_READY` edge;
  gate: `scripts/crew-smoke.sh`.

### TUI layout

```
┌─────────────────────────────────┬──────────────────────────────┐
│ GRAPH / BUILDER                 │ MONITOR PANE (persistent)    │
│  ● planner   done               │ tabs: output │ status        │
│  └──▶ ● coder working ░░        │ live tail of selected node   │
│         └▶ ● reviewer idle      │ node meta: model, state,     │
│ [a]dd agent  [e]dge  [r]un      │ elapsed, dispatch history    │
├─────────────────────────────────┴──────────────────────────────┤
│ EVENT LOG  12:04:31 prompt→coder   12:04:02 planner settled    │
└────────────────────────────────────────────────────────────────┘
```

Builder screens (left side): graph view, agent CRUD forms (name/kind/args/
prompt), edge forms (from/to/when), adopt-live-agent import (turn an already
running herdr agent into a node). Save validates and writes `crew.yaml`.

### Boundaries (Phase 5)

- One active run per machine in v1 (herdr socket is per-user anyway).
- Deterministic routing only — an LLM-routed conditional node type is an
  explicit future item, not v1.
- ttt optional; no ttt plugin changes required for crew v1.
- Per-agent model/provider config goes through pi's own args (S3 passthrough) —
  forseti still implements no provider logic.

## Phase 6 — observability + agent sandbox/E2E (as-built 2026-10-01)

Two goals: crew runs visible from everywhere (TUI, run log, herdr sidebar, ttt
status bar), and hermetic cheap E2E runs of whole agent graphs. Spikes S11–S15
carried the unknowns; all resolved live (`docs/spikes.md`).

### E2E harness — the crew file is the test

- **`check` nodes (6A-1)**: `checks: [{after: <node>, run: "<shell>"}]`. The
  runner executes each after its after-node settles (any settle counts; a
  non-done node makes the check fail), `sh -c` in the crew cwd, 5 min cap.
  `check_pass`/`check_fail` events land in the run log; any failure flips the
  process exit code (headless and TUI paths both). `crew-smoke.sh` now runs the
  **checked crew** — the gate and the harness are the same artifact.
- **`--session <name>` (6A-2)**: the run targets a named herdr session socket
  (`~/.config/herdr/sessions/<name>/herdr.sock`). Named servers are NOT
  auto-started by the CLI and attach needs a TTY + passes only when nesting
  detection (inherited `HERDR_*` env) is cleared — so the runner bootstraps:
  temp pane in the live session runs `env -u HERDR_ENV -u HERDR_PANE_ID
  -u HERDR_SOCKET_PATH … herdr --session <name>`, poll socket, close pane
  (server persists — `detached_server_daemon`). From then on the run is fully
  hermetic: tabs, agents, waits, views all inside the sandbox session.
- **`--worktree <branch>` (6A-4)**: `worktree.create` before tab build → all
  node panes, bus files, and checks run in the worktree checkout (herdr picks
  `~/.herdr/worktrees/<repo>/<branch>`); removed at teardown unless
  `--keep-worktree`. Hardened against the two live failure modes: leftover
  husks under `.herdr/worktrees` (sweep + `worktree prune` + retry) and branch
  deletion racing worktree registration (prune + retry). Nodes get `pi -a`
  because a disposable dir hits pi's "Trust project folder?" dialog, which
  otherwise stalls every prompt (verified: dialog observed, `-a` skips it).
- **Halogen discipline (6A-3)**: example crews + gates pin the free LAN model;
  glm stays the interactive path. Empty settle → one retry (6A-3 guard), then
  surface.

### Observability

- **`forseti-crew watch` (6B-1)**: post-hoc/following renderer for
  `.forseti/runs/<run>.jsonl` — node table (status glyph, duration, cost),
  check results, summary. Pure file reader; no herdr dependency.
- **Status stream (6B-2)**: one `events.subscribe` with a
  `pane.agent_status_changed` entry per crew pane (schema: one pane_id per
  entry — a list is rejected) → `node_status` events + instant `LiveStatus`
  badges in the TUI.
- **Blocked alerts (6B-3)**: on the stream, `blocked` → `notification.show
  --sound request` (best-effort: this setup reports `disabled`, confirmed
  live) + the badge below carries the real signal.
- **Sidebar projection (6B-4)**: `agent.view.set` with source `crew:<name>`,
  filter `pane_id in <crew panes>`, sort attention desc; cleared at teardown.
  UI-only (S10).
- **ttt bridge (6B-5)**: runner writes `.forseti/crew-status.json` on every
  event, removes it at teardown; ttt plugin polls it (2 s) and renders
  `crew <name> <done>/<total> ●|!|✗` in the status bar. `open.sh` now writes
  `repo.json` into the plugin dir so the Lua side knows the repo root (fs
  sandbox has no env; lazy first-tick read because `ttt.fs` wires after plugin
  load).
- **Cost capture (6C-3)**: on settle, parse pi's status footer
  (`$x.xx` + `y.y%/ctx`) — last match wins. Node records carry
  `cost_usd`/`ctx_pct`; summary totals. Halogen reads $0.0000.
- **Meta tab (6C-2)**: `tab` in the TUI flips the right pane to node meta
  (pane id, resolved model, status + herdr status, started/duration, visits,
  retries, output size, cost/ctx).
- **Watchers (6C-1)**: `watch: [{node, match}]` arms `pane.wait_for_output`
  (S12: matches past scrollback → emitted-line dedupe) while the node runs;
  advisory `pattern_matched` events only.

### Switchyard routing (P6-D)

`routes: [{id, type: stage_router, efficient, capable, picker, confidence}]`;
agents attach with `route: <id>` (mutually exclusive with `model:`). The
runner, only when routes exist:

1. Generates `switchyard.toml` (schema_version 1, stage_router routes) into the
   run dir. Upstream `base_url`s resolve from `~/.pi/agent/models.json`
   (forseti's provider/model strings live there); API keys resolve the same
   way (`!cmd` executed) and pass ONLY as `SWITCHYARD_KEY_<PROVIDER>` env vars
   to the server process — never in the TOML.
2. Spawns `switchyard-server` (bin: `FORSETI_SWITCHYARD_BIN` →
   `~/.cargo/bin` → PATH) on a free port with `--routing-log-file` inside the
   run dir; waits for `GET /health`.
3. Materializes the `switchyard` provider entry in pi's `models.json`
   (one model entry per route id, `openai-completions`, compat block per
   upstream pi doc), restoring the previous bytes at teardown.
4. Route-attached nodes start as `pi --model switchyard/<route-id>`; settle
   logic unchanged.
5. Tails the routing JSONL → `route_decision` events; `/v1/stats` counters are
   available for run-level per-model tallies, and `run_end` carries the
   per-model request tally (e.g. `models: glm-5.3-flash=2
   halogen-qwen3.8-flash-next=0 (errors=2)`).

Resilience (verified live during a halogen outage): when an upstream is
unreachable, switchyard falls back to the other tier
(`fallback_reason: "unavailable"` in the routing log) — a routed crew keeps
running while a direct-model crew on the same dead model fails at pi's
connection-error path. Known blind spot: routed nodes report the ROUTE's
declared cost in pi's footer ($0.0000), not the actual upstream's; the
`route_decision` tokens + per-model tally are the truthful cost view for
routed runs.

Version drift note (S14/S15): server 0.2.0 has no `auto` route type (that's the
0.3.0 preset == stage_router efficient_first 0.5) and reads the session header
`proxy_x_session_id`, not `x-session-id`; pi's session-affinity header therefore
doesn't attach in 0.2.0 (routing still works; affinity is effectively
per-request). Pin the version; the `routes:` abstraction is the swap point for
a future libsy sidecar.

### Boundaries (Phase 6)

- Edges remain deterministic; switchyard routes only model calls within nodes.
- No daemon: watch and run are user-invoked processes.
- Sandbox sessions are never stopped by the runner (only bootstrapped);
  `herdr session stop` remains manual.
- One active run per session in v1; `switchyard.toml`/`server.log` are per-run
  overwritten, routing JSONL is per-run unique.

## Phase 7 — lazygit integration (as-built 2026-10-01)

Git becomes a pane app like everything else. lazygit 0.65.1 (brew) is the
human surface for the changes agents write; it can dispatch back into the
live ttt and pi. Spikes S16/S17; full scope P7-1…P7-5.

### Bring-up: `forseti.git` action (P7-1)

`herdr-plugin/scripts/git.sh`, mirroring `open.sh`'s contract:

- resolves the target checkout from `HERDR_PLUGIN_CONTEXT_JSON`
  (checkout_path → focused_pane_cwd → workspace_cwd; `TTT_TARGET_DIR` override)
- requires the forseti tab (label `FORSETI_EDITOR_LABEL`, default `forseti`)
- **idempotency keys off `pane.process_info`**: a pane whose foreground
  process is `lazygit` IS the git pane → focus it (a user-quit lazygit leaves
  a shell, so re-invoking relaunches safely). Only shell panes are hostable —
  a running ttt/pi is never typed into; when no shell pane exists a fresh
  pane is split off the tab's first pane
- ends with `tab focus` + a JSON result line

### ttt palette: `Forseti: Git (lazygit pane)` (P7-2)

Re-dispatches to the action via the existing `herdr_cmd` helper; surfaces the
result (`opened`/`focused`) as a status item. No TUI nesting.

### Crew review loop: `forseti-crew run --review` (P7-3)

Opt-in; implies `--keep-worktree` + `--keep-tab`. When the graph completes on
a worktree run, the runner splits a lazygit pane onto the crew tab
(socket: `pane.split` + `pane.send_text "lazygit -p <checkout>"` + Enter —
there is no socket `pane.run` surface) and `run_end` carries the merge recipe
(worktree path + attach hint + commit/merge order). Example:
`crew/examples/crew-worktree.yaml` with git-state checks
(`git status --porcelain` shape + content grep).

### lazygit → forseti customCommands (P7-4)

`install_lazygit_config` appends a marked, marker-guarded block to lazygit's
config (idempotent, self-healing on every invoke):

```yaml
customCommands:
  - key: '<c-g>'   # Open in forseti (ttt)
    context: 'files'
    command: '<repo>/herdr-plugin/scripts/git-jump.sh "{{.SelectedFile.Name}}"'
  - key: '<c-y>'   # Ask pi about this file
    context: 'files'
    command: '<repo>/herdr-plugin/scripts/pi-ask.sh --file "{{.SelectedFile.Name}}" "review this file in the current working tree"'
```

`git-jump.sh` writes the jump handoff (absolute path resolved against the
repo toplevel) and re-dispatches into the running ttt; `pi-ask.sh` resolves
the live pi agent (named one first, else first `pi` agent) and prompts it.
Both verified live end-to-end.

### ttt: `Forseti: Ask pi about uncommitted changes` (P7-5)

Palette command: `git status --porcelain` (no-op with a status message when
clean) → `git diff` excerpt capped at 2k chars → prompts the coder agent
through the ask plumbing. Requires `git` in the ttt manifest's
`system.exec` allowlist (approved at first load).

### Load-order lesson (recurring)

Handlers must be DEFINED above `ttt.register` — later definitions are
captured as nil handler slots and palette exec fails with "command not
found"; a reload after a failed reload latches stale state, so the cure is
file order, not plugin reload.

### Boundaries (Phase 7)

- lazygit is a **human surface only** — agents use `git` directly (check
  nodes, pi tools); no lazygit automation for agents.
- One lazygit pane per forseti tab (focus-or-create), one per crew tab for
  `--review`.
- Config edits are append-only under the `# >>> forseti >>>` marker; user
  settings are never rewritten. CI gates skip lazygit probes when the binary
  is absent (skip, not fail).

## Phase 8 — Laya decision layer (as-built 2026-10-01)

Bounded local decisions enter the workflow: crew edges can branch on what the
upstream MEANT, and pi can ask routing questions mid-task — both served by one
local Laya endpoint (Convai Innovations' ~421M ModernBERT decision model,
Apache 2.0, `laya==0.3.22`, mps on this Mac, warm predict ~21 ms). Spikes
S18/S19; plan `~/.opencode/plan/forseti-phase8-laya.md`.

### Runtime (P8-1)

- `scripts/laya-setup.sh`: pinned venv at `~/.config/forseti/laya-venv`
  (pip never global), one-time checkpoint fetch + warmup.
- `scripts/laya-serve.sh start|stop|status|restart`: binds 127.0.0.1:8751
  (env: `FORSETI_LAYA_PORT`), preloads `english`, polls `/health` 40 s
  (cold load ~20 s), pidfile idempotent start. The endpoint speaks Jev's
  `POST /v1/systemone {state, questions}` wire — answers carry
  `choice|score|noul` + `probabilities` + calibrated `confidence` +
  `answer_confidence` (top share).

### Crew edges: `when: laya:choice:<instructions>` (P8-2)

Schema: the edge gains `min_confidence` (default 0.5, the abstention dial)
and `state_file` (judge a file artifact instead of the pane transcript — the
scrollback truncates the 512-token encoder and carries the prompt echo, hit
live; the artifact is the review target).

Runner: a settled node's laya edges evaluate as ONE grouped decision —

- criteria: each target's instructions + implicit `other` ("none fit")
- argmax wins; below the winning edge's `min_confidence` → abstain (skip,
  full distribution recorded); `other` or a dead endpoint → fail-safe skip
  (the graph never hangs on a decision service)
- `laya_decision` events carry chosen/confidence/answer_confidence/latency;
  `edge_skip` explains why (`not chosen` | `abstained: conf < min_confidence`
  | `max_visits reached`)

Verified E2E (`crew/examples/crew-laya.yaml`, sandbox, glm override):
planner writes the incident note → gate picks `opsfix` (conf 0.675, 119 ms)
→ codefix skipped → opsfix runs. Also verified live: the abstention path
(conf 0.431 < 0.45 → skip with distribution).

### pi tool: `forseti_decide` (P8-3)

`pi-extension/index.ts`: bounded questions from the agent —

```
forseti_decide(input, question, options[{name, description}])
→ "decision: <name> (confidence <c>)\ndistribution: k: v, …\ncheckpoint: <m>"
```

Options map to laya criteria (+ `other` escape); degrades with a start hint
when the endpoint is down. Verified live: pi routed an ambiguous state and
hedged correctly on low confidence. Never mutates the editor; not for
drafting (promptSnippet says so).

### Calibration + abstention (P8-4)

- `scripts/laya-eval.sh` runs `crew/eval/laya-probe.jsonl` (9 probes: choice
  rubric rows, one ABSTENTION-contract row — a confusable must NOT resolve
  above 0.5 confidence —, noul rows, score rows with index→level mapping).
  Gate: 9/9; exit 1 on wrong answers or pathological calibration; deterministic
  (no generative model needed).
- The eval taught the same lesson the article preaches twice over: probe
  criteria must be internally consistent with the expected label, and
  ambiguous states belong under abstention contracts, not forced labels.

### Boundaries (Phase 8)

- Laya never generates text: no switchyard target, no pi provider entry.
- v1 activation = argmax-with-threshold; probability-mass fan-out is a
  documented future item.
- The endpoint is a user-invoked process; the runner/tool degrade to hints,
  never auto-spawn.
- Abstention is honored: low confidence surfaces as skipped edges with the
  distribution in the run log — a human can read why the graph did not
  proceed.

## Phase 10 — shared agent memory (as-built 2026-10-01)

A standalone memory service for the agents: structured, cross-run, vector
recall. (User decision: dedicated service, not vault-anchored — the vault
stays the human's evergreen store.) Spike S22a; Opik spike S22b = deferred
(no-go documented).

### `forseti-memory` (P10-1)

- FastAPI in the laya venv (`crew/memory/memory_serve.py`), port 8752
  (`FORSETI_MEMORY_PORT`), started by `scripts/memory-serve.sh`
  (same start/stop/status/restart contract as laya-serve.sh; hardened stop
  waits for the port to close — the restart race hit live).
- SQLite + sqlite-vec at `~/.config/forseti/memory.db`; row =
  `{id, ts, agent, run, source, tags, text}` + a cosine-distance vec0 shadow
  table (`distance_metric=cosine` — vec0's default is L2, hit live).
- Embeddings: `all-MiniLM-L6-v2` (384-d, load ~5 s once, ~100 ms per embed).
- `/write {agent, text, tags?, source?, run?}` → `{id}`;
  `/recall {query, agent?, k?, since?, min_score=0.3}` → rows (the relevance
  floor keeps noise out of prompts — unfiltered recalls injected 0.04-score
  rows, hit live); `/forget {id}`; `/health`.
- Namespacing: a caller sees its own agent's rows + `shared` — never another
  agent's private rows (verified both ways).

### Consumers (P10-2)

- pi tools `memory_write` / `memory_recall` (same registerTool shape as
  `forseti_decide`; start-hint degradation). Verified live: an agent recalled
  a fact and answered strictly from it.
- crew prompt helper `{{ memory "query" }}` — recalls at render time (cap
  2000 chars); any failure renders "" (memory never fails a run). Verified:
  the rendered prompt carried both recalled entries into the agent.
- Run-log events: `memory_recall` per render (entry count / unavailable).

### Eval gate (P10-5)

`scripts/memory-eval.sh`: writes 3 facts + a SECRET probe fact in a unique
per-run namespace, recalls by paraphrase (3 probes), asserts the secret never
leaks across namespaces → 4/4 PASS; deterministic.

### Boundaries (Phase 10)

- Memory is advisory context, never control flow.
- The vault is the human's; agents write to the DB.
- The service is user-invoked; clients degrade to hints.

## Phase 11 — crew phases: planner work assignment (implemented 2026-10-05)

The crew builder gains **phases**: ordered work stages, each with its own
instructions and a member list. A phase is a strict barrier — every node in
phase N must reach a terminal state (done/failed/skipped) before any phase N+1
node dispatches. Inside a phase, the existing edge/wave logic is unchanged
(parallel fan-out, `when:` gates, laya edges). A crew without `phases:`
behaves exactly as before (single implicit phase). User decisions:
builder-TUI surface, strict barrier, release-on-terminal, full vertical.

### crew.yaml addition

```yaml
phases:                       # optional; order = list order
  - name: research
    instructions: |           # template prepended to member prompts
      Read-only exploration. No edits. Record findings for the build phase.
    agents: [explorer, analyst]
  - name: build
    instructions: |
      Implement what research found. Run tests before declaring done.
    agents: [coder, reviewer]
```

- **Instruction injection**: dispatch renders `instructions + "\n\n" + agent
  prompt` as ONE template — instructions get `{{ .upstream }}` outputs and the
  `{{ memory "query" }}` helper for free.
- **Validation** (`internal/schema`): phase names unique + herdr name rule;
  members must exist; each agent in **at most one** phase; with `phases:`
  declared, unphased agents are an error; **edges to an earlier phase
  rejected** (cross-phase loop-backs are a documented future item); cycles
  within a phase keep the `max_visits` rule.
- **Runner**: the wave scheduler's ready-check gates on `phaseUnlocked(idx)` —
  every earlier phase fully terminal. `blocked` is NOT terminal (surfaced,
  never auto-answered): a blocked node holds its phase, the run ends with
  later phases skipped (same as today's blocked-node behavior).
- **Events**: `phase_start` (first member dispatches), `phase_done` (last
  member settles) in the JSONL run log; `node_start` carries `phase`.
- **Observability**: `forseti-crew watch` groups rows under phase headers;
  `crew-status.json` gains `phase` (first not-yet-done phase) and the ttt
  badge renders `crew <name> [<phase>] <done>/<total>`.
- **Builder TUI**: `p` in build mode opens the phase form (name,
  instructions, comma-separated members — validated on save); graph view
  groups nodes under `— phase N <name>` headers; node meta shows the phase.

Gate: `scripts/crew-smoke.sh` runs the phased checked crew
(`crew/examples/crew-phases.yaml`) as a second sandbox run and asserts the
barrier from the run log (research `phase_done` before build `phase_start`).

Verified live (2026-10-05, sandbox, `FORSETI_CREW_MODEL=opencode-go/glm-5.3-flash`):
the phased run's log reads `phase_start research → planner done + check_pass →
phase_done research (1/1) → phase_start build → builder done + check_pass →
phase_done build → run_end done=2` — the build node dispatched only after
research fully settled; both agents' scrollback contained their phase
instructions (injection confirmed); gate **PASS** end-to-end. The TUI phase
form was driven in a real pane (`scripts/crew-tui-drive.py`): form opens,
phase added, **save correctly rejected an unphased agent** (validation-on-save),
second phase added, saved file re-validates clean.

Boundaries (Phase 11): phase-boundary checks (`after: phase:<name>`), agents
in multiple phases, and cross-phase loop-backs are future items.

## Phase 12 — memory system v2 (as-built 2026-10-05)

Survey-driven upgrade of the shared memory service (S23 in `spikes.md`;
primary sources: Mem0 OSS `scoring.py`, Zep/Graphiti paper + docs, Letta
papers, LangMem source, A-MEM arXiv, RRF SIGIR 2009). User decisions:
survey-only spike; all eight improvement areas; RRF k=60 as the default
fusion; link expansion default-on bounded.

### Retrieval — hybrid, RRF-fused (P12-3)

- Two legs: sqlite-vec cosine (v1 unchanged) + **FTS5 BM25** (external-content
  table `mem_fts`, `porter unicode61` tokenizer, official 3-trigger sync
  pattern + one-time `rebuild` backfill). Both over-fetch `max(k×4, 60)`.
- **Semantic pre-gate BEFORE fusion** (Mem0's ordering): candidates with
  cosine < `min_score` (0.3 floor, unchanged) are dropped before the fuse —
  the keyword leg can never rescue a semantically-dead candidate. FTS-only
  hits carry `score: null` (lexical presence, no cosine).
- **RRF, k=60** (`FORSETI_MEMORY_RRF_K`): rank-only fusion, robust to
  incomparable BM25/cosine scales; SIGIR pilot shows k flat 30–100.
- Soft boosts: `final = rrf_norm + W_RECENCY·0.995^hours_since_last_access +
  W_IMPORTANCE·importance` (Generative Agents recency + writer importance;
  all constants in env config — S23's calibration caveat: published constants
  were tuned on 1024-d embeddings, not our 384-d MiniLM).
- Recall bumps `access_count`/`last_access` fire-and-forget (MemoryBank
  spacing effect: recalled memories persist longer).

### Write path — heuristic gates, no LLM in the service

- Exact dedup (MD5 hash within the caller's namespace) → return existing id.
- **Near-dup merge**: cosine ≥ `FORSETI_MEMORY_DEDUP` (0.95, Mem0's gate) →
  update the existing row (fresh ts, merged tags) instead of inserting.
- **Explicit supersedes wins over every heuristic** (Zep semantics):
  `supersedes: <id>` inserts the new fact as CURRENT and marks the old row
  `superseded_by` — history stays queryable (`include_superseded`), and the
  near-dup check is skipped for that write (a 14:00→15:00 contradiction is
  ≥0.95 cosine — the heuristic must not swallow a declared conflict).
- New optional write params: `type` (fact|episode|procedure|preference),
  `importance` (0–1), `expires_at` (TTL), `links` (id array).
- S23's write-quality rules (self-contained ~15–80-word statements, relative
  dates grounded against the observation date, proper nouns verbatim) live in
  the pi tool DESCRIPTIONS — agents write the facts; the service runs no
  model (repo non-goal).

### Consumers (P12-4)

- `memory_write`: optional `type`/`importance`/`supersedes`/`expires_at`/
  `links`; surfaces `dedup: exact|near` in the result.
- `memory_recall`: optional `type` filter + `include_superseded`; renders
  linked rows as `(linked from #id)`.
- Crew helper `{{ memory "query" }}`: unchanged (additive API).

### Backup (P12-5)

`GET /export` → NDJSON dump (all rows, incl. superseded/expired);
`POST /import {jsonl}` → re-embeds and skips existing ids (idempotent restore).

### Evaluation

- `scripts/memory-eval.sh` extended to **11/11**: the four P10 contracts
  (paraphrase ×3 + namespace isolation) plus exact dedup, near-dup merge,
  hybrid keyword-leg recall (bare identifier), supersede default + history
  queryable, TTL, type filter + importance. Deterministic.
- `scripts/memory-embed-eval.sh` (new, P12-6): bounded ranking comparison of
  four 384-d candidates (all-MiniLM-L6-v2, BAAI/bge-small-en-v1.5,
  thenlper/gte-small, intfloat/e5-small-v2 — all vec0-schema compatible) on
  the probe set. **Result 2026-10-05: 5/5 ties → KEEP INCUMBENT** (swap rule:
  only on a measurable win; re-embed migration would ship with the swap).

Boundaries (Phase 12): no write-time extraction pipeline (future item when
volume justifies an LLM pass); no entity/graph store (Mem0 retired their own
— co-occurrence linking beat typed triplets in practice); history table for
merged facts is a documented future item; memory stays advisory context.

## Phase 13 — full-repo bug review & scheduler semantics (as-built 2026-10-05)

Four review agents over the whole repo; every confirmed bug fixed and re-verified
(full table + verification evidence: docs/spikes.md S24). The changes that alter
contract or semantics — mechanical fixes are in the code and the spike record:

### Scheduler semantics (crew/internal/runner)

- **Fan-in is AND**: a node dispatches only when EVERY incoming edge has fired
  (`when:` satisfied). A missed edge (gate not matched) marks the edge and the
  target is `skipped` — transitively for pure-downstream targets.
- **Back edges gate re-dispatch only**: cycle back edges (target can reach the
  source; computed by `schema.BackEdges()`) are ignored for the INITIAL readiness
  check — strict AND over them deadlocks every cycle by construction. Once the
  target has run, its back edges participate like any edge: a firing back edge
  **re-arms** a settled target to pending, bounded by `max_visits` (N allowed
  fires; the skip event lands on attempt N+1). Self-loops are the same
  mechanism on an entry-reachable node (a self-loop-only crew has no entry and
  is rejected).
- **Entry validation**: a crew with no entry node (every node has an incoming
  edge) is rejected at `Validate()`; so are duplicate `(from,to)` edges (they
  collided on the scheduler's edge key) and invalid crew names
  (`schema.ValidName`: `[a-z][a-z0-9_-]{0,31}` — names reach run-log filenames
  and view sources).
- **No emit under lock**: skip/fail bookkeeping collects events under `r.mu` and
  emits after unlocking (emit → status bridge → … was a reentrancy hazard).
- `Run.Snapshot()` returns VALUE copies; `Run.NodeSnapshot()` single-node read.
  The TUI never dereferences a shared `*NodeState` the runner is writing.

### Event + verdict contract

- New event types: `check_skip` (after-node skipped → the check is skipped —
  NOT a failure; only a failed after-node sets `check_fail`) and `teardown`
  (teardown notes/failures; they must never emit `run_end`/`node_failed` — a
  second `run_end` used to overwrite watch's summary and defeat its exit code).
- Exit verdict (TUI + headless `watch`, identical rules): failure = any failed
  node, or a `check_fail` event, or a **blocked** node (a blocked run needs a
  human — exit nonzero) — blocked also forces KeepTab + KeepWorktree so the
  state survives for takeover.
- The empty-settle re-prompt guard is LAN-box-only (`halogen`/`valhalla` in the
  resolved model) — on healthy models it false-positived on every
  short-prompt/short-answer node (double cost).

### Client fixes worth remembering

- herdrd: the live 0.9.3 `agent.prompt` + `wait` response is
  `{"type":"agent_prompted","agent":{…,"agent_status":…}}` — parse it; an
  unknown shape is an ERROR now (B21 caught itself: the first P13 smoke run
  failed loudly instead of false-settling).
- switchyard: shared 2 s `http.Client`; restore = delete the `switchyard` key
  from a FRESH read of models.json (never byte-restore); stale-pre-entry probe
  sweeps dead prior runs and refuses live ones.
- forseti-tools: executor failures set `isError`; args pass `ValidateArgs` on
  BOTH the MCP and CLI paths; unquoted `{{ .param }}` in shell templates is a
  Load-time refusal (injection via model-supplied values).

### Gates (post-P13 state)

- `go build/vet/test ./...` green (new scheduler unit tests incl.
  `TestNodeReadyFanInAND`, `TestFireEdgeReArmsSettledTarget`).
- `scripts/memory-eval.sh` — SELF-CONTAINED now (throwaway service, private
  port, scratch SQLite; no shared-DB pollution): **13/13** incl. two new
  error-path probes (forget namespace guard → 404 + row survives; `k=0` → 422).
- `scripts/memory-embed-eval.sh` — real gate: incumbent sanity floor 4/5 drives
  the exit code (was always 0; a swapped tuple-unpack hid behind short-circuit).
- `scripts/laya-eval.sh` 9/9; `scripts/crew-smoke.sh` PASS (checked + phased
  crews) with `FORSETI_CREW_MODEL=valhalla/valhalla-flash-next` — the halogen
  registry is gone (box rebranded) and the opencode-go key is out of funds.
- `scripts/vault-init.sh` — daily template renders via `__TODAY__` (Lua),
  stamping touches fresh-created files only, portable in-place sed, legacy
  `PLACEHOLDERDATE` templates migrated (live vault repaired).

Boundaries (Phase 13): the review's deferred items (memory `/clear`, auto-links
at write, history audit table, importance-boost calibration, shared-DB probe-row
cleanup, laya-in-memory) are planned as 12.5/12.6 — see the handoff plan file.
