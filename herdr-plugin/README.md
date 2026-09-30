# Forseti herdr plugin

Bring-up component: one herdr action → a dedicated tab containing the ttt editor
(`--listen`) and a pi agent pane.

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

1. Resolves the target dir from context (`checkout_path` → `focused_pane_cwd` →
   `workspace_cwd`), mirroring ttt.editor.
2. If a pi agent named `coder` (configurable via `FORSETI_AGENT_NAME`) is already
   live → focuses it and exits. **Never double-spawns.**
3. Probes port 4242 — if already bound, launches ttt *without* `--listen` and warns
   (jump/follow pushes will no-op; see docs/spikes.md S2 for why the port is fixed).
4. Creates a dedicated tab (`--label forseti`): ttt in the root pane, pi in a right
   split, launched via `herdr agent start coder --kind pi` (herdr handles readiness).
5. Focuses the tab, prints a JSON summary of created IDs.

Consecutive invocations → one tab, one ttt, one pi.

## Optional keybinding (herdr config.toml)

```toml
[[keys.command]]
key = "prefix+f"
type = "shell"
command = "herdr plugin action invoke forseti.open"
description = "open Forseti (ttt + pi)"
```

## Knobs (environment)

| Var | Default | Meaning |
|---|---|---|
| `FORSETI_AGENT_NAME` | `coder` | live agent name for the pi pane |
| `FORSETI_TTT_PORT` | `4242` | port probed before enabling `ttt --listen` |
| `FORSETI_EDITOR_LABEL` | `forseti` | herdr tab label |
