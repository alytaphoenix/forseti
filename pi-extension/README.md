# Forseti pi extension

Agent-side component: drive the ttt editor pane **from** pi.

```
/ttt jump <path> [line] [end_line]   open the file at that position
/ttt open <path>                     open without a position
/ttt follow on|off                   auto-jump to every code change pi makes
                                     (default off; lands on pi's exact changed line)
/ttt diff                            open ttt's "Git: Open Changes" view
/herd agents                         list live herdr agents from inside pi
```

Follow mode pairs `tool_execution_start` (file) with `tool_execution_end` (pi's
`details.firstChangedLine`) and pushes through the same jump-file + palette-command
hand-off the ttt side consumes. Pushes are debounced (1.2 s) and tolerate a dead
ttt listener silently (one-time notice instead).

## Install

**User-level (recommended)** — every pi session gets it:

```sh
cp pi-extension/index.ts ~/.pi/agent/extensions/forseti.ts
```

**As a package** (shareable):

```sh
pi install /path/to/forseti/pi-extension   # local source package
pi install git:github.com/alytaphoenix/forseti@v0.1.0   # pinned git ref
```

The directory qualifies as a pi package (`package.json` with `pi.extensions`;
verified loadable via `pi -e ./pi-extension`).

**Try for one invocation:** `pi -e ./pi-extension`

## Notes

- Extension state resets on `/reload` — re-run `/ttt follow on` after reloads.
- Follow mode is off by default (it jumps your editor while pi edits).
- Read-only: the extension never edits code itself; it only reports where pi edited.
