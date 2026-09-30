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
| `git init` + GitHub private remote (`alytaphoenix/forseti`) | ✅ done |

Exit criteria: skeleton in place; S3 answered; AGENTS.md current.

## Phase 1 — herdr plugin: idempotent bring-up (ttt + pi in a dedicated tab) — ⬜ not started

Component: `herdr-plugin/`.

1. `herdr-plugin.toml` — id `forseti`, `min_herdr_version = "0.7.0"`, platforms
   linux/macos, one `[[actions]] open` (contexts `workspace`) → `sh scripts/open.sh`.
2. `scripts/open.sh`:
   resolve dir from `HERDR_PLUGIN_CONTEXT_JSON` → idempotency check (`herdr agent
   list --json`; live agent → focus + exit 0) → port probe (:4242 busy → ttt without
   `--listen` + warning) → `herdr tab create --cwd DIR --label forseti` → `herdr pane
   run <root_pane> ttt --listen` → `herdr pane split <root_pane> --direction right
   --no-focus` → `herdr agent start coder --kind pi --pane <id>` → focus tab →
   print JSON summary of created IDs. Never close/mutate user topology.
3. Keybinding recipe in README (`prefix+f` → `herdr plugin action invoke forseti.open`).
4. Test in a named test session; assertions via `agent list --json` / `pane list --json`.

**Acceptance:** two consecutive `forseti.open` invocations produce one tab, one ttt,
one pi; second run focuses the existing pair. Unit-level: shellcheck clean; smoke
script logs JSON transcript of the run.

## Phase 2a — ttt plugin: ask, status sidebar, `Forseti: Jump` — ⬜ not started

Component: `ttt-plugin/` (`plugin.ttt.json`, `init.lua`). Permissions: `panel.sidebar`,
`commands`, `keybindings`, `editor.read`, `editor.write`, `panel.editor`, `fs.read`,
`system.exec: ["herdr"]`.

1. Agent resolution helper: `herdr agent list --json` → filter `kind=="pi"` → prefer the
   agent whose pane is in ttt's workspace (via `HERDR_*` env); clear errors for
   none/ambiguous.
2. `Forseti: Jump` command: read `FORSETI_JUMP_FILE` (`{path, line, end_line}`) →
   `ttt.open_tab` → `set_cursor` + `set_selection` (hunk highlight).
3. `forseti.ask` (`ctrl+k a`): buffer path + cursor + selection → `herdr agent prompt
   <resolved> "…" --wait`; notify on completion; never auto-answer `blocked`.
4. Sidebar `Forseti`: `set_interval(3000)` poll of `agent list --json` while panel
   visible (TBD: visibility check); render name + state; highlight `blocked`.

**Acceptance:** approval dialog shows expected permissions; ask sends the *selected*
text and reports settled state; jump file + `curl -X POST --data 'exec "Forseti:
Jump"' :4242/exec` opens the file with the hunk selected (screenshot-verified).

## Phase 2b — pi extension: `/ttt` commands, follow mode — ⬜ not started

Component: `pi-extension/index.ts`. Lifecycle: no timers/sockets in the factory;
HTTP client + state in `session_start`, closed in `session_shutdown`.

1. `/ttt jump <path> [line] [end_line]` — write jump file → `POST /exec "Forseti:
   Jump"` (fire-and-forget; closed port → silent no-op + notice).
2. `/ttt follow on|off` (default off) — `tool_result` hook on edit tools → derive
   target (path + first-changed-line from old/new strings) → emit jump.
3. `/ttt diff` — palette title TBD-4; keystroke-chain fallback.
4. `/herd list|agents` — read-only herdr pass-throughs when `HERDR_ENV=1`.

**Acceptance:** scripted pi edit (`pi -p`) with follow on shows the edited file +
approximate line in ttt; follow off changes nothing; all pushes tolerate a dead 4242.

## Phase 3 — Sync, polish, shipping — ⬜ not started

- Event-driven status if S4 finds a surface; else tuned polling visibility rules.
- Notifications on `done`/`blocked` transitions (herdr notification surface).
- Jump accuracy: exact hunk from pi edit payloads; optional diagnostics follow.
- Packaging: `herdr plugin install` slug, ttt plugin zip/dir install, `pi install`
  package; per-component READMEs documenting trust + single-ttt constraint.
- Post-upgrade smoke script covering bring-up → ask → jump roundtrip.

## Parallel / housekeeping

- **U1**: upstream issue to ttt — request an `open FILE[:LINE[:COL]]` exec command
  (kills the palette+state-file dance; maintainer already ships a herdr plugin).
- Optional: `git init` + first commit (repo currently has no VCS).
- ~~Run S3 spike at the next live herdr session~~ → **done** (see below).

## Test infrastructure (added 2026-09-30)

- pi's default model in this repo is **OpenCode Go `glm-5.3-flash`** (project
  `.pi/settings.json`), with the halogen server (`halogen-qwen3.8-flash-next`) as a
  free secondary. Phase 1+ test runs launch `herdr agent start --kind pi` sessions
  against these. Both verified with live `pi -p` calls (2026-09-30).
- Go API key stored at `~/.config/forseti/opencode-go.key` (0600, outside the repo);
  referenced by pi via `!cat` in `~/.pi/agent/models.json`.
