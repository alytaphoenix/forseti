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

## S18 — Laya runtime + serve (Phase 8, 2026-10-01) ✅ resolved (live)

- `laya 0.3.22` (pip, venv-only at `~/.config/forseti/laya-venv`): Router +
  `laya-serve` binary; `laya[serve]` adds the FastAPI/uvicorn surface.
- **Wire** (TypeSafe Jev-compatible): `POST /v1/systemone {state, questions}`
  → `{model, answers{type, choice|score|noul, probabilities, confidence,
  answer_confidence, action}, usage{state_tokens, truncated…}, routing}`.
  `GET /health` → status + loaded checkpoints + device + revisions.
- Env config only: `LAYA_HOST/PORT` (bind 127.0.0.1:<port> for forseti),
  `LAYA_MODELS=english` preload, `LAYA_DEVICE`, `LAYA_API_KEY` (optional
  bearer), `LAYA_MAX_CONCURRENT`. No CLI flags; `--help` BOOTS the server.
- **Latency on this Mac**: warm predict ~21 ms (direct), ~90-170 ms over HTTP;
  cold start ~20 s (model load) — the serve script polls /health for 40 s.
- Device: **mps** (Apple GPU), revision `55cf4c4e` pinned in /health.
- Checkpoint caveat (from its own runtime warning): "invalid temperatures…
  Treat confidence from the affected entries as uncalibrated" — empirically
  the calibrated `confidence` still discriminates uncertainty well (clear
  cases 0.79-0.96, near-ties 0.16-0.19, confusables 0.18), but it must be
  treated as a threshold dial, not a probability.

## S19 — laya edge-gate semantics (Phase 8, 2026-10-01) ✅ resolved (live)

- `when: laya:choice:<instructions>` + `min_confidence` + `state_file` per
  edge; the runner groups a node's laya edges into ONE decision: criteria =
  each target's instructions (+ implicit "other" escape), state = the
  `state_file` content (if declared) else the captured output.
- **State quality is the deciding factor** (the article's "evidence, not a
  dump" made real): the full pane transcript truncated the 512-token encoder
  (341 tokens dropped) AND carried the prompt echo → abstained on a clear
  outage note (conf 0.056); the tail-2500 also abstained; the incident NOTE
  file alone → `opsfix` conf 0.43-0.68. **Rule: laya gates should judge a
  file artifact, not terminal scrollback.**
- Prompt pollution is real: a planner instructed to write "no code changes
  needed" produced a note whose own text pulled toward the codefix option —
  decisive note content → decisive confidences (0.675 demo run).
- E2E (sandbox, glm override): planner → laya gate (opsfix conf 0.675,
  119 ms) → codefix skipped → opsfix ran → run_end done=2. Abstention path
  verified live at 0.431 vs min_confidence 0.45 (skipped, distribution
  recorded).

## S23 — memory-systems survey for v2 (Phase 12, 2026-10-05) ✅ resolved (survey, primary sources)

Survey of Mem0 / Zep-Graphiti / Letta-MemGPT / LangMem / A-MEM + hybrid-fusion
literature (arXiv full texts, OSS sources, official docs — URLs in the session
notes). Findings for a <10k-row local SQLite service:

- **Hybrid fusion = RRF, k=60** (Cormock SIGIR 2009; Zep's default reranker;
  Elasticsearch rank_constant default 60; Weaviate/OpenSearch both ship it):
  rank-only fusion `Σ 1/(60+rank_i)` — robust to incomparable BM25/cosine
  scales, zero tuning (k flat 30–100 in the pilot). Weighted fusion (Weaviate
  relativeScoreFusion, ~6% recall gain) requires per-leg min-max normalization
  + coverage-asymmetry gotchas — upgrade path only, not the default.
- **SQLite FTS5 mechanics** (official docs): external-content table
  `content='memories', content_rowid='id'` + 3 triggers (insert/delete/update)
  + one-time `INSERT INTO fts(fts) VALUES('rebuild')` backfill; `bm25()` is
  lower=better (−1 sign convention), k1=1.2/b=0.75 hard-coded, `rank` column
  faster for sorting; tokenizer `porter unicode61`; on delete/update write the
  FTS index FIRST; REPLACE is unsupported on external-content tables.
- **Supersede-not-delete (Zep)** is the highest-value portable idea: a fact
  carries `created_at` + `valid_at`/`invalid_at` (+ optional `expired_at`);
  contradiction sets `invalid_at` on the old row, history stays queryable,
  `latest_only` reads filter `invalid_at IS NULL`. This REPLACES expensive
  write-time adjudication: Mem0's current OSS "additive" model does exactly
  this — ADD new facts freely, link them to contradicted old ones
  (`linked_memory_ids`), resolve truth at read time; the paper-era
  ADD/UPDATE/DELETE/NOOP tool-call flow is retired from `add()`.
- **Mem0's recall scoring is fully published in OSS `scoring.py`** — the most
  transplantable design: over-fetch `max(top_k×4, 60)` per leg; BM25
  sigmoid-normalized with query-length-adaptive midpoint/steepness tables;
  entity boost `similarity × 0.5 × 1/(1+0.001(n−1)²)` gated ≥0.5, ≤8 entities;
  additive fusion divided by an adaptive `max_possible`; semantic gate
  (threshold 0.1) applied BEFORE fusion (keyword can never rescue a
  semantically-dead candidate).
- **Write-quality prompt rules (Mem0 V3)** — the highest-leverage zero-infra
  improvement: ground relative dates against the observation date, never
  "current date"; preserve proper nouns/quantities verbatim; facts 15–80
  words, self-contained; when in doubt extract (dedup handles redundancy).
  Forseti note: writes come from agents themselves (no LLM inside the
  service — a repo non-goal), so these rules belong in the pi tool
  descriptions, not in a service-side extraction pipeline.
- **Dedup (Mem0)**: MD5 exact-hash + semantic near-dup ≥ **0.95** (their
  entity-dedup gate) → merge/update instead of insert.
- **Scoring boosts (Generative Agents, arXiv 2304.03442 §4.1)**: `recency =
  0.995^hours_since_last_access` (≈138 h half-life; time unit must be stated
  explicitly) + importance (LLM 1–10 rated once at write) + relevance cosine;
  min-max normalize all three, equal weights. **MemoryBank**: `R = e^(−t/S)`,
  S += 1 and t→0 on recall (spacing effect) — use as a compaction criterion,
  never hard delete. **LangMem** states the same intent (similarity +
  importance + strength-from-recent-frequent-use) but publishes NO formula.
- **A-MEM (NeurIPS 2025)**: embed the CONCATENATION of content + generated
  context/keywords/tags (they use the SAME all-MiniLM-L6-v2); links = top-10
  cosine neighbors, retrieval includes linked neighbors ("box" propagation);
  ablation: link generation carries most of the gain, LLM evolution loop adds
  the rest — the evolution loop (one LLM call per neighbor) is the part to
  skip. No numeric thresholds published anywhere in it.
- **Letta/MemGPT**: tiering is by context, not by DB ("root files in prompt,
  indexed dirs out"); the efficiency rule for persist-vs-discard: *don't
  store what a message search can recover — persist decisions, preferences,
  corrections, navigational references; generalize, don't log.* No published
  archival scoring (tool-mediated agent judgment). The popular "MemGPT
  recency×importance×relevance weights" are NOT in the paper (product code,
  unpublished) — don't cite them as published.
- **Explicit overkill at this scale** (with receipts): graph DBs (Mem0 retired
  their own external graph store; platform graph = schema-free co-occurrence),
  typed triplets/hyper-edges/communities (Zep; their heavy derivation layers
  cost ~600k tokens per 26k-token conversation and lagged availability —
  Mem0 paper §4.5), cross-encoder rerankers as default (~150–200 ms, RRF
  suffices), LLM-adjudicated ops on every write, background synthesis until
  volume justifies it. Mem0's own telemetry calls >2000 memories "at scale" —
  forseti is below every line they draw.
- **Calibration caveat**: every published constant (0.95/0.5 gates, sigmoid
  tables, decay band 0.3–1.5, threshold 0.1) is tuned for OpenAI/1024-d
  embeddings; with 384-d MiniLM the distributions differ → keep ALL constants
  in config, validate on our own probes before trusting any.

## S22 — memory runtime + opik viability (Phase 10, 2026-10-01) ✅ resolved (live)

**S22a — memory runtime (in the laya venv):**
- `sentence-transformers 6.1.0` + `sqlite-vec` install clean into the existing
  venv (torch already there); `all-MiniLM-L6-v2` load 5.3 s one-time.
- Embed 5 texts ~0.5 s (~100 ms each, CPU); sqlite-vec `MATCH` returns
  distances — **vec0's default metric is L2**, not cosine; use the
  `distance_metric=cosine` column option so `score = 1 - distance`.
- Recall quality: correct best-match on a paraphrased question over a 5-fact
  set; namespace filter verified both ways (private rows hidden from other
  agents' recalls).

**S22b — Opik viability: DECISION = no-go for now (deferred):**
- The opik python client installs fine; a real integration needs either a
  hosted API key (none in env) or the self-host docker-compose stack
  (postgres+clickhouse+redis+backend — several GB, docker not running here).
- Forseti's native surface already covers current observability: JSONL run
  logs, `forseti-crew watch`, the ttt badge bridge, laya/memory eval gates.
- Designed future item (one small exporter, zero runner deps):
  `forseti-crew export --opik <run.jsonl>` — replay node spans +
  laya_decision/route_decision attributes post-hoc. Activates when an
  `OPIK_API_KEY` exists or docker comes up.

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

## S24 — P13 full-repo bug review (Phase 13, 2026-10-05) ✅ resolved (live + test)

Four read-only review agents over the whole repo (memory service; Go runner+schema;
pi/ttt/shell surfaces; Go cmd+clients). Every finding was adjudicated, fixed, and
re-verified (unit test, live probe, or gate). The non-obvious ones — the rest are
mechanical (missing timeouts, unchecked errors, dead code):

| ID | Area | Finding | Fix | Verified |
|---|---|---|---|---|
| BUG-05 | scheduler | fan-in was OR-any: a diamond target dispatched on its FIRST parent | explicit AND (`nodeReady` waits for every incoming edge); `fireEdge`/`missEdge` bookkeeping | unit `TestNodeReadyFanInAND` |
| BUG-02 | scheduler | a re-firing edge into a settled node was dropped → `max_visits` cycles could never loop | `fireEdge` re-arms a done/skipped target to pending (bounded by `max_visits`) | unit `TestFireEdgeReArmsSettledTarget` |
| — | scheduler | **fan-in AND + cycles deadlock by construction**: a back edge (target can reach source) can only fire after its own target ran | `schema.BackEdges()` (reachability DFS); `nodeReady` skips back edges for INITIAL dispatch — they gate re-dispatch only | unit tests + `crew/examples` all validate |
| BUG-01 | scheduler | `emit` called under `r.mu` → reentrancy (emit→statusBridge→…) | skip events collected under lock, emitted after unlock | unit (skip drain) |
| BUG-09 | runner | a run ending `blocked` tore down the tab/worktree — the human had nothing to take over | blocked forces KeepTab + KeepWorktree | code read + smoke |
| BUG-25 | checks | a check whose after-node was skipped counted `check_fail` | `check_skip` event; only a genuinely failed after-node sets fail | smoke (checks crew live) |
| BUG-11 | schema | `inCycle` matched any edge TOUCHING a cycle → an unrelated max_visits edge satisfied the bound | `inCycle` = both endpoints consecutive on the cycle | unit `TestUnrelatedEdgeDoesNotBoundCycle` |
| BUG-12/03 | schema | duplicate (from,to) edges collided on the scheduler key; an all-cycle crew had no entry and silently ran nothing | both rejected at `Validate()` | unit `TestDuplicateEdgeRejected`/`TestNoEntryRejected` |
| BUG-21 | schema | crew `name` was unchecked → landed in run-log filenames + view sources | `Validate()` enforces `[a-z][a-z0-9_-]{0,31}` (exported as `schema.ValidName`) | unit `TestCrewNameRule` |
| B21 | herdrd | `AgentPromptWait` returned `"idle"` for ANY unrecognised response (wire drift = false settle) | parse the observed 0.9.3 `agent_prompted` shape; unknown shape → error | **caught by crew-smoke** (first run failed loudly, fix verified on re-run) |
| B10 | switchyard | `http.Get` on the default client (no deadline) → a wedged proxy hangs a run forever | shared `hc = &http.Client{Timeout: 2s}` | code |
| B9 | switchyard | byte-restore of `models.json` clobbered edits made mid-run; no guard vs a concurrent crew | restore = remove the `switchyard` KEY (re-read fresh); refuse a live pre-existing entry, sweep a dead (stale-port) one | code |
| B22 | switchyard | `[llm_clients.%s]` with a dotted provider id silently became a nested TOML table | quoted table key `%q` | code |
| B6/B7/B13 | toolspec | non-2xx HTTP set a dead `ExitCode` instead of `isError`; the http probe matched `expect_contains` against empty stdout; model-supplied tool args reached executors unchecked | shell+http both set `isError`; http probe matches on `res.Body`; `ValidateArgs` (declared-type + reject-undeclared) on serve AND cli | code |
| B13 | toolspec | a `{{ .repo }}` shell template interpolated a model value UNQUOTED (`repo="; rm -rf ~"` = execution) | `lintUnquotedParams` refuses it at Load (must write `{{ q .repo }}`) | code |
| M4 | memory | semantic min_score gate unioned with the FTS leg → a 0.05-cosine lexical twin took full dual-leg credit | only FTS-ONLY ids bypass the gate | live probe + eval |
| M7 | memory | two concurrent identical writes raced past the dedup SELECT → permanent twins | `BEGIN IMMEDIATE` around dedup+insert (also supersedes target mark) | live |
| M10 | memory | forgetting a superseding row orphaned its predecessor (`superseded_by` dangled at a deleted id) | forget un-supersedes children | live probe (predecessor visible again) |
| M1 | memory | `/forget` had no namespace guard (any caller could delete any row) | 404 unless owner-or-shared; `agent` field added | live probe + eval row |
| M14 | memory | extra request fields were silently dropped (the links-lost incident mechanism) | `model_config = ConfigDict(extra="forbid")` on all requests | live probe (422) |
| M16 | memory | `/export`→`/import` across two embedding models mixed vector spaces silently | `_meta` header (model+dim); mismatched restore 422s | live probe |
| M2 | serve | `stop` waited on `/health` to fail, not the process to die → restart spawned a corpse against a dying server; `kill -0` on a recycled pid killed a bystander | wait pid-death; `ps` identity check; `/health` boot nonce | two back-to-back restarts, each nonce-verified |
| — | serve | a healthy server with a lost/empty pidfile was unmanageable (old `rm -f` bug class) | `adopt()`: manage it only on a UNIQUE pgrep of the exact venv/script path | live (adopted the unowned laya, eval 9/9) |
| M21 | gates | embed-eval always exited 0 (and had a swapped tuple-unpack hidden by short-circuiting) | incumbent sanity floor 4/5 gates the exit code | live 5/5 → exit 0 |
| — | vault-init | daily template was date-STAMPED at scaffold (PLACEHOLDERDATE) but Lua daily_note substitutes `__TODAY__` → every daily note got a stale/literal date | daily template ships `__TODAY__`; stamp fresh-created-only; legacy `PLACEHOLDERDATE` daily template migrated | live (scaffold + migrate + real vault repaired) |

Skipped deliberately: memory-service `closing()` refactor of per-request
connections — CPython refcounting already closes them deterministically; the
change was churn without an observed failure.

**Contract additions this phase** (all as-built in the code, this is the record):
- `Event.Type` gained `check_skip` (after-node skipped → check skipped, not failed)
  and `teardown` (teardown's own notes/failures — a second `run_end` used to
  overwrite watch's summary + defeat its exit code).
- `schema.BackEdges()` — the scheduler's initial-vs-re-entry distinction.
- `schema.ValidName()` — shared by crew-name validation and the TUI phase form.
- `Run.Snapshot()` returns VALUE copies (+ new `Run.NodeSnapshot`) so the TUI
  never reads a shared `*NodeState` the runner is writing.
- Blocked node at run end forces KeepTab + KeepWorktree.
- The LAN decision/model box (192.168.0.142) rebranded **halogen → valhalla**
  (registry now `valhalla/valhalla-flash-next` + a 27b); the opencode-go key is
  out of funds. The crew empty-settle guard matches `valhalla` too; E2E gates
  run `FORSETI_CREW_MODEL=valhalla/valhalla-flash-next`.
- memory-eval is SELF-CONTAINED (scratch service on a private port + scratch
  SQLite; 13/13 with two new error-path probes: forget namespace guard, k=0→422).

## S25 — laya as the memory gray-zone adjudicator (Phase 12.6, 2026-10-05) ✅ resolved (live measurement)

Verified against the local laya 0.3.22 `/v1/systemone` endpoint (healthy, warm
~21 ms). The gray band [0.7, 0.95) asks “merge the restatement or supersede the
stale value?” — how to pose that to a decision endpoint turned out to matter:

- **3-way relation rubrics mislabel**: `reworded|conflicting|different` on
  “freeze window tuesday morning” → “moved to wednesday morning” answered
  `different` (p 0.67, conf 0.28). The 0.7 similarity band already excludes
  unrelated facts, so the question is BINARY (`same`|`changed`) with criteria
  naming the slots (“did any date/number/name/decision change”): 5/5 argmax
  on labeled pairs (3 conflicts + 2 restatements), including a 500→900
  rate-limit pair the 3-way framing got wrong.
- **laya’s `confidence` field is unusable as a gate for rubric questions**:
  it reads 0.09–0.27 exactly when the top-probability label is right
  (0.68–0.80). It is calibrated for the binary-routing use (P8); gating on
  conf ≥ 0.5 means laya never fires here.
- **P(top label) is the honest abstention dial**: measured separation — clean
  pairs p ∈ [0.677, 0.97]; genuinely-ambiguous same-slot/different-aspect
  pairs (“rate limit is 500 rpm” vs “applies per tenant”) p ∈ [0.504,
  0.513] — coin flips. Gate p ≥ 0.65 (`FORSETI_MEMORY_LAYA_PROB`) takes every
  clean case and drops every ambiguous one. Borderline live case (“cutline
  thursday 18:00” → “moved to friday 12:00”, TWO values moved): p = 0.642
  → abstained (verified on the shared service: old fact survived, got
  auto-linked, awaits a confident write or explicit supersedes).
- **Type classification** (4 labels): procedure p 0.893, preference p 0.959,
  episode p 0.881 fire; an “is named” fact-like text scored p 0.443 → abstain
  → `fact` default (empirically the right label — abstention is benign here).
- Lock discipline: pool fetch + laya round-trips happen BEFORE
  `BEGIN IMMEDIATE`; inside the txn the candidate’s live state is rechecked
  and a stale verdict dropped (DB beats cache).
- Eval-design consequence: probe texts must be picked with the band in mind
  — “alpha/beta” reads as a value slot (auto-superseded), same-value
  paraphrases read `same` (merged). The auto-links probe now uses a measured
  abstention pair (cos 0.75, p ~0.50); the /clear probe uses unrelated topics
  (cos ~0.38) so it tests /clear, not the gray band.

## S26 — MemTree hierarchy + crew memory namespaces (Phase 14, 2026-10-07) resolved (paper + live)

Verified arXiv:2410.14052 (MemTree, HTML fetch) against the shipped
`crew/memory/memtree.py`. Paper mechanics that ported 1:1:
- **Insert**: descend from the namespace root taking the max-cosine child
 while cos >= theta(d) = theta0 * exp(lambda*d/max_depth) (paper 0.4/0.5;
 env dials `FORSETI_MEMTREE_THETA0/_LAMBDA`). At a leaf: old leaf + new row
 become children of a fresh internal node.
- **Retrieval = COLLAPSED**: flat cosine over ALL nodes; the paper's ablation
 shows collapsed >= traversal gating. A summary-node hit resolves to its
 descendant leaves (`descendants_leaves`), never surfaces the node text.
- **Branching is learned, not binary** (~2.1 children/node average) -> we
 keep n-ary fanout, no child cap.

Our deliberate deviations (recorded so future edits don't re-litigate):
- **Rows are the leaves** (`memories.parent_id` -> `tree_nodes.id`, NULL =
 the implicit per-namespace root) instead of the paper's separate leaf class.
- **One expansion per insert max** — the paper's re-expansion loop can build
 chains; ours caps placement work (dial `_EXPAND_MAX`, boot-validated).
- **Structure inside the write txn (pure SQL), summary + encode AFTER
 commit** (M6 lock rule): embedding/LLM work never holds the write lock.
- **Aggregation recomputes from the CURRENT living children** on every
 merge/forget, so pruning cannot leave stale aggregates. Default heuristic
 heads; `FORSETI_MEMTREE_AGG=llm` folds pairwise via valhalla (paper A.1.2
 prompt, 8 s cap, any failure -> heuristic: a down box never blocks writes).
- **One tree per namespace**, not per visibility set: a shared-visibility
 tree would leak private text into shared-visible summaries.

Crew integration findings:
- **herdr socket API has NO per-pane env** (`herdr api schema` grep: zero
 matches for env): the crew namespace reaches pi panes through the runner's
 `.forseti/memory-crew` hand-off file (+ `FORSETI_MEMORY_CREW` env for
 manually started panes); pi-extension resolves env-first, file-second.
- Crew namespace = `crew-<name>` (service normalizes `req.agent` when
 `crew` is set); ns capped at 26 chars in schema so `crew-<ns>` fits the
 agent limit. Recall WITHOUT the crew arg hides crew rows (isolation).
- Auto-recall prepends a `## Shared memory` block AFTER the prompt template
 render (query = `memory_query` else node name + prompt head); auto-write
 posts capped node output on node_done from a fire-and-forget goroutine.
- Live gates: scratch-service probes 7/7; `scripts/memory-eval.sh` 28/28
 (5 new P14 rows: internal-node formation, aggregate summary, crew
 visibility, crew isolation, forget-collapse); Go suite green incl.
 MemoryClient payload tests.
