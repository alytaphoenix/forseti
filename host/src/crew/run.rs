//! Run orchestration (Phase 18D-5b): the Go `Run.Run` loop on herdr.
//!
//! Composition, bottom-up:
//!
//! * `scheduler` decides what runs (18D-4).
//! * `runner` executes one node on a `PaneBackend` (18D-5a).
//! * this module sets the stage — one herdr tab with one pane per node, every
//!   agent started up front and gated on readiness — then drives the wave loop
//!   until quiescence, flushing the run log (JSONL) and the
//!   `.forseti/crew-status.json` bridge as it goes, and tears the stage down.
//!
//! Ported decisions, deltas, and what is deliberately deferred:
//!
//! * Waves are **sequential** (Go dispatches a wave's nodes in goroutines). The
//!   scheduler's edge pass still runs after the WHOLE wave, so a node never sees
//!   a same-wave sibling's output: results and the run log's event set are
//!   identical, only wall-clock and interleaving differ. Parallelism returns
//!   with the shared-state TUI (18D-6).
//! * The live status stream (`pane_agent_status_changed` badges/notifications)
//!   and the `watch:` pane waiters are UI-only in the Go engine; both land with
//!   the TUI (18D-6). Run correctness rests on `agent.prompt.wait`.
//! * Memory auto-recall/write, laya, the switchyard proxy, worktrees and the
//!   review pane are 18D-5c. A crew that needs one of them fails with an
//!   explicit error rather than silently running without it.
//! * The status bridge and run log are flushed per NODE and per wave, not per
//!   event (the Go engine writes inside `emit`): the event stream is identical,
//!   only how often the file is rewritten differs.

use std::fs::File;
use std::io::Write;
use std::path::{Path, PathBuf};
use std::time::{Duration, SystemTime, UNIX_EPOCH};

use serde_json::{Value, json};

use super::herdr;
use super::live::{Live, LiveMaps};
use super::model::Crew;
use super::model::Edge;
use super::proxy::{self, Proxy, RoutingTailer};
use super::role::RolePack;
use super::runner::{HerdrBackend, MemoryLink, NoMemory, NodeExecutor, PaneBackend, truncate_utf8};
use super::scheduler::run_check;
use super::scheduler::{EdgeState, Event, Options, Scheduler, Status};
use super::services::{LayaChoice, LayaClient, LayaDecider, MemoryClient};

/// What a finished run reports.
#[derive(Debug, Clone, Default)]
pub struct RunOutcome {
    /// The Go `summary()` line: `done=N failed=0 cost=$0.0000`.
    pub summary: String,
    /// Any node ended blocked (the human-in-the-loop outcome).
    pub blocked: bool,
    /// Any declared check is currently failing.
    pub checks_failed: bool,
    /// Any node ended failed.
    pub failed: bool,
    /// Run-log path (`.forseti/runs/<stamp>-<crew>.jsonl`).
    pub log_path: PathBuf,
    /// Every event, in order (the headless stream and the tests).
    pub events: Vec<Event>,
}

/// A staged crew run. `new` builds the scheduler and role pack; `run` connects,
/// stages, drives and tears down.
pub struct Run {
    sched: Scheduler,
    roles: RolePack,
    crew: Crew,
    opts: Options,
    /// Worktree workspace id ("" = the main checkout).
    worktree_ws: String,
    /// Where panes/checks/bus live (the worktree when one is created).
    effective_cwd: String,
    /// The per-run router (crews with `routes:` only).
    proxy: Option<Proxy>,
    /// Removes the `switchyard` provider from pi's models.json.
    restore_provider: Option<proxy::Restore>,
    /// Tails the router's NDJSON into `route_decision` events.
    tailer: Option<RoutingTailer>,
    /// The live status stream + `watch:` waiters (UI-only).
    live: Option<Live>,
}

impl Run {
    pub fn new(crew: Crew, mut opts: Options) -> Self {
        if opts.cwd.is_empty() {
            opts.cwd = std::env::current_dir()
                .map(|p| p.to_string_lossy().into_owned())
                .unwrap_or_default();
        }
        // --review implies keeping both (P7-3); normalize here as well as in
        // the scheduler, because teardown reads the scheduler and create/start
        // read this copy.
        if opts.review {
            opts.keep_tab = true;
            opts.keep_worktree = true;
        }
        let roles = RolePack::new(&crew.roles);
        let sched = Scheduler::new(crew.clone(), opts.clone());
        let effective_cwd = opts.cwd.clone();
        Run {
            sched,
            roles,
            crew,
            opts,
            worktree_ws: String::new(),
            effective_cwd,
            proxy: None,
            restore_provider: None,
            tailer: None,
            live: None,
        }
    }

    pub fn scheduler(&self) -> &Scheduler {
        &self.sched
    }

    /// Stages and runs the crew. Blocking; returns the outcome even when nodes
    /// failed (the caller maps `blocked`/`checks_failed` to an exit code).
    pub fn run(&mut self) -> Result<RunOutcome, String> {
        let cwd = self.opts.cwd.clone();
        if std::env::var("FORSETI_CREW_HOST")
            .ok()
            .is_some_and(|v| !v.is_empty())
            && self.opts.host.is_empty()
        {
            self.opts.host = std::env::var("FORSETI_CREW_HOST").unwrap_or_default();
        }
        if self.opts.host.is_empty() {
            self.opts.host = "herdr".to_string();
        }
        // the outage override applies to BOTH hosts, so read it before dispatch
        if std::env::var("FORSETI_CREW_MODEL")
            .ok()
            .as_deref()
            .is_some_and(|v| !v.is_empty())
            && self.opts.model_override.is_empty()
        {
            self.opts.model_override = std::env::var("FORSETI_CREW_MODEL").unwrap_or_default();
        }
        if self.opts.host == "ttt" {
            return self.run_ttt(cwd);
        }

        // Arc: the live streams and the watch waiters own their own handles
        let client = std::sync::Arc::new(connect(&self.opts.session)?);

        std::fs::create_dir_all(Path::new(&cwd).join(".forseti/bus"))
            .map_err(|e| format!("bus dir: {e}"))?;
        let runs = Path::new(&cwd).join(".forseti/runs");
        std::fs::create_dir_all(&runs).map_err(|e| format!("runs dir: {e}"))?;

        self.write_memory_handoff(&cwd);

        // 1. run log (the Go name shape)
        let log_path = runs.join(format!("{}-{}.jsonl", log_stamp(), self.crew.name));
        let mut log = File::create(&log_path).map_err(|e| format!("run log: {e}"))?;
        let mut sink = Sink::new(&cwd, &self.crew.name, &mut log, self.opts.headless);
        self.emit(Event::info(
            "run_start",
            format!(
                "crew={} agents={} edges={} session={:?} worktree={:?}",
                self.crew.name,
                self.crew.agents.len(),
                self.crew.edges.len(),
                self.opts.session,
                self.opts.worktree_branch
            ),
        ));
        sink.flush(&mut self.sched);

        // 1a. switchyard proxy (6D-1): one router per run, before any agent
        self.start_proxy(&mut sink, &runs, &log_path)?;

        // 1b. worktree (6A-4): a disposable checkout; panes + checks run there
        if !self.opts.worktree_branch.is_empty() {
            let (ws, path) = self.create_worktree(&client)?;
            self.worktree_ws = ws;
            self.effective_cwd = path.clone();
            self.sched.set_effective_cwd(&path);
            self.emit(Event::info("run_start", format!("worktree ready: {path}")));
            std::fs::create_dir_all(Path::new(&path).join(".forseti/bus"))
                .map_err(|e| format!("worktree bus dir: {e}"))?;
            sink.flush(&mut self.sched);
        }

        // 2. workspace + the crew tab (right-leaning BSP spine, S8)
        let ws = if self.opts.workspace_id.is_empty() {
            client
                .focused_workspace()
                .map_err(|e| format!("focused workspace: {e}"))?
        } else {
            self.opts.workspace_id.clone()
        };
        let n = self.crew.agents.len();
        let effective_cwd = self.effective_cwd.clone();
        let (pane_ids, tab_id) =
            create_crew_tab(&client, &ws, &self.opts.tab_label, &effective_cwd, n)?;
        for (i, a) in self.crew.agents.iter().enumerate() {
            if let Some(st) = self.sched.node_mut(&a.name) {
                st.pane_id = pane_ids[i].clone();
            }
        }
        self.sched.set_tab_id(&tab_id);

        // 3. project the crew into herdr's Agents sidebar (UI-only, S10)
        let source = format!("crew:{}", self.crew.name);
        let filter = json!({"op": "in", "field": "pane_id", "values": pane_ids});
        let sort = vec![json!({"field": "attention", "order": "desc"})];
        if let Err(e) = client.agent_view_set(&source, &self.crew.name, Some(&filter), &sort) {
            self.emit(Event::info("node_failed", format!("view projection: {e}")));
        }

        // 4. start every agent up front, then gate on readiness (S9)
        let mut startup_failed = false;
        for a in self.crew.agents.clone() {
            if let Err(e) = self.start_agent(&client, &a.name, &a.kind, &a.args) {
                self.fail(&a.name, &format!("agent.start: {e}"));
                startup_failed = true;
            }
        }
        if startup_failed {
            self.emit(Event::info(
                "run_end",
                "aborted: an agent failed during startup",
            ));
            sink.flush(&mut self.sched);
            self.teardown(&client, &source, &tab_id);
            sink.flush(&mut self.sched);
            let events = std::mem::take(&mut sink.events);
            return Ok(self.outcome(&log_path, events));
        }
        sink.flush(&mut self.sched);

        // 5. live status stream + watch waiters (UI-only; 6B-2/6C-1)
        let live = Live::start(std::sync::Arc::clone(&client), &self.crew, &pane_ids);
        sink.live = Some(live.maps());
        sink.extra = Some(live.queued());
        self.live = Some(live);

        // 6. drive the graph
        let mut backend = HerdrBackend::new(&client);
        let mut memory: Box<dyn MemoryLink> = if self.crew.memory.is_some() {
            Box::new(MemoryClient::new())
        } else {
            // NoMemory keeps the node path unconditional for crews without a
            // `memory:` block (the Go engine still queries, but the namespace
            // is "" and nothing is written).
            Box::new(NoMemory)
        };
        let mut laya = LayaClient::new();
        let live_maps = self.live.as_ref().map(|l| l.maps());
        drive(
            &mut self.sched,
            &self.roles,
            &mut backend,
            &mut *memory,
            &mut laya,
            live_maps.as_ref(),
            &mut |sched| sink.flush(sched),
        );

        // 6. summary + teardown (blocked keeps the tab, P13 BUG-09)
        let _ = self.sched.settle_unreachable();
        self.sched.update_phase_progress();
        // fold the tailed + live events in BEFORE the summary, so they
        // precede run_end deterministically
        if let Some(mut l) = self.live.take() {
            l.stop();
        }
        if let Some(mut t) = self.tailer.take() {
            t.stop();
        }
        sink.flush(&mut self.sched);
        let mut info = summary(&self.sched);
        // run-level route tallies (per-node attribution is impossible in
        // switchyard 0.2.0 — pi's session header does not attach, S15)
        if let Some(p) = &self.proxy
            && let Ok(st) = p.stats()
        {
            if let Some(tiers) = st.get("tiers").and_then(Value::as_object)
                && !tiers.is_empty()
            {
                info += &format!(" | routed tiers: {}", proxy::fmt_tallies(tiers));
            }
            if let Some(models) = st.get("models").and_then(Value::as_object)
                && !models.is_empty()
            {
                info += &format!(" | models: {}", proxy::fmt_tallies(models));
            }
        }
        // P7-3: end the run on a lazygit review pane of the worktree
        if self.opts.review && !self.worktree_ws.is_empty() && self.effective_cwd != self.opts.cwd {
            match open_review_pane(&client, &tab_id, &self.effective_cwd) {
                Err(e) => info += &format!(" | review pane failed: {e}"),
                Ok(()) => {
                    info += &format!(
                        " | review ready: lazygit on {} — attach the sandbox session (`herdr session attach {}`), commit, then merge from the main checkout",
                        self.effective_cwd, self.opts.session
                    )
                }
            }
        }
        self.emit(Event::info("run_end", info));
        sink.flush(&mut self.sched);
        self.teardown(&client, &source, &tab_id);
        sink.flush(&mut self.sched);

        let events = std::mem::take(&mut sink.events);
        Ok(self.outcome(&log_path, events))
    }

    fn emit(&mut self, ev: Event) {
        self.sched.emit(ev);
    }

    /// P14: pi panes get no per-pane env through the herdr socket API (S26), so
    /// the crew namespace rides a file the pi tools read.
    fn write_memory_handoff(&mut self, cwd: &str) {
        if self.crew.memory.is_none() {
            return;
        }
        let ns = self.crew.memory_ns();
        let path = Path::new(cwd).join(".forseti/memory-crew");
        if let Err(e) = std::fs::write(&path, format!("{ns}\n")) {
            self.emit(Event::info(
                "run_start",
                format!("memory-crew hand-off write failed: {e}"),
            ));
        }
    }

    /// The per-run switchyard router (host-independent: it spawns forseti-host
    /// itself). Sets the provider key, the routing-log tailer and the proxy.
    fn start_proxy(
        &mut self,
        sink: &mut Sink<'_>,
        runs: &Path,
        log_path: &Path,
    ) -> Result<(), String> {
        if self.crew.routes.is_empty() {
            return Ok(());
        }
        let routes = self.crew.routes.clone();
        let routing_name = format!(
            "{}.routing.jsonl",
            log_path
                .file_stem()
                .and_then(|s| s.to_str())
                .unwrap_or("run")
        );
        let p = Proxy::start(&routes, runs, &routing_name)
            .map_err(|e| format!("switchyard proxy: {e}"))?;
        let ids: Vec<String> = routes.iter().map(|r| r.id.clone()).collect();
        let restore = p
            .materialize_provider(&ids)
            .map_err(|e| format!("pi provider materialization: {e}"))?;
        self.emit(Event::info(
            "route_decision",
            format!(
                "proxy on :{} (routing log {})",
                p.port,
                p.routing_log.display()
            ),
        ));
        self.restore_provider = Some(restore);
        let tailer = RoutingTailer::spawn(p.routing_log.clone());
        sink.tail = Some(tailer.events());
        self.tailer = Some(tailer);
        self.proxy = Some(p);
        sink.flush(&mut self.sched);
        Ok(())
    }

    /// The pi argv for a node: the route (through switchyard) or the direct
    /// model, plus the node's extra args. The herdr path appends `-a` itself.
    fn pane_args(&self, agent: &super::model::Agent) -> Vec<String> {
        let mut args: Vec<String> = Vec::new();
        if !agent.route.is_empty() {
            args.push("--model".to_string());
            args.push(format!("switchyard/{}", agent.route));
        } else if !agent.model.is_empty() {
            let model = if !self.opts.model_override.is_empty() {
                self.opts.model_override.clone()
            } else {
                agent.model.clone()
            };
            args.push("--model".to_string());
            args.push(model);
        }
        args.extend_from_slice(&agent.args);
        args
    }

    /// Phase 21: run the crew with pi inside ttt (one terminal tab per node)
    /// through the forseti plugin bridge. No herdr, no live streams; the tabs
    /// stay open at the end (they are the human surface).
    fn run_ttt(&mut self, cwd: String) -> Result<RunOutcome, String> {
        if !self.opts.worktree_branch.is_empty() {
            return Err(
                "--worktree needs the herdr host (ttt tabs have no worktree lifecycle)".to_string(),
            );
        }
        if self.opts.review {
            return Err("--review needs the herdr host".to_string());
        }
        std::fs::create_dir_all(Path::new(&cwd).join(".forseti/bus"))
            .map_err(|e| format!("bus dir: {e}"))?;
        let runs = Path::new(&cwd).join(".forseti/runs");
        std::fs::create_dir_all(&runs).map_err(|e| format!("runs dir: {e}"))?;
        self.write_memory_handoff(&cwd);

        let log_path = runs.join(format!("{}-{}.jsonl", log_stamp(), self.crew.name));
        let mut log = File::create(&log_path).map_err(|e| format!("run log: {e}"))?;
        let mut sink = Sink::new(&cwd, &self.crew.name, &mut log, self.opts.headless);
        self.emit(Event::info(
            "run_start",
            format!(
                "crew={} agents={} edges={} host=ttt",
                self.crew.name,
                self.crew.agents.len(),
                self.crew.edges.len()
            ),
        ));
        sink.flush(&mut self.sched);
        self.start_proxy(&mut sink, &runs, &log_path)?;

        // one pi tab per node; the bridge passes FORSETI_AGENT_NAME so the
        // status channel (S51) is on
        let mut bridge = super::ttt::TttBridge::from_env(&self.effective_cwd);
        let mut ids: Vec<(String, String)> = Vec::new();
        for a in self.crew.agents.clone() {
            let args = self.pane_args(&a);
            match bridge.spawn(&a.name, &args, &self.effective_cwd) {
                Ok(id) => ids.push((a.name.clone(), id)),
                Err(e) => {
                    self.fail(&a.name, &format!("ttt spawn: {e}"));
                    self.emit(Event::info("run_end", "aborted: a ttt tab could not spawn"));
                    sink.flush(&mut self.sched);
                    self.stop_host_side();
                    let events = std::mem::take(&mut sink.events);
                    return Ok(self.outcome(&log_path, events));
                }
            }
        }
        let mut ready_failed = false;
        for (name, _) in &ids {
            if let Err(e) = bridge.await_ready(name, Duration::from_secs(90)) {
                self.fail(name, &e);
                ready_failed = true;
            }
        }
        if ready_failed {
            self.emit(Event::info("run_end", "aborted: an agent was not ready"));
            sink.flush(&mut self.sched);
            self.stop_host_side();
            let events = std::mem::take(&mut sink.events);
            return Ok(self.outcome(&log_path, events));
        }
        let mut backend = super::ttt::TttBackend::new(bridge);
        for (name, id) in &ids {
            backend.register(name, id);
            if let Some(st) = self.sched.node_mut(name) {
                st.pane_id = id.clone();
            }
        }

        let mut memory: Box<dyn MemoryLink> = if self.crew.memory.is_some() {
            Box::new(MemoryClient::new())
        } else {
            Box::new(NoMemory)
        };
        let mut laya = LayaClient::new();
        drive(
            &mut self.sched,
            &self.roles,
            &mut backend,
            &mut *memory,
            &mut laya,
            None,
            &mut |sched| sink.flush(sched),
        );
        let _ = self.sched.settle_unreachable();
        self.sched.update_phase_progress();
        if let Some(mut t) = self.tailer.take() {
            t.stop();
        }
        sink.flush(&mut self.sched);
        let info = summary(&self.sched);
        self.emit(Event::info("run_end", info));
        sink.flush(&mut self.sched);
        self.stop_host_side();
        sink.flush(&mut self.sched);
        let events = std::mem::take(&mut sink.events);
        Ok(self.outcome(&log_path, events))
    }

    /// Stops the host-independent run pieces (router, provider key, tailer).
    fn stop_host_side(&mut self) {
        if let Some(mut t) = self.tailer.take() {
            t.stop();
        }
        if let Some(restore) = self.restore_provider.take() {
            restore();
        }
        if let Some(mut p) = self.proxy.take() {
            p.stop();
        }
    }

    /// Creates the disposable worktree, sweeping a leftover husk once (a
    /// crashed earlier run can leave an unregistered dir under herdr's root).
    fn create_worktree(&mut self, client: &herdr::Client) -> Result<(String, String), String> {
        let cwd = self.opts.cwd.clone();
        let branch = self.opts.worktree_branch.clone();
        let create = |c: &herdr::Client| {
            c.worktree_create(
                &cwd,
                &branch,
                "",
                "forseti-crew-wt",
                Duration::from_secs(90),
            )
        };
        let (ws, path) = match create(client) {
            Ok(v) => v,
            Err(first) => {
                let msg = first.to_string();
                let Some(swept) = husk_to_sweep(&msg) else {
                    return Err(format!("worktree create: {msg}"));
                };
                self.emit(Event::info(
                    "run_start",
                    format!("sweeping leftover worktree husk: {swept}"),
                ));
                let _ = std::fs::remove_dir_all(&swept);
                let _ = std::process::Command::new("git")
                    .args(["-C", &cwd, "worktree", "prune"])
                    .status();
                create(client).map_err(|e| format!("worktree create: {e}"))?
            }
        };
        if ws.is_empty() || path.is_empty() {
            return Err("worktree create returned no workspace/checkout".to_string());
        }
        Ok((ws, path))
    }

    /// Clears the projection, removes the worktree unless kept (with the husk
    /// sweep + branch delete), closes the tab unless kept, drops the bridge.
    fn teardown(&mut self, client: &herdr::Client, source: &str, tab_id: &str) {
        // the scheduler owns the mutable policy: settle_unreachable flips the
        // keep flags for a blocked run, and --review set them at construction
        let keep_worktree = self.sched.opts().keep_worktree;
        let keep_tab = self.sched.opts().keep_tab;
        let _ = client.agent_view_clear(source);
        // the router goes after the view: live streams, tailer, key, process
        if let Some(mut l) = self.live.take() {
            l.stop();
        }
        if let Some(mut t) = self.tailer.take() {
            t.stop();
        }
        if let Some(restore) = self.restore_provider.take() {
            restore();
        }
        if let Some(mut p) = self.proxy.take() {
            p.stop();
        }
        if !self.worktree_ws.is_empty() && !keep_worktree {
            if let Err(e) = client.worktree_remove(&self.worktree_ws, true) {
                // teardown's own failures are `teardown` events — a
                // node_failed here would flip the watch/TUI exit codes
                self.emit(Event::info("teardown", format!("worktree remove: {e}")));
            }
            let wt = self.effective_cwd.clone();
            if wt != self.opts.cwd && wt.contains(".herdr/worktrees") && Path::new(&wt).exists() {
                let _ = std::fs::remove_dir_all(&wt);
                let _ = std::process::Command::new("git")
                    .args(["-C", &self.opts.cwd, "worktree", "prune"])
                    .status();
            }
            if !self.opts.worktree_branch.is_empty() {
                let branch = self.opts.worktree_branch.clone();
                let deleted = std::process::Command::new("git")
                    .args(["-C", &self.opts.cwd, "branch", "-D", &branch])
                    .status()
                    .map(|s| s.success())
                    .unwrap_or(false);
                if !deleted {
                    // the worktree may still be registered: prune, retry once
                    let _ = std::process::Command::new("git")
                        .args(["-C", &self.opts.cwd, "worktree", "prune"])
                        .status();
                    let _ = std::process::Command::new("git")
                        .args(["-C", &self.opts.cwd, "branch", "-D", &branch])
                        .status();
                }
            }
            self.emit(Event::info("teardown", "worktree removed"));
        }
        if !keep_tab && !tab_id.is_empty() {
            let _ = client.call(
                "tab.close",
                &json!({"tab_id": tab_id}),
                Duration::from_secs(10),
            );
        }
        if !self.opts.cwd.is_empty() {
            let _ = std::fs::remove_file(
                Path::new(&self.opts.cwd)
                    .join(".forseti")
                    .join("crew-status.json"),
            );
        }
    }

    fn fail(&mut self, node: &str, msg: &str) {
        if let Some(st) = self.sched.node_mut(node) {
            st.status = Status::Failed;
            st.ended = Some(SystemTime::now());
        }
        self.emit(Event::on_node("node_failed", node).with_info(msg));
    }

    fn outcome(&self, log_path: &Path, events: Vec<Event>) -> RunOutcome {
        let summary_line = events
            .iter()
            .rev()
            .find(|ev| ev.kind == "run_end")
            .map(|ev| ev.info.clone());
        RunOutcome {
            summary: summary_line.unwrap_or_else(|| summary(&self.sched)),
            blocked: self
                .sched
                .snapshot()
                .iter()
                .any(|st| st.status == Status::Blocked),
            checks_failed: self.sched.checks_failed(),
            failed: self
                .sched
                .snapshot()
                .iter()
                .any(|st| st.status == Status::Failed),
            log_path: log_path.to_path_buf(),
            events,
        }
    }

    /// Starts one agent and waits for readiness (`idle|done|blocked`, 90 s),
    /// retrying once on `agent_pane_busy` (a freshly-spawned sandbox answers
    /// ping before its pane PTYs attach — hit live).
    fn start_agent(
        &mut self,
        client: &herdr::Client,
        name: &str,
        kind: &str,
        extra: &[String],
    ) -> Result<(), String> {
        let pane = self
            .sched
            .node(name)
            .map(|st| st.pane_id.clone())
            .unwrap_or_default();
        let mut args: Vec<String> = self
            .crew
            .agent(name)
            .map(|agent| self.pane_args(agent))
            .unwrap_or_default();
        // a disposable worktree is a fresh dir every run: pi's trust prompt
        // would stall every prompt, so auto-trust (user args can override)
        if !self.opts.worktree_branch.is_empty() {
            args.push("-a".to_string());
        }
        args.extend_from_slice(extra);
        let kind = if kind.is_empty() { "pi" } else { kind };
        let mut attempt = client
            .agent_start(name, kind, &pane, &args, Duration::from_secs(90))
            .map_err(|e| e.to_string());
        if let Err(e) = &attempt
            && e.contains("agent_pane_busy")
        {
            std::thread::sleep(Duration::from_millis(1200));
            attempt = client
                .agent_start(name, kind, &pane, &args, Duration::from_secs(90))
                .map_err(|e| e.to_string());
        }
        attempt?;
        let until = ["idle", "done", "blocked"].map(String::from);
        client
            .agent_wait(name, &until, Duration::from_secs(90))
            .map_err(|e| e.to_string())
    }
}

/// The wave loop (Go `Run.Run` step 7), backend-parameterized so a whole crew can
/// be driven over a fake backend in tests. Returns whether the run ended blocked.
pub(crate) fn drive<B: PaneBackend, F: FnMut(&mut Scheduler)>(
    sched: &mut Scheduler,
    roles: &RolePack,
    backend: &mut B,
    memory: &mut dyn MemoryLink,
    laya: &mut dyn LayaDecider,
    running: Option<&std::sync::Arc<std::sync::Mutex<LiveMaps>>>,
    flush: &mut F,
) -> bool {
    // the scheduler owns the per-node settle budget (Options.node_timeout)
    let timeout = sched.opts().node_timeout;
    let mut es = EdgeState::new();
    loop {
        let ready = sched.ready(&es);
        if ready.is_empty() {
            break;
        }
        let names: Vec<&str> = ready.iter().map(String::as_str).collect();
        sched.emit_phase_starts(&names);

        for name in &ready {
            if let Some(maps) = running
                && let Some(pane) = sched.node(name).map(|st| st.pane_id.clone())
                && !pane.is_empty()
            {
                Live::set_running(maps, name, Some(&pane));
            }
            {
                let mut ex = NodeExecutor {
                    sched: &mut *sched,
                    roles,
                    backend: &mut *backend,
                    memory: &mut *memory,
                    timeout,
                };
                ex.run(name);
            }
            if let Some(maps) = running {
                Live::set_running(maps, name, None);
            }
            flush(sched);
        }

        sched.run_due_checks(run_check);
        // laya-gated edges first: ONE grouped decision per settled node; the
        // plain pass below skips them (Go's evaluateLayaEdges ordering).
        for name in &ready {
            if sched.node(name).map(|st| st.status) == Some(Status::Done) {
                evaluate_laya_edges(sched, &mut es, laya, name);
            }
        }
        for name in &ready {
            if sched.node(name).map(|st| st.status) == Some(Status::Done) {
                let out = sched
                    .node(name)
                    .map(|st| st.output.clone())
                    .unwrap_or_default();
                sched.settle_edges(&mut es, name, &out);
            }
        }
        for ev in es.drain() {
            sched.emit(ev);
        }
        sched.update_phase_progress();
        flush(sched);
    }
    sched.settle_unreachable()
}

/// Resolves a settled node's laya-gated outgoing edges with ONE bounded
/// decision (S19): a choice over the group's targets (+ the implicit "other").
/// The argmax above the winning edge's `min_confidence` activates exactly that
/// edge; everything else skips with the reason recorded. A dead or failing
/// endpoint fail-safes to skipping — the graph never hangs on a decision.
///
/// State precedence: the group's first declared `state_file` (the artifact the
/// agent wrote — the pane scrollback truncates the encoder and carries the
/// prompt echo), else the node's captured output.
fn evaluate_laya_edges(
    sched: &mut Scheduler,
    es: &mut EdgeState,
    laya: &mut dyn LayaDecider,
    node: &str,
) {
    let edges: Vec<Edge> = sched
        .crew()
        .out_edges(node)
        .into_iter()
        .filter(|e| e.is_laya())
        .collect();
    if edges.is_empty() {
        return;
    }
    let mut state = sched
        .node(node)
        .map(|st| st.output.clone())
        .unwrap_or_default();
    if let Some(f) = edges.iter().find(|e| !e.state_file.is_empty()) {
        let path = Path::new(sched.effective_cwd()).join(&f.state_file);
        match std::fs::read_to_string(&path) {
            Ok(text) => {
                state = text;
                sched.emit(
                    Event::on_node("laya_decision", node)
                        .with_info(format!("state from file: {}", f.state_file)),
                );
            }
            Err(e) => sched.emit(Event::on_node("laya_decision", node).with_info(format!(
                "state_file unreadable ({e}) — falling back to output"
            ))),
        }
    }
    if state.len() > 4000 {
        // evidence, not a full dump (the upstream cap is 50k)
        state = truncate_utf8(&state, 4000);
    }
    let choices: Vec<LayaChoice> = edges
        .iter()
        .map(|e| LayaChoice {
            target: e.to.clone(),
            instructions: e.laya_instructions(),
        })
        .collect();
    match laya.decide(&state, &choices) {
        Ok(d) => {
            sched.emit(
                Event::on_node("laya_decision", node)
                    .with_model(d.checkpoint.clone())
                    .with_info(format!(
                        "chosen={} conf={:.3} latency={}ms",
                        d.chosen, d.confidence, d.latency_ms
                    )),
            );
            for e in &edges {
                if e.to == d.chosen && d.confidence >= e.min_confidence {
                    sched.fire_edge(es, e);
                } else {
                    let why = if e.to == d.chosen {
                        format!(
                            "abstained: conf {:.3} < min_confidence {:.2}",
                            d.confidence, e.min_confidence
                        )
                    } else {
                        "not chosen".to_string()
                    };
                    sched.miss_edge(es, e, &format!("laya gate: {why}"));
                }
            }
        }
        Err(err) => {
            for e in &edges {
                sched.miss_edge(es, e, &format!("laya gate failed-safe: {err}"));
            }
        }
    }
}

// MARK: staging helpers

/// Connects to the default session, or `socket_path_for(session)` when a named
/// session is requested. Bootstrapping a DOWN named session (the Go `connect`
/// temp-pane trick) is not ported: run the session first.
fn connect(session: &str) -> Result<herdr::Client, String> {
    if session.is_empty() {
        return herdr::Client::new().map_err(|e| e.to_string());
    }
    let sock = herdr::socket_path_for(session).map_err(|e| e.to_string())?;
    herdr::Client::at(sock).map_err(|e| format!("session {session:?} down (start it first): {e}"))
}

/// Builds the right-leaning spine of `n` panes and applies it (S8 verified
/// shape). Returns pane ids left→right + the tab id.
fn create_crew_tab(
    client: &herdr::Client,
    ws: &str,
    label: &str,
    cwd: &str,
    n: usize,
) -> Result<(Vec<String>, String), String> {
    if n == 0 {
        return Err("crew has no agents".to_string());
    }
    let mut root: Option<Value> = None;
    for i in 0..n {
        let pane = json!({"type": "pane", "label": format!("crew-{}", i + 1), "cwd": cwd});
        root = Some(match root {
            None => pane,
            Some(prev) => json!({
                "type": "split", "direction": "right",
                "ratio": i as f64 / (i + 1) as f64,
                "first": prev, "second": pane,
            }),
        });
    }
    let res = client
        .call(
            "layout.apply",
            &json!({
                "workspace_id": ws, "tab_label": label, "focus": false,
                "root": root.unwrap_or(Value::Null),
            }),
            Duration::from_secs(20),
        )
        .map_err(|e| format!("layout.apply: {e}"))?;
    let tab_id = res
        .pointer("/layout/tab_id")
        .and_then(Value::as_str)
        .unwrap_or_default()
        .to_string();
    let mut pane_ids = Vec::new();
    walk_panes(res.pointer("/layout/root"), &mut pane_ids);
    if pane_ids.len() != n {
        return Err(format!(
            "expected {n} panes from layout, got {}",
            pane_ids.len()
        ));
    }
    Ok((pane_ids, tab_id))
}

fn walk_panes(node: Option<&Value>, out: &mut Vec<String>) {
    let Some(node) = node else { return };
    match node.get("type").and_then(Value::as_str) {
        Some("pane") => {
            if let Some(id) = node.get("pane_id").and_then(Value::as_str) {
                out.push(id.to_string());
            }
        }
        _ => {
            walk_panes(node.get("first"), out);
            walk_panes(node.get("second"), out);
        }
    }
}

/// Extracts a leftover worktree path from a create failure
/// (`'/…/.herdr/worktrees/<repo>/<branch>' already exists`). Only paths under
/// herdr's own worktrees root are sweepable.
pub fn husk_to_sweep(msg: &str) -> Option<String> {
    let i = msg.find(".herdr/worktrees")?;
    let start = msg[..i].rfind('\'')? + 1;
    let end = i + msg[i..].find('\'')?;
    if !msg.is_char_boundary(start) || !msg.is_char_boundary(end) {
        return None;
    }
    Some(msg[start..end].to_string())
}

/// Splits a lazygit review pane onto the crew tab (P7-3).
fn open_review_pane(client: &herdr::Client, tab_id: &str, worktree: &str) -> Result<(), String> {
    if tab_id.is_empty() {
        return Err("no crew tab".to_string());
    }
    let res = client
        .call("pane.list", &json!({}), Duration::from_secs(10))
        .map_err(|e| e.to_string())?;
    let panes = res
        .get("panes")
        .and_then(Value::as_array)
        .cloned()
        .unwrap_or_default();
    let first = panes
        .iter()
        .find(|p| p.get("tab_id").and_then(Value::as_str) == Some(tab_id))
        .and_then(|p| p.get("pane_id").and_then(Value::as_str))
        .map(str::to_string)
        .ok_or_else(|| "no panes in crew tab".to_string())?;
    let split = client
        .call(
            "pane.split",
            &json!({"target_pane_id": first, "direction": "right", "focus": false}),
            Duration::from_secs(15),
        )
        .map_err(|e| e.to_string())?;
    let pane = split
        .pointer("/pane/pane_id")
        .and_then(Value::as_str)
        .unwrap_or_default()
        .to_string();
    if pane.is_empty() {
        return Err("split returned no pane id".to_string());
    }
    // no socket pane.run surface (S8): type into the fresh shell + Enter,
    // mirroring what `herdr pane run` does
    client
        .call(
            "pane.send_text",
            &json!({"pane_id": pane, "text": format!("lazygit -p {worktree}")}),
            Duration::from_secs(10),
        )
        .map_err(|e| e.to_string())?;
    client
        .call(
            "pane.send_keys",
            &json!({"pane_id": pane, "keys": ["enter"]}),
            Duration::from_secs(10),
        )
        .map_err(|e| e.to_string())?;
    let _ = client.call(
        "tab.focus",
        &json!({"tab_id": tab_id}),
        Duration::from_secs(10),
    );
    Ok(())
}

// MARK: sinks

/// The run-log writer + status-bridge writer. The bridge path is always the
/// main checkout (`opts.cwd`), never an effective worktree path — a badge in a
/// disposable checkout would go stale in the stable one.
struct Sink<'a> {
    cwd: String,
    crew: String,
    log: &'a mut File,
    /// `--headless`: echo every event on stdout as the Go CLI does.
    headless: bool,
    /// Every event written so far (the outcome's stream).
    events: Vec<Event>,
    /// Router events from the tailer thread (drained on every flush).
    tail: Option<std::sync::Arc<std::sync::Mutex<Vec<Event>>>>,
    /// Live status/watch events from the stream threads.
    extra: Option<std::sync::Arc<std::sync::Mutex<Vec<Event>>>>,
    /// herdr's live statuses to merge into the node states before draining.
    live: Option<std::sync::Arc<std::sync::Mutex<LiveMaps>>>,
}

impl<'a> Sink<'a> {
    fn new(cwd: &str, crew: &str, log: &'a mut File, headless: bool) -> Self {
        Sink {
            cwd: cwd.to_string(),
            crew: crew.to_string(),
            log,
            headless,
            events: Vec::new(),
            tail: None,
            extra: None,
            live: None,
        }
    }

    fn flush(&mut self, sched: &mut Scheduler) {
        if let Some(live) = &self.live
            && let Ok(maps) = live.lock()
        {
            super::live::apply_live(&maps, sched);
        }
        let mut batch = sched.drain_events();
        if let Some(t) = &self.tail
            && let Ok(mut queued) = t.lock()
        {
            batch.append(&mut queued);
        }
        if let Some(t) = &self.extra
            && let Ok(mut queued) = t.lock()
        {
            batch.append(&mut queued);
        }
        for ev in batch {
            if let Ok(line) = serde_json::to_string(&ev) {
                if self.headless {
                    println!("{line}");
                }
                let _ = writeln!(self.log, "{line}");
            }
            self.events.push(ev);
        }
        let _ = self.log.flush();
        self.write_bridge(sched);
    }

    fn write_bridge(&self, sched: &Scheduler) {
        if self.cwd.is_empty() {
            return;
        }
        let mut counts = std::collections::BTreeMap::new();
        let mut nodes = Vec::with_capacity(sched.crew().agents.len());
        for st in sched.snapshot() {
            *counts.entry(st.status.as_str()).or_insert(0usize) += 1;
            nodes.push(json!({"name": st.name, "status": st.status.as_str()}));
        }
        let body = json!({
            "crew": self.crew,
            "ts": super::scheduler::rfc3339_now(),
            "total": sched.crew().agents.len(),
            "done": counts.get("done").copied().unwrap_or(0),
            "running": counts.get("running").copied().unwrap_or(0),
            "blocked": counts.get("blocked").copied().unwrap_or(0),
            "failed": counts.get("failed").copied().unwrap_or(0),
            "checks_failed": sched.checks_failed_count(),
            "phase": sched.current_phase().unwrap_or(""),
            "tab": sched.tab_id(),
            "nodes": nodes,
        });
        let path = Path::new(&self.cwd)
            .join(".forseti")
            .join("crew-status.json");
        if let Some(dir) = path.parent() {
            let _ = std::fs::create_dir_all(dir);
        }
        let _ = std::fs::write(path, body.to_string());
    }
}

/// The Go `summary()` line: counts in a fixed order + the check tally + cost.
pub fn summary(sched: &Scheduler) -> String {
    let mut counts: std::collections::BTreeMap<&'static str, usize> =
        std::collections::BTreeMap::new();
    let mut cost = 0.0;
    for st in sched.snapshot() {
        *counts.entry(st.status.as_str()).or_insert(0) += 1;
        cost += st.cost_usd;
    }
    let mut parts = Vec::new();
    for s in ["done", "blocked", "failed", "skipped", "running", "pending"] {
        if let Some(n) = counts.get(s) {
            parts.push(format!("{s}={n}"));
        }
    }
    let failed_checks = sched.checks_failed_count();
    if failed_checks > 0 {
        parts.push(format!("checks_failed={failed_checks}"));
    }
    parts.push(format!("cost=${cost:.4}"));
    parts.join(" ")
}

/// `YYYYMM-HHMMSS.micro` — the Go run-log name shape (`200601-150405.000000`).
fn log_stamp() -> String {
    let now = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .unwrap_or_default();
    let secs = now.as_secs() as i64;
    let micros = now.subsec_micros();
    let days = secs.div_euclid(86_400);
    let sod = secs.rem_euclid(86_400);
    let (y, mo, _d) = super::scheduler::civil_from_days(days);
    format!(
        "{y}{mo:02}-{:02}{:02}{:02}.{micros:06}",
        sod / 3600,
        (sod % 3600) / 60,
        sod % 60
    )
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::crew::load;
    use crate::crew::services::{LayaDecision, NoLaya};
    use std::collections::VecDeque;

    struct FakeBackend {
        reads: VecDeque<String>,
        waits: VecDeque<String>,
    }

    impl PaneBackend for FakeBackend {
        fn read(&mut self, _t: &str, _s: &str, _l: i64) -> Result<String, String> {
            Ok(self.reads.pop_front().unwrap_or_default())
        }
        fn prompt(&mut self, _t: &str, _text: &str) -> Result<(), String> {
            Ok(())
        }
        fn prompt_wait(
            &mut self,
            _t: &str,
            _text: &str,
            _u: &[String],
            _timeout: Duration,
        ) -> Result<String, String> {
            Ok(self.waits.pop_front().unwrap_or_else(|| "idle".to_string()))
        }
        fn focus(&mut self, _t: &str) -> Result<(), String> {
            Ok(())
        }
        fn status(&mut self, _t: &str) -> Result<String, String> {
            Ok("idle".to_string())
        }
    }

    fn setup(yaml: &str) -> (Scheduler, RolePack, std::path::PathBuf) {
        use std::sync::atomic::{AtomicUsize, Ordering};
        static N: AtomicUsize = AtomicUsize::new(0);
        let n = N.fetch_add(1, Ordering::SeqCst);
        let dir = std::env::temp_dir().join(format!("forseti-drive-{}-{n}", std::process::id()));
        let _ = std::fs::remove_dir_all(&dir);
        std::fs::create_dir_all(dir.join(".forseti/bus")).expect("bus dir");
        let crew = load(yaml).expect("crew validates");
        let roles = RolePack::new(&crew.roles);
        let sched = Scheduler::new(
            crew,
            Options {
                cwd: dir.to_string_lossy().into_owned(),
                ..Default::default()
            },
        );
        (sched, roles, dir)
    }

    #[test]
    fn drive_runs_a_linear_crew_to_done() {
        let (mut sched, roles, dir) = setup(
            r#"
name: linear
agents:
  - {name: plan, prompt: plan}
  - {name: build, prompt: "build {{ .plan }}"}
edges:
  - {from: plan, to: build}
"#,
        );
        let mut backend = FakeBackend {
            reads: VecDeque::from(vec![
                String::new(),
                "PLAN".into(),
                String::new(),
                "BUILT".into(),
            ]),
            waits: VecDeque::from(vec!["idle".into(), "idle".into()]),
        };
        let mut memory = NoMemory;
        let mut flushed = 0;
        let blocked = drive(
            &mut sched,
            &roles,
            &mut backend,
            &mut memory,
            &mut NoLaya,
            None,
            &mut |_s: &mut Scheduler| flushed += 1,
        );
        assert!(!blocked);
        assert_eq!(sched.node("plan").unwrap().status, Status::Done);
        assert_eq!(sched.node("build").unwrap().status, Status::Done);
        assert_eq!(
            std::fs::read_to_string(dir.join(".forseti/bus/build.md")).unwrap(),
            "BUILT"
        );
        // one flush per node + one per wave end
        assert_eq!(flushed, 4);
    }

    #[test]
    fn drive_settles_a_verdict_loop_and_a_phase_barrier() {
        let (mut sched, roles, _dir) = setup(
            r#"
name: loop
agents:
  - {name: entry, prompt: go}
  - {name: subject, prompt: write}
  - {name: reviewer, prompt: review}
edges:
  - {from: entry, to: subject}
  - {from: subject, to: reviewer}
  - {from: reviewer, to: subject, when: "verdict:lbtm", max_visits: 2, loop: true}
"#,
        );
        // Reads are baseline+answer per DISPATCH, in wave order:
        // entry, subject, reviewer, subject, reviewer, subject, reviewer.
        fn push(reads: &mut VecDeque<String>, answer: &str) {
            reads.push_back(String::new());
            reads.push_back(answer.to_string());
        }
        let mut reads = VecDeque::new();
        push(&mut reads, "go");
        push(&mut reads, "draft");
        push(&mut reads, "body\n\nVERDICT: LBTM");
        push(&mut reads, "draft2");
        push(&mut reads, "body\n\nVERDICT: LBTM");
        push(&mut reads, "draft3");
        push(&mut reads, "body\n\nVERDICT: LGTM");
        let mut backend = FakeBackend {
            reads,
            waits: VecDeque::new(),
        };
        let mut memory = NoMemory;
        let blocked = drive(
            &mut sched,
            &roles,
            &mut backend,
            &mut memory,
            &mut NoLaya,
            None,
            &mut |_s: &mut Scheduler| {},
        );
        assert!(!blocked);
        assert_eq!(sched.node("subject").unwrap().visits, 3);
        assert_eq!(sched.node("reviewer").unwrap().visits, 3);
        assert_eq!(sched.checks_failed_count(), 0);
    }

    #[test]
    fn drive_honours_a_phase_barrier() {
        let (mut sched, roles, _dir) = setup(
            r#"
name: phased
agents:
  - {name: a1, prompt: x}
  - {name: b1, prompt: y}
phases:
  - {name: p1, instructions: first, agents: [a1]}
  - {name: p2, instructions: second, agents: [b1]}
"#,
        );
        let mut backend = FakeBackend {
            reads: VecDeque::from(vec![String::new(), "A".into(), String::new(), "B".into()]),
            waits: VecDeque::new(),
        };
        let mut memory = NoMemory;
        drive(
            &mut sched,
            &roles,
            &mut backend,
            &mut memory,
            &mut NoLaya,
            None,
            &mut |_s: &mut Scheduler| {},
        );
        let phases: Vec<&str> = sched
            .events()
            .iter()
            .filter(|e| e.kind == "phase_start")
            .map(|e| e.phase.as_str())
            .collect();
        assert_eq!(phases, ["p1", "p2"], "phase 2 starts after phase 1");
        assert_eq!(sched.current_phase(), None);
    }

    #[test]
    fn a_blocked_node_blocks_the_run_and_keeps_the_tab() {
        let (mut sched, roles, _dir) = setup(
            r#"
name: block
agents:
  - {name: a, prompt: x}
"#,
        );
        let mut backend = FakeBackend {
            reads: VecDeque::from(vec![String::new()]),
            waits: VecDeque::from(vec!["blocked".to_string()]),
        };
        let mut memory = NoMemory;
        let blocked = drive(
            &mut sched,
            &roles,
            &mut backend,
            &mut memory,
            &mut NoLaya,
            None,
            &mut |_s: &mut Scheduler| {},
        );
        assert!(blocked);
        assert!(sched.opts().keep_tab && sched.opts().keep_worktree);
        assert_eq!(sched.node("a").unwrap().status, Status::Blocked);
    }

    #[test]
    fn a_flush_drains_the_router_tailer_into_the_run_log() {
        let (mut sched, _roles, dir) = setup(
            r#"
name: tail-sink
agents:
  - {name: a, prompt: x}
"#,
        );
        let path = dir.join("routing.jsonl");
        let tailer = RoutingTailer::spawn(path.clone());
        let log_path = dir.join("run.jsonl");
        let mut log = File::create(&log_path).unwrap();
        {
            let mut sink = Sink::new(&dir.to_string_lossy(), "tail-sink", &mut log, false);
            sink.tail = Some(tailer.events());
            std::fs::write(
                &path,
                b"{\"ts\":\"T\",\"route_id\":\"auto\",\"algorithm\":\"stage_router\",\"model\":\"m\",\"prompt_tokens\":1,\"completion_tokens\":2,\"total_tokens\":3}\n",
            )
            .unwrap();
            std::thread::sleep(Duration::from_millis(1100));
            sink.flush(&mut sched);
            assert_eq!(
                sink.events
                    .iter()
                    .filter(|e| e.kind == "route_decision")
                    .count(),
                1,
                "the tailed record must reach the sink"
            );
        }
        let mut tailer = tailer;
        tailer.stop();
    }

    #[test]
    fn flush_writes_jsonl_lines_and_drains_the_scheduler() {
        let (mut sched, _roles, dir) = setup(
            r#"
name: sink
agents:
  - {name: a, prompt: x}
"#,
        );
        sched.node_mut("a").unwrap().status = Status::Done;
        sched.emit(Event::info("run_start", "hello"));
        let path = dir.join("run.jsonl");
        let mut log = File::create(&path).unwrap();
        {
            let mut sink = Sink::new(&dir.to_string_lossy(), "sink", &mut log, false);
            sink.flush(&mut sched);
        }
        assert!(sched.events().is_empty(), "flush drains the queue");
        let text = std::fs::read_to_string(&path).unwrap();
        let lines: Vec<&str> = text.lines().collect();
        assert_eq!(lines.len(), 1, "{text}");
        let v: Value = serde_json::from_str(lines[0]).unwrap();
        assert_eq!(v["type"], "run_start");
        assert_eq!(v["info"], "hello");
        assert!(v["ts"].as_str().unwrap().ends_with('Z'));
    }

    #[test]
    fn the_status_bridge_matches_the_ttt_contract() {
        // crew-status.json is consumed by the ttt sidebar + badge (P15):
        // crew/total/done/running/blocked/failed/checks_failed/phase/tab/nodes.
        let (mut sched, _roles, dir) = setup(
            r#"
name: bridge
agents:
  - {name: a, prompt: x}
  - {name: b, prompt: y}
phases:
  - {name: p1, instructions: one, agents: [a]}
  - {name: p2, instructions: two, agents: [b]}
"#,
        );
        sched.set_tab_id("w1:t7");
        sched.node_mut("a").unwrap().status = Status::Done;
        sched.node_mut("b").unwrap().status = Status::Blocked;
        // a wave's progress pass is what closes p1 (blocked holds p2 open)
        sched.update_phase_progress();
        let mut log = File::create(dir.join("run.jsonl")).unwrap();
        {
            let sink = Sink::new(&dir.to_string_lossy(), "bridge", &mut log, false);
            sink.write_bridge(&sched);
        }
        let body =
            std::fs::read_to_string(dir.join(".forseti/crew-status.json")).expect("bridge written");
        let v: Value = serde_json::from_str(&body).unwrap();
        assert_eq!(v["crew"], "bridge");
        assert_eq!(v["total"], 2);
        assert_eq!(v["done"], 1);
        assert_eq!(v["blocked"], 1);
        assert_eq!(v["failed"], 0);
        assert_eq!(v["checks_failed"], 0);
        assert_eq!(v["phase"], "p2", "first not-done phase");
        assert_eq!(v["tab"], "w1:t7");
        let nodes = v["nodes"].as_array().unwrap();
        assert_eq!(nodes.len(), 2);
        assert_eq!(nodes[0]["name"], "a");
        assert_eq!(nodes[1]["status"], "blocked");
        assert!(v["ts"].as_str().unwrap().ends_with('Z'));
    }

    struct StubLaya {
        decision: Result<LayaDecision, String>,
        seen: Vec<String>,
    }

    impl StubLaya {
        fn choosing(chosen: &str, confidence: f64) -> Self {
            StubLaya {
                decision: Ok(LayaDecision {
                    chosen: chosen.to_string(),
                    confidence,
                    checkpoint: "ckpt".to_string(),
                    latency_ms: 12,
                }),
                seen: Vec::new(),
            }
        }

        fn failing(err: &str) -> Self {
            StubLaya {
                decision: Err(err.to_string()),
                seen: Vec::new(),
            }
        }
    }

    impl LayaDecider for StubLaya {
        fn decide(&mut self, state: &str, _choices: &[LayaChoice]) -> Result<LayaDecision, String> {
            self.seen.push(state.to_string());
            self.decision.clone()
        }
    }

    fn laya_crew() -> &'static str {
        r#"
name: laya-fire
agents:
  - {name: a, prompt: x}
  - {name: b, prompt: y}
  - {name: c, prompt: z}
edges:
  - {from: a, to: b, when: "laya:choice:go b", min_confidence: 0.5}
  - {from: a, to: c, when: "laya:choice:go c", min_confidence: 0.5}
"#
    }

    fn drive_laya(yaml: &str, laya: &mut StubLaya) -> Scheduler {
        let (mut sched, roles, _dir) = setup(yaml);
        let mut backend = FakeBackend {
            reads: VecDeque::from(vec![
                String::new(),
                "decide this".into(),
                String::new(),
                "b did it".into(),
            ]),
            waits: VecDeque::new(),
        };
        let mut memory = NoMemory;
        drive(
            &mut sched,
            &roles,
            &mut backend,
            &mut memory,
            laya,
            None,
            &mut |_s: &mut Scheduler| {},
        );
        sched
    }

    #[test]
    fn a_laya_gate_activates_only_the_chosen_edge() {
        let mut laya = StubLaya::choosing("b", 0.9);
        let sched = drive_laya(laya_crew(), &mut laya);
        assert_eq!(laya.seen, ["decide this"], "state = the node output");
        assert_eq!(sched.node("b").unwrap().status, Status::Done);
        assert_eq!(sched.node("c").unwrap().status, Status::Skipped);
        let decision = sched
            .events()
            .iter()
            .find(|e| e.kind == "laya_decision")
            .expect("decision event");
        assert_eq!(decision.model, "ckpt");
        assert_eq!(decision.info, "chosen=b conf=0.900 latency=12ms");
        let skips: Vec<&Event> = sched
            .events()
            .iter()
            .filter(|e| e.kind == "edge_skip")
            .collect();
        assert_eq!(skips.len(), 1);
        assert_eq!(skips[0].info, "a>c laya gate: not chosen");
    }

    #[test]
    fn a_below_threshold_confidence_abstains_from_its_own_edge() {
        let mut laya = StubLaya::choosing("b", 0.2);
        let sched = drive_laya(laya_crew(), &mut laya);
        assert_eq!(sched.node("b").unwrap().status, Status::Skipped);
        assert_eq!(sched.node("c").unwrap().status, Status::Skipped);
        let skips: Vec<&Event> = sched
            .events()
            .iter()
            .filter(|e| e.kind == "edge_skip")
            .collect();
        assert_eq!(skips.len(), 2);
        assert_eq!(
            skips[0].info,
            "a>b laya gate: abstained: conf 0.200 < min_confidence 0.50"
        );
        assert_eq!(skips[1].info, "a>c laya gate: not chosen");
    }

    #[test]
    fn a_dead_laya_endpoint_fail_safes_to_skips() {
        let mut laya = StubLaya::failing("laya endpoint unreachable (http://127.0.0.1:8751)");
        let sched = drive_laya(laya_crew(), &mut laya);
        assert_eq!(sched.node("b").unwrap().status, Status::Skipped);
        assert_eq!(sched.node("c").unwrap().status, Status::Skipped);
        for ev in sched.events().iter().filter(|e| e.kind == "edge_skip") {
            assert!(ev.info.contains("laya gate failed-safe:"), "{}", ev.info);
        }
        assert_eq!(
            sched
                .events()
                .iter()
                .filter(|e| e.kind == "laya_decision")
                .count(),
            0,
            "a failed decision emits no chosen event"
        );
    }

    #[test]
    fn a_state_file_is_the_evidence_instead_of_the_output() {
        let (mut sched, roles, dir) = setup(
            r#"
name: laya-file
agents:
  - {name: a, prompt: x}
  - {name: b, prompt: y}
edges:
  - {from: a, to: b, when: "laya:choice:go", min_confidence: 0.5, state_file: ARTIFACT.md}
"#,
        );
        std::fs::write(dir.join("ARTIFACT.md"), "FROM FILE").unwrap();
        let mut backend = FakeBackend {
            reads: VecDeque::from(vec![
                String::new(),
                "pane output that must not be judged".into(),
                String::new(),
                "done".into(),
            ]),
            waits: VecDeque::new(),
        };
        let mut memory = NoMemory;
        let mut laya = StubLaya::choosing("b", 0.9);
        drive(
            &mut sched,
            &roles,
            &mut backend,
            &mut memory,
            &mut laya,
            None,
            &mut |_s: &mut Scheduler| {},
        );
        assert_eq!(laya.seen, ["FROM FILE"]);
        assert_eq!(sched.node("b").unwrap().status, Status::Done);
        assert!(
            sched
                .events()
                .iter()
                .any(|e| e.kind == "laya_decision" && e.info == "state from file: ARTIFACT.md")
        );
    }

    #[test]
    fn summary_matches_the_go_shape() {
        let (mut sched, _roles, _dir) = setup(
            r#"
name: sum
agents:
  - {name: a, prompt: x}
  - {name: b, prompt: y}
edges:
  - {from: a, to: b}
"#,
        );
        sched.node_mut("a").unwrap().status = Status::Done;
        sched.node_mut("a").unwrap().cost_usd = 0.002;
        sched.node_mut("b").unwrap().status = Status::Skipped;
        assert_eq!(summary(&sched), "done=1 skipped=1 cost=$0.0020");
    }

    #[test]
    fn husk_to_sweep_extracts_only_herdr_worktree_paths() {
        let msg = "worktree create: '/w/repo/.herdr/worktrees/forseti/wt-1' already exists";
        assert_eq!(
            husk_to_sweep(msg).as_deref(),
            Some("/w/repo/.herdr/worktrees/forseti/wt-1")
        );
        assert_eq!(husk_to_sweep("some other failure"), None);
        assert_eq!(husk_to_sweep("'/tmp/x' exists"), None);
        // a path prefix with no closing quote is not sweepable
        assert_eq!(husk_to_sweep("'/w/.herdr/worktrees/x"), None);
    }

    #[test]
    fn log_stamp_has_the_go_shape() {
        let s = log_stamp();
        // YYYYMM-HHMMSS.micro
        assert_eq!(s.len(), 20, "{s}");
        assert_eq!(&s[6..7], "-");
        assert_eq!(&s[13..14], ".");
        assert!(
            s.chars()
                .all(|c| c.is_ascii_digit() || c == '-' || c == '.')
        );
    }
}
