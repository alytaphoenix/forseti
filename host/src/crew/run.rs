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
use super::model::Crew;
use super::role::RolePack;
use super::runner::{HerdrBackend, MemoryLink, NoMemory, NodeExecutor, PaneBackend};
use super::scheduler::run_check;
use super::scheduler::{EdgeState, Event, Options, Scheduler, Status};

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
}

impl Run {
    pub fn new(crew: Crew, mut opts: Options) -> Self {
        if opts.cwd.is_empty() {
            opts.cwd = std::env::current_dir()
                .map(|p| p.to_string_lossy().into_owned())
                .unwrap_or_default();
        }
        let roles = RolePack::new(&crew.roles);
        let sched = Scheduler::new(crew.clone(), opts.clone());
        Run {
            sched,
            roles,
            crew,
            opts,
        }
    }

    pub fn scheduler(&self) -> &Scheduler {
        &self.sched
    }

    /// Stages and runs the crew. Blocking; returns the outcome even when nodes
    /// failed (the caller maps `blocked`/`checks_failed` to an exit code).
    pub fn run(&mut self) -> Result<RunOutcome, String> {
        let client = connect(&self.opts.session)?;
        let cwd = self.opts.cwd.clone();

        // 0. deferred integrations must fail loudly, never silently
        if !self.crew.routes.is_empty() {
            return Err("routes need the switchyard proxy (18D-5c)".to_string());
        }
        if !self.opts.worktree_branch.is_empty() {
            return Err("--worktree needs the worktree lifecycle (18D-5c)".to_string());
        }
        if std::env::var("FORSETI_CREW_MODEL")
            .ok()
            .as_deref()
            .is_some_and(|v| !v.is_empty())
            && self.opts.model_override.is_empty()
        {
            self.opts.model_override = std::env::var("FORSETI_CREW_MODEL").unwrap_or_default();
        }

        std::fs::create_dir_all(Path::new(&cwd).join(".forseti/bus"))
            .map_err(|e| format!("bus dir: {e}"))?;
        let runs = Path::new(&cwd).join(".forseti/runs");
        std::fs::create_dir_all(&runs).map_err(|e| format!("runs dir: {e}"))?;

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

        // 2. workspace + the crew tab (right-leaning BSP spine, S8)
        let ws = if self.opts.workspace_id.is_empty() {
            client
                .focused_workspace()
                .map_err(|e| format!("focused workspace: {e}"))?
        } else {
            self.opts.workspace_id.clone()
        };
        let n = self.crew.agents.len();
        let (pane_ids, tab_id) = create_crew_tab(&client, &ws, &self.opts.tab_label, &cwd, n)?;
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
            teardown(&client, &source, &self.opts, tab_id.as_str());
            let events = std::mem::take(&mut sink.events);
            return Ok(self.outcome(&log_path, events));
        }
        sink.flush(&mut self.sched);

        // 5. drive the graph
        let mut backend = HerdrBackend::new(&client);
        // 18D-5c swaps this for the HTTP link (auto-recall/auto-write).
        let mut memory = NoMemory;
        drive(
            &mut self.sched,
            &self.roles,
            &mut backend,
            &mut memory,
            self.opts.node_timeout,
            &mut |sched| sink.flush(sched),
        );

        // 6. summary + teardown (blocked keeps the tab, P13 BUG-09)
        let _ = self.sched.settle_unreachable();
        self.sched.update_phase_progress();
        let summary = summary(&self.sched);
        self.emit(Event::info("run_end", summary.clone()));
        sink.flush(&mut self.sched);
        teardown(&client, &source, &self.opts, tab_id.as_str());

        let events = std::mem::take(&mut sink.events);
        Ok(self.outcome(&log_path, events))
    }

    fn emit(&mut self, ev: Event) {
        self.sched.emit(ev);
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
        let mut args: Vec<String> = Vec::new();
        if let Some(agent) = self.crew.agent(name) {
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
pub fn drive<B: PaneBackend, F: FnMut(&mut Scheduler)>(
    sched: &mut Scheduler,
    roles: &RolePack,
    backend: &mut B,
    memory: &mut dyn MemoryLink,
    timeout: Duration,
    flush: &mut F,
) -> bool {
    let mut es = EdgeState::new();
    loop {
        let ready = sched.ready(&es);
        if ready.is_empty() {
            break;
        }
        let names: Vec<&str> = ready.iter().map(String::as_str).collect();
        sched.emit_phase_starts(&names);

        for name in &ready {
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
            flush(sched);
        }

        sched.run_due_checks(run_check);
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

/// Clears the projection, closes the tab unless kept, removes the status bridge.
fn teardown(client: &herdr::Client, source: &str, opts: &Options, tab_id: &str) {
    let _ = client.agent_view_clear(source);
    if !opts.keep_tab && !tab_id.is_empty() {
        let _ = client.call(
            "tab.close",
            &json!({"tab_id": tab_id}),
            Duration::from_secs(10),
        );
    }
    if !opts.cwd.is_empty() {
        let _ = std::fs::remove_file(
            Path::new(&opts.cwd)
                .join(".forseti")
                .join("crew-status.json"),
        );
    }
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
}

impl<'a> Sink<'a> {
    fn new(cwd: &str, crew: &str, log: &'a mut File, headless: bool) -> Self {
        Sink {
            cwd: cwd.to_string(),
            crew: crew.to_string(),
            log,
            headless,
            events: Vec::new(),
        }
    }

    fn flush(&mut self, sched: &mut Scheduler) {
        for ev in sched.drain_events() {
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
            Duration::from_secs(5),
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
            Duration::from_secs(5),
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
            Duration::from_secs(5),
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
            Duration::from_secs(5),
            &mut |_s: &mut Scheduler| {},
        );
        assert!(blocked);
        assert!(sched.opts().keep_tab && sched.opts().keep_worktree);
        assert_eq!(sched.node("a").unwrap().status, Status::Blocked);
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

    #[test]
    fn a_run_with_routes_fails_loudly() {
        let crew = load(
            r#"
name: routed
routes:
  - {id: auto, type: passthrough, capable: opencode-go/beam-free}
agents:
  - {name: a, prompt: x, route: auto}
"#,
        )
        .expect("routes validate");
        let mut run = Run::new(crew, Options::default());
        let err = run.run().unwrap_err();
        assert!(err.contains("18D-5c"), "{err}");
    }
}
