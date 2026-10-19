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

## switchyard headers — `switchyard.ts`

A second, independent extension (`index.ts` is the ttt bridge; this one is the
crew-routing bridge). When the active provider is `switchyard` (the provider the
crew materializes per run) it decorates every provider request so the router can
attribute traffic per session:

| header | value | why |
|---|---|---|
| `x-switchyard-session-id` | pi's session id | explicit override — top precedence in switchyard's header-normalization table (S28) |
| `proxy_x_session_id` | pi's session id | the 0.2.0 legacy name the server still reads |
| `x-switchyard-origin` | `$FORSETI_SWITCHYARD_ORIGIN` or `pi` | client label in the routing log |
| `x-switchyard-request-id` | a uuid per request | joins routing-log rows with host telemetry |
| `x-switchyard-agent-id` | `$FORSETI_SWITCHYARD_AGENT_ID` | opt-in per-agent identity (per-pane env is unavailable over the herdr socket, S26) |

pi already emits `x-session-id` for this provider (`compat.sendSessionAffinityHeaders`),
but switchyard reads that only as a *fallback*; the explicit override above plus
`origin`/`request-id` is what this extension adds. `/switchyard` reports the gate,
the tagged-request count, and the model that actually served the last response
(`x-model-router-selected-model`).

## Crew status channel — `index.ts`

When `FORSETI_AGENT_NAME` is set (the ttt host spawns each crew node that way),
the extension publishes the agent's lifecycle state to
`.forseti/agent-status/<name>.json`:

```json
{"name": "builder", "state": "working", "pid": 1234, "ts": "…", "tool": "write"}
```

- `session_start` → `idle`, `agent_start` → `working`, `tool_call` → `working` +
  the tool name, `agent_end` → `idle`.
- `FORSETI_STATUS_DIR` overrides the directory (default `<cwd>/.forseti/agent-status`).
- The write is atomic (tmp + rename); `pid` lets a reader tell a crashed agent
  from a live idle one. Outside a crew nothing is written.
- `blocked` is not observable: pi's approval/question UI is not an event, so a
  stuck `working` is the signal.

## Install

**User-level (recommended)** — every pi session gets it:

```sh
cp pi-extension/index.ts ~/.pi/agent/extensions/forseti.ts
cp pi-extension/switchyard.ts ~/.pi/agent/extensions/forseti-switchyard.ts
```

**As a package** (shareable):

```sh
pi install /path/to/forseti/pi-extension   # local source package
pi install git:github.com/alytaphoenix/forseti@v0.7.0   # pinned git ref
```

The directory qualifies as a pi package (`package.json` with `pi.extensions`;
verified loadable via `pi -e ./pi-extension`).

**Try for one invocation:** `pi -e ./pi-extension`

## Notes

- Extension state resets on `/reload` — re-run `/ttt follow on` after reloads.
- Follow mode is off by default (it jumps your editor while pi edits).
- Read-only: the extension never edits code itself; it only reports where pi edited.
