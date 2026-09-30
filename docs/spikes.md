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

## S4 — herdr event subscriptions

`herdr api schema --json` (protocol 22) defines subscription/event schemas
(`AgentStatus` enum present). No CLI subcommand exposing subscribe/unsubscribe was
found. Status: optional — Lua polling via `set_interval` is sufficient for v1 status.

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
