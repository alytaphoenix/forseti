# forseti — one command for the herdr + ttt + pi workspace

A stdlib-only Go CLI that wraps the pieces forseti already ships. Install it
with `scripts/install-cli.sh` (the build stamps the repo path, so the installed
binary finds the plugins/scripts from anywhere).

```
forseti                         status (default)
forseti start [dir]             bring the whole IDE up (services, herdr, one tab)
forseti down                    stop the forseti-managed services
forseti status [--json]         environment status
forseti doctor [--json]         preflight checks with fixes (exit 1 on failure)
forseti open <path[:line[:col]]>  jump the running ttt
forseti run [args...]           crew run (engine/host/model from config)
forseti watch [args...]         tail a crew run log
forseti validate [args...]      validate a crew file
forseti flow <sub>              validate|explain|check|eval|ci|run|simulate|edit|waive|install-hooks|init
forseti config <sub>            list|get|set|path|edit|sync|init
forseti version
```

`forseti up` is an alias of `forseti start`.

## `start`

1. syncs `ttt-plugin/init.lua`, `ttt-plugin/plugin.ttt.json`, the pi extensions
   and the managed SoL-Pi config into their installed locations
   (content-compared, atomic; `--dry-run` shows the diff);
2. starts the configured services (memory on, laya off by default);
3. ensures a herdr server: a missing one is spawned headless (`herdr server`,
   detached, log `~/.config/forseti/herdr-boot.log`) and the local plugin is
   linked — `--attach` execs the TUI instead, `--no-attach` fails as before;
4. reconciles **exactly one owned IDE tab** (ttt in the editor pane, pi in the
   agent pane). The tab is found by the identity recorded in
   `~/.config/forseti/state.json`, then by the live agent, then by the label.
   herdr restores layout (not processes) across a server restart, so a restored
   tab is **revived in place** — the editor pane gets `ttt --listen <abs dir>`
   again and a missing pi pane gets `agent start coder --kind pi` — keeping the
   same tab number. A pane running anything other than a shell is left alone;
5. prunes dead duplicates: a tab carrying the same label whose panes are all
   idle shells is closed (same workspace; `--clean` widens to every workspace).
   A live agent or a running program protects a tab. `--prune-dead` opt-in also
   closes *foreign* tabs whose saved directory is gone.

Flags: `--dir`, `--label`, `--no-sync`, `--no-services`, `--no-open`,
`--attach`, `--no-attach`, `--replace` (fresh tab), `--clean`, `--prune-dead`,
`--yes` (no prompt), `--dry-run`, `--json`.

`status` and `doctor` report the owned tab (`ide up · tab 52 · ttt · agent
idle`). The herdr plugin's Open action runs the same command, so the plugin and
the CLI can never disagree about which tab is the IDE.

## SoL-Pi — the efficiency extension we ship

[SoL-Pi](https://github.com/NVlabs/SoL-Pi) is a third-party pi extension package
(four opt-in mechanisms: action fusion, observation pack, evidence-preserving
reducer, online context compact). forseti distributes it:

- `pi-extension/sol-pi.json` is the shipped default — the two local mechanisms
  that need no extra model calls are on, the reducer and context compaction are
  off;
- `forseti config sync` / `forseti up` render it from the `sol_pi.*` keys into
  `~/.pi/agent/sol-pi.json` (atomic; SoL-Pi rejects unknown keys, so the CLI
  writes exactly its schema);
- `forseti up` installs the package (`sol_pi.package`, default
  `git:github.com/NVlabs/SoL-Pi`) when pi has no sol-pi package declared.
  `sol_pi.install=false` (or a pre-installed `npm:sol-pi`) opts out;
- `forseti doctor` checks the managed file, the package, pi/node floors
  (pi ≥ 0.84.2, node ≥ 22.19) and warns about a project `.pi/sol-pi.json`, which
  replaces (does not merge with) the managed file.

```sh
forseti config set sol_pi.observation_pack false      # toggle one mechanism
forseti config set sol_pi.evidence_preserving_reducer true
forseti config set sol_pi.reducer_provider openrouter # route pair (both or none)
forseti config set sol_pi.reducer_model deepseek/deepseek-chat
forseti config set sol_pi.enabled false               # leave everything alone
forseti config sync                                   # rewrite the file
```

Every command is idempotent: `up`/`config sync` rewrite the file only when the
rendered bytes differ, and a disabled distribution never touches it.

## `flow`

`forseti flow` wraps `forseti-flow` against the repo's `.forseti/flow.yaml`
(a committed, declarative per-project workflow):

```sh
forseti flow init --preset review-loop     # draft a hybrid flow
forseti flow validate --explain            # resolved stages/evidence/phases
forseti flow check --stage pre_commit      # evaluate the gates (exit 1 = blocked)
forseti flow waive --stage pre_commit --evidence tests --reason "…"
forseti flow install-hooks                 # pre-commit/pre-push/commit-msg
```

Repo mode is deterministic (shell/diff/artifact/human evidence); agentic phases
drive `flow_next`/`flow_review` with a fresh `pi -p` reviewer, and `run`
compiles the graph to a crew. `scripts/flow-smoke.sh` is the end-to-end gate.

## Config

`~/.config/forseti/config.json` (created by `forseti config init`):

```
repo  herdr.bin  herdr.agent  herdr.editor_label
ttt.bin  ttt.port  pi.bin
services.memory  services.laya
crew.engine  crew.host  crew.model  crew.default
sol_pi.enabled  sol_pi.install  sol_pi.package  sol_pi.action_fusion
sol_pi.observation_pack  sol_pi.evidence_preserving_reducer
sol_pi.reducer_provider  sol_pi.reducer_model  sol_pi.online_context_compact
sol_pi.cache_write_read_ratio
```

The `sol_pi.*` keys are overrides on the shipped `pi-extension/sol-pi.json`, so
`config get` prints the effective value and `config set` records only what you
changed. A missing key keeps the shipped default.

`crew.default` is used by `forseti run` when no `-f/--file` is given (a bare
name resolves under `~/.config/forseti/crews/<name>.yaml`). Example:

```sh
cp crew/examples/crew-default.yaml ~/.config/forseti/crews/default.yaml
forseti config set crew.default default
forseti run                       # planner/advisor/reviewer on DeepSeek, executor on valhalla
```

The executor in that example carries `fallback_model: openrouter/deepseek/…`;
a provider failure (429/5xx/timeout/connection) re-runs the node once on it
(ttt host).

`forseti config edit <target>` opens the real tool file with `$EDITOR`:
`forseti`, `pi-models`, `pi-settings`, `pi-mcp`, `ttt-settings`, `ttt-plugins`,
`ttt-registry`, `crew`, `tools`, `sol-pi`. `sol-pi` is the generated file — edit
the `sol_pi.*` keys instead and run `forseti config sync`.

Provider keys and plugin permission grants are never edited by the CLI.
`forseti doctor` reports missing grants and suggests the fix.

## Notes

- `ttt` must be on PATH; use the forseti fork to get `ttt.terminal`
  (`docs/ttt-terminal-api.md`). `doctor` warns when the binary is upstream.
- `run` defaults to the Rust engine (`crew.engine=rust`) and the herdr host; set
  `crew.engine=go` or `crew.host=ttt` in the config to change that.
