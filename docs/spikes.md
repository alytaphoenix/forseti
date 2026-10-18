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

## S27 — ttt terminal surface audit + focus-bridge primitives (Phase 15, 2026-10-09) ✅ resolved (source + live)

Source audit of ttt v1.6.0 (module cache) for the "run pi as a ttt terminal
tab" idea, then live verification of everything P15 consumes:

**ttt terminal (the Phase-16 gap, all source-verified):**
- No Lua API for the integrated terminal — plugin surface (internal/plugin/*)
  has ZERO terminal bindings; /exec remains the only automation channel.
- /exec input is FOCUS-GLOBAL: key/type/paste post tcell events to the focused
  widget (exec_script.go execKey/execType); `panel ID` addresses bottom panels
  (terminal/problems/references), NOT individual terminal tabs; `wait-for TEXT`
  polls RENDERED screen text (visible tab only).
- `debug` dump exposes per-tab raw PTY byte tails (DebugTerminal.RawTail,
  debug_dump.go:33) — ground-truth bytes, no ANSI parsing, pull-only.
- SpawnTerminal (app.go:291) hardcodes settings shell + workspace cwd; tab ids
  are `terminal-<N>`; the vertical tab bar renders Count/Active ONLY — tabs
  cannot carry labels. → full analysis + patch design: docs/ttt-terminal-api.md.

**Panel widgets that DO exist (v1.6.0, docs plugin-authoring.md + lua_panel.go):**
- `panel:list/panel:tree{items, on_select=function(node)}` — node table has
  {id,label,icon,badge,muted}; Enter/double-click activates (lua_panel.go:345).
- `ttt.set_status_item(side,id,text,{priority=,on_click=})` — clickable badge
  (sandbox.go:262). Both used by the P15 sidebar switcher + crew badge.

**herdr 0.9.3 focus mechanics (live, named session `sandbox` via the PTY
recipe):**
- `agent list` carries `pane_id` + `tab_id` per agent (herdrd.AgentInfo was
  right); the sidebar's brace-scan parser handles the real output.
- `agent focus <name>` → response shows `"focused":true` — the P15 switcher
  path, verified.
- `tab focus <id>` works; unknown id → `{"error":{"code":"tab_not_found"}}`
  exit 1 → sys.exec stdout empty → Lua `herdr_cmd` returns nil → the
  crew_jump fallback path fires as designed.
- Mid-run crew-status.json (throwaway crew, valhalla override): `nodes:
  [{name,status}]` + `tab:"w1:tR"` appear while running — the P15 contract.

## S28 — switchyard 0.3.0: embeddable server + route/header surface (Phase 18, 2026-10-15) ✅ resolved (source + live)

**Upgrade.** `cargo install --locked --force switchyard-server --version 0.3.0`
built on macOS (rustc 1.97.1, edition 2024; crate needs ≥1.96.1) in ~47 s. 0.3.0
published 2026-09-22; libsy/protocol/runner/translation/llm-client all 0.3.0 on
crates.io. Backup of the 0.2.0 binary kept at `~/.cargo/bin/switchyard-server-0.2.0.bak`.

**Config validation.** `switchyard-server --config X --dry-run` validates without
binding; serde is STRICT (missing/unknown fields are hard errors) → usable as a
pre-flight check inside our config generator. Caveat: `api_key_env` is resolved
at load, so dry-run needs the var set (keyless clients may omit it).

**Route types accepted (empirical probe matrix, all PASS):** `passthrough`,
`random`, `noop`, `plan_execute`, `llm_classifier`, `stage_router`, `auto`,
`composite`, `advisor` (plus `prefill_router`, feature-gated). `stage_router`
REQUIRES `picker`. `plan_execute` uses `capable_target`/`efficient_target` +
`planning_prompt`/`handoff_prompt`/`planner_reasoning_as_text` (**not**
planner/executor_target). Our existing emitted shape (stage_router +
picker/confidence_threshold) still validates unchanged → the 0.2.0 config is
0.3.0-clean.

**Key surface.** Route: `id/type/context_window/tool_calling/reasoning/vision`.
Target: `id/llm_client/system_prompt/extra_body/reasoning_effort`. Client:
`format/base_url/api_key_env?/timeout_ms/max_retries/forward_auth/extra_headers`.
Top-level `fallback_client` forwards unimplemented paths verbatim.
`tool_semantics.{observe,mutate,plan,new}` classifies **custom** tool names only —
built-ins (`read`, `grep`, `edit`, `todo_write`, …) are rejected ("already has
built-in semantics"). `subagents` (on passthrough/stage_router/composite) IS in
the 0.3.0 release despite the doc's "unreleased features" banner; an
llm_classifier subagent needs `mode` + `models{judge,capable,efficient,any}` +
`default_target` + `classify_trigger` + `max_output_tokens` + `prompt` +
`response_schema` + `policy{type="target_selector",selector}`; every
`models.<tier>` target must also appear in `models.any`. Sub-agent identity rides
`x-claude-code-agent-id` (Claude Code ≥2.1.139) — **pi does not send it**, so
subagent routing is unused unless we emit that header ourselves.

**Headers.** `x-switchyard-session-id` = normalized session id (session affinity
+ `GET /v1/routing/session-stats`); `proxy_x_session_id` = legacy fallback (0.2.0
name); `x-switchyard-origin` labels the client in the routing log;
`message_hash_fallback` covers clients with no session header (requires
`classify_trigger = "new_session"`).

**The Phase 18 enabler — the server is a LIBRARY.** `switchyard-server` ships
`[lib] switchyard_server`: `config::load_server_state(path) -> ServerState`;
`ServerState::from_runner(Runner)` / `.with_routing_log(path)` / `.models()` /
`.caller_auth_kind(model)`; `ServerRunOptions{addr,backlog,dry_run,
shutdown_timeout,tls}`; `BoundServer::bind(state,opts) -> BoundServer` with
`.local_addr()` (port 0 → OS-assigned) and async `.serve(shutdown)`; plus
`run_server`, `build_llm_router`, `build_switchyard_router`.

**Live proof (no separate server process).** New Rust crate `host/`
(`forseti-host`) embeds `BoundServer` and loads a TOML route — `--dry-run`
prints `ok models=sy-route`; bound to 127.0.0.1:8777, `GET /health` →
`{"status":"ok"}`, and `POST /v1/chat/completions` returned real content
("ROUTED-OK") from `valhalla-flash-next`. `/v1/stats` → `total_requests: 1` +
model attribution. `--routing-log` NDJSON record:
`{ts,route_id,algorithm,origin,session_id,model,tier,prompt_tokens,…}` →
per-node session attribution, which 0.2.0 could not do (the `runner.go:528`
"attribution impossible" note is now fixable).

**Decision (Phase 18).** Staged Rust host with embedded switchyard; Python
(laya/memory) stays isolated 127.0.0.1 services — NOT PyO3-embedded: that venv is
1.3 GB (torch + sentence-transformers) on Python 3.14, and embedding adds
libpython/GIL coupling while losing crash isolation + graceful degradation.

## S29 — pi → switchyard headers: the extension surface (Phase 18A, 2026-10-15) ✅ resolved (source + live)

**Question.** How does pi 0.99.1 let an extension inject HTTP headers on provider
requests, and which headers does switchyard 0.3.0 actually read back?

**pi hook (verified from `dist/core/extensions/types.d.ts`).**
`pi.on("before_provider_headers", (event, ctx) => …)` — `BeforeProviderHeadersEvent
{ type, headers: ProviderHeaders }` where `ProviderHeaders = Record<string,string|null>`.
Handlers **mutate `event.headers` in place**; the return value is ignored, and
`null` deletes a header. Companion `after_provider_response { status, headers }`,
and `ExtensionContext` exposes `model` (`{id, provider: ProviderId, …}`),
`sessionManager.getSessionId()`, `cwd`. So the gate is *provider id*, the session id
comes from the session manager, and no build step is needed (jiti).

**Which headers matter (verified `switchyard-protocol/src/metadata.rs`).** Correlation
fields are resolved through a precedence table (`HEADER_CONFIG`); the explicit
`x-switchyard-*` names are first, harness-native ones only fallbacks. Notably pi's
native `x-session-id` (`OPENCODE_SESSION_ID_HEADER`) sits **fifth** for `session_id`,
so an explicit `x-switchyard-session-id` wins. Other normalized fields exist
(`agent_id`/`parent_agent_id`/`is_subagent`/`agent_kind`/`agent_role`/`task_id`/
`task_kind`/`turn_id`/`session_final`) plus host-specific paths (Codex
`x-codex-turn-metadata.*`, OpenCode, Dynamo). `switchyard-server/src/routing_log.rs`
additionally reads the legacy `proxy_x_session_id`, and the response carries
`x-model-router-selected-model`. There are also `x-switchyard-intake-task` and
`x-switchyard-trial-id` request headers (recorded into the log as `task`/`trial_id`).

**Result — shipped `pi-extension/switchyard.ts`.** On `before_provider_headers`, gated
to `provider === switchyard`, it sets `x-switchyard-session-id` +
`proxy_x_session_id` (= pi's session id), `x-switchyard-origin`
(`FORSETI_SWITCHYARD_ORIGIN`, default `pi`), `x-switchyard-request-id` (uuid/request),
and opt-in `x-switchyard-agent-id`; `after_provider_response` captures the served
target for a `/switchyard` command.

**Live proof (2026-10-15).** `pi -a -ne -e ./pi-extension/switchyard.ts --model
switchyard/forseti-auto -p "Reply with exactly: PI-ROUTED-OK"` against `host/`
(`forseti-host`, embedded switchyard, route `forseti-auto` → `valhalla-flash-next`)
answered `PI-ROUTED-OK`, and the routing log recorded
`{route_id:"forseti-auto", algorithm:"passthrough", origin:"forseti-e2e",
session_id:"…", model:"valhalla-flash-next", …}`. Re-running with
`FORSETI_SWITCHYARD_ORIGIN=forseti-e2e FORSETI_SWITCHYARD_AGENT_ID=node-alpha`
changed `origin` to `forseti-e2e` — the header came from the extension, not pi's
built-in affinity path. (The routing log has no `agent_id` column; agent identity is
consumed by algorithms/telemetry, not the log record.)

## S30 — crew → forseti-host: supervision + the full route surface (Phase 18B/18C, 2026-10-15) ✅ resolved (source + live)

**Which binary the crew runs (18B).** `crew/internal/switchyard.ResolveRouter`
prefers, in order: `FORSETI_CREW_ROUTER_BIN` (explicit path; kind from basename
unless `FORSETI_CREW_ROUTER=switchyard-server` forces it) → a `forseti-host`
built in this repo (walking up from the CWD to the repo root that owns
`host/Cargo.toml`, `target/release` then `target/debug`; `FORSETI_HOST_BIN`
overrides) → `forseti-host` on `$PATH` → legacy
`FORSETI_SWITCHYARD_BIN`/`~/.cargo/bin/switchyard-server`/`$PATH`. The two
routers take different flags, so `Router.Args` is kind-aware:
`--addr 127.0.0.1:<port> --routing-log <ndjson>` (ours) vs
`--host/--port/--routing-log-file` (upstream). Readiness is unchanged
(`/health` within 15 s), and the run log now names the router kind on failure.

**Dry-run is the validation seam.** Both binaries accept `--config X --dry-run`.
The crew runs it twice: as a pre-flight inside `Start` (before binding a port, so
a bad routes file fails with the router's own message instead of "exited early"),
and in `forseti-crew validate` via `switchyard.DryRunRoutes`. Two facts make the
CLI-side dry-run robust: (1) unknown/changed providers are substituted with a
placeholder endpoint (the dry-run never dials), and (2) `api_key_env` is resolved
by the router at LOAD time through `std::env::var` — an empty value is a hard
error ("api_key_env … is empty") — so DryRunRoutes resolves real keys when it can
and otherwise passes a stub. Live: a bogus `options` key is rejected with
`unknown field \`not_a_switchyard_key\`` at `[routes.broken]`.

**18C route surface (source-verified, `switchyard-runner/src/algorithm.rs`
`AlgorithmSpec`).** Types: `noop`, `random{targets,weights,seed}`,
`passthrough{target,subagents}`, `plan_execute{capable_target,efficient_target,
planning_prompt,handoff_prompt,planner_reasoning_as_text}`,
`llm_classifier{…mode,classifier_target,strong_target,weak_target,base_threshold,
threshold_step,…}`, `stage_router{tiers…,picker,classifier,subagents}`,
`auto{capable_target,efficient_target}` (= stage_router preset efficient_first +
0.5, no knobs), `composite{classifier,stage}`, `advisor{executor_target,
advisor_target,…}`, `prefill_router{…}`. serde is `deny_unknown_fields` with
`tag = "type"`, `rename_all = "snake_case"`.
- `advisor` REQUIRES `advisor_target` (dry-run rejection), `llm_classifier`
  REQUIRES `base_threshold` (dry-run rejection) — both confirmed live.
- forseti's mapping: tier types (`stage_router`, `auto`, `plan_execute`,
  `llm_classifier`) derive `<id>_cap`/`<id>_eff` (or `_strong`/`_weak`) from
  `capable`/`efficient`; `passthrough` uses `capable` as its single target;
  `random` uses `[cap, eff]`; a new optional `judge: provider/model` mints
  `<id>_judge` and auto-wires `classifier_target`; `auto`/`noop` want no knobs.
  Everything else is **option-driven**: the whole route table comes from
  `options:` and may reference forseti's generated target names (documented in
  `crew/examples/crew-routes.yaml`). Reserved keys (`id`, `type`, and any typed
  key of that type) are rejected so `options` can never silently win.
- Route-level `context_window`/`tool_calling`/`reasoning`/`vision` and nested
  sub-tables (`tool_semantics`, `classifier`, `subagents`) all pass through
  `options`; nested maps are emitted as TOML **inline tables** (serde parses them
  identically to sub-tables) by a small dependency-free value encoder.
- `crew/examples/crew-routes.yaml` carries one route of each shape and validates
  against the real router.

**Routing-log records carry real attribution.** `switchyard-server/src/routing_log.rs`
`RoutingRecord`: `ts, route_id, algorithm, origin, task, trial_id, session_id,
model, tier, prompt_tokens, cached_tokens, cache_creation_tokens,
completion_tokens, reasoning_tokens, total_tokens` (missing fields default). The
runner's `route_decision` event now carries route id, algorithm, tier, session
and origin alongside the model — the previous `{model,total_tokens,ts}` shim (and
the "per-node attribution impossible" note) is gone. The panes' pi sessions are
distinguishable by `session_id`; mapping one to a node name needs a per-pane env
the herdr socket does not expose (S26), so the header channel stays opt-in via
`FORSETI_SWITCHYARD_AGENT_ID`.

**Live proof (`FORSETI_LIVE_ROUTER=1 go test ./internal/switchyard/`, 20.5 s).**
`switchyard.Start` with a `stage_router` over `valhalla/valhalla-flash-next`:
resolved `host/target/debug/forseti-host`, dry-ran the generated TOML, bound a
free port, answered `/health`, served a real
`POST /v1/chat/completions` (`ROUTED_OK`), and appended
`{route_id:"live-pool", algorithm:"stage_router", model:"valhalla-flash-next"}`.
`crew/examples/crew-routes.yaml` (5 route shapes incl. an advisor naming
generated targets) validates against the same router.

## S31 — MetaGPT role/SOP contracts + the forseti SOP layer (Phase 19, 2026-10-15) ✅ resolved (source + live)

**Why MetaGPT.** Forseti's crew layer had ad-hoc prompts and hand-written edges.
MetaGPT (MIT) is the reference for *role archetypes* (a profile/goal/constraints
prefix, a set of actions) and *SOPs* (role pipelines whose hand-offs are typed
messages). Forseti borrows the SPEC and the vocabulary, not the code: the SOP
text is our own, nothing is vendored, and every pattern compiles down to the
existing agent graph so the P13 scheduler, phases, bounded loops, run log, watch
TUI and the routing layer are reused unchanged. Source read (pinned, scratch):
`metagpt/roles/role.py`, `roles/{engineer,qa_engineer,researcher}.py`,
`actions/{write_review,write_prd_review,design_api_review,write_code_review,
execute_task,write_test,project_management}.py`, `team.py`,
`environment/base_env.py`, `strategy/planner.py`.

**Role contract (source-verified).** `Role{name, profile, goal, constraints,
desc, actions[], is_human, enable_memory}` carries a `RoleContext{msg_buffer,
memory, working_memory, state, todo, watch, react_mode, max_react_loop}`; the
prompt prefix is the literal template
`"You are a {profile}, named {name}, your goal is {goal}. the constraint is
{constraints}. "` installed as each action's system prompt (`_get_prefix` →
`Action.set_prefix`). `watch` is a set of message TAGS (`cause_by`) — a role
observes only messages whose `cause_by` is in its watch set, so subscription is
declared by artifact type, not by sender.

**React modes.** `REACT` (the LLM picks a numbered state, `-1` = done),
`BY_ORDER` (actions run in list order), `PLAN_AND_ACT`. The loop is
`run → _observe → react → _think → _act → publish_message(cause_by=action)`.

**Verdict actions (our verdict-edge source).** `WriteReview` emits a structured
`LGTM`/`LBTM` token (its ActionNode output IS the bare token), and the
engineering roles loop on it: `WritePRDReview`, `DesignAPIReview`,
`WriteCodeReview`, `WriteTest` (a `VERDICT`-ish pass/fail), and
`execute_task`/`write_test`/`run` in the QA engineer close a bounded fix loop.
That is the shape forseti's `verdict:<label>` edges reproduce.

**Messages are typed.** `Message{content, role, cause_by, send_to, ...}` —
`environment/base_env.py` routes by `cause_by` membership in a role's watch
list. Forseti keeps the graph as the source of truth and DERIVES those
subscription edges from declared artifact types (`watch:`/`writes:`), so pub/sub
is compiled rather than interpreted.

**Engine finding — back edges had to be classified, not guessed (P19 fix).** The
P13 rule ("an edge is a back edge when its target reaches its source") marks
EVERY edge of a cycle: in `x→a→b→a` both `a→b` and `b→a` were "back edges", so
`nodeReady` skipped both and the loop BODY dispatched at t0 out of order. A
multi-agent review loop cannot be expressed on top of that. Forseti now
classifies with a DFS from the crew's entry nodes (target on the current DFS
path ⇒ back edge), so only the true loop-back is deferred; an ambiguous cycle
(two nodes, no other way in) is broken explicitly with `loop: true` on the
loop-back edge. The entry check still counts every non-`loop` edge, so an
all-cycle crew is still rejected ("no entry node"). Proven by
`schema/cycle_test.go` + `runner.TestNodeReadyCycleBodyWaitsItsGate`.

**Engine finding — the implicit edge cap of 1 stalled every multi-edge loop.** The
P13 scheduler capped an edge's firings at `max_visits` (default **1**). So in a
reviewer loop only the loop-back edge (`reviewer→subject`, `max_visits: N`)
could re-fire; the body edge (`subject→reviewer`) fired once and was then
permanently skipped ("max_visits reached"), so a revised subject never reached
the reviewer again — the loop performed exactly one revision and stopped. P13
only ever exercised the capped edge (`TestFireEdgeReArmsSettledTarget` fires the
edges by hand), so the gap was invisible. Fix: an edge with no `max_visits` is
now UNCAPPED — it fires on every settle of its source. Termination is still
guaranteed because validation requires every cycle to carry at least one
`max_visits` edge and a re-dispatch only happens when an edge fires (every trip
around a cycle consumes a capped edge); acyclic crews are byte-for-byte
unaffected (a node settles once). Proven by `runner.TestUncappedEdgeRefiresLoopBody`
+ `TestCappedEdgeStillStops`.

**Pattern chaining resolves to the target's ENTRY nodes.** `then: <pattern-id>`
would otherwise add one edge to `<id>-<first-node>`, which for a panel is
`<id>-critic-1` — critics 2..N kept their plain subject edge and ran in parallel
with the previous stage, and the judge (AND fan-in over the critics) could accept
before the previous stage's verdict arrived. `then` now resolves a pattern id to
its entry nodes (all critics, the implementer, the reviewer), so fan-in AND
makes the whole next stage wait on the verdict edge. In `crew-sop.yaml` this is
what sequences reviewer_loop → security_review → panel → executor over one
shared subject; `validate -explain` shows the double incoming edges.

**The forseti SOP layer (as built).**
- Roles (`crew/internal/schema/role.go`): `Action{Name, Desc, Expected,
  Instruction, Reads, Writes}` + `Role{Name, Profile, Goal, Constraints, Desc,
  Tier, Watch, Writes, ReactMode, Actions[], Instruction}`; `Role.Prefix(name)`
  is the MetaGPT template verbatim (minus the clause when constraints are
  empty). Built-ins: `planner`, `researcher` (BY_ORDER: search → gather →
  synthesize), `engineer`, `reviewer` (lgtm|lbtm), `security_reviewer`
  (pass|fail), `adversarial_reviewer`, `judge` (accept|revise), `verifier`
  (pass|fail). A crew re-declares a name to override it. Roles resolve their
  tier through a crew-level `models: {efficient, capable}` map, so an archetype
  never pins a churning model id; `tier` without `models:` is a validation error.
- Patterns (`crew/internal/schema/pattern.go`): `id`, `pattern`, `subject`,
  `members`, `params`. Kinds: `reviewer_loop` (subject → reviewer, LBTM loops,
  LGTM continues), `security_review` (same shape, security archetype, `lenses`),
  `adversarial_panel` (`critics` 2–7 × `lenses` → judge, revise loops, accept
  continues), `executor` (subject/task → implementer → verifier + real shell
  checks, fail loops). Synthesized nodes are `<id>-<part>`
  (`rl-reviewer`, `ap-critic-2`, `ap-judge`, `ex-implement`, `ex-verify`);
  `then:` chains to a declared node or another pattern id — a pattern id
  resolves to that pattern's ENTRY nodes (all critics of a panel, the
  implementer of an executor), so the next stage's entries all wait on the
  verdict edge (fan-in AND). A declared `members:` mapping reuses an
  existing node instead of synthesizing one. In a phased crew a synthesized
  node joins its subject's phase, so pattern loops stay intra-phase.
- Typed pub/sub (`crew/internal/schema/bus.go`): `watch:` declares the artifact
  types a node consumes and `writes:` what it publishes (default: the node's own
  name); Validate DERIVES one `producer → watcher` idle edge per type, skips
  edges the user already declared, and errors when a watched type has no
  producer. `{{ artifact "type" }}` (runner template function) resolves the
  in-run producer's output, else `.forseti/bus/typed/<type>.md` from the last
  run.
- Verdicts: `ParseVerdict` takes the LAST `VERDICT: <label>` line (pi panes echo
  the prompt, which names the expected verdicts) and also accepts a bare
  `LGTM`/`LBTM` on its own line. `verdict:<label>` edges fire only on that
  label; laya dials (`min_confidence`/`state_file`) are rejected on them, and
  loop edges need `max_visits`.

**Gates.** `validate -explain` renders the resolved graph (nodes, archetypes,
tiers, artifact subscriptions, per-edge gates, phases, checks); the smoke gate
asserts all four pattern kinds expand and that an entry node and a
`verdict:lbtm` edge survive expansion. Unit tests: `schema/{role,pattern,bus,
cycle}_test.go` + runner verdict / cycle-body / uncapped-edge-refire tests.
Example: `crew/examples/crew-sop.yaml` (planner → architect → reviewer_loop →
security_review → adversarial_panel(3) → executor).

## S32 — the synthesized prompts must not use field access (Phase 19, 2026-10-16) ✅ resolved (live)

The first **live** SOP run (`forseti-crew run -f crew/examples/crew-sop.yaml
--session sandbox --worktree sop-live`, valhalla) died one node in, before the
model was asked anything:

```
node_start  rev-reviewer  pane=w1:p1D
node_failed rev-reviewer  prompt template: template: rev-reviewer:5: function "design" not defined
```

Three facts, in order:

1. **A bare name is a function.** The reviewer prompt bound its subject as
   `{{ draft }}`, and text/template parses a dot-less action as a FUNCTION call
   — `function "draft" not defined` (not the "map has no entry" a missing
   variable gives). The runner's upstream map is keyed by node name, so the
   reference is `{{ .draft }}`; the dot was simply missing. Every hand-written
   example already used the dotted form, so the failure was local to the
   generated prompts.
2. **The dot cannot be used for a synthesized name.** `{{ .ap-critic-1 }}` is a
   PARSE error — `bad character U+002D '-'` — because field access only accepts
   an identifier. Every pattern-synthesized node is `<pattern>-<part>`
   (`rl-reviewer`, `ap-critic-1`, `ex-implement`), i.e. every generated
   reference is un-writable with the documented syntax. Node names legitimately
   contain `-` (they are herdr names), so this is a general template-layer trap,
   not a pattern-only one.
3. **The judge could never bind the artifact anyway.** The panel's judge
   renders the artifact under judgement, but it edges only from the critics —
   the subject is not one of its incoming edges, so the upstream map has
   nothing for `{{ .<subject> }}` and the prompt would have rendered an EMPTY
   artifact (or an unapproved lookup fallback). The judge genuinely needs a
   value that is not a predecessor.

Resolution: a runner template function, `{{ node "name" }}` (the name is a
string literal, hyphens included), returning the node's latest text — `""`
until it has finished, matching the unconditional upstream binding of P13
BUG-06. All six generated references (subject, subject in the panel, the panel
row per critic, the executor's task, the verifier's implementer, the judge's
subject) use it. `{{ .<node> }}` for identifier-safe names and
`{{ artifact "<type>" }}` are unchanged.

Guards: `schema.TestPatternPromptsRender` parses+renders every node of a crew
with all four patterns under the runner's rules (its `node` stub flags a
nonexistent name as `UNKNOWN:`), so a bare or hyphenated-broken reference fails
a unit test instead of a live run; `pattern_test.go` asserts the generated
prompts use the function form. The smoke gate was already green — this class of
bug is invisible to `validate`, because template PARSE happens at dispatch,
inside `runNode`.

Lesson for the pattern layer: the generated prompt is a *runtime artifact*
(the templates are rendered node-by-node at dispatch, not at Validate), so
every generated prompt needs a parse/render test in the schema package, not
just an expansion assertion.

Three more live-run findings, same session:

- **The entry node needs the request.** The planner archetype's instruction
  reads "write the plan for the request below"; `crew-sop.yaml`'s `plan` node
  had no prompt and no request anywhere, so the model invented the task (plan
  23k chars, then design 49k). The reviewer then dispatched on a 49k-char
  subject and hit a 4-minute node timeout (`agent.prompt+wait: timeout: timed
  out waiting for agent status`) without producing a verdict. The example now
  states a small, self-verifying request on the entry node.
- **`checks:` run in the crew cwd — the repo ROOT, not the Go module.**
  `runCheck(ch.Run, r.effectiveCwd)`, and with `--worktree` the effective cwd
  is the worktree checkout. There is no `go.mod` at this repo's root (the
  module is `crew/`), so the example's `go build ./...`/`go test ./...` could
  only ever FAIL. They now assert the demo's own artifact, independent of the
  layout.
- **A killed run leaves herdr agents behind.** A sandbox session restarted
  from persisted state still had the old agents registered, so every node of
  the next run failed with `agent.start: agent_name_taken` (agent names ARE
  the node names). Recovery: `herdr session stop <name> && herdr session
  delete <name>` (the runner re-bootstraps the session) plus `git worktree
  remove --force <path>` and `git branch -D <branch>`.

## S33 — OpenCode Go's free tier as the test provider (2026-10-17) ✅ resolved (live)

The LAN box is reserved for the user's clustering work and the paid OpenCode Go
ids are unfunded, so the test path moved to the Go plan's **free** ids.

- **Endpoint gates**: `https://opencode.ai/zen/go/v1` (OpenAI-completions)
  **requires an `x-opencode-session` header** — without it every request 400s
  `{"type":"MissingSessionID"}`. pi emits it natively (provider-attribution),
  so only hand-rolled probes must add it. A raw `curl` needs
  `-H "x-opencode-session: <uuid>"` *and* the bearer key, or the error is
  indistinguishable from a bad model id.
- **`GET /models` lists 37 ids**, but availability ≠ authorization: the paid
  ids (`glm-5.3-flash`, `space-bunny`, …) all returned
  **402 `Insufficient account funds`**, `big-pickle` returned 400
  `Model is unavailable`, and the contributor ids 400'd as well.
- **Two ids are genuinely free and live-verified**:
  `beam-free` and `longcat-2.5-preview-free` (chat 200; tool-calling 200 with a
  proper `tool_calls` payload; `max_tokens: 64000` accepted; `beam-free` took a
  ~180k-token prompt). Both are **reasoning** models (`reasoning_content` in
  the response, and `content: null` with `finish_reason: "length"` when
  `max_tokens` is small — the S15 empty-content failure mode, on a *hosted*
  model).
- A probe also saw a transient `429 Endpoint is unavailable` on the longcat id
  — free routes are rate-limited; the runner's one-shot empty-settle re-prompt
  absorbs a single empty settle, and `FORSETI_CREW_MODEL` stays the escape
  hatch.
- **Consequences in the repo**: example crews and the smoke gate pin
  `opencode-go/beam-free` (routed examples use `longcat-2.5-preview-free` as
  the capable tier, so the routing demo still shows a per-tier choice);
  `runner.emptySettleProne` now covers `halogen|valhalla|beam-free|longcat`
  (reasoning models with the S15 quirk) instead of the LAN box alone; the live
  switchyard gate dials `beam-free`. Live proof: `scripts/crew-smoke.sh` PASSes
  with **no env override** (checked + phased crews, every `node_start`
  `"model":"opencode-go/beam-free"`).

## S33b — checks re-run per visit (2026-10-17) ✅ resolved (unit)

P13 keyed a check's done-flag on the check index (`check:<i>`), so a declared
`checks:` entry ran on its after-node's **first** settle only: an executor
`max_fixes` round could never clear a failure, and a later PASS was invisible
to the exit code (the documented P19 "known limitation").

Fix: the flag is keyed on the after-node's **visit**
(`check:<i>#<visits>`), and a passing re-run **deletes** the
`"<name>_failed"` marker, so the exit code reflects the latest iteration. Same
visit re-checks stay no-ops (idempotent), which keeps the old behavior for
acyclic crews (`visits == 1`). `runner_test.go:
TestChecksRerunWhenAfterNodeIsRevisited` covers missing-artifact → fail, same
visit → no-op, fix round → pass + marker cleared.

## S33c — a verdict gate needs acceptance criteria (2026-10-17) ✅ resolved (live)

First live SOP run on the free model: the reviewer loop **never** passed.
`design` (16k chars) → `rev-reviewer` `VERDICT: LBTM`; after the bounded
revision (`design` 30k chars) → `LBTM` again → the `verdict:lgtm` edge to the
security stage skipped → every downstream stage skipped. Nothing was wrong with
the engine: the generated reviewer brief is "list defects worst-first, each with
what is wrong, why it matters and the fix" — a reasoning model can always find
*an* objection (scope, naming, tooling, "consider adding tests"), so a
criteria-less verdict gate is a coin flip that biases to LBTM. This is a
property of the SOP, not of the model: MetaGPT's own SOPs carry acceptance
criteria (WriteReview judges against the requirement; the QA Engineer judges
against the test run).

Resolution: `params.criteria` on every verdict-gated pattern
(`reviewer_loop`, `security_review`, `adversarial_panel`) renders as
"Acceptance criteria — judge against these and nothing else; an objection that
does not violate them is out of scope: …" in the reviewer / critic / judge
prompts. The crew file owns the standard; the model only decides against it.
`crew-sop.yaml` states the one-line criteria for the design review, the
no-security-surface criterion for the security lens, and the
objections-out-of-scope criterion for the panel —
`pattern_test.go: TestPatternCriteriaLandsInEveryVerdictPrompt`.

## S33d — the full SOP run, live on a free hosted model (2026-10-17) ✅ resolved (live)

After `criteria:` landed, `forseti-crew run --headless -f
crew/examples/crew-sop.yaml --session sandbox --worktree sop-free` (10 agents,
20 edges) ran end to end in **45 s** and exited 0:

```
11:23:11 node_start  plan
11:23:14 node_done   plan                      6543 chars
11:23:14 node_start  design
11:23:17 node_done   design                   19462 chars
11:23:17 node_start  rev-reviewer
11:23:22 node_done   rev-reviewer             47738 chars
11:23:22 verdict     rev-reviewer             lgtm
11:23:22 edge_skip   design   rev-reviewer>design when "verdict:lbtm" not matched
11:23:22 node_start  sec-security_reviewer
11:23:26 verdict     sec-security_reviewer    pass
11:23:26 edge_skip   design   sec>design when "verdict:fail" not matched
11:23:26 node_start  adv-critic-1/2/3         (parallel)
11:23:32 node_done   adv-critic-*             accept, accept, revise
11:23:32 node_start  adv-judge
11:23:39 verdict     adv-judge                accept
11:23:39 node_start  run-implement            (executor)
11:23:45 check_pass  run-check-1 / run-check-2
11:23:45 node_start  run-verify
11:23:56 verdict     run-verify               pass
11:23:56 run_end     done=10 cost=$0.0000
11:23:56 teardown    worktree removed
```

All four pattern legs proven against a live model: the reviewer loop, the
security lens, the three-critic panel fan-in (dispatched concurrently the
instant the verdict edge fired) + judge, and the executor's implementer →
**real shell checks** → verifier. Every `node_start` carries
`"model":"opencode-go/beam-free"` — a hosted free id, no LAN box. The four
`edge_skip`s are the corrects: each negative-verdict edge simply did not fire.

One live `node_retry` is worth keeping: `run-verify` settled with a **negative
pane delta** (`-278 chars` — the pane shrank), the extended reasoning-model
empty-settle guard re-prompted once, and the retry produced the clean
`VERDICT: PASS`. So the guard is load-bearing on the hosted free reasoning ids
too, not just the LAN box.

## S33e — driving the TUI in a real pane: readiness, not focus (Phase 19, 2026-10-17) ✅ resolved (live)

Extending the P11 in-pane driver to the new role/pattern forms
(`scripts/crew-tui-drive-roles.py`) failed twice before it passed. The cause was
**not** what it first looked like, and both the wrong and the right answer are
worth recording:

- **Wrong guess (retracted):** that `pane.send_text`/`send_keys` need a FOCUSED
  pane. They do not. Probe: a tab created unfocused (`layout.apply`, `focus:
  false`), `pane.send_text` + `send_keys ["enter"]` → the shell ran the command
  and wrote the file (`UNFOCUSED_OK`); the pane's `focused` field stayed
  `false` throughout. All three transports (`send_text`, `send_keys`,
  `send_input`) return `ok` and deliver to an unfocused pane.
- **Real cause:** the readiness wait keyed on the **crew name**, which also
  appears in the shell's echo of the typed command line
  (`.../p19-tui-roles-<rand>/crew.yaml`). The wait returned while the shell was
  still echoing, so the next keystroke (`b`) arrived as **type-ahead**: the TUI
  installs its input reader during terminal setup and drops input queued before
  that, so the build key was lost and the screen stayed in `view` mode. Adding
  `pane.focus` "fixed" it only because it changed the timing — the same drive
  passes with no focus call once the readiness signal is correct (verified: the
  final script has no `pane.focus`).

Rules for TUI drives in a pane:

- readiness = a string only the TUI emits (`"CREW: <name> [view]"`), never a
  path/name the shell also echoes; keep the scratch dir prefix and the crew name
  distinct so the two can never alias;
- a short settle delay after readiness is still worth it — `pane.wait_for_output`
  returns on the first matching frame, which can precede the fully drawn screen.

Live run (2026-10-17, `beam-free`, `scripts/crew-tui-drive-roles.py`): build-mode
`l` (role form, 7 fields) and `g` (pattern form, 5 fields) plus the agent form's
`role` field were driven in a real sandbox pane; the saved file came back in
SOURCE form — `roles:` + `patterns:` + `role: architect`, **no** synthesized
`rev-reviewer` — and `forseti-crew validate -f` accepted it (exit 0). TUI
authoring of roles/patterns round-trips through `MarshalSource`/`Load` exactly
like a hand-written crew.

## S34 — porting the crew schema to Rust: the oracle harness and its edges (Phase 18D-1, 2026-10-17) ✅ resolved (unit + live CLI)

Stage 18D-1 moves `crew validate` into the Rust host (`forseti-host crew
validate -f FILE [--explain]`, `host/src/crew/*`) with the Go engine as the
compatibility oracle. Findings:

- **The oracle must be a *recorded* comparison, not a spawn.** `scripts/
  crew-host-oracle.py` captures `{file, argv, exit, stdout, stderr}` per crew
  from the Go binary into `host/tests/golden/<kind>/*.json`; the Rust side is a
  plain integration test (`host/tests/crew_oracle.rs`) that replays them, so
  `cargo test` needs no Go toolchain and no network. `--check` re-captures and
  diffs byte for byte. Corpus = 8 examples (minus the pinned `crew-sop.yaml`,
  which needs 18D-2) + 2 valid fixtures + 50 invalid fixtures, one rule each:
  **60 records**. (S35 grew it to 85 once 18D-2 landed.)
- **The Go `validate` shell-outs to the router, so the oracle needs a stub.**
  With `routes:` present, `validate` runs `forseti-host --config … --dry-run`.
  Pointing `FORSETI_CREW_ROUTER_BIN` at an `exit 0` script removes the dry-run
  note and makes the capture router-build-independent. Verified that the stub is
  load-bearing: a failing stub turns the same command into
  `INVALID (routes): forseti-host --dry-run: …: exit status 3`. (The Rust engine
  has no dry-run yet — a later stage; `validate` in Rust therefore cannot
  diverge here.)
- **`explain`'s arrow line is not the obvious `%-24s`.** The incoming-edge line
  is `"  %-24s   ← %s\n"` (three spaces, a literal `←`, then the label) — a
  field-width port alone is off by one column. Everything else is plain `%-24s`
  / `%-20s` / `%-10s`. 8/8 base examples now match byte for byte, including
  `← planner [re:PLAN_READY]` and `← planner [laya:choice:…]`.
- **Go subtleties the port forced out of hiding** (unit tests pinned them):
  `Crew.Entry()` counts loop edges as incoming, while the *validation* entry
  check deliberately ignores `loop: true` edges — so `a→b, b→a(loop)` has an
  empty `Entry()` yet still validates. A self-loop (`a→a` with `max_visits`) is a
  normal edge, so a crew whose only node self-loops has **no entry** and is
  rejected; it needs a separate entry node. `BackEdges` DFS-classification from
  the roots (not "target reaches source") means `c→a→b→a` yields exactly one
  back edge (`b>a`) — the old rule marked both cycle edges.
- **Documented divergences, both parse-level and outside the corpus:** bad user
  regexes report the Rust `regex` crate's message (Go reports RE2's) and a
  malformed YAML document reports serde_norway's (Go yaml.v3's). The Go
  *prefixes* are preserved (`edge N: bad regex %q: …`, `parse crew.yaml: …`), and
  the rule still fires — unit tests assert both. Only *validation* error strings
  aim for byte parity.
- **Defaults are applied during validation, not at use** (Go does the same), so
  `explain` shows them: `kind: pi`, `when: idle`, `check-<after>-<i>` names,
  memory `recall_k 3` / `write_max_chars 4000`, route `picker
  efficient_first` / `confidence 0.5`. The Rust `memory_ns()` mirrors
  `MemorySpec.NS`: the memory block absent or `namespace:` empty → the crew name.

## S35 — 18D-2: `prompts`, the parity surface `--explain` cannot see (Phase 18D-2, 2026-10-18) ✅ resolved (unit + recorded diff)

Stage 18D-2 ports the Phase-19 authoring layer (roles, SOP patterns, verdicts)
into the Rust engine. The oracle already diffs the resolved *graph*
(`validate --explain`), but the patterns' value is largely in the **prompt text**
they synthesize — and a prompt is invisible to `--explain`. Findings:

- **A new Go subcommand is the cheapest honest fix.** `forseti-crew prompts -f
  FILE` loads + validates the crew and prints `{"agents":[{name,prompt},…]}` after
  expansion. The oracle captures it into each golden record under `"prompts"`
  (`{argv,exit,stdout,stderr}`), and `crew_oracle.rs` loads the crew through the
  Rust library and diffs `name`+`prompt` node-by-node — parsed, so JSON escaping
  is irrelevant. This keeps the existing `validate` contract untouched (no new
  template engine needed before 18D-5).
- **The prompt leg is load-bearing, not decorative.** A one-word mutation
  (`worst-first` → `worst first`) in `pattern.rs` fails two goldens
  (`examples/crew-sop`, `valid/sop-patterns`); the same mutation is invisible to
  `--explain`. All ~10 synthesized templates match Go on the first capture, so the
  ported code is faithful rather than merely self-consistent.
- **What the Rust port had to mirror exactly** (learned while diffing): only the
  *review-loop* reviewer prompt is `strings.TrimSpace`'d, the panel's judge prompt
  is not; the executor splices `params.task` **after** the `{{ node }}` binding
  and falls back to `Implement the task.` only when both are empty; the verify
  prompt's bullet list is `- cmd\n` per item followed by a blank line; the panel
  extends `lenses` with `lens N: your own strongest objection` when `critics`
  exceeds the declared lens count; `verdictLabel`/`badLabel` take the text after
  the **last** `:`.
- **`explain` additions.** A roled node is tagged `role=<id>(<Profile>)` (the
  profile only when the archetype has one), and a `patterns:` block
  (`"  %-10s %s subject=%s\n"`) lists each pattern's id/kind/subject after the
  nodes and before the checks.
- **Validation-order parity is what makes the 22 new fixtures pass on the first
  try.** `Crew::validate` now runs `expand_patterns()` → `RolePack::validate` →
  `crew has no agents` → … in the Go order, so a role/pattern error always wins
  over a graph error for a crew that is broken in both ways.
- **One nondeterminism conceded:** Go iterates `p.Members` (a map) when checking
  for unknown members, so a pattern with *two* bad members reports an arbitrary
  one; Rust reports the `BTreeMap` (lexicographic) first. Fixtures use a single
  bad member, so the corpus stays deterministic; the divergence is outside it.
- **Corpus:** 85 records = 9 examples + 4 valid + 72 invalid. The valid additions
  exercise role overrides + pub/sub (`sop-roles.yaml`) and a pattern chain with a
  reused judge/implementer member (`sop-patterns.yaml`); 22 invalid fixtures cover
  the role rules (unknown role, bad tier/react_mode/action `expected`, missing
  `models:`, no instruction fallback) and the pattern rules (bad/long/duplicate
  id, unknown pattern, unknown param, `max_revisions`/`critics`/`max_rounds`/
  `max_fixes` ranges, missing subject/task, bad subject/member/`then`, node
  collision, edge collision, a synthesized node's missing model tier).

## S36 — can pi host a pluggable side panel? (design question, 2026-10-18) ✅ resolved (source: pi 1.0.3 dist + docs)

Question carried from the Phase-15/19 planning: *"Can pi host a pluggable side
panel (ttt bridge, crew TUI, latency), or should we fork pi into the forseti
agent?"* Investigated against the installed package
(`~/.hermes/node/lib/node_modules/@earendil-works/pi-coding-agent`, **1.0.3** —
not 0.99.1 as AGENTS.md said; every forseti-relevant hook re-verified present:
`before_provider_headers`/`after_provider_response`, `registerTool`,
`registerCommand`, `typebox` bundled, `edit.details.firstChangedLine`).

- **pi has a real extension UI surface, but no docked side panel.** The
  authoritative type is `ExtensionUIContext`
  (`dist/core/extensions/types.d.ts`). `ctx.ui` offers `setWidget`, `setFooter`,
  `setHeader`, `setEditorComponent`, `setStatus`, `setTitle`, `custom`, plus
  dialogs (`select`/`confirm`/`input`/`editor`) and `onTerminalInput()`.
- **What maps to "panel", and its shape:**
  - `setWidget(key, content, {placement: "aboveEditor"|"belowEditor"})` — accepts
    a **component factory** `(tui, theme) => Component`, so arbitrary persistent
    content can live above/below the editor. Placement is only the main vertical
    column: there is no left/right dock.
  - `ctx.ui.custom(factory, {overlay: true, overlayOptions})` — a component with
    **keyboard focus**, positioned via `OverlayOptions` (`width`, `anchor:
    "right"`, `row`/`col`, `offsetX`, `margin`, `nonCapturing`, a `visible`
    predicate for responsive show/hide). This is the only shape that yields a
    focus-holding region coexisting with the transcript: it *draws over* rather
    than re-flowing, and `OverlayHandle` (`focus`/`unfocus`/`setHidden`/
    `getBounds`) arbitrates focus. `nonCapturing: true` gives a display-only
    right-docked panel that never steals the editor.
  - `setFooter(factory)` / `setHeader(factory)` — replace a whole region; the
    footer factory receives a `FooterDataProvider` carrying `setStatus()`
    entries (this is what the ttt crew-status badge already rides) plus git
    branch. `setEditorComponent` swaps the editor itself.
- **Component model is public and sufficient** (`@earendil-works/pi-tui`):
  `Component { render(width), handleInput?, handleMouse?, invalidate() }`,
  `Focusable { focused }`, `CURSOR_MARKER`, and composable `Container`/`VStack`/
  `HStack`/`Box`/`ScrollView`/`MouseRegion`. Docs warn: *do not create a second
  terminal renderer inside an extension* — compose these instead.
- **Answer:** no fork needed. A ttt-bridge / crew / latency panel is buildable as
  a pi extension — a `nonCapturing` right-anchored overlay (always-on display) or
  a `custom({overlay:true})` panel (interactive), fed by the same shared files the
  ttt plugin uses (`crew-status.json`, `.forseti/bus/*.md`, the memory/panes
  sockets). The one thing pi cannot do is a true split-pane with independent
  focus side-by-side with the editor; the overlay is the sanctioned approximation.
  Forking pi would buy that layout at the cost of losing upstream — not warranted.
- **Caveat (not yet exercised live):** widget/overlay focus routing and mouse
  behavior were read from types + docs, not driven in a live pi session; if the
  panel is built, prove the focus model with a real extension first (the
  `doom-overlay` / `custom-footer` examples in the package are the starting
  points).


## S37 — 18D-3: porting the herdr client, and the framing bug the Go one has (Phase 18D-3, 2026-10-18) ✅ resolved (source + unit + live)

The Rust engine needs the herdr socket client before it can schedule anything, so
18D-3 ports `crew/internal/herdrd/client.go` (523 lines, no Go tests) to
`host/src/crew/herdr.rs`. There is **no oracle for this layer**: `herdrd` speaks
to a live server, so there is no deterministic stdout to pin. The gate is instead
a real `UnixListener` in the temp dir with scripted replies (§Gates 18D-3).

- **Ported 1:1 (signatures and messages):** `socket_path`/`socket_path_for`
  (`HERDR_SOCKET_PATH` → `HERDR_SESSION` → `$HERDR_CONFIG_DIR|~/.config/herdr`;
  a named session is `<base>/sessions/<name>/herdr.sock`), `Client::at`'s stat
  (`herdr socket <p> not present (server running?): <err>`), the `crew-<prefix>-<n>`
  id shape off a mutex-guarded `seq`, `call`'s empty-id rule + the
  `bad response frame %q` / `reply id %q does not match request %q` checks, the
  `{"code":…,"message":…}` error rendering `<code>: <message>`, and every helper
  (`ping`, `agent_list`, `agent_read` → `result.read.text`, `agent_start`,
  `agent_prompt`, `agent_prompt_wait`, `agent_focus`, `agent_wait`,
  `focused_workspace`, `notification_show`, `agent_view_set`/`clear`,
  `pane_wait_for_output`, `worktree_create` → `(workspace_id, checkout_path)`,
  `worktree_remove`).
- **`agent_prompt_wait` has three settle shapes and the order matters**: the
  `wait_matched` frame carrying `event.data.agent_status`, herdr 0.9.3's live
  `{"type":"agent_prompted","agent":{…,"agent_status":…}}`, and a bare
  `wait_matched` → `idle`; anything else is an **error**, never a silent settle
  (P13-B21). `agent_blocked` arrives as an API *error code* and maps to the
  string `"blocked"`.
- **Found: the Go client can drop the first pushed event.** `readFrame` builds a
  fresh `bufio.Reader` and closes over it, so on `events.subscribe` the ack read
  may buffer bytes belonging to the first `{"event":…}` frame and discard them
  when the reader is dropped. This has not bitten a live run — herdr writes the
  ack and the pushes in separate writes — but it is a latent race in the Go
  client, not a wire property. The Rust `subscribe` reads the ack **byte-wise**
  (no look-ahead) and then hands the socket to the pump, so the hazard is gone by
  construction. Worth fixing in Go too if that client is touched again.
- **Second Go weakness: a partial frame is lost on the 60 s idle deadline.** Go
  sets a read deadline and treats a timeout as keep-alive, but `ReadBytes`
  returns the partial bytes it already consumed with the error and the caller
  drops them. Rust keeps an `acc: Vec<u8>` across timeouts and only drains
  `\n`-terminated lines, so a frame that straddles the deadline still lands.
- **Divergence accepted:** `UnixStream::connect` (UDS connect does not block;
  Go's `DialTimeout` only bounds an operation that cannot block anyway), and
  `Subscription` is closable (`shutdown` + join) instead of leaking a goroutine.
- **Live check:** an `#[ignore]`d `live_round_trips_against_a_real_socket` runs
  `Client::new()` + `ping` + `agent.list` against the running 0.9.3 server — green
  on this box, so the port is proven on the wire and not just against the fake.
- **Carried into 18D-4/18D-5:** the runner needs `pane_wait_for_output` for TUI
  readiness (S33e: drive readiness off TUI-only output, not focus) and
  `agent_prompt_wait` as ONE request (a separate wait races the pre-prompt idle),
  and the scheduler's `until` lists must carry **both** `done` and `idle`
  (unfocused agents settle `done`).

## S38 — "put pi inside ttt": what the ttt source actually offers (2026-10-18) ✅ resolved (source: ttt v1.6.0)

Direction (user): pi should run **inside ttt**, not beside it. Checked against
ttt v1.6.0 source (`~/go/pkg/mod/github.com/eugenioenko/ttt@v1.6.0`, re-fetched
with `go mod download github.com/eugenioenko/ttt@v1.6.0` — the module cache had
been cleaned, so the AGENTS.md "only local source" claim needed restoring).

- **The PTY/VT layer already exists and is complete for hosting pi.**
  `internal/terminal/terminal.go` wraps `gitpod-io/xterm-go` (VT screen,
  scrollback, OSC 133 prompt markers) around `aymanbagabas/go-pty`, and exposes
  `WriteString`, `RawTail` (64 KB), `Resize`, `ScrollbackLen`, `Close`,
  `OnUpdate`, `OnExit`. Nothing at the emulator level needs writing.
- **The gap is argv + a handle, not rendering.** `terminal.New(shell, cols,…)`
  execs `pt.Command(shell)` — ONE program name, no args. The single spawn site is
  `internal/app/app.go:316` `App.SpawnTerminal()`, using
  `Settings.Terminal.Shell` (a single string) and registering
  `TerminalTab{ID: "terminal-<n>", Term, Widget}`; there is no per-tab command,
  no label, and **no terminal surface in `internal/plugin`** (grep: zero hits).
- So `shell: "pi"` already gives a pi tab (all tabs; PATH must resolve pi), while
  `pi --model …` and "spawn a *named* pi tab from Lua, send it text, read its
  tail, focus it" are the three increments in `docs/ttt-terminal-api.md`
  (A argv → B per-tab spec/label → C `ttt.terminal.*` behind a permission).
- **`OnOpenFile(path, line, col)`** on the terminal widget routes to
  `EditorGroup.OpenFile` + `GoToLineCol` — a program printing `file:line` gets
  click-to-open. That is the concrete win of pi-in-ttt over a herdr pane.
- **The load-bearing gap is agent status.** herdr recognizes `kind: pi` natively
  and streams `pane_agent_status_changed` (`idle|working|blocked|done`), which is
  what `agent.prompt.wait` gates on. ttt has no agent concept — OSC 133 prompt
  marks are a *shell* integration and pi is not a shell. A ttt-hosted pi needs a
  status channel; the low-risk one is the forseti pi extension writing
  `.forseti/agent-status/<name>.json` (inside the ttt plugin's fs sandbox, and
  readable by herdr runs too).
- **Does not change the fork decision**: fork neither wholesale; patch ttt
  additively (A+B+C are upstreamable and A/B are tiny). `docs/ttt-terminal-api.md`
  now carries the plan. For the Rust engine it means 18D-5 should put pane
  operations behind a `PaneBackend` trait (herdr first, `TttBackend` later)
  rather than hardcoding the herdr client as the Go runner does.

## S39 — the prompt renderer: an un-oracled surface, pinned by tests (Phase 18D-5a, 2026-10-18) ✅ resolved (unit)

18D-5 needed a prompt renderer, and this is the first part of the port the
oracle CANNOT see: the goldens pin every node's prompt TEMPLATE (S35), but a
template only becomes a prompt at dispatch, when upstream outputs exist. So the
question was what pins the Rust renderer.

- **Scope, measured rather than guessed.** Every template form in the repo is
  one of: `{{ .<node> }}`, `{{ node "<name>" }}`, `{{ artifact "<type>" }}`,
  `{{ memory "<query>" }}` (plus the deliberate error cases below). So
  `host/src/crew/template.rs` implements that subset — field access, a
  one-string-arg call, `{{- … -}}` trim markers — and errors clearly on
  anything else instead of silently rendering nothing.
- **Go's parse-time failures are reproduced VERBATIM**, because they are the
  failures authors actually hit (S32): `template: <node>:<line>: function
  "draft" not defined` for a bare name, `...: bad character U+002D` for a
  hyphenated field. Both tested with Go's exact strings and line numbers.
- **`<no value>` is intentional.** Go's `missingkey=default` renders `<no value>`
  for an absent map key. The runner binds every INCOMING edge unconditionally
  (P13 BUG-06), so the only way to see it is a prompt naming a non-upstream —
  an authoring bug, which must look the same in both engines. A test pins it.
- **What replaces the oracle**: a test that loads every EXAMPLE crew and
  parse+renders every agent prompt AND every phase's instructions (28 prompts
  today) with stub functions. That catches the entire S32 failure class (bare or
  hyphenated references) in Rust, before a live run, exactly as
  `schema.TestPatternPromptsRender` does on the Go side.
- **Rust-specific design: two-phase render.** Go's `Execute` calls the function
  closures in-string, while they mutate the runner. Rust cannot hold those
  mutable borrows simultaneously, so the runner parses first, resolves each
  call, then renders. Deliberately NOT deduplicated: one resolution per
  OCCURRENCE, so two `{{ memory "q" }}` in one prompt are two recalls and two
  events, matching Go.
- **Not covered**: `{{ if }}`/`{{ range }}`/pipelines/`printf` — no crew uses
  them, and a crew that did would fail loudly at dispatch (a clear
  `unsupported template action` error) rather than render wrongly.

## S40 — the Rust engine's first live crew runs (Phase 18D-5b, 2026-10-18) ✅ resolved (live)

18D-5b staged the herdr orchestration, so the Rust engine finally ran crews
itself. Four live runs against the `sandbox` session on the free
`opencode-go/beam-free`, plus the unit gate.

- **Base crew (`crew.yaml`, planner→builder, `when: re:PLAN_READY`)** — exit 0,
  `done=2 cost=$0.0000`, twice. Startup (tab + 2 agents + readiness) ~8 s;
  planner ~2.5 s, builder ~4 s. The builder actually created `HELLO_CREW.md`
  ("crew pipeline works"), so the templated plan + the regex gate carried real
  work end to end.
- **Checked crew (`crew-checks.yaml`)** — both shell checks ran after their
  after-nodes and passed (`check_pass` ×2), exit 0. Checks write to the crew cwd
  and the exit code follows them.
- **Failing-check crew** — `check_fail` (`exit status 1 |`) and
  `checks_failed=1` in the summary, **exit 1**. The CI contract holds on the
  Rust path.
- **Teardown is clean**: after each run `herdr agent list` in the session was
  empty and `tab list` showed only the pre-existing tab, and
  `.forseti/crew-status.json` was gone. The bridge is live-only (the Go engine
  removes it in teardown too), so its contract is pinned by a unit test rather
  than a post-run file.
- **Bug found by the first live run**: the CLI's success path fell through the
  `crew run` match arm into the router path, printing `--config is required` and
  exiting 2 *after* a successful run. Fixed with an explicit exit; the second
  run returned 0. A reminder that the orchestration has no oracle — live runs
  are the only way to see this class of bug.
- **Sequential waves are shipping behavior for now.** The Go engine dispatches a
  wave's nodes in goroutines; `drive()` runs them one at a time. The edge pass
  still happens only after the whole wave, so no node can observe a same-wave
  sibling: results, checks and the event set are the same, and only wall-clock
  and interleaving differ (3 critics would be ~3× slower). Parallelism needs the
  scheduler shared-borrow refactor and a thread-safe backend; it is deferred to
  the TUI stage where shared state exists anyway.
- **`agent.read` includes pi's TUI chrome** (the startup box/status line) in the
  captured output, exactly as the Go engine's `bus/*.md` files do — not a port
  bug, and the reason the verdict parser reads the LAST `VERDICT:` line.
- **Deferred and loud**: `routes:` and `--worktree` return an error naming
  18D-5c instead of running without them; memory is a `NoMemory` link; the live
  status stream (`pane_agent_status_changed`) and `watch:` waiters are UI-only
  and land with the TUI.

## S41 — pi context hot-swap: `context` + `before_agent_start` sections (2026-10-18) ✅ resolved (source + gate)

hashtext needs the opposite of usual context injection: bulk must be *present
at request time* but *absent from the transcript*. Two pi surfaces make that
possible; both were read in source and pinned by `scripts/hashtext-smoke.ts`.

- **`pi.on("context", h)` runs before every LLM call** (`runner.js` →
  `emitContext`). The handler receives `messages` **without system messages**
  and may return `{messages}`. Emit wraps each call in `structuredClone`, so a
  handler's output is per-request only — the on-disk transcript is untouched.
  That is the property the whole design rests on: a marker restored on every
  call, never consumed. Returning `undefined` means "unchanged"; returning the
  same array reference also counts as unchanged (`sameMessages` compares refs).
  pi restores system messages itself (`restoreSystemMessages`) — a handler
  cannot drop the prompt by returning visible messages only.
- **`pi.on("before_agent_start", h)` is the sanctioned prompt-section hook.**
  The bundled MCP extension mutates `event.systemPromptOptions.sections[tag]`
  in place and returns nothing; pi re-renders `buildSystemPrompt` from the
  mutated options (each handler's `systemPrompt` getter renders current
  options). So the cold index is one XML-wrapped section, deleted when empty.
- **`ctx.getContextUsage()`** returns `{tokens, contextWindow, percent}`
  (`tokens` null right after compaction/before a response), which the plugin
  uses for the 90% injection ceiling.
- **Tool results are the right place for markers.** The expand tool returns the
  marker itself; the next `context` call substitutes it. Nothing else is needed
  — no `context_edit` entries, no transcript rewrite.
- **Not verifiable from types alone**: whether a later handler's full
  `systemPrompt` override would drop an earlier section mutation (it would —
  `forceSystemPrompt` wins). The plugin only mutates sections, so ordering with
  the MCP extension is safe.

## S42 — memory + laya on the Rust runner: two live surprises (Phase 18D-5c-1, 2026-10-18) ✅ resolved (live)

Both loopback services were up, so 18D-5c-1 could be proven live rather than
unit-only (`crew-memory.yaml`, `crew-laya.yaml`, sandbox session, beam-free).

- **Memory: a node can recall its own upstream's write IN THE SAME RUN.** The
  builder's dispatch printed `auto: 1 entries for "builder Execute this plan…"`
  — the planner's `memory_write` (which happens at `node_done`, before the next
  node dispatches) was already ingested and recalled. That is the intended P14
  flow, but it is the first time the Rust ordering proved it end-to-end.
- **Both writes reported the SAME id (#999042).** Not a bug in the port: the
  builder's output contains the plan verbatim, so its embedding cosine with the
  planner's row is ≥ 0.95 and memory v2's near-dup MERGE updated the existing
  row instead of inserting (P12). Lesson for reading logs: `memory_write` ids
  are row ids, not per-node ids — a crew that echoes upstream output will merge
  into it, and later recalls return the merged text.
- **Laya: the gate fired at conf 0.511 over a 0.4 floor**, `chosen=opsfix`
  (the availability option), `codefix` skipped with `not chosen`, latency 299 ms.
  `state_file: LAYA_TRIAGE.md` was honored — the judged evidence was the
  artifact, not the pane (`state from file: LAYA_TRIAGE.md`), which is exactly
  the S19 finding that the scrollback truncates the encoder and carries the
  prompt echo.
- **The event's `model` is the routing name, not a checkpoint**: laya reported
  `model: "english"` via `routing.model`. It is carried into `laya_decision` as
  `Model` exactly as the Go runner does, so `watch`/the badge see the same field.
- **Ordering inside a wave**: laya edges resolve BEFORE the plain edge pass
  (which skips `laya:` edges), one grouped call per settled node — a failed
  endpoint skips every laya edge of that node with `laya gate failed-safe: …`,
  never hanging the graph.

## S43 — worktree + review on the Rust runner, and the options that had two owners (Phase 18D-5c-2a, 2026-10-18) ✅ resolved (live)

- **herdr's worktree root is `~/.herdr/worktrees/<repo>/<branch>`**, not under
  the checkout (the S13 note said "under herdr's worktrees root" without the
  path). The husk sweep keys on `.herdr/worktrees`, which holds for both the
  create-failure message and the effective path.
- **Cleanup from the CLI** (when a runner leaves something behind): `herdr
  worktree remove --workspace <workspace_id> --force --trust-repository` (the
  workspace id, not the path; `--force` skips the dirty check), then `git
  branch -D <branch>`. The worktree-backed workspace appears in `herdr
  workspace list` with `worktree.is_linked_worktree: true`, and its id is what
  the runner gets back from `worktree.create`.
- **Live pass**: `crew-worktree.yaml --worktree rust-wt` — the agent wrote into
  the checkout, both git-state checks (`git status --porcelain | grep …`,
  `grep … WORKTREE_REVIEW.md`) passed there, and teardown removed the worktree
  and branch. `--review` kept the worktree, the branch and a 2-pane tab
  (author + lazygit split) and appended the review hint to `run_end`.
- **Bug caught only by the live review run**: `Run` and `Scheduler` each held a
  copy of `Options`. `Scheduler::new` applies `review ⇒ keep_tab +
  keep_worktree`, and `settle_unreachable` flips them for a blocked run — but
  `Run::teardown` read its OWN copy, so the first `--review` run *deleted* the
  worktree it had just promised to keep. Fix: normalize `--review` on both
  copies at construction, and make teardown read the scheduler's policy (the
  single mutable owner). This is the second orchestration bug the live runs
  found that neither unit tests nor the oracle could (the first was the CLI
  fall-through, S40): state duplicated across two owners is the recurring
  failure mode of this stage.
- **`git branch -D` prints to stdout** (`Deleted branch …`) because the child
  inherits the terminal — the Go runner does the same, so `--headless` output is
  not strictly pure JSON. Noted, not changed (parity first).

## S44 — hashtext cost model: native zstd, tiered index, Laya as selector (2026-10-18) ✅ resolved (measured)

The S41 mechanics worked, so this pass asked what actually costs tokens and
latency, and which parts of the cold store a small local model can decide.

- **Node 22.22 has zstd in `node:zlib`** (`zstdDecompressSync`, and
  `{dictionary}` works). Verified against a real store: 200 metadata files
  read+parsed in **4.7 ms**; **all 200 blocks** (302 KB) decompressed and
  scanned in **8.3 ms**. The Python CLI costs **37.6 ms** for `list` and
  **30 ms** for `show` (interpreter startup). So reads moved native; writes
  stay on the CLI (single format owner). The gate cross-checks native output
  against `hashtext show`.
- **Standing index cost dominates storage.** 200 blocks at the original flat
  format = **2,294 tokens on every call**; 500 blocks = 7,194. A tiered index
  (resident line + 20 recent + “N more”) is ~180 tokens; a compact line
  (8-char ref, capped description, expansion cost) is ~14 → ~10 tokens.
- **Duplicate markers double the payload.** A block referenced twice was
  injected twice (754 tokens); first-occurrence-only + back-reference halves it
  and keeps the transcript semantics.
- **Progressive disclosure is the small-context multiplier.** A 40 KB log at
  `mode=auto` enters as an outline (~100–300 tokens); `focus` retrieves matching
  lines ±2 with `… lines a-b omitted …`; `hashtext_read` pulls exact ranges. The
  block behaves like a searchable document rather than a bucket.
- **Laya is a selector, not a retriever.** Its encoder sees ~512 tokens, so the
  index is shortlisted lexically first (≤7 candidates). Over 5–7 candidates it
  picked correctly 4/4 (confidence 0.88–1.00), abstained on an unrelated prompt
  (“other”, 0.49), and ran in 27–45 ms warm (412 ms first). A binary
  “does this turn need a block?” gate was tested and **rejected**: it answered
  “no” to every probe at 0.09–0.75 confidence. The janitor therefore asks a
  bounded choice (“which ONE resident block is least needed, or keep all”),
  matching the shape that works.
- **Request-time archival projects `turn_end.toolResults`.** The event carries
  `toolCallId`/`toolName`/`content`; storing the text cold and replacing the
  message’s `content` with a stub in the `context` hook hides bulky output from
  requests while the session file keeps the original. Verified in the gate
  (stub carries the ref; expanding restores the content). This is why the
  earlier `context_edit` rewrite is not needed.
- **Still deferred, measured:** a trained zstd dictionary saves **42–61%** on
  realistic 300–740 B blocks (24–69% on larger corpora); CDC + one packed
  stream saves **45%** on “log v1 + appended lines”. Both are store-level wins,
  not prompt wins, so they rank below the index work.

## S45 — hashtext picker: retrieval beats ranking, and Laya is under-confident (2026-10-18) ✅ resolved (measured)

First pass with telemetry + a labelled eval harness (`scripts/hashtext-dataset.ts`,
`scripts/hashtext-eval.ts`), on an 8-block store (README, plugin source, docs,
logs) with 19 rows.

- **The shortlist was the bottleneck, not the model.** Name/description-only
  scoring found the expected block in **56.3%** of labelled rows (12.5% on
  content-derived queries). Adding a content-aware pass — scan block text
  natively only when metadata scoring yields <3 candidates — took recall@7 to
  **100%** and lexical top-1 to **100%** on decision rows. Retrieval had to be
  fixed before any ranking claim could be made.
- **Laya's ranking tied the lexical baseline** on decision rows (8 vocab + 3
  abstain: 100% pick, 100% abstain, lexical top-1 also 100%). Its clear value is
  the **abstention head**, not ordering easy candidates. Sample is tiny; treat
  as a pipeline signal, not a win.
- **But its confidence is systematically low on this distribution.** Correct
  picks came back at **0.05–0.16**; ECE **0.61** over 11 decision rows, and a
  manual probe showed `other` absorbing 0.13–0.37 of the mass. The checkpoint's
  `temperature_by_options` is calibrated for its original task mixture. Effect:
  the plugin's **0.85 auto-expand threshold would never fire** even when the
  pick is right. Recalibration (`Agent.fit_temperatures`, L1) must come before
  any weight tuning; head-only fine-tuning (L2) is only justified if real-query
  data still shows ranking errors.
- **Synthetic content queries are retrieval probes, not judgement labels.**
  Keyword-soup queries produce labels a human would not reliably agree with, and
  Laya scored 0% on them. The dataset tags them `retrieval`; accuracy/ECE/
  abstention are computed on `decision` rows only.
- **Warm latency** across 19 Laya calls: 39.5 ms mean, 69 ms p95 (first-call
  load excluded from the mean by the warm server).
- **Telemetry now exists**: `decisions.jsonl` records `find` (query, candidate
  details, pick, confidence, probabilities, latency) and `outcome` (expand/evict
  with `matched_pick`, `resident_turns`), rotated at 8 MB. The dataset builder
  can dump it (`--from-decisions`), and `--queries FILE` accepts LLM-authored
  `expected<TAB>query` rows — the recommended path for a real training set.

## S46 — hashtext picker: the wrong confidence field, and a calibration that works (2026-10-18) ✅ resolved (measured)

S45 left two puzzles: Laya looked under-confident on our distribution, and the
0.85 auto-expand threshold never fired. Both had the same root cause.

- **We were gating on the wrong number.** Laya returns `confidence` (normalized
  Shannon entropy over the options — "how decisive is this distribution") and
  `answer_confidence` (`max(p)` — the quantity temperature scaling fits and ECE
  measures). The plugin read `confidence`. On the same live calls:
  entropy **0.164 / 0.052**, answer_confidence **0.367 / 0.238**. The library
  documentation is explicit that the two "must not be compared against the same
  threshold". The plugin now gates on `answer_confidence` and logs both
  (`confidence`, `entropy_confidence`, `calibrated`).
- **Even the right field is under-confident here** (0.24–0.37 on correct picks;
  ECE 0.273 over 32 decisions). The checkpoint's temperatures are fit to its
  original task mixture, not 7-option block metadata.
- **Consumer-side isotonic calibration fixes the gate without touching weights.**
  `scripts/hashtext-calibrate.ts` fits a monotone map from answer_confidence to
  empirical accuracy and writes `<store>/calibration.json`; the plugin loads it
  and gates on the mapped value. In-sample (n=32 decisions): ECE **0.273 →
  0.068**, and the **0.85** operating point selected **65.6%** of picks at
  **95.2%** precision — the threshold became usable. Accuracy is invariant by
  construction (monotone map, same argmax).
- **The fitter refuses toy data**: default `--min 30`; the gate uses `--min 3`
  only to exercise the write path. Holdout ECE is reported when ≥5 rows are held
  out, otherwise flagged as in-sample (an upper bound on the gain).
- **Label quality matters more than model choice**: on the 41-row set, authored
  queries scored 72.2%, synthetic vocab 100%, abstentions 7/7, retrieval probes
  unscored. Some authored labels are themselves ambiguous — the next dataset
  should be judged, not just authored.
- **Warm latency** across 40 calls: 37.5 ms mean / 43 ms p95.
- **Test gotcha**: a blocking `spawnSync` starves an in-process mock server, so
  the calibration child timed out against the canned Laya endpoint. The gate
  uses async spawn for that step.

Next (L1b) is re-fitting this map on real telemetry; head-only fine-tuning (L2)
still waits on evidence that ranking, not retrieval or confidence, is the gap.

## S47 — hashtext telemetry labels: outcomes are weak, but usable (2026-10-18) ✅ resolved (gate)

S45/S46 built telemetry and a calibration fitter; the missing link was turning
recorded outcomes into labelled rows without hand-authoring every query.

- **The label is "what the agent actually opened", not "what was correct".**
  `scripts/hashtext-label.ts` links `find` records to subsequent `outcome`
  records by query string and turn distance, then emits: matched expand →
  expected = Laya's pick (`high`); divergent expand → expected = the block the
  agent opened, a negative for the pick (`medium`); eviction within
  `--min-resident` turns downgrades to `low` (excluded by default). A find with
  **no** expand stays unlabelled — silence is ambiguous, not a negative.
- **Constants are explicit**: `--window` (turns between find and outcome,
  default 3), `--min-resident` (default 2), `--include-low`. The default output
  is the same `hashtext-eval/1` row the eval/calibration scripts already read.
- **Gate demo (canned Laya)**: 2 finds → 1 labelled `matched_expand` (high) +
  1 unlabelled; the labelled row is concatenated with authored rows and fed to
  the calibration refit (`--min 3`), which succeeds. The loop telemetry →
  label → refit is now exercised end to end.
- **Caveat recorded on purpose**: matched-expand is a *usage* signal, not proof
  of correctness — an agent can expand the wrong block and not notice.
  `--review` emits `expected<TAB>query` for a human/LLM judge; judged rows are
  the input to any real fine-tune. Keep `--min 30` for calibration writes.

## S48 — the routed runner on OpenRouter: a header trap, slash-splitting, and two tailer bugs (Phase 18D-5c-2b, 2026-10-18) ✅ resolved (live)

The proxy port itself came up clean (config render → router `--dry-run` → spawn on a free port → `/health` → pi provider materialized → routing NDJSON tailed → provider restored at teardown, models.json verified unchanged). The live runs found four things worth recording.

- **OpenCode Go's free ids cannot be routed at all.** Through the router they 400
  with `MissingSessionID`: pi emits `x-opencode-session` only when
  `model.provider` is `opencode`/`opencode-go` **or** the baseUrl host is
  `opencode.ai` (pi 1.0.3 `provider-attribution.js`). A switchyard provider is
  neither, so the header never reaches the upstream. The router *can* add static
  headers (`switchyard-llm-client` 0.3.0 `extra_headers`, or `forward_auth`),
  but forwarding cannot conjure a header pi never sent — the fix would be a pi
  extension adding it for the switchyard provider, or a provider that does not
  require it.
- **OpenRouter model ids contain a slash, and a switchyard target splits at the
  FIRST one.** `efficient: openrouter/free` renders `llm_client = "openrouter"`
  + `id = "free"`, and OpenRouter rejects `free` ("not a valid model ID"). The
  crew file must write the DOUBLED provider form:
  `openrouter/openrouter/free` → llm_client `openrouter`, model
  `openrouter/free`. Verified live (2 routed calls, `models:
  openrouter/free=2`). The Go engine's `modelTarget` splits identically, so this
  is a shared crew-authoring rule, not a port difference.
- **The routing record carries `"origin": null`, and serde rejects null for a
  `String`.** Every record was silently dropped by the tailer until every field
  became `Option<...>`. (Go's `encoding/json` leaves the zero value.) A unit
  test now pins a record with an explicit null.
- **The tailer could emit a record twice** when the router appended between the
  metadata stat and the read: the offset advanced to the *stale* size, so the
  next poll re-read the tail. It now advances by the bytes actually read.
  Tailed events also needed their own `ts` (they bypass `Scheduler::emit`).
- **Model policy (live evidence).** `openrouter/free` is a meta-router that
  picks an available free upstream per request: a 6/6 probe succeeded, but in
  the checked crew the *builder* skipped tool use in 1 of 2 runs (a weak
  upstream), and fixed free ids are worse (`google/gemma-4-31b-it:free` returned
  upstream **429** shared-pool rate limits; `nvidia/nemotron-3.5-lightning:free`
  took ~3 min per node and also skipped the tool call). So: the **example crews
  pin `openrouter/free`**, while **`scripts/crew-smoke.sh` overrides to
  `opencode-go/beam-free`** via `FORSETI_CREW_MODEL` for a deterministic gate
  (the override exists exactly for this). Routed crews must use OpenRouter,
  since OpenCode Go cannot traverse the router.
- **Secrets**: `openrouter` reads its key from `~/.config/forseti/openrouter.key`
  (0600, extracted once from OpenCode's local credential store; never printed or
  committed). `~/.pi/agent/models.json` was backed up to `models.json.bak`
  before the provider entry was added.
