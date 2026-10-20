# Forseti herdr plugin

Bring-up component: one herdr action → the ONE forseti IDE tab, containing the
ttt editor (`--listen`) and a pi agent pane.

## Install (dev)

```sh
herdr plugin link /Users/alytaphoenix/repos/forseti/herdr-plugin
herdr plugin list          # confirm "forseti" appears
```

Then, inside herdr (or over its socket API):

```sh
herdr plugin action invoke forseti.open
```

## Bring-up behavior (idempotent)

The action resolves the target dir from context (`checkout_path` →
`focused_pane_cwd` → `workspace_cwd`, made absolute) and `exec`s
`forseti start --dir DIR --no-sync --no-services --json`, so the plugin and the
CLI share one code path. `scripts/open-legacy.sh` is the pre-CLI fallback (used
only when the `forseti` binary is absent).

`forseti start` owns exactly one tab:

1. it finds the tab by identity (`~/.config/forseti/state.json` records the
   tab + pane ids), then by the live agent, then by the `forseti` label;
2. an existing tab is **revived in place**: herdr restores the layout across a
   server restart but not the TUI process, so a restored editor pane (a bare
   shell) gets `ttt --listen <abs dir>` again and a missing pi pane gets
   `agent start coder --kind pi` — same tab number, no second tab;
3. a pane running anything other than a shell is left alone;
4. dead duplicate `forseti` tabs (all panes idle shells) are pruned;
5. the tab is focused and its ids are recorded.

Port 4242 is probed first — if it is already bound, ttt starts *without*
`--listen` and warns (jump/follow pushes no-op; see docs/spikes.md S2 for why
the port is fixed).

## Optional keybinding (herdr config.toml)

```toml
[[keys.command]]
key = "prefix+f"
type = "shell"
command = "forseti start --no-sync --no-services"
description = "open/revive Forseti (ttt + pi)"
```

## Knobs (environment)

| Var | Default | Meaning |
|---|---|---|
| `FORSETI_AGENT_NAME` | `coder` | live agent name for the pi pane (legacy path) |
| `FORSETI_TTT_PORT` | `4242` | port probed before enabling `ttt --listen` (legacy path) |
| `FORSETI_EDITOR_LABEL` | `forseti` | herdr tab label (legacy path) |
| `FORSETI_BIN` | `forseti` | CLI the action delegates to |

The CLI reads `herdr.agent`, `herdr.editor_label`, `ttt.bin` and `ttt.port` from
`~/.config/forseti/config.json`; the environment knobs above only matter for the
legacy fallback.
