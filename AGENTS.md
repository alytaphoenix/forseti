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

- `~/.pi/agent/models.json` defines two providers; both verified with a live `pi -p` call:
  - `opencode-go` → `https://opencode.ai/zen/go/v1` (OpenAI-completions API), model
    `glm-5.3-flash`. **Forseti's default test model** (set in `.pi/settings.json`:
    `defaultProvider: opencode-go`, `defaultModel: glm-5.3-flash`, thinking `low`).
  - `halogen` → `http://192.168.0.142:8731/v1` (keyless OpenAI-compatible), model
    `halogen-qwen3.8-flash-next`. Note: this server emits `reasoning_content` and
    can return empty `content` when `max_tokens` is small — budget tokens generously.
- The OpenCode Go API key lives at `~/.config/forseti/opencode-go.key` (0600); pi reads
  it via a `!cat …` command in models.json — never commit, echo, or move it into the repo.
- Project `.pi/` settings load only after project trust; use `pi -a` (or approve the
  prompt) when testing from a fresh session.


## herdr CLI facts (verified 0.9.3)

- `tab create` accepts `--workspace --cwd PATH --label TEXT --env KEY=VALUE --focus/--no-focus`; `tab focus <tab_id>` exists.
- `pane split [PANE_ID] --pane|--current --direction right|down --ratio FLOAT --cwd --env --focus/--no-focus`; `pane run <PANE> <CMD...>` types a command into the pane's shell.
- `agent start <name> --kind KIND --pane <id> [-- <agent-args>]` — kinds include **pi** (native recognition). Readiness ~30 s default. Names `[a-z][a-z0-9_-]{0,31}`, unique among live agents.
- Server errors: JSON on stderr, exit 1; syntax errors exit 2. Parse IDs from JSON, never from examples.
- Server may be not running (`herdr status`; socket `~/.config/herdr/herdr.sock`) — CLI control needs it. Isolate experiments in a named test session; never `herdr server stop` from a session.
- Protocol 22 (`herdr api schema --json`) exposes event subscriptions (incl. `AgentStatus` enum `idle|working|blocked|done|unknown`), but no CLI surface for them was found yet — Phase 3 topic.

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

## Repo conventions

- Components: `herdr-plugin/` (TOML + sh), `ttt-plugin/` (JSON manifest + Lua), `pi-extension/` (TypeScript), `scripts/` (spikes and dev helpers), `docs/` (design, plan, spikes).
- Only claims you verified; when investigating, record new findings in `docs/spikes.md` and update `docs/implementation-plan.md` statuses.
