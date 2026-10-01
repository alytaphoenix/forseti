# Forseti — Spike Log

Verified findings from source-level investigation. New investigations append here.
Verified against: herdr 0.9.3, ttt 1.6.0 (`~/go/pkg/mod/github.com/eugenioenko/ttt@v1.6.0/`), pi 0.99.1.

## S1 — herdr plugin context contract ✅ resolved

Source: `ttt@v1.6.0/herdr-plugin/herdr-plugin.toml` + `scripts/open-worktree.sh`.

- Plugin context is delivered as environment variable `HERDR_PLUGIN_CONTEXT_JSON` (a JSON object). Script parses it with grep/sed (keys: `checkout_path`, `focused_pane_cwd`, `workspace_cwd`; resolution in that order).
- `HERDR_BIN_PATH` points at the herdr binary (fallback `herdr`).
- Actions run **headless** — exec'ing a TUI from an action panics on `/dev/tty`. Pattern: detect action context (`HERDR_PLUGIN_ENTRYPOINT_ID` unset + no tty) and re-dispatch via `$HERDR_BIN_PATH plugin pane open --plugin <id> --entrypoint <pane> --env KEY=VAL --focus`.
- herdr spawns plugin manifest commands from the **plugin root** with a relative command path — so `--cwd` on the spawned script would make the script unfindable; the script must `cd` itself instead.
- Manifest fields: `id`, `name`, `version`, `min_herdr_version`, `platforms`; `[[build]]`, `[[actions]]` (id/title/contexts/command), `[[panes]]` (id/title/placement/command).
- Linking: `herdr plugin link <abs path>`; requires an absolute path.

## S2 — ttt exec vocabulary ✅ resolved

Source: `internal/app/exec_script.go` (v1.6.0).

Full command list:
`click|rclick|hover|drag` (mouse), `key COMBO`, `type TEXT`, `paste TEXT`,
`copy`, `exec "Palette Command"` (runs any command-palette command by title),
`screenshot PATH`, `debug PATH` (JSON state dump), `wait MS`,
`wait-for TEXT [timeout=MS]` (default 5000 ms, poll 25 ms), `panel ID`, `quit`/`shutdown`.

- **No `open file` command exists.** For pi→ttt jumps this drives the design:
  pi writes a jump state file, then POSTs `exec "Forseti: Jump"` — a palette command
  registered by the forseti Lua plugin — or falls back to a keystroke chain
  (`key ctrl+k p` → `type path` → `key enter` → `key ctrl+g` → `type line` → `key enter`).
- `POST /exec` responses return non-2xx on invalid/failed actions; the HTTP surface is
  the same script format.
- The listener binds the hardcoded const `127.0.0.1:4242` (`internal/app/listen.go`) and
  is commented in source as *"a single-operator debug tool, not a public API"*.
  → One forseti-enabled ttt per machine; unauthenticated; recheck across ttt upgrades.
- Quick Open palette (file mode) exists (`selectdialog` `paletteFileMode`); `keybindings.md`
  confirms `Ctrl+K P` = `file.quickOpen`, `Ctrl+G` = `editor.goToLine`.

## S5 — ttt fs sandbox + `sys.env` quirks (Phase 4 debug, 2026-09-30) — ✅ resolved

Root-caused two Phase 4 ttt-plugin failures via actual invocations + partial
`ttt@v1.6.0` sources:

1. **`sys.env(HOME)`/`sys.env(FORSETI_VAULT)` return `""` inside the Lua sandbox** —
   environment-tag reads cannot be trusted for arbitrary keys. Fix: deploy-time
   state file. Bring-up writes `vault.json` into the plugin dir (shell side has
   full env); the Lua side reads it at init. This is the third state-file hand-off
   besides `jump.json`/`review.json`.
2. **`fs.read` said "filesystem API not available"** even though
   `ttt.plugin_dir()` returned the right path. Root cause: `Plugin.Filesystem`
   is wired **per plugin**, and two distinct call sites exist:
   - startup: `cmd/ttt/main.go` — `pluginManager.SetFilesystemAPI(...)` was only
     reachable for plugin panels loaded *after* the sidebar widget exists; but
     actually `LoadAll()`-loaded startup plugins get `p.Filesystem` set only
     via `RegisterStartupPluginCommands()` → `WirePlugin()` → `NewPluginFilesystemAPI(
     workspace paths + p.Dir)`.
   - Later plugins (installed from the panel) get it via `SetFilesystemAPI`.
   `p.Dir` is populated from `manager.go` scanning the plugins dir, so the plugin
   dir *is* in the allowed roots list. The failure mode must therefore be in
   ordering: if a plugin initialized before WirePlugin runs, its Lua `fs` module
   has `nil` Filesystem. When the plugin panel reloads (Plugins: Reload All), the
   stale runtime is replaced and `fs.read` begins working (observed: after a
   clean restart + reload, `vault.json read` succeeded).
   Lesson: order the load → any ttt API call that depends on `FilesystemAPI`
   behind `pcall`-gated availability.

Practical hedge (implemented): the Lua plugin treats "vault undefined" as a
soft error — it shows the vault as unavailable in the Forseti sidebar and
re-invokes `ttt` reload each time Daily Note / Open Obsidian is used, so
restarting ttt is not required, just retry the command after ~1s.

## S3 — `herdr agent start --kind pi -- <args>` passthrough ✅ resolved (2026-09-30)

Live result, per run (fresh scratch workspace per run, cleaned up after):

| Run | Args | Outcome |
|---|---|---|
| control | *(none)* | ✅ `agent start` succeeded; pi reached `idle`; terminal title becomes `pi - forseti` |
| positive | `-- --version` | ✅ args **do** reach pi: pane shows `pi --version` → `0.99.1`, then exits (hence readiness timeout) |
| negative | `-- --bogus-flag` | ❌ pi rejects unknown flag and exits → same readiness timeout |

Interpretation: herdr passes everything after `--` verbatim to the pi binary (a bogus
flag kills startup — nothing is swallowed or filtered). Caveat learned: **`agent start`
waits for interactive readiness by default**, so any non-interactive arg set
(`--version`, `-p`) times out (~30 s) *after* the command ran. Forseti always launches
pi interactively, so this is fine — but the pattern to know: arg-passthrough works,
and a startup timeout with `--version`-style args is the expected signature of args
arriving intact.

One CLI nuance found along the way: running `herdr tab create` / `workspace create`
from **outside** herdr has no implicit workspace — create one explicitly first
(`workspace create --cwd …`); responses are nested JSON (e.g. agent-id in pane objects,
`.result.root_pane.pane_id`), and flat leaf keys are reliably greppable.

## S4 — herdr event subscriptions — **partially resolved** (2026-09-30)

No CLI subcommand exposes subscribe/unsubscribe, but the **socket API surface
exists** — verified live 2026-09-30 by enumerating `herdr api schema --json`
(protocol 22) against the running server:

- Request methods include `events.subscribe`, `events.wait`, `agent.read`,
  `agent.prompt`, `agent.wait`, `agent.view.set/clear`, `agent.send_keys`,
  `layout.apply`, `layout.export`, `pane.edit_scrollback`, `pane.copy_search`.
- `AgentReadParams`: `target`, `source` (enum TBD), `format` (default `text`),
  `lines`, `strip_ansi` (default true) → pane/agent output capture at API level.
- `AgentPromptParams.wait`: `{until: [...AgentStatus], timeout_ms}` → synchronous
  dispatch-with-settle semantics without polling.
- `AgentStartParams`: `name`, `kind`, `pane_id`, `args[]`, `timeout_ms`
  (3000–300000 ms).
- Event vocabulary: workspace/worktree/tab/pane lifecycle events plus
  `pane_output_changed`, `pane_agent_status_changed` (carries `pane_id`,
  `workspace_id`, `agent_status`, `state_labels`), `pane_agent_detected`,
  `pane_exited`, `layout_updated`.
- `PaneOutputMatchedEvent` carries `matched_line` + a `PaneReadResult` →
  output-match subscriptions exist at schema level.

**Resolved live 2026-09-30 by S6–S10 (see below).** Driver:
`scripts/spike-socket.py` (kept; re-runnable; transcript `/tmp/forseti-socket-spike.jsonl`).
Lua polling (3 s cadence) remains sufficient for the Phase 1–4 status sidebar.

## S6 — socket framing + handshake ✅ resolved (live, 2026-09-30)

- **Framing: NDJSON over the Unix socket, NO handshake for the JSON API.**
  Request `{"id":"...","method":"...","params":{}}` → response echoes `id`.
  (The `id` field is tolerated but absent from the bundled JSON Schema's request
  envelope — schema covers method/params only.)
- **One-shot requests close the connection after the response.** Pipelining two
  requests on one socket yields exactly one response line, then EOF. So a client
  must open a fresh connection per request — or use a subscription connection.
- **`events.subscribe` is the only persistent connection**: ack line
  `{"id":...,"result":{"type":"subscription_started"}}`, then pushed lines
  shaped `{"event":"<name>","data":{...}}` **with no `id`**. Verified pushes:
  `pane.agent_status_changed` (`data: {agent, agent_status, pane_id, workspace_id}`)
  and `tab.created` (`data: {tab: {...}, type: "tab_created"}`).
- Multi-type subscriptions in one request work (`pane.agent_status_changed` +
  `tab.created` + `tab.closed` on one connection).
- The TUI uses a different **bincode endpoint protocol** (`endpoint.hello.v1` /
  `endpoint.welcome.v1`, `TerminalHello`/`ClientShellHello`, 2 MiB frame cap —
  server log: `oversized handshake from client claimed=… max=2097152`).
  Irrelevant to crew: the JSON API path is independent and needs none of it.
- Error shape confirmed: `{"id":...,"error":{"code":"...","message":"..."}}`.

## S7 — agent.read + output-event reality ✅ resolved (live, 2026-09-30)

- `agent.read` response nests under **`result.read`**:
  `{type:"pane_read", read:{pane_id, workspace_id, tab_id, source, format, text}}`.
  All four `source` values work: `visible`, `recent`, `recent_unwrapped`,
  `detection` (444/444/443/444 chars on the live coder pane).
- **`events.wait` supports ONLY `pane_agent_status_changed`** in 0.9.3.
  `pane_output_changed` match → `{"code":"unsupported_event_wait_match",
  "message":"events.wait currently supports pane agent status matches"}` —
  despite being present in the EventMatch schema.
- **`pane_output_changed` is not subscribable either** (not in the Subscription
  oneOf). Proxy test: a global `pane.updated` subscription saw **0 events in 20 s**
  while the coder agent streamed a 40-line answer → `pane.updated` is
  metadata-only (title/agent), NOT output-driven.
- **Consequence for the crew monitor (5-4):** no push surface for output.
  Design = persistent `pane.agent_status_changed` subscription (drives
  working/idle/blocked) + debounced `agent.read recent` polling while working
  (500 ms–1 s cadence is cheap: read latency measured <50 ms).
- Status transitions observed with sub-second accuracy on the stream:
  `working` at +1.17 s, `idle` at +2.45 s after prompt.

## S8 — layout.apply / layout.export ✅ resolved (live, 2026-09-30)

- `layout.apply` with a BSP tree (`split{direction,ratio,first,second}` /
  `pane{label,cwd,command,env}`) **creates a fresh tab** — response
  `{type:"layout_apply", layout:{workspace_id, tab_id, zoomed,
  focused_pane_id, root}}`; pane ids are assigned in the returned tree
  (read them by walking `root`, not from a flat list).
- `layout.export` round-trips the same BSP shape (split direction/ratio +
  pane labels/cwd preserved, `pane_id` added).
- **Declarative N-pane crew tabs are fully viable**: build tree → apply →
  walk tree for pane ids → `agent.start` each.

## S9 — two concurrent pi agents, different models ✅ resolved (live, 2026-09-30)

- Two `agent start --kind pi` in one crew tab with different `--` args:
  `spike-a --model opencode-go/glm-5.3-flash`,
  `spike-b --model halogen/halogen-qwen3.8-flash-next`. Both started, both
  answered their prompts, pi status lines showed **their own model** per agent.
- Per-node models work. Caveat learned the hard way: **wait for `idle` before
  first prompt** (agent readiness) — a prompt fired during startup got lost;
  `agent wait <name> --until idle` first, then prompt, then wait again.

## S10 — agent.view.set/clear semantics ✅ resolved (live, 2026-09-30)

- `agent.view.set` → `{type:"agent_view", active:true, source, label}`;
  ownership enforced (re-set by owner works; `agent.view.clear` by owner →
  `active:false`).
- Confirmed **UI-only projection**: `agent.list` is unaffected by an active
  view. crew must NOT use it for control flow — it exists to steer the built-in
  Agents sidebar. Optional nicety for crew: project its run's agents into the view.

## S11 — named-session lifecycle (Phase 6, 2026-10-01) ✅ resolved (live)

- Named session sockets live at `~/.config/herdr/sessions/<name>/herdr.sock`
  (confirmed via `herdr --session X status` path resolution + live server).
- **CLI never auto-starts a named server**: `herdr --session X <cmd>` →
  `server_not_running` error pointing at `herdr session attach X`.
- Attach requires a TTY: non-TTY `herdr --session X < /dev/null` →
  "cannot attach without a usable terminal" **before** the server spawns.
- **Nested herdr is blocked by default** inside panes ("recursive descent
  denied"); knob is `[experimental] allow_nested` in config.toml. BUT nesting is
  detected via inherited env — **clearing `HERDR_ENV` (+ `HERDR_PANE_ID`,
  `HERDR_SOCKET_PATH`) lets a pane spawn a named-session server** without any
  user config change. Verified: `env -u HERDR_ENV -u HERDR_PANE_ID
  -u HERDR_SOCKET_PATH herdr --session forseti-spike` inside a live-session
  pane started the sandbox server in ~2 s.
- Isolation verified: tab created in sandbox visible in sandbox `tab.list`,
  **absent** from default session; separate workspaces/agents/logs per session.
- Sandbox server **persists after the bootstrap pane closes** (detached_server_daemon
  capability) → bootstrap pane can be closed immediately once the socket appears.
- `herdr session stop <name>` stops only that session; default untouched.
- crew `--session` design: runner bootstraps via a temp live-session pane if the
  named socket is absent (spawn, poll socket, close pane), then operates fully
  headless against the named socket; `herdrd.SocketPath()` already resolves it.

## S12 — pane.wait_for_output semantics (Phase 6, 2026-10-01) ✅ resolved (live)

- `pane.wait_for_output {pane_id, source, strip_ansi, match:{type:substring|regex, value}, timeout_ms}`.
- Blocks server-side until match; returns `{type:"output_matched", pane_id,
  revision, matched_line, read:{...}}` — `matched_line` is the actual line.
- **Matches existing scrollback instantly** (source=recent) — watchers wanting
  only NEW output must baseline (unique per-run markers, or wait after settle).
- Future output: fired 2.6 s after the line was printed by a slow pane. ✓
- Timeout → error `{"code":"timeout"}` after exactly timeout_ms.
- Bad regex → error `{"code":"invalid_regex"}` with the Rust regex parse error.
- One-shot per call; re-arm per event for repeated watching.

## S13 — worktree.create from a run (Phase 6, 2026-10-01) ✅ resolved (live)

- `worktree.create {cwd, branch, path, label, trust_repository:true, focus:false}` →
  `{type:"worktree_created", workspace:{workspace_id, worktree:{repo_key,
  repo_root, checkout_path, is_linked_worktree:true}}, tab:{...}}` — creates the
  git worktree AND a dedicated herdr workspace in one call.
- `worktree.list {workspace_id|cwd, trust_repository}` → source + worktree set.
- `worktree.remove {workspace_id, force:true, trust_repository}` →
  `{type:"worktree_removed"}`; removes dir + workspace. Branch deletion is ours
  (`git branch -D`).
- Crew `--worktree` design: create before tab build, use `checkout_path` as node
  cwd, remove after run (unless `--keep-worktree`).

## S14 — switchyard-server bring-up (Phase 6, 2026-10-01) ✅ resolved (live)

- `switchyard-server 0.2.0` (crates.io, `~/.cargo/bin`) — NVIDIA NeMo Switchyard;
  `switchyard-libsy` is the embeddable routing core, the server is the proxy path.
- TOML: `schema_version = 1` + `[llm_clients.X] format="openai_chat"
  base_url api_key_env?` + `[targets.X] id llm_client` + `[routes.X] id type
  capable_target efficient_target picker confidence_threshold`.
- **0.2.0 route types: `noop|random|passthrough|llm_classifier|stage_router` —
  no `auto`** (docs describe 0.3.0; the "auto" preset == stage_router
  efficient_first + threshold 0.5 + no classifier).
- Mixed pool works: keyless halogen + keyed opencode-go in one route
  (same-provider constraint applies only to `forward_auth` routes).
- `--dry-run` validates config+env without binding; `--host/--port` dynamic;
  `--routing-log-file` appends JSONL `{ts, session_id, model, tier, tokens…}`.
- Readiness: `GET /health` → `{"status":"ok"}`; `GET /v1/models` lists route ids;
  `GET /v1/stats` → totals, per-model/tier counters, routing overhead percentiles.
- Every response carries `x-model-router-selected-model` +
  `x-model-router-rationale` (e.g. "fall-through selected
  halogen-qwen3.8-flash-next (confidence 0.000)").
- **Session header drift**: 0.2.0 reads `proxy_x_session_id` (verified recorded in
  routing log); upstream docs' `x-session-id`/openrouter format is 0.3.0.

## S15 — pi ⇄ switchyard live loop (Phase 6, 2026-10-01) ✅ resolved (live)

- pi 0.99.1 round-trips through a route: `models.json` provider `switchyard`
  (`api: openai-completions`, placeholder apiKey, `compat` per upstream doc),
  model id = route id → `pi -p --model switchyard/forseti-auto` answered,
  routing log recorded the call (prompt_tokens 5254 = pi system prompt).
- pi's request logged `session_id: null` — pi's session-affinity header does NOT
  match 0.2.0's `proxy_x_session_id` (0.3.0 aligns these). Stage-router
  session-scoped hold/affinity is effectively per-request in 0.2.0. Not a
  blocker for crew (routing still works); revisit on upgrade.
- Crew integration: node `route: <id>` ⇒ pi `--model switchyard/<route-id>`;
  runner owns proxy lifecycle + provider-entry materialization.

## S16 — lazygit in panes (Phase 7, 2026-10-01) ✅ resolved (live)

- `brew install lazygit` → 0.65.1 (`/opt/homebrew/bin/lazygit`).
- `lazygit -p <repo>` renders correctly inside a herdr pane (Status/Diff/Files
  panels visible within ~3 s); `q` quits cleanly back to the shell prompt —
  relaunch via `pane run` is safe.
- macOS config path: `~/Library/Application Support/lazygit/config.yml`
  (auto-created on first quit; NOT `~/.config/lazygit/` unless
  `XDG_CONFIG_HOME` is set). Repo-scoped config also exists: `<repo>/.git/lazygit.yml`
  plus parent-dir `.lazygit.yml` (docs) — not used in v1.
- 0.65.x collision notes: `Ctrl+O` is universal copy-to-clipboard; custom
  commands therefore bind `Ctrl+G` (files context) and `Ctrl+Y`.
- Custom keybindings override inbuilt ones in the SAME context; global custom
  keys lose to context-specific inbuilt ones (upstream docs).

## S17 — lazygit customCommands → forseti (Phase 7, 2026-10-01) ✅ resolved (live)

- `customCommands` shape (0.65.x): `{key, command (Go template), context,
  description, loadingText, output}`; context `files` exposes
  `{{.SelectedFile.Name}}` (repo-root-relative; `| quote` available).
- Verified loop: a managed, marker-guarded block binds
  - `<c-g>` → `git-jump.sh "{{.SelectedFile.Name}}"` — resolves the file
    against `git rev-parse --show-toplevel`, writes `jump.json`, re-dispatches
    into the live ttt via `POST /exec exec "Forseti: Jump"`. Live drive: ttt
    opened the selected file.
  - `<c-y>` → `pi-ask.sh --file …` — prompts the live forseti pi agent. Live
    drive: the coder agent went `working` and answered.
- ttt plugin load-order lesson (recurring U2 class): **handlers defined AFTER
  `ttt.register` are captured as nil**; the fix is definition order (handlers
  above `ttt.register`), not reloads — a reload following a failed reload
  latches stale state (previously verified).

## Supporting findings

- ttt Lua API: `set_interval/set_timeout` run callbacks on the editor main loop
  (min 50 ms, auto-cleared on plugin disable/reload/uninstall, no permission needed) —
  makes a polling status sidebar safe without goroutines. Docs confirm permission model
  ("if not granted, the corresponding functions are simply not available on the module").
- ttt `--version` exists (populated binary). Dependencies for full features: `git`, `rg`.
- herdr CLI help advertises `herdr --skill` (agent control guide) and
  `herdr api snapshot` (requires running server).
- pi extension surfaces verified: factory + lifecycle rules, `tool_call`/`tool_result`
  events with toolName+input (edit inputs carry path + old/new strings),
  `registerCommand`, TS via jiti, project-local install needs `-a`.
