# Forseti

Ties three local terminal tools into one workflow:

- **[herdr](https://herdr.dev)** — terminal workspace manager (caster/kernel: owns tabs, panes, agent lifecycle)
- **[pi](https://pi.dev)** (`@earendil-works/pi-coding-agent`) — coding agent CLI
- **[ttt](https://tttedit.dev)** — terminal IDE

## The workflow

```
herdr plugin action invoke forseti.open
   → dedicated tab: ttt editor + pi agent pane
   → ttt: ctrl+k a  asks pi about the selection / buffer
   → pi:  edits code  ->  ttt jumps to the changed hunk (follow mode)
   → ttt: status sidebar shows pi: idle | working | blocked | done
```

Three components, one per host (no daemon anywhere):

| Component | Surface | Status |
|---|---|---|
| `herdr-plugin/` | herdr plugin (TOML + sh) | Phase 1 — bring-up |
| `ttt-plugin/` | ttt Lua plugin | Phase 2a — ask/status/jump |
| `pi-extension/` | pi TS extension | Phase 2b — /ttt commands, follow mode |

Docs: [`design`](docs/design.md) · [`implementation plan`](docs/implementation-plan.md) ·
[`spike log (verified facts)`](docs/spikes.md) · [`AGENTS.md`](AGENTS.md)

## Known constraints (v1, by design)

- herdr server must be running; forseti never starts/stops it.
- One forseti-enabled ttt per machine (`--listen` binds fixed `127.0.0.1:4242`,
  unauthenticated — local-machine trust accepted in v1).
- Follow mode (ttt auto-jumps to pi's edits) is off by default.
