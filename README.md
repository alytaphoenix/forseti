# Forseti

<p align="center">
  <img src="docs/images/architecture.svg" alt="forseti architecture" width="100%">
</p>

Forseti ties three local terminal tools into one workflow:

- **[herdr](https://herdr.dev)** — terminal workspace manager (caster/kernel: owns tabs, panes, agent lifecycle)
- **[pi](https://pi.dev)** (`@earendil-works/pi-coding-agent`) — coding agent CLI
- **[ttt](https://tttedit.dev)** — terminal IDE

License: [MIT](LICENSE) · Current release: **v0.1.0** (see
[Releases](https://github.com/alytaphoenix/forseti/releases))

## The loop

<p align="center">
  <img src="docs/images/loop.svg" alt="the forseti loop" width="100%">
</p>

```sh
# 1 · one command brings up the workspace: ttt editor + pi agent pane
herdr plugin action invoke forseti.open

# 2 · in ttt: select code, ask (input row or ctrl+k a) — answer streams in the pi pane
# 3 · say "open X" — pi itself navigates your editor (ttt_open tool)
# 4 · pi edits — follow lands on the exact changed line, or collects the turn
# 5 · Forseti: Review — one tab lists every change it made, at exact lines
```

## Components (one per host — no daemon anywhere)

| Component | Surface | Implements |
|---|---|---|
| [`herdr-plugin/`](herdr-plugin/README.md) | herdr plugin (TOML + sh) | idempotent bring-up: dedicated tab with ttt (`--listen`) + pi via native `agent start --kind pi`; `forseti.git` opens/focuses a lazygit pane on the current checkout |
| [`ttt-plugin/`](ttt-plugin/README.md) | ttt Lua plugin | ask (input row + `ctrl+k a`), status sidebar/badges, `Forseti: Jump` / `Forseti: Review`, vault commands (Daily Note / Backlinks / wikilink / Obsidian) |
| [`pi-extension/`](pi-extension/README.md) | pi TS extension | `/ttt jump·open·follow·review·context·diff`, `/herd agents`, model tools (`ttt_open`, `ttt_diff`, `ttt_read_context`, `vault_*`), prompt context injection |
| [`crew/`](crew/) | standalone Go binary (`forseti-crew`) | deterministic agent-graph runner on herdr: `crew.yaml` (agents + `when`-gated edges, `checks`, `watch`, `routes`), Bubble Tea builder + live monitor (meta tab), bus-file handoff, JSONL run log, sandbox sessions + disposable worktrees, switchyard model routing |

Docs: [`design`](docs/design.md) · [`implementation plan`](docs/implementation-plan.md) ·
[`spike log (verified facts)`](docs/spikes.md) · [`AGENTS.md`](AGENTS.md)

## Git — lazygit in the loop

lazygit (0.65.1) joins ttt and pi as a pane app:

- `herdr plugin action invoke forseti.git` — opens (or focuses) a lazygit pane
  on the current checkout inside the forseti tab; safe to re-invoke, relaunches
  after a quit, never types into a running editor/agent.
- From ttt: palette `Forseti: Git (lazygit pane)`.
- From lazygit: `Ctrl+G` opens the selected file in the live ttt, `Ctrl+Y`
  asks the live pi agent about it (both bind via a managed, marker-guarded
  block in lazygit's config).
- `forseti-crew run --worktree <branch> --review` ends a run on a lazygit pane
  of the agents' worktree with the merge recipe in the run log.

## Decisions — Laya, the bounded edge-gate

Crew edges stay deterministic (`idle` | `re:<regex>`) **plus** one more gate:
`when: laya:choice:<instructions>` — a ~421M local decision model (Laya,
Apache 2.0, ~21 ms warm on Apple silicon) branches the graph on what the
upstream output MEANS, over a bounded option set the crew file declares. The
runner stays the authority (argmax above `min_confidence`, else abstain with
the distribution in the run log); a dead endpoint fail-safes to skip. Agents
get the same service as the `forseti_decide` tool.

```sh
scripts/laya-setup.sh && scripts/laya-serve.sh start
forseti-crew run -f crew/examples/crew-laya.yaml --session sandbox
scripts/laya-eval.sh   # 9-probe gate incl. an abstention contract
```

## Memory — shared agent recall

Agents share structured cross-run memory through a local service: hybrid
retrieval (sqlite-vec cosine + FTS5 BM25, RRF-fused), dedup/supersede with
queryable history, and a **MemTree** layer that rolls related facts into
summary nodes (retrieval hits a summary but answers with its leaf facts).
pi's `memory_write`/`memory_recall` tools, crew prompts pull recall via
`{{ memory "query" }}`, and a crew declaring `memory:` gets **auto-recall**
(a `## Shared memory` block prepended to every node prompt) and **auto-write**
(node outputs posted back to the crew namespace `crew-<name>`, invisible to
other crews and to non-crew callers).

```sh
scripts/memory-serve.sh start        # SQLite+vec0 at ~/.config/forseti/memory.db
scripts/memory-eval.sh               # 28-probe gate: isolation, dedup, supersede,
                                     # TTL, laya gray-band, MemTree + crew probes
```

A standalone lineage of the service lives at
[github.com/alytaphoenix/mimir](https://github.com/alytaphoenix/mimir)
(`MIMIR_*` env config, `FORSETI_*` fallbacks). Both copies exist on purpose:
edit mindfully.

## Crew — multi-agent pipelines

`crew.yaml` describes a deterministic graph (no LLM routing of edges — LLMs do
node work only). `forseti-crew run` builds a dedicated herdr tab with one pane
per node, dispatches prompts with race-free settle waits, gates edges on agent
status or output regex (`re:PLAN_READY`), and templates upstream outputs into
downstream prompts (`{{ .planner }}`).

```sh
cd crew && go build -o bin/forseti-crew ./cmd/forseti-crew
bin/forseti-crew run -f examples/crew.yaml            # TUI: builder + live monitor
bin/forseti-crew run --headless -f examples/crew.yaml # CI path
bin/forseti-crew watch                                # tail-render the newest run log
```

Phase 6 additions:

- **E2E harness**: `checks: [{after, run}]` run shell assertions after a node
  settles (`examples/crew-checks.yaml`) — a failing check flips the exit code,
  so the crew file *is* the CI gate.
- **Hermetic runs**: `--session sandbox` targets a named herdr session
  (bootstrapped automatically when down) so crew tabs/agents never appear in
  your live session; `--worktree <branch>` runs everything inside a disposable
  git worktree (removed at teardown, `--keep-worktree` to keep).
- **Observability**: `forseti-crew watch` tail-renders any run log; live status
  stream + blocked alerts; herdr sidebar projection; ttt status-bar badge
  (`crew demo 2/3 !`); monitor meta tab (`tab` key) with per-node cost/ctx.
- **Model routing**: `routes:` + `route:` attach agents to a Switchyard
  stage_router pool (efficient=halogen, capable=glm) — per-call model choice,
  edges stay deterministic (`examples/crew-routed.yaml`). When an upstream is
  down, Switchyard falls back to the other tier, so routed crews survive
  outages that kill direct-model crews.
- **Outage override**: `--model <p/m>` / `FORSETI_CREW_MODEL` swaps direct-model
  nodes for one run without editing the halogen-pinned files.

`blocked` agents surface in the TUI and are never auto-answered; teardown
closes only the tab the run created. Example: planner → coder pipeline that
actually ships a file end-to-end (`examples/crew.yaml`).

## Install (all three, from this public repo)

```sh
# herdr plugin
herdr plugin install alytaphoenix/forseti/herdr-plugin

# pi extension (user-level, every pi session)
cp pi-extension/index.ts ~/.pi/agent/extensions/forseti.ts
# or: pi install git:github.com/alytaphoenix/forseti@v0.1.0

# ttt plugin (Lua, once permissions are approved)
cp -R ttt-plugin ~/.config/ttt/plugins/forseti
```

Then run `herdr plugin action invoke forseti.open` (bind it to `prefix+f` via
herdr's `config.toml` — recipe in [`herdr-plugin/README.md`](herdr-plugin/README.md)).

## Status

All phases (0–8) implemented and **verified live** — bring-up, ask, follow,
review, tools, context injection, the evergreen vault, the crew
multi-agent runner, the Phase 6 observability/sandbox/routing layer
(check nodes, sandbox sessions, disposable worktrees, live status stream,
ttt bridge, switchyard routing), and the lazygit git surface (pane app +
back-dispatch loop). See the [plan](docs/implementation-plan.md) for the test
matrix. Gates: `scripts/smoke.sh` (IDE loop) +
`scripts/crew-smoke.sh` (checked crew in a sandbox session on the free halogen
model), both green.

## Model setup

pi's provider/auth is your own (`/login` inside pi). This repo pins pi's default
model via `.pi/settings.json` (OpenCode Go `glm-5.3-flash` at author time) and works
with any provider pi speaks. Templates: `herdr-plugin` needs herdr ≥ 0.7.0,
`ttt-plugin` needs ttt ≥ 1.6.0 running with `--listen`.

## Known constraints (v1, by design)

- Single forseti-enabled ttt per machine — the jump hand-off goes through ttt's
  `--listen` control server, which binds fixed `127.0.0.1:4242` (unauthenticated
  local-machine trust, documented in ttt source as a debug tool).
- Follow mode and Review are mutually exclusive; Review supersedes per-edit jumps.
- The Forseti sidebar tab may sit behind ttt's tab-strip overflow (`»`) on first
  run — drag it left once; ttt persists the order.
