# AGENTS.md

Forseti glues three locally installed terminal tools into one workflow:
**herdr** (terminal workspace manager), **pi** (coding agent CLI), **ttt** (terminal IDE).
Read `docs/design.md` (architecture & decisions) and `docs/implementation-plan.md`
(phase status) before working here. `docs/spikes.md` records verified integration
facts from source-level investigation.

## The three hosts (verified identities)

| Tool | Binary | Version / source | Note |
|---|---|---|---|
| herdr | `herdr` (`~/.local/bin/herdr`) | 0.9.3 (Rust) | persistent server + socket API; config validated via `herdr config check` |
| pi | `pi` (`~/.hermes/node/bin/pi`) | `@earendil-works/pi-coding-agent` 0.99.1 (Node) | this is pi-coding-agent — don't confuse with other `pi` binaries |
| ttt | `ttt` (`~/go/bin/ttt`) | `github.com/eugenioenko/ttt` v1.6.0 (Go) | only local source: `~/go/pkg/mod/github.com/eugenioenko/ttt@v1.6.0/` (no git clone in `~/repos`) |

## pi model providers (wired & verified 2026-09-30)

- `~/.pi/agent/models.json` defines four providers; all verified with live calls:
  - `opencode-go` → `https://opencode.ai/zen/go/v1` (OpenAI-completions API), model
    `glm-5.3-flash`. **Interactive bring-up agent's default** (set in
    `.pi/settings.json`: `defaultProvider: opencode-go`, `defaultModel:
    glm-5.3-flash`, thinking `low`). Paid — and as of 2026-10-05 the key is
    **out of funds (402)**, so no gate or probe can lean on it; use valhalla.
  - `valhalla` → `http://192.168.0.142:8731/v1` (keyless OpenAI-compatible), model
    `valhalla-flash-next`. **E2E/test model.** The old `halogen` provider entry is
    gone — llama-swap rebranded the box on 2026-10-05 (server-side ids also show
    as `gufo-*`; `valhalla-flash-next` aliases through and answers with real
    content). Same quirk family: emits `reasoning_content`, can return empty
    `content` when `max_tokens` is small — budget tokens generously (crew runner:
    one empty-settle retry, guard matches `halogen` OR `valhalla`), and it is a
    flaky LAN box (drops requests while overloaded; registry churns). Example
    crew files still pin `halogen/halogen-qwen3.8-flash-next` (user decision:
    files stay pinned to the free box); run E2E with the override
    `FORSETI_CREW_MODEL=valhalla/valhalla-flash-next` (or `--model`).
  - **Provider-state log**: 2026-10-01 halogen registry swap; 2026-10-05 the
    opencode-go key returns **402 Insufficient funds** (no gate can lean on it)
    and the box rebranded halogen→valhalla.
  - `switchyard` → per-run proxy `127.0.0.1:<dynamic>/v1`, model ids = crew route
    ids; materialized by the crew runner when the crew file declares `routes:`
    and restored at teardown. Server: `switchyard-server` 0.2.0 (crates.io).
- The OpenCode Go API key lives at `~/.config/forseti/opencode-go.key` (0600); pi reads
  it via a `!cat …` command in models.json — never commit, echo, or move it into the repo.
- Project `.pi/` settings load only after project trust; use `pi -a` (or approve the
  prompt) when testing from a fresh session. The trust dialog ("Trust project
  folder?") blocks every prompt until answered — crew worktree runs auto-pass
  `-a` for this reason.


## herdr CLI facts (verified 0.9.3)

- `tab create` accepts `--workspace --cwd PATH --label TEXT --env KEY=VALUE --focus/--no-focus`; `tab focus <tab_id>` exists.
- `pane split [PANE_ID] --pane|--current --direction right|down --ratio FLOAT --cwd --env --focus/--no-focus`; `pane run <PANE> <CMD...>` types a command into the pane's shell.
- `agent start <name> --kind KIND --pane <id> [-- <agent-args>]` — kinds include **pi** (native recognition). Readiness ~30 s default. Names `[a-z][a-z0-9_-]{0,31}`, unique among live agents.
- Server errors: JSON on stderr, exit 1; syntax errors exit 2. Parse IDs from JSON, never from examples.
- Server may be not running (`herdr status`; socket `~/.config/herdr/herdr.sock`) — CLI control needs it. Isolate experiments in a named test session; never `herdr server stop` from a session.
- `notification show` reports `{"reason":"disabled"}` on this setup (reconfirmed 2026-10-01) — alerts must ride the ttt status-bar bridge (crew-status.json → badge), not toasts.
- Named sessions (S11, verified 2026-10-01): sockets at `~/.config/herdr/sessions/<name>/herdr.sock`; the CLI never auto-starts them and attach needs a TTY + no inherited `HERDR_*` env (nesting guard) — spawn servers with `env -u HERDR_ENV -u HERDR_PANE_ID -u HERDR_SOCKET_PATH herdr --session <name>` inside a real PTY; the server persists after the spawning pane closes. `herdr session stop <name>` is session-scoped.
- Protocol 22 (`herdr api schema --json`): the **socket API** exposes `events.subscribe`/`events.wait`, `agent.read` (format/lines/strip_ansi), `agent.prompt` with `wait {until, timeout_ms}`, `layout.apply/export`, `pane.wait_for_output` (S12: substring|regex match, blocks for future output, matches past scrollback instantly, errors `timeout`/`invalid_regex`), `worktree.create/list/remove` (S13: returns a full workspace with `worktree.checkout_path`), and events incl. `pane_agent_status_changed` (`AgentStatus` enum `idle|working|blocked|done|unknown`; **one `pane_id` string per subscription entry — a list is rejected**) + `pane_output_changed`. Still **no CLI subscribe surface** — direct socket client shipped as `crew/internal/herdrd`.
- Socket wire facts (verified live 2026-09-30, spikes S6–S10): **NDJSON, no handshake**; one-shot requests **close the connection after the response** (dial per call); `events.subscribe` = persistent stream (ack `subscription_started`, pushes `{"event","data"}` no id); `agent.read` nests `result.read.text`; **`events.wait` is status-only** (`pane_output_changed` rejected `unsupported_event_wait_match`) and **not subscribable** — no output-push surface; `layout.apply` BSP round-trips (walk `result.layout.root` for pane ids); unfocused agents settle **`done` not `idle`** — include both in `until`; prompt+wait must be ONE request (`agent.prompt.wait`) or the wait races the pre-prompt idle. The prompt+wait success frame is `{"type":"agent_prompted","agent":{…,"agent_status":…}}` (P13 live) — the settled status rides in `agent.agent_status`; treat unknown shapes as errors.

## herdr plugin contract (from ttt's shipped plugin source, spike S1)

- Context arrives as `HERDR_PLUGIN_CONTEXT_JSON`; resolution order `checkout_path` → `focused_pane_cwd` → `workspace_cwd`.
- `HERDR_BIN_PATH` names the herdr binary. Plugin commands are spawned from the **plugin root** — relative script paths work, passing `--cwd` to spawned scripts breaks them.
- Actions run **headless** (no PTY) — never exec a TUI from an action; re-dispatch into panes instead.
- Dev: `herdr plugin link <abs path>`, confirm with `herdr plugin list`.

## ttt facts (verified against v1.6.0 source & docs, plus live runs)

- Exec vocabulary (`internal/app/exec_script.go`): `click|rclick|hover|drag`, `key COMBO`, `type TEXT`, `paste TEXT`, `copy`, `exec "Palette Command"`, `screenshot PATH`, `debug PATH`, `wait MS`, `wait-for TEXT [timeout=MS]`, `panel ID`, `quit|shutdown`. **No `open file` command.** Invalid actions exit nonzero / POST → non-2xx.
- `--listen` enables HTTP `POST /exec` on the hardcoded `127.0.0.1:4242`; ttt source calls it *"a single-operator debug tool, not a public API"* → one forseti-enabled ttt per machine, unauthenticated local control (v1 accepts, docs must state it).
- Quick Open `Ctrl+K P` (`file.quickOpen`); Go to Line `Ctrl+G` (`editor.goToLine`); palette `exec` matches by title; `debug /path.json` dumps a rich state snapshot incl. sidebar panel list + plugin output log — best remote-debug tool for plugin work.
- Lua plugin APIs (verified live): `ttt.open_file(path, line)` opens a real buffer and needs NO permission (unlike `open_tab` which needs `panel.editor`); `ttt.json` module for encode/decode; `ttt.set_status_item(side, id, text)` / `remove_status_item(id)`; `sys.env(name)` needs `system.env`; `sys.exec(binary, args)` needs `system.exec` allowlist and returns `{stdout, exit_code, ...}`.
- **fs sandbox**: `ttt.fs` reads are restricted to workspace folders + the plugin's own dir — `/tmp` is NOT readable. Jump hand-off uses the plugin dir.
- **Plugin loading gotchas (verified)**: symlinks in `~/.config/ttt/plugins/` are NOT loaded (copy, don't link); new plugins require a restart *or* "Plugins: Reload All"; first load shows the approval dialog (persisted in `~/.config/ttt/plugins.ttt.json`); `ttt.log` output is visible in the `debug` dump's `output` array.

## pi extension facts (verified against 0.99.1 docs + live runs)

- Default-export factory receiving `ExtensionAPI`; TS loaded via jiti (no build step). Dev: `pi --extension ./file.ts`, or install at `~/.pi/agent/extensions/` (user-level) / `.pi/extensions/` (project). Probe a load with `pi -p --extension <file> "Reply OK"` — note anthropic OAuth refresh noise if provider not specified.
- Factory must not spawn processes/sockets/watchers/timers — start/stop in `session_start` / `session_shutdown`. State resets on `/reload`.
- Events: `tool_execution_end` has NO `args` (only toolCallId/toolName/result/isError) — pair with `tool_execution_start` keyed by `toolCallId` to get inputs. `edit` tool input is `{path, edits:[{oldText,newText}]}`; its result `details.firstChangedLine` gives the first changed line for free ("for editor navigation").
- Command handler shape: `pi.registerCommand(name, {description, handler: async (args, ctx) => ...})`; feedback via `ctx.ui.notify(msg, "info"|"warning"|"error")`; `pi.exec(program, args)` for subprocesses.
- Slash commands arrive fine through `herdr agent prompt` (bracketed paste → pi parses leading `/` commands).
- pi natively emits `x-opencode-session` headers (`provider-attribution.js` in dist) — OpenCode Go validated-client requirement is satisfied.
- Tools & prompt hooks (verified live 2026-09-30): `pi.registerTool({name, label, description, promptSnippet, parameters: Type.Object(...), async execute(_toolCallId, params) -> {content:[{type:"text",text}], details}})` — `Type` comes from `typebox` (bundled dependency, resolvable from user-dir extensions); `pi.on("input", h)` can `{action:"transform", text}` to prepend context (skip leading-`/` inputs); `turn_start`/`turn_end` bracket a turn for per-turn edit collection.

## ttt Lua plugin facts — 2c/2d additions (verified 2026-09-30)

- `ttt.events` module: `events.on("cursor.change"|"file.open"|"file.save", cb)` — needs `events.editor` / `events.file` manifest permissions; `require` it inside pcall and degrade if missing.
- Sidebar input widget: `panel:input({placeholder, prefix, on_submit = function(text) end})` inside `render` — widget state (typed text) survives re-renders.
- Command registration is cached per plugin load: adding a palette command in init.lua needs "Plugins: Reload All" or a fresh ttt start; **a reload following a failed reload latches stale state** (verified — restart ttt via bring-up instead).
- `exec "X"` over /exec returns a non-2xx "command N …: command "X" not found" when a palette title isn't registered — cheap probe for command availability.
- Keep plugin state files in `ttt.plugin_dir()` (fs sandbox): `jump.json`, `review.json`, `context.json` are the three hand-offs (pi ⇄ Lua).

## Repo conventions

- Components: `herdr-plugin/` (TOML + sh), `ttt-plugin/` (JSON manifest + Lua), `pi-extension/` (TypeScript), `scripts/` (spikes and dev helpers), `docs/` (design, plan, spikes), `crew/` (Go + Bubble Tea — **shipped & verified**, Phases 5+6: `forseti-crew` binary; `internal/{herdrd,schema,runner,switchyard}`, `cmd/forseti-crew` with `run`/`validate`/`watch`; examples `crew/examples/crew.yaml` (halogen), `crew-checks.yaml` (E2E harness), `crew-routed.yaml` (switchyard pool), `crew-worktree.yaml` (review flow); gates `scripts/crew-smoke.sh` = checked crew + **phased crew (P11)** in `--session sandbox`). Outage model override: `--model` flag / `FORSETI_CREW_MODEL` env (route nodes keep their switchyard pools). Saved crew files omit zero-value fields (omitempty) — defaults re-apply on Load/Validate.
- Crew phases (P11, verified 2026-10-05): `phases: [{name, instructions, agents}]` in crew.yaml — ordered strict barriers; a phase's nodes dispatch only after ALL earlier-phase nodes are terminal (done/failed/skipped; `blocked` holds the phase, never auto-answered). Phase instructions prefix member prompts at dispatch through the SAME template render (`{{ .upstream }}` + `{{ memory "q" }}` work inside instructions). Validation: unique phase names (herdr rule), members must exist, each agent in at most one phase, unphased agents rejected when phases are declared, edges to an earlier phase rejected (cross-phase loop-backs = future item). Events `phase_start`/`phase_done` in the run log; `node_start` carries `phase`; `watch` groups rows under phase headers; `crew-status.json` + ttt badge carry the current phase. Builder TUI: `p` = phase form (validation-on-save rejects unphased members). No `phases:` key = identical to pre-P11 behavior.
- Crew scheduler semantics (P13, verified 2026-10-05): fan-in is **AND** — a node dispatches only after EVERY incoming edge fired; a missed edge skips the target (transitive downstream). Cycle **back edges** (`schema.BackEdges()`, target reaches source) gate RE-DISPATCH only — ignored for initial readiness (strict AND over them deadlocks every cycle); a firing back edge re-arms a settled target, bounded by `max_visits` = N allowed fires (skip event on attempt N+1). Schema rejects: entry-less crews, duplicate (from,to) edges, bad crew names (`schema.ValidName`). New events: `check_skip` (skipped after-node ≠ check failure) and `teardown` (teardown notes must never emit `run_end`/`node_failed`). Blocked at run end → KeepTab+KeepWorktree + nonzero exit (TUI ≡ watch). `Run.Snapshot()` = value copies. Full record: spikes S24, design.md Phase 13.
- lazygit (Phase 7, verified): `herdr-plugin/scripts/git.sh` = `forseti.git` action (focus-or-create lazygit pane; idempotency via `pane.process_info` foreground process; only shell panes host); per-invocation, marker-guarded custom commands live in lazygit's config under the `# >>> forseti >>>` marker (Ctrl+G → `git-jump.sh` → live ttt; Ctrl+Y → `pi-ask.sh` → live pi). macOS config: `~/Library/Application Support/lazygit/config.yml` (NOT ~/.config unless XDG_CONFIG_HOME). Lesson: handlers must be defined ABOVE `ttt.register` — later definitions capture nil handler slots.
- laya (Phase 8, verified): local decision endpoint on 127.0.0.1:8751 (`scripts/laya-serve.sh`; pinned venv `laya==0.3.22`, mps, warm ~21 ms; wire = `POST /v1/systemone {state, questions}`). Crew edges: `when: laya:choice:<instructions>` + `min_confidence` (abstention dial, default 0.5) + `state_file` (judge a file artifact — the scrollback truncates the 512-token encoder AND carries the prompt echo; always prefer the artifact). Gate = calibrated `confidence` (clear cases 0.7+, near-ties ~0.16). Eval gate: `scripts/laya-eval.sh` (9/9 probes incl. an abstention contract row).
- tools (Phase 9, verified): declarative tool specs at `~/.config/forseti/tools.d/*.yaml` (`FORSETI_TOOLS_DIR`) → `crew/bin/forseti-tools serve` (MCP stdio NDJSON; initialize/tools/list/tools/call). Registered in pi via `pi mcp add forseti-tools -- <abs>/crew/bin/forseti-tools serve`; `pi mcp list` = verification. Gates: `validate` (probes inline), `eval` (crew/eval/tools.d/ probe sets), `call`, `doctor`.
- memory (Phase 10, verified): shared agent memory at 127.0.0.1:8752 (`scripts/memory-serve.sh`; FastAPI in the laya venv; SQLite+vec0 cosine at `~/.config/forseti/memory.db`; `all-MiniLM-L6-v2`). pi tools `memory_write`/`memory_recall`; crew prompt helper `{{ memory "query" }}` (failure → ""). Namespacing: caller's own rows + `shared` only. `/recall` has a relevance floor (min_score 0.3 — unfiltered recalls inject noise). Eval gate: `scripts/memory-eval.sh` (4/4 incl. secret-namespace isolation). Opik: spike no-go (no hosted key, docker down) — future post-hoc exporter only.
- memory v2 (Phase 12, verified 2026-10-05): hybrid retrieval — sqlite-vec cosine + **FTS5 BM25** (external-content `mem_fts` + triggers) fused by **RRF k=60** (`FORSETI_MEMORY_RRF_K`); semantic pre-gate (min_score 0.3) applies BEFORE fusion (keyword never rescues a semantically-dead candidate; FTS-only hits carry `score: null`); boosts `final = rrf_norm + 0.5·0.995^hours + 0.5·importance`. Write path: exact-dedup (MD5, namespace-scoped) + near-dup merge (cosine ≥ 0.95 → update, not insert) + **explicit `supersedes` beats every heuristic** (Zep semantics — old row marked, history queryable via `include_superseded`; a declared 14:00→15:00 conflict is itself ≥0.95 cosine, so the heuristic must not swallow it). New params: `type` fact|episode|procedure|preference, `importance` 0–1, `expires_at`, `links` (A-MEM light; bounded box expansion of the top hit, default ON). Backup: `GET /export` NDJSON + idempotent `/import`. Eval gates: `memory-eval.sh` **23/23** (was 13/13 at P13) (P13: SELF-CONTAINED — spawns a throwaway service on a scratch DB + private port, plus forget-namespace-guard and k=0→422 probes; the shared DB no longer takes probe pollution); `memory-embed-eval.sh` compared four 384-d candidates (bge/gte/e5 tie 5/5) → **keep all-MiniLM-L6-v2** (P13: exit code now gates the incumbent at a 4/5 sanity floor). Model constant is env-config (`FORSETI_MEMORY_MODEL`); every published S23 constant was tuned on 1024-d embeddings — keep dials in env, validate on our probes.
- memory 12.5/12.6 (verified 2026-10-05): `/clear {agent}` namespace cleanup (shared requires `confirm:true`; rows whose superseder was cleared are REVIVED) + `GET /namespaces`; `history` audit = **schema v4** (events add|merge|supersede|delete|revive commit INSIDE the mutation txn; `GET /history?memory_id=` namespace-guarded; merge `old_text` keeps the destroyed statement). Auto-links on every insert: top-10 visible candidates, cosine [0.7, 0.95) — shared rows NEVER link private ones. Recall ranks boost-then-truncate (12.5-D fixed RRF-order truncation that made the importance/recency boosts decorative). laya-in-memory: gray band [0.7, 0.95) gets a BINARY value-changed? verdict per top-3 candidate, computed outside the write txn — `same`→merge, `changed`→auto-supersede (race-guarded), abstain/down→pure heuristics (writes NEVER fail because laya is down); omitted `type` auto-classifies (abstain→fact). Gate = P(top label) ≥ 0.65 (`FORSETI_MEMORY_LAYA_PROB`) — laya’s `confidence` field is binary-routing-calibrated and discounts rubric questions to ~0.1 (S25). Eval 23/23 with 3 laya rows SKIP-not-fail (total adapts) when laya is down.
- Only claims you verified; when investigating, record new findings in `docs/spikes.md` and update `docs/implementation-plan.md` statuses.
