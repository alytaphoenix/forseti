# forseti — one command for the herdr + ttt + pi workspace

A stdlib-only Go CLI that wraps the pieces forseti already ships. Install it
with `scripts/install-cli.sh` (the build stamps the repo path, so the installed
binary finds the plugins/scripts from anywhere).

```
forseti                         status (default)
forseti up [dir]                sync assets, start services, open the tab
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

## `up`

1. syncs `ttt-plugin/init.lua`, `ttt-plugin/plugin.ttt.json` and
   `pi-extension/index.ts` into the installed locations (content-compared,
   atomic; `--dry-run` shows the diff);
2. starts the configured services (memory on, laya off by default);
3. verifies the herdr server (and links the local herdr plugin if needed);
4. runs `herdr-plugin/scripts/open.sh` in the target directory, which opens the
   dedicated tab with ttt + a pi agent (idempotent: a live agent is reused).

`--no-sync`, `--no-services`, `--no-open`, `--attach`, `--dry-run`, `--json`.

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
```

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
`ttt-registry`, `crew`, `tools`.

Provider keys and plugin permission grants are never edited by the CLI.
`forseti doctor` reports missing grants and suggests the fix.

## Notes

- `ttt` must be on PATH; use the forseti fork to get `ttt.terminal`
  (`docs/ttt-terminal-api.md`). `doctor` warns when the binary is upstream.
- `run` defaults to the Rust engine (`crew.engine=rust`) and the herdr host; set
  `crew.engine=go` or `crew.host=ttt` in the config to change that.
