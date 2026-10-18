# ttt terminal API — pi as a real ttt tab

**Decision (2026-10-18, user direction):** pi runs **inside ttt**. ttt stays
the shell — it owns the keymap, the focus and the file buffer — and pi becomes a
PTY-backed tab/panel of it, not a forked editor and not a second window. herdr
remains the machine-side multiplexer for headless crew fan-out (N panes, tabs,
worktrees); the two are complementary, not competing.

Why this way (S36/S38): forking pi means re-authoring an editor inside a
fast-moving 1.0.x extension surface; ttt already **has** the PTY + VT emulator
and only lacks a way to launch another program in a tab and hand it to a plugin.
That is a small, additive, upstreamable delta.

## Verified source facts (ttt v1.6.0)

Source: `~/go/pkg/mod/github.com/eugenioenko/ttt@v1.6.0` (re-fetched with
`go mod download github.com/eugenioenko/ttt@v1.6.0`; the module cache had been
cleaned, so AGENTS.md's "only local source" claim needed restoring).

- `internal/terminal/terminal.go` — `Terminal` wraps `github.com/gitpod-io/xterm-go`
  (VT screen, scrollback, OSC 133 prompt markers) around `github.com/aymanbagabas/go-pty`.
  Constructor: `New(shell string, cols, rows, scrollbackMax int, env []string, dir string)`.
  The shell is executed as **one program name** (`pt.Command(shell)`), so
  `"pi"` works but `"pi --model x"` does not (it is looked up as a filename).
- **The host-side API already exists**: `WriteString` (input), `RawTail`
  (64 KB recent raw bytes), `Resize`, `ScrollbackLen`, `Close`, `OnUpdate`,
  `OnExit`. Nothing needs writing at the emulator level.
- **One spawn site**: `internal/app/app.go:316` `App.SpawnTerminal()` →
  `terminal.New(a.Settings.Terminal.Shell, …, a.Workspace.Primary())`, then
  `ui.NewTerminalWidget` is appended to `a.Terminals` as
  `TerminalTab{ID: "terminal-<n>", Term, Widget}` inside `a.TerminalPanel`.
  There is no per-tab command and no label.
- Config: `internal/config/settings.go:33` `Terminal{Shell string, Scrollback int}`;
  the settings UI exposes "Terminal shell" (a single string).
- `ui.NewTerminalWidget` sets `OnOpenFile(path, line, col)` → `EditorGroup.OpenFile`
  + `GoToLineCol`, and `OnOpenURL` — i.e. a program printing `file:line` gets
  click-to-open for free. This is the main reason pi-in-ttt is worth it.
- **No terminal surface in `internal/plugin`** (grep: zero hits). Lua can read
  the *rendered screen* only through `/exec`/`wait-for`; there is no handle to a
  tab, no send, no read. The permissions machinery (`system.exec`,
  `events.editor`, …) is the model to extend.

## Status: A, B and C are implemented in the fork

The fork lives at **`~/repos/ttt`**, branch **`forseti/terminal-argv`**, based on
the **v1.6.0** tag (the version the installed `~/go/bin/ttt` binary reports;
upstream has since moved to v1.8.0, so a future rebase is expected).

- **A ✅ `5fcaffc`** — `terminal.New(..., args ...string)` + `TerminalSettings.Args`.
- **B ✅ `5fcaffc`** — `App.SpawnTerminalSpec({Shell, Args, Cwd, Label})` +
  `TerminalTab.Label` + strip badges.
- **C ✅ `414d130`** — the `ttt.terminal` Lua module
  (`spawn`/`send`/`read`/`focus`/`list`) behind `terminal.control` and
  `terminal.read`.

- **pi status channel ✅ (S51)** — `pi-extension/index.ts` writes
  `.forseti/agent-status/<name>.json` on the pi lifecycle when
  `FORSETI_AGENT_NAME` is set; the name arrives through the fork's
  `ttt.terminal.spawn{env=…}` (`82d88d3`). `FORSETI_STATUS_DIR` names the
  directory; the crew host gives each run `.forseti/agent-status/<run>/`, and
  writes are serialized (a later state can never lose to an earlier one).

- **`TttBackend` ✅ (S52, live; hardened v0.4.1)** — `host/src/crew/ttt.rs`
  implements `PaneBackend` over the plugin's `Forseti: Agent Bridge` command
  (`agent-req-<tag>.json` → `POST /exec` → `agent-res-<tag>.json`, both deleted
  after use) plus this status channel; `--host ttt` stages one pi tab per node.
  Readiness requires the status record, a 750 ms grace, AND a drawn pane (the
  TUI input reader does not exist at `session_start`). A 2-node crew ran end to
  end.

## The delta (three increments, smallest first)

**A. argv for the terminal program.** `Terminal.Shell` → `Shell` + `ShellArgs`
(or `Command []string`), and `terminal.New` → `New(argv []string, …)` calling
`pt.Command(argv[0], argv[1:]…)`. One-line change at the single call site.
This alone gives "pi as the terminal" (all tabs), enough for manual use:
`shell: "pi"` already works today **when `pi` resolves on ttt's PATH** (use the
absolute `~/.hermes/node/bin/pi` if ttt is launched from a GUI); an argv-bearing
`shell: "pi --model …"` is what needs A.

**B. per-tab spawn spec + label.** `SpawnTerminal(spec)` taking cmd/args/cwd/label
(shell as the default), `TerminalTab.Label`, and a palette command that opens a
named pi tab. Needed so a second/third agent tab can be told apart, and so the
strip and `debug` dump are legible.

**C. a plugin handle** (the "terminal API" this file was for):
`ttt.terminal.spawn{cmd, args, cwd, label} -> id`, `send(id, text)`,
`read(id, n)` (raw tail), `focus(id)`, `list()`, plus an exit event. Gate it
behind a new permission (`terminal.control`), mirroring `system.exec`.
`spawn` = B's path; `send` = `WriteString`; `read` = `RawTail`; `focus` =
strip select + `Root.SetFocus`.

A+B+C are additive and upstreamable. A and B are small enough that a fork can
track upstream with a rebase-sized diff; C is the only real API commitment.

## What ttt does not give us: agent status (the load-bearing gap)

The crew engine gates every node on an **agent status** (`idle|working|blocked|
done`) — herdr recognizes `kind: pi` natively and streams
`pane_agent_status_changed`, which is what `agent.prompt.wait` waits on (and why
`sched`/`runner` never poll text). ttt has no agent concept: its OSC 133 prompt
marks are a **shell** integration, and pi is not a shell. A ttt-hosted pi
therefore needs an explicit status channel:

1. **preferred** — the forseti pi extension emits a status file
   (`.forseti/agent-status/<name>.json`: state, turn count, last tool, ts) on
   `turn_start` / `turn_end` / `tool_execution_*` / idle. Files are inside the
   ttt plugin's fs sandbox, and herdr runs can consume the same file.
2. alternative — a custom OSC emitted by pi and surfaced by ttt's terminal
   (needs a ttt change; more invasive than (1)).

Until that exists, a ttt-hosted pi can only be driven as "type, then watch for
the prompt/status line" — the heuristic the herdr path deliberately replaced.

## Consequence for the Rust crew engine (18D-5)

The Go runner hardcodes `*herdrd.Client`. The Rust port should put the pane
operations behind a `PaneBackend` trait — `spawn / prompt / prompt_wait / read /
focus / status` — with the herdr impl first (18D-3 already provides it) and a
ttt impl added once C + the status channel exist. That keeps "pi inside ttt"
from becoming a second runner, and it is the one place the port improves on the
Go design without diverging.

## Sequencing

1. 18D-5 runner behind `PaneBackend` (herdr only) — the staged port finishes.
2. ttt patch A+B (argv + per-tab spec/label), upstream PR.
3. pi extension: status file.
4. ttt patch C (plugin handle), upstream PR.
5. `TttBackend` behind `PaneBackend`; the ttt sidebar/badge become the human
   surface for crew runs, herdr optional for fan-out.

Do not fork pi, and do not merge the two into one project: the seam is a PTY,
and both tools already agree on it.
