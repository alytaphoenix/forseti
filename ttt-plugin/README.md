# Forseti ttt plugin

Editor-side component: ask the pi agent about your selection, watch its status,
and jump to code pi changed.

## Install

ttt loads plugins from `~/.config/ttt/plugins/` (a real directory — symlinks are
not picked up):

```sh
cp -R ttt-plugin ~/.config/ttt/plugins/forseti    # from the forseti repo
```

Restart ttt, or run **Plugins: Reload All**. First load shows the permission
dialog (persisted in `~/.config/ttt/plugins.ttt.json`).

## What you get

| Feature | How |
|---|---|
| Ask pi | select code → `ctrl+k a` (or palette "Forseti: Ask pi") — sends buffer path + line + selection to the live pi agent via `herdr agent prompt` |
| Status | sidebar "Forseti" panel: live pi agents + `idle/working/blocked/done`; a right status-bar badge pops on working→settled transitions (`! name needs you` when blocked) |
| Jump | palette "Forseti: Jump" opens the file/line from `jump.json` in this plugin dir — written by the pi side (follow mode / `/ttt jump`) |

## Requirements

- [herdr](https://herdr.dev) with a live pi agent (bring-up: `herdr plugin
  action invoke forseti.open`, from the repo's herdr-plugin/)
- [ttt](https://tttedit.dev) ≥ 1.6.0 running with `--listen` (bring-up does this)

## Notes

- Agent targeting: the pi agent in *this* workspace (via `HERDR_*` env) wins;
  else a single live pi agent is used; ambiguous → error in the panel.
- `system.exec` is scoped to `herdr` only. No network permissions requested.
- fs reads (jump.json) are sandboxed to workspace + this plugin dir — the pi
  side writes exactly there.
