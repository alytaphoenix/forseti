//! The crew scheduler (Phase 18D-4): the Rust port of the P13/P19 wave
//! scheduler in `crew/internal/runner/runner.go`.
//!
//! It owns the *decisions* — which node dispatches, which edge fires, when a
//! phase barrier opens, when a declared check is due — and nothing else: no
//! herdr connection, no panes, no threads. The runner (18D-5) drives it wave by
//! wave, exactly as the Go `Run.Run` loop does:
//!
//! ```text
//! loop { ready = agents where node_ready(); run them; run_due_checks();
//!        for each settled node: for each out edge: fire or miss;
//!        update_phase_progress() }
//! ```
//!
//! Semantics ported 1:1 (each has a test below mirroring the Go suite):
//!
//! * **fan-in AND** (P13 BUG-05): a node dispatches only once EVERY incoming
//!   edge fired; no incoming edges = entry.
//! * **cycle back edges** (`Crew::back_edges`, DFS-classified from the entry
//!   nodes + explicit `loop:`) gate re-dispatch only — requiring them up front
//!   would deadlock every cycle.
//! * **re-arm on a re-fire** (P13 BUG-02): a firing edge into a settled target
//!   resets it to pending (`output`/`retries` cleared) so bounded loops loop.
//! * **`max_visits`** is a hard cap per edge; an uncapped edge fires on EVERY
//!   settle of its source (P19: the loop body must re-enter, not just the
//!   loop-back edge). `fire_edge` past the cap collects an `edge_skip` event.
//! * **a missed gate** permanently skips a still-pending target and collects a
//!   skip event (nothing dispatches half-connected).
//! * **phase barriers** (P11): a phase's nodes dispatch only once every earlier
//!   phase is fully terminal (done/failed/skipped — blocked HOLDS the barrier);
//!   `phase_start`/`phase_done` are emitted from the scheduler.
//! * **per-visit checks** (P19 follow-up): a check's done-flag is keyed
//!   `check:<i>#<visits>`, so a re-dispatched node re-proves the artifact; a
//!   later pass clears the `_failed` marker (the exit code follows the LATEST
//!   iteration).
//!
//! Event emission is collected, not streamed: the driver drains [`events`] and
//! writes the run log / status bridge / TUI rows (18D-5). That keeps this
//! module pure and its tests deterministic.
//!
//! [`events`]: Scheduler::events

use std::collections::{BTreeMap, HashMap, HashSet};
use std::process::{Command, Stdio};
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Condvar, Mutex};
use std::time::{Duration, SystemTime, UNIX_EPOCH};

use serde::{Deserialize, Serialize};

use super::bus;
use super::model::{Crew, Edge};

/// The per-check shell budget (Go's 5 minutes).
pub const CHECK_TIMEOUT: Duration = Duration::from_secs(5 * 60);

/// A node's run state. The Go engine keeps these as strings; the enum exists so
/// a typo cannot silently widen a terminal set, and [`Status::as_str`] is the
/// only spelling that reaches the log/status file.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash, Default)]
pub enum Status {
    #[default]
    Pending,
    Running,
    Done,
    Blocked,
    Failed,
    Skipped,
}

impl Status {
    /// The exact string the Go engine writes.
    pub fn as_str(self) -> &'static str {
        match self {
            Status::Pending => "pending",
            Status::Running => "running",
            Status::Done => "done",
            Status::Blocked => "blocked",
            Status::Failed => "failed",
            Status::Skipped => "skipped",
        }
    }

    /// done/failed/skipped release a phase barrier; **blocked holds it** (P11:
    /// surfaced, never auto-answered).
    pub fn releases_barrier(self) -> bool {
        matches!(self, Status::Done | Status::Failed | Status::Skipped)
    }

    /// A declared check may run once the after-node is settled; blocked/skipped
    /// are advisory `check_skip`s, only failed is a failure (P13 BUG-25).
    pub fn is_settled(self) -> bool {
        !matches!(self, Status::Pending | Status::Running)
    }
}

impl std::fmt::Display for Status {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str(self.as_str())
    }
}

/// One node's scheduling state (a value copy leaves the scheduler via
/// [`Scheduler::snapshot`], so readers never race a writer — P13 BUG-15).
#[derive(Debug, Clone, Default)]
pub struct NodeState {
    pub name: String,
    pub status: Status,
    pub pane_id: String,
    pub output: String,
    /// Settles of this node — the key a check's done-flag is visit-stamped on.
    pub visits: u32,
    pub started: Option<SystemTime>,
    pub ended: Option<SystemTime>,
    /// herdr's `agent_status` from the status stream (not the run state).
    pub live_status: String,
    pub cost_usd: f64,
    pub ctx_pct: f64,
    pub retries: u32,
    /// Phase position (-1 when the crew declares no phases).
    pub phase_idx: i32,
}

/// P11 barrier bookkeeping for one declared phase.
#[derive(Debug, Clone, Default)]
pub struct PhaseState {
    pub name: String,
    pub nodes: Vec<String>,
    /// Terminal members as of the last [`Scheduler::update_phase_progress`].
    pub terminal: usize,
    /// `phase_start` emitted.
    pub started: bool,
    /// `phase_done` emitted (the barrier is open).
    pub done: bool,
}

/// A run-log record (also the TUI row update). Field names and `omitempty`
/// behavior mirror the Go `runner.Event`, so the JSONL stays consumable by the
/// same `watch`/ttt readers.
#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct Event {
    /// RFC3339 UTC, millisecond precision (`time.Time`'s JSON shape).
    #[serde(default)]
    pub ts: String,
    #[serde(rename = "type")]
    pub kind: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub node: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub phase: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub info: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub model: String,
    #[serde(default, skip_serializing_if = "is_zero")]
    pub cost_usd: f64,
    #[serde(default, skip_serializing_if = "is_zero")]
    pub ctx_pct: f64,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub line: String,
    /// Set when a live monitor already wrote this line to the run log (so the
    /// regular flush appends it to the outcome without duplicating it).
    #[serde(skip)]
    #[doc(hidden)]
    pub written: bool,
}

fn is_zero(v: &f64) -> bool {
    *v == 0.0
}

impl Event {
    /// An event of `kind` with everything else defaulted.
    pub fn new(kind: &str) -> Self {
        Event {
            kind: kind.to_string(),
            ..Default::default()
        }
    }

    /// An event of `kind` about `node`.
    pub fn on_node(kind: &str, node: &str) -> Self {
        Event {
            kind: kind.to_string(),
            node: node.to_string(),
            ..Default::default()
        }
    }

    /// An event of `kind` about `phase`.
    pub fn on_phase(kind: &str, phase: &str) -> Self {
        Event {
            kind: kind.to_string(),
            phase: phase.to_string(),
            ..Default::default()
        }
    }

    /// An informational event of `kind` (no node).
    pub fn info(kind: &str, info: impl Into<String>) -> Self {
        Event {
            kind: kind.to_string(),
            info: info.into(),
            ..Default::default()
        }
    }

    pub fn with_info(mut self, info: impl Into<String>) -> Self {
        self.info = info.into();
        self
    }

    pub fn with_model(mut self, model: impl Into<String>) -> Self {
        self.model = model.into();
        self
    }

    pub fn with_metrics(mut self, cost_usd: f64, ctx_pct: f64) -> Self {
        self.cost_usd = cost_usd;
        self.ctx_pct = ctx_pct;
        self
    }
}

/// The scheduler's shared edge bookkeeping (one per run). Skip events are
/// collected here and drained by the driver AFTER the edge pass, so an emit
/// never happens under a borrowed scheduler state (P13 BUG-01).
#[derive(Debug, Clone, Default)]
pub struct EdgeState {
    /// "from>to" → times fired.
    visited: HashMap<String, usize>,
    /// "from>to" → last evaluation matched.
    ok: HashMap<String, bool>,
    skips: Vec<Event>,
}

impl EdgeState {
    pub fn new() -> Self {
        Self::default()
    }

    /// The edge's identity key, `from>to`.
    pub fn key(e: &Edge) -> String {
        format!("{}>{}", e.from, e.to)
    }

    /// Whether this edge has fired at least once (and still counts as satisfied).
    pub fn fired(&self, e: &Edge) -> bool {
        self.ok.get(&Self::key(e)).copied().unwrap_or(false)
    }

    /// Times this edge fired.
    pub fn visited(&self, e: &Edge) -> usize {
        self.visited.get(&Self::key(e)).copied().unwrap_or(0)
    }

    /// Collects and clears the pending skip events.
    pub fn drain(&mut self) -> Vec<Event> {
        std::mem::take(&mut self.skips)
    }
}

/// Runner options that influence scheduling. The remaining Go `Options`
/// (worktree/tab/session/model override) are 18D-5's business.
#[derive(Debug, Clone)]
pub struct Options {
    /// Pane cwd / checks cwd (the repo root).
    pub cwd: String,
    /// herdr tab label (default `forseti-crew`).
    pub tab_label: String,
    /// Per-node prompt settle timeout (default 10 m).
    pub node_timeout: Duration,
    pub keep_tab: bool,
    pub keep_worktree: bool,
    /// End the run on a lazygit review of the worktree; implies keeping both.
    pub review: bool,
    /// Substitutes the model of every direct-model node during an outage; route
    /// nodes keep their switchyard pools (`--model` / `FORSETI_CREW_MODEL`).
    pub model_override: String,
    /// herdr workspace to stage the crew tab in ("" = the focused workspace).
    pub workspace_id: String,
    /// Named herdr session ("" = the default session).
    pub session: String,
    /// Run inside a disposable worktree ("" = the main checkout). 18D-5c.
    pub worktree_branch: String,
    /// Stream every event as a JSON line on stdout (`--headless`).
    pub headless: bool,
    /// Where agents run: "herdr" (default) or "ttt" (Phase 21).
    pub host: String,
}

impl Default for Options {
    fn default() -> Self {
        Options {
            cwd: String::new(),
            tab_label: "forseti-crew".to_string(),
            node_timeout: Duration::from_secs(10 * 60),
            keep_tab: false,
            keep_worktree: false,
            review: false,
            model_override: String::new(),
            workspace_id: String::new(),
            session: String::new(),
            worktree_branch: String::new(),
            headless: false,
            host: String::new(),
        }
    }
}

/// The scheduling state of one run. Mirrors the Go `runner.Run` minus the
/// herdr/panes/log machinery.
pub struct Scheduler {
    crew: Crew,
    opts: Options,
    nodes: BTreeMap<String, NodeState>,
    phases: Vec<PhaseState>,
    /// Cycle re-entry keys (`from>to`) from `Crew::back_edges`.
    back_edges: HashSet<String>,
    /// Done-flags: `check:<i>#<visits>` and `<name>_failed`.
    checks_done: HashSet<String>,
    events: Vec<Event>,
    /// Stable per-run id for memory provenance (Go `.UTC()` format).
    run_id: String,
    effective_cwd: String,
    /// The crew tab id (set by the orchestrator; "" before staging).
    tab_id: String,
    /// The pi models.json provider this run's switchyard proxy materialized
    /// (`switchyard-<port>`; `switchyard` for crews without routes).
    route_provider: String,
}

impl Scheduler {
    pub fn new(crew: Crew, mut opts: Options) -> Self {
        if opts.tab_label.is_empty() {
            opts.tab_label = "forseti-crew".to_string();
        }
        if opts.node_timeout.is_zero() {
            opts.node_timeout = Duration::from_secs(10 * 60);
        }
        // --review implies keeping both: the human lands on the agents' diff
        // when the run ends (P7-3).
        if opts.review {
            opts.keep_tab = true;
            opts.keep_worktree = true;
        }
        let effective_cwd = opts.cwd.clone();
        let back_edges = crew.back_edges();
        let mut nodes = BTreeMap::new();
        for a in &crew.agents {
            nodes.insert(
                a.name.clone(),
                NodeState {
                    name: a.name.clone(),
                    status: Status::Pending,
                    phase_idx: crew.phase_of(&a.name),
                    ..Default::default()
                },
            );
        }
        let phases = crew
            .phases
            .iter()
            .map(|p| PhaseState {
                name: p.name.clone(),
                nodes: p.agents.clone(),
                ..Default::default()
            })
            .collect();
        Scheduler {
            crew,
            opts,
            nodes,
            phases,
            back_edges,
            checks_done: HashSet::new(),
            events: Vec::new(),
            run_id: run_id_now(),
            effective_cwd,
            tab_id: String::new(),
            route_provider: "switchyard".to_string(),
        }
    }

    // ---- accessors ----

    pub fn crew(&self) -> &Crew {
        &self.crew
    }

    pub fn opts(&self) -> &Options {
        &self.opts
    }

    /// Teardown may flip `keep_*` (a blocked run keeps the tab and worktree).
    pub fn opts_mut(&mut self) -> &mut Options {
        &mut self.opts
    }

    pub fn effective_cwd(&self) -> &str {
        &self.effective_cwd
    }

    /// Switches panes/checks/bus to a worktree checkout (the bridge path stays
    /// the main checkout).
    pub fn set_effective_cwd(&mut self, path: impl Into<String>) {
        self.effective_cwd = path.into();
    }

    pub fn run_id(&self) -> &str {
        &self.run_id
    }

    /// The crew's herdr tab id ("" until the orchestrator stages the run).
    pub fn tab_id(&self) -> &str {
        &self.tab_id
    }

    pub fn set_tab_id(&mut self, id: impl Into<String>) {
        self.tab_id = id.into();
    }

    /// The pi provider a route node resolves through (set by the proxy stage).
    pub fn route_provider(&self) -> &str {
        &self.route_provider
    }

    pub fn set_route_provider(&mut self, provider: impl Into<String>) {
        self.route_provider = provider.into();
    }

    pub fn node(&self, name: &str) -> Option<&NodeState> {
        self.nodes.get(name)
    }

    pub fn node_mut(&mut self, name: &str) -> Option<&mut NodeState> {
        self.nodes.get_mut(name)
    }

    pub fn phases(&self) -> &[PhaseState] {
        &self.phases
    }

    pub fn back_edges(&self) -> &HashSet<String> {
        &self.back_edges
    }

    // ---- events ----

    /// Records an event (timestamped) for the driver to drain.
    pub fn emit(&mut self, mut ev: Event) {
        ev.ts = now_rfc3339();
        self.events.push(ev);
    }

    pub fn events(&self) -> &[Event] {
        &self.events
    }

    /// Takes the pending events (the run log / TUI sink).
    pub fn drain_events(&mut self) -> Vec<Event> {
        std::mem::take(&mut self.events)
    }

    // ---- snapshots ----

    /// Value copies in crew order (race-free readers, P13 BUG-15).
    pub fn snapshot(&self) -> Vec<NodeState> {
        self.crew
            .agents
            .iter()
            .filter_map(|a| self.nodes.get(&a.name).cloned())
            .collect()
    }

    pub fn node_snapshot(&self, name: &str) -> Option<NodeState> {
        self.nodes.get(name).cloned()
    }

    // ---- edges ----

    /// Records a matched edge. A re-fire into a settled target re-arms it, so
    /// bounded cycles loop; past `max_visits` the fire is refused and a skip is
    /// collected.
    pub fn fire_edge(&mut self, es: &mut EdgeState, e: &Edge) {
        let key = EdgeState::key(e);
        let fired = es.visited.get(&key).copied().unwrap_or(0);
        if e.max_visits > 0 && fired as i64 >= e.max_visits {
            let mut ev = Event::on_node("edge_skip", &e.to);
            ev.info = format!("{key} max_visits reached");
            es.skips.push(ev);
            return;
        }
        *es.visited.entry(key.clone()).or_insert(0) += 1;
        es.ok.insert(key, true);
        if let Some(t) = self.nodes.get_mut(&e.to)
            && matches!(t.status, Status::Done | Status::Skipped)
        {
            t.status = Status::Pending;
            t.output.clear();
            t.retries = 0;
        }
    }

    /// Records a failed gate: the target can never be AND-satisfied via this
    /// edge again, so a still-pending target is skipped now. A later cycle
    /// re-fire of the same edge can re-arm it.
    pub fn miss_edge(&mut self, es: &mut EdgeState, e: &Edge, why: &str) {
        let mut ev = Event::on_node("edge_skip", &e.to);
        ev.info = format!("{} {}", EdgeState::key(e), why);
        es.skips.push(ev);
        if let Some(t) = self.nodes.get_mut(&e.to)
            && t.status == Status::Pending
        {
            t.status = Status::Skipped;
        }
    }

    /// Fan-in AND: every non-back incoming edge must have fired, the node must
    /// be pending, and its phase barrier must be open.
    pub fn node_ready(&self, es: &EdgeState, name: &str) -> bool {
        let Some(st) = self.nodes.get(name) else {
            return false;
        };
        if st.status != Status::Pending || !self.phase_unlocked(st.phase_idx) {
            return false;
        }
        for e in self.crew.in_edges(name) {
            if self.back_edges.contains(&EdgeState::key(&e)) {
                continue; // cycle re-entry edge: gates re-dispatch, not entry
            }
            if !es.fired(&e) {
                return false;
            }
        }
        true
    }

    // ---- phases ----

    /// A node is dispatchable when every earlier phase is fully terminal.
    /// Index < 0 = no phases → always open.
    pub fn phase_unlocked(&self, idx: i32) -> bool {
        if idx <= 0 {
            return true;
        }
        let upto = (idx as usize).min(self.phases.len());
        self.phases[..upto].iter().all(|p| p.done)
    }

    /// Fires `phase_start` for each phase whose first node dispatches with this
    /// wave and has not started yet.
    pub fn emit_phase_starts(&mut self, ready: &[&str]) {
        if self.phases.is_empty() {
            return;
        }
        let mut fired: Vec<(String, Vec<String>)> = Vec::new();
        for name in ready {
            let Some(st) = self.nodes.get(*name) else {
                continue;
            };
            let idx = st.phase_idx;
            if idx < 0 {
                continue;
            }
            let idx = idx as usize;
            if idx >= self.phases.len() || self.phases[idx].started {
                continue;
            }
            self.phases[idx].started = true;
            let p = &self.phases[idx];
            fired.push((p.name.clone(), p.nodes.clone()));
        }
        for (name, nodes) in fired {
            self.emit(
                Event::on_phase("phase_start", &name)
                    .with_info(format!("agents: {}", nodes.join(", "))),
            );
        }
    }

    /// Re-counts terminal members per phase after each wave; a phase whose
    /// members are ALL terminal fires `phase_done` and opens the next barrier.
    pub fn update_phase_progress(&mut self) {
        if self.phases.is_empty() {
            return;
        }
        let mut fired: Vec<(String, usize, usize)> = Vec::new();
        for j in 0..self.phases.len() {
            if self.phases[j].done {
                continue;
            }
            let terminal = self.phases[j]
                .nodes
                .iter()
                .filter(|n| {
                    self.nodes
                        .get(n.as_str())
                        .is_some_and(|st| st.status.releases_barrier())
                })
                .count();
            self.phases[j].terminal = terminal;
            let total = self.phases[j].nodes.len();
            if terminal == total {
                self.phases[j].done = true;
                fired.push((self.phases[j].name.clone(), terminal, total));
            }
        }
        for (name, terminal, total) in fired {
            self.emit(
                Event::on_phase("phase_done", &name)
                    .with_info(format!("{terminal}/{total} settled")),
            );
        }
    }

    /// The phase the run is in: the first not-yet-done phase (None when the
    /// crew declares no phases or every phase finished).
    pub fn current_phase(&self) -> Option<&str> {
        self.phases
            .iter()
            .find(|p| !p.done)
            .map(|p| p.name.as_str())
    }

    // ---- checks ----

    /// Runs every due declared check. `check(shell, dir)` is the executor —
    /// [`run_check`] in production, a stub in tests.
    ///
    /// A check is due once its after-node is settled AND this visit has not been
    /// proven yet (`check:<i>#<visits>`). A later PASS clears the `_failed`
    /// marker, so the exit code follows the latest iteration.
    pub fn run_due_checks<F>(&mut self, mut check: F)
    where
        F: FnMut(&str, &str) -> (bool, String),
    {
        let checks = self.crew.checks.clone();
        let dir = self.effective_cwd.clone();
        for (i, ch) in checks.iter().enumerate() {
            let (status, visits) = match self.nodes.get(&ch.after) {
                Some(st) => (st.status, st.visits),
                None => (Status::Pending, 0),
            };
            let key = format!("check:{i}#{visits}");
            if self.checks_done.contains(&key) {
                continue;
            }
            if !status.is_settled() {
                continue;
            }
            self.checks_done.insert(key);
            if status != Status::Done {
                let failed = status == Status::Failed;
                if failed {
                    self.checks_done.insert(format!("{}_failed", ch.name));
                }
                let kind = if failed { "check_fail" } else { "check_skip" };
                self.emit(Event::on_node(kind, &ch.name).with_info(format!(
                    "node {} settled {} before check ran",
                    ch.after,
                    status.as_str()
                )));
                continue;
            }
            let (pass, tail) = check(&ch.run, &dir);
            let mut ev = Event::on_node(if pass { "check_pass" } else { "check_fail" }, &ch.name);
            if pass {
                // clear a previous iteration's failure: latest evidence wins
                self.checks_done.remove(&format!("{}_failed", ch.name));
            } else {
                self.checks_done.insert(format!("{}_failed", ch.name));
            }
            ev.info = tail.trim().to_string();
            self.emit(ev);
        }
    }

    /// Whether any check is currently failed (the process exit-code input).
    pub fn checks_failed(&self) -> bool {
        self.crew
            .checks
            .iter()
            .any(|ch| self.checks_done.contains(&format!("{}_failed", ch.name)))
    }

    pub fn checks_failed_count(&self) -> usize {
        self.crew
            .checks
            .iter()
            .filter(|ch| self.checks_done.contains(&format!("{}_failed", ch.name)))
            .count()
    }

    // ---- helpers for the driver ----

    /// Applies a settled node's outgoing edges to the edge state — the wave
    /// loop's inner pass, decision and bookkeeping together so the driver
    /// cannot drift on the skip text. Laya edges are the runner's (grouped
    /// endpoint call), not this pass's.
    pub fn settle_edges(&mut self, es: &mut EdgeState, name: &str, output: &str) {
        for e in self.crew.out_edges(name) {
            if e.is_laya() {
                continue;
            }
            if edge_satisfied(&e, output) {
                self.fire_edge(es, &e);
            } else {
                let why = format!("when {:?} not matched on {} output", e.when, e.from);
                self.miss_edge(es, &e, &why);
            }
        }
    }

    /// The names of the nodes that are dispatchable right now, in crew order.
    pub fn ready(&self, es: &EdgeState) -> Vec<String> {
        self.crew
            .agents
            .iter()
            .filter(|a| self.node_ready(es, &a.name))
            .map(|a| a.name.clone())
            .collect()
    }

    /// Sweeps any node still pending into `skipped` (unreachable) and reports
    /// whether the run ended blocked.
    pub fn settle_unreachable(&mut self) -> bool {
        for st in self.nodes.values_mut() {
            if st.status == Status::Pending {
                st.status = Status::Skipped;
            }
        }
        let blocked = self.nodes.values().any(|st| st.status == Status::Blocked);
        if blocked {
            // P13 BUG-09: a blocked run is the human-in-the-loop outcome — keep
            // the tab and worktree so the summoned human still has them.
            self.opts.keep_tab = true;
            self.opts.keep_worktree = true;
        }
        blocked
    }
}

/// Whether an edge's gate is satisfied by the producer's output.
///
/// `verdict:<label>` gates on the LAST verdict line (`bus::parse_verdict`), a
/// `re:` edge on its regex, anything else on `idle` (the producer completed).
pub fn edge_satisfied(e: &Edge, output: &str) -> bool {
    if let Some(label) = e.when.strip_prefix("verdict:") {
        return matches!(bus::parse_verdict(output), Some(got) if got == label);
    }
    if let Some(pat) = e.when.strip_prefix("re:") {
        return cached_regex(pat).is_some_and(|re| re.is_match(output));
    }
    e.when == "idle"
}

/// Regex cache: the same pattern is re-evaluated for every settle of its
/// source, and validation has already proven it compiles (Go `compileCached`).
fn cached_regex(pat: &str) -> Option<regex::Regex> {
    use std::sync::{LazyLock, Mutex};
    static CACHE: LazyLock<Mutex<HashMap<String, regex::Regex>>> =
        LazyLock::new(|| Mutex::new(HashMap::new()));
    let mut cache = CACHE.lock().unwrap_or_else(|e| e.into_inner());
    if let Some(re) = cache.get(pat) {
        return Some(re.clone());
    }
    let re = regex::Regex::new(pat).ok()?;
    cache.insert(pat.to_string(), re.clone());
    Some(re)
}

/// Runs a check shell command in `dir` (5-minute budget).
pub fn run_check(shell: &str, dir: &str) -> (bool, String) {
    run_check_with_timeout(shell, dir, CHECK_TIMEOUT)
}

fn run_check_with_timeout(shell: &str, dir: &str, timeout: Duration) -> (bool, String) {
    use std::os::unix::process::CommandExt;

    let mut cmd = Command::new("sh");
    cmd.arg("-c")
        .arg(shell)
        .current_dir(dir)
        .stdin(Stdio::null())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped());
    // Own process group: the timeout kill must sweep grandchildren too, or they
    // hold the pipes and the wait outlives the budget (P13 BUG-08).
    cmd.process_group(0);

    let child = match cmd.spawn() {
        Ok(c) => c,
        Err(e) => return (false, e.to_string()),
    };
    let pid = child.id() as libc::pid_t;
    // Watchdog: kill the group at the deadline unless the child finishes first.
    let done = Arc::new((Mutex::new(false), Condvar::new()));
    let timed_out = Arc::new(AtomicBool::new(false));
    let watchdog = {
        let done = Arc::clone(&done);
        let timed_out = Arc::clone(&timed_out);
        std::thread::spawn(move || {
            let (lock, cv) = &*done;
            let guard = lock.lock().unwrap_or_else(|e| e.into_inner());
            let (guard, _) = cv
                .wait_timeout_while(guard, timeout, |finished| !*finished)
                .unwrap_or_else(|e| e.into_inner());
            if *guard {
                return; // the child finished inside the budget
            }
            drop(guard);
            timed_out.store(true, Ordering::SeqCst);
            // SIGKILL the whole group (negative pid), like the Go pgid sweep.
            unsafe {
                libc::kill(-pid, libc::SIGKILL);
            }
        })
    };

    let output = child.wait_with_output();

    {
        let (lock, cv) = &*done;
        *lock.lock().unwrap_or_else(|e| e.into_inner()) = true;
        cv.notify_all();
    }
    let _ = watchdog.join();

    let out = match output {
        Ok(o) => o,
        Err(e) => return (false, e.to_string()),
    };
    // CombinedOutput order (stdout then stderr here: only the tail is used).
    let mut bytes = out.stdout;
    bytes.extend_from_slice(&out.stderr);
    if bytes.len() > 240 {
        bytes = [b"\xe2\x80\xa6".as_slice(), &bytes[bytes.len() - 240..]].concat();
    }
    let tail = String::from_utf8_lossy(&bytes).into_owned();
    // Go's WaitDelay treats "the budget expired before the pipes closed" as a
    // failure even when the direct child exited 0 (its grandchild held them).
    if timed_out.load(Ordering::SeqCst) {
        return (
            false,
            format!(
                "check timed out after {}s | {tail}",
                timeout.as_secs_f64().round() as u64
            ),
        );
    }
    if out.status.success() {
        return (true, tail);
    }
    let why = match out.status.code() {
        Some(code) => format!("exit status {code}"),
        None => {
            use std::os::unix::process::ExitStatusExt;
            match out.status.signal() {
                Some(sig) => format!("signal: {sig}"),
                None => "exit status unknown".to_string(),
            }
        }
    };
    (false, format!("{why} | {tail}"))
}

// MARK: timestamps

/// RFC3339 UTC with millisecond precision (the status bridge's `ts`).
pub fn rfc3339_now() -> String {
    now_rfc3339()
}

/// RFC3339 UTC with millisecond precision — the JSON shape of Go's
/// `time.Time` (which the run log and status file carry).
fn now_rfc3339() -> String {
    let (y, mo, d, h, mi, s, ms) = utc_parts();
    format!("{y:04}-{mo:02}-{d:02}T{h:02}:{mi:02}:{s:02}.{ms:03}Z")
}

/// The Go run id: `20060102-150405` in UTC (memory-write provenance).
fn run_id_now() -> String {
    let (y, mo, d, h, mi, s, _) = utc_parts();
    format!("{y:04}{mo:02}{d:02}-{h:02}{mi:02}{s:02}")
}

fn utc_parts() -> (i64, u32, u32, u32, u32, u32, u32) {
    let now = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .unwrap_or_default();
    let secs = now.as_secs() as i64;
    let millis = now.subsec_millis();
    let days = secs.div_euclid(86_400);
    let sod = secs.rem_euclid(86_400);
    let (y, mo, d) = civil_from_days(days);
    (
        y,
        mo,
        d,
        (sod / 3600) as u32,
        ((sod % 3600) / 60) as u32,
        (sod % 60) as u32,
        millis,
    )
}

/// Howard Hinnant's civil-from-days (days since 1970-01-01 → y/m/d).
pub(crate) fn civil_from_days(z: i64) -> (i64, u32, u32) {
    let z = z + 719_468;
    let era = if z >= 0 { z } else { z - 146_096 } / 146_097;
    let doe = (z - era * 146_097) as u64; // [0, 146096]
    let yoe = (doe - doe / 1460 + doe / 36_524 - doe / 146_096) / 365; // [0, 399]
    let y = yoe as i64 + era * 400;
    let doy = doe - (365 * yoe + yoe / 4 - yoe / 100); // [0, 365]
    let mp = (5 * doy + 2) / 153; // [0, 11]
    let d = (doy - (153 * mp + 2) / 5 + 1) as u32; // [1, 31]
    let m = if mp < 10 { mp + 3 } else { mp - 9 } as u32; // [1, 12]
    (if m <= 2 { y + 1 } else { y }, m, d)
}

// MARK: tests

#[cfg(test)]
mod tests {
    use super::*;
    use crate::crew::load;

    static SEQ: std::sync::atomic::AtomicUsize = std::sync::atomic::AtomicUsize::new(0);

    fn scheduler(yaml: &str) -> Scheduler {
        let crew = load(yaml).expect("test crew validates");
        Scheduler::new(
            crew,
            Options {
                cwd: temp_dir(),
                ..Default::default()
            },
        )
    }

    fn temp_dir() -> String {
        let n = SEQ.fetch_add(1, Ordering::SeqCst);
        let dir = std::env::temp_dir().join(format!("forseti-sched-{}-{n}", std::process::id()));
        let _ = std::fs::remove_dir_all(&dir);
        std::fs::create_dir_all(&dir).expect("temp dir");
        dir.to_string_lossy().into_owned()
    }

    fn kinds(evs: &[Event], kind: &str) -> usize {
        evs.iter().filter(|e| e.kind == kind).count()
    }

    /// Drives the scheduler the way the Go wave loop does (`Run.Run`): dispatch
    /// every ready node, settle it via `settle`, run due checks, apply the
    /// fired/missed edges, then re-count phases. Test-only: the real runner
    /// (18D-5) differs in parallelism, laya gates and retries.
    fn drive(
        s: &mut Scheduler,
        mut settle: impl FnMut(&str) -> (Status, String),
        max_waves: usize,
    ) -> (Vec<String>, Vec<Event>) {
        let mut es = EdgeState::new();
        let mut dispatched = Vec::new();
        let mut events = Vec::new();
        for _ in 0..max_waves {
            let ready = s.ready(&es);
            if ready.is_empty() {
                break;
            }
            let names: Vec<&str> = ready.iter().map(String::as_str).collect();
            s.emit_phase_starts(&names);
            for name in &ready {
                let (status, out) = settle(name);
                let st = s.node_mut(name).expect("ready node exists");
                st.status = status;
                st.output = out;
                st.visits += 1;
                dispatched.push(name.clone());
            }
            s.run_due_checks(|_, _| (true, String::new()));
            for name in &ready {
                if s.node(name).map(|st| st.status) == Some(Status::Done) {
                    let out = s.node(name).unwrap().output.clone();
                    s.settle_edges(&mut es, name, &out);
                }
            }
            events.append(&mut s.drain_events());
            events.append(&mut es.drain());
            s.update_phase_progress();
        }
        s.settle_unreachable();
        events.append(&mut s.drain_events());
        (dispatched, events)
    }

    #[test]
    fn a_reviewer_loop_drives_to_lgtm_within_max_visits() {
        // End-to-end: fan-in AND + re-arm + the uncapped body edge + the
        // verdict gate + the max_visits cap, all in one run that terminates.
        let mut s = scheduler(
            r#"
name: loop-drive
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
        let mut reviews = 0;
        let (dispatched, events) = drive(
            &mut s,
            |name| match name {
                "entry" => (Status::Done, "go".to_string()),
                "subject" => (Status::Done, "draft".to_string()),
                _ => {
                    reviews += 1;
                    let verdict = if reviews < 3 { "LBTM" } else { "LGTM" };
                    (Status::Done, format!("body\n\nVERDICT: {verdict}"))
                }
            },
            20,
        );
        assert_eq!(
            dispatched,
            [
                "entry", "subject", "reviewer", "subject", "reviewer", "subject", "reviewer"
            ]
        );
        assert_eq!(s.node("subject").unwrap().visits, 3);
        assert_eq!(s.node("reviewer").unwrap().visits, 3);
        // the final LGTM misses the LBTM edge (the subject is already done);
        // the cap is never exceeded because the loop converges first
        assert_eq!(kinds(&events, "edge_skip"), 1);
        assert!(!s.checks_failed());
    }

    #[test]
    fn a_phase_barrier_delays_the_next_wave() {
        // Without the barrier both entries dispatch in wave 1; the phase layer
        // holds b1 until p1 is fully terminal.
        let mut s = scheduler(
            r#"
name: wave-barrier
agents:
  - {name: a1, prompt: x}
  - {name: b1, prompt: y}
phases:
  - {name: p1, instructions: first, agents: [a1]}
  - {name: p2, instructions: second, agents: [b1]}
"#,
        );
        let (dispatched, events) = drive(&mut s, |name| (Status::Done, name.to_string()), 10);
        assert_eq!(dispatched, ["a1", "b1"], "phase 2 must wait for phase 1");
        let phases: Vec<(String, String)> = events
            .iter()
            .filter(|e| e.kind == "phase_start" || e.kind == "phase_done")
            .map(|e| (e.kind.clone(), e.phase.clone()))
            .collect();
        assert_eq!(
            phases,
            [
                ("phase_start".to_string(), "p1".to_string()),
                ("phase_done".to_string(), "p1".to_string()),
                ("phase_start".to_string(), "p2".to_string()),
                ("phase_done".to_string(), "p2".to_string()),
            ]
        );
    }

    #[test]
    fn edge_satisfied_forms() {
        let idle = Edge {
            when: "idle".into(),
            ..Default::default()
        };
        assert!(edge_satisfied(&idle, "anything"));
        let re = Edge {
            when: "re:NEVER_XYZ".into(),
            ..Default::default()
        };
        assert!(!edge_satisfied(&re, "output without it"));
        let re = Edge {
            when: "re:READY".into(),
            ..Default::default()
        };
        assert!(edge_satisfied(&re, "PLAN_READY here"));
    }

    #[test]
    fn edge_satisfied_verdict() {
        // P19B: a verdict edge fires only on its own label; the label is the
        // LAST verdict line (the pane echoes the prompt first).
        let out = "End with `VERDICT: LGTM` or `VERDICT: LBTM`.\n\nreview body\n\nVERDICT: LBTM";
        let lbtm = Edge {
            when: "verdict:lbtm".into(),
            ..Default::default()
        };
        let lgtm = Edge {
            when: "verdict:lgtm".into(),
            ..Default::default()
        };
        assert!(edge_satisfied(&lbtm, out));
        assert!(!edge_satisfied(&lgtm, out));
        assert!(!edge_satisfied(&lbtm, "no verdict here"));
        assert!(edge_satisfied(&lgtm, "all good\nLGTM"));
    }

    #[test]
    fn phase_unlocked_barrier() {
        let mut s = scheduler(
            r#"
name: barrier-test
agents:
  - {name: a1, prompt: x}
  - {name: a2, prompt: y}
  - {name: b1, prompt: z}
phases:
  - {name: p1, instructions: first, agents: [a1, a2]}
  - {name: p2, instructions: second, agents: [b1]}
"#,
        );
        assert!(s.phase_unlocked(0) && s.phase_unlocked(-1));
        // phase 1 locked while p1 has a running node
        s.node_mut("a1").unwrap().status = Status::Done;
        s.node_mut("a2").unwrap().status = Status::Running;
        assert!(!s.phase_unlocked(1));
        // a skipped node is terminal — releases the barrier (progress is
        // re-counted after each wave, matching update-then-scan)
        s.node_mut("a2").unwrap().status = Status::Skipped;
        s.update_phase_progress();
        assert!(s.phase_unlocked(1));

        // blocked holds the barrier
        let mut s2 = scheduler(
            r#"
name: barrier-test-2
agents:
  - {name: a1, prompt: x}
  - {name: a2, prompt: y}
  - {name: b1, prompt: z}
phases:
  - {name: p1, instructions: first, agents: [a1, a2]}
  - {name: p2, instructions: second, agents: [b1]}
"#,
        );
        s2.node_mut("a1").unwrap().status = Status::Done;
        s2.node_mut("a2").unwrap().status = Status::Blocked;
        s2.update_phase_progress();
        assert!(!s2.phase_unlocked(1), "blocked must hold the barrier");
        assert_eq!(s2.current_phase(), Some("p1"));
    }

    #[test]
    fn phase_progress_events() {
        let mut s = scheduler(
            r#"
name: progress-test
agents:
  - {name: a1, prompt: x}
  - {name: b1, prompt: y}
phases:
  - {name: p1, instructions: first, agents: [a1]}
  - {name: p2, instructions: second, agents: [b1]}
"#,
        );
        s.emit_phase_starts(&["a1"]);
        assert!(s.phases()[0].started, "phase_start not marked");
        assert_eq!(kinds(s.events(), "phase_start"), 1);

        s.node_mut("a1").unwrap().status = Status::Done;
        s.update_phase_progress();
        assert!(s.phases()[0].done && !s.phases()[1].done);
        assert_eq!(s.current_phase(), Some("p2"));
        assert_eq!(kinds(s.events(), "phase_done"), 1);

        // all terminal → no current phase
        s.node_mut("b1").unwrap().status = Status::Skipped;
        s.update_phase_progress();
        assert_eq!(s.current_phase(), None);
        assert_eq!(kinds(s.events(), "phase_done"), 2);

        // a phase_start for an already-started phase is not repeated
        s.emit_phase_starts(&["a1"]);
        assert_eq!(kinds(s.events(), "phase_start"), 1);
    }

    #[test]
    fn fire_edge_rearms_settled_target() {
        // P13 BUG-02: a re-firing edge into a settled node re-arms it — bounded
        // cycles (max_visits) actually loop; past the limit a skip is collected
        // and nothing re-arms.
        let mut s = scheduler(
            r#"
name: cycle-test
agents:
  - {name: c, prompt: entry}
  - {name: a, prompt: x}
  - {name: b, prompt: y}
edges:
  - {from: c, to: a}
  - {from: a, to: b}
  - {from: b, to: a, max_visits: 2}
"#,
        );
        let edges = s.crew().edges.clone();
        let mut es = EdgeState::new();
        s.fire_edge(&mut es, &edges[0]); // c→a
        assert!(s.node_ready(&es, "a"), "a ready after its only gate fires");
        s.node_mut("a").unwrap().status = Status::Done;
        s.fire_edge(&mut es, &edges[1]); // a→b
        assert!(s.node_ready(&es, "b"), "b ready after a→b fires");
        s.node_mut("b").unwrap().status = Status::Done;

        s.fire_edge(&mut es, &edges[2]); // b→a, 1st use
        assert_eq!(s.node("a").unwrap().status, Status::Pending);
        assert!(s.node_ready(&es, "a"), "re-armed a must be ready");
        assert!(es.drain().is_empty(), "unexpected skips");

        // 2nd use is within max_visits: no skip, and no double re-arm
        s.node_mut("b").unwrap().status = Status::Done;
        s.fire_edge(&mut es, &edges[2]);
        assert!(es.drain().is_empty(), "2nd use within budget must not skip");
        assert_eq!(s.node("a").unwrap().status, Status::Pending);

        // 3rd use exceeds the cap → one collected skip, no re-arm
        s.fire_edge(&mut es, &edges[2]);
        let skips = es.drain();
        assert_eq!(skips.len(), 1);
        assert_eq!(skips[0].kind, "edge_skip");
        assert_eq!(s.node("a").unwrap().status, Status::Pending);
    }

    #[test]
    fn uncapped_edge_refires_loop_body() {
        // P19: only the loop-back edge carries max_visits; the loop BODY's
        // forward edge (subject→reviewer) is uncapped and must re-fire when the
        // subject is re-armed, otherwise the reviewer never re-runs.
        let mut s = scheduler(
            r#"
name: loop-refire
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
        let edges = s.crew().edges.clone();
        let mut es = EdgeState::new();
        s.fire_edge(&mut es, &edges[0]); // entry→subject
        s.node_mut("subject").unwrap().status = Status::Done;
        s.fire_edge(&mut es, &edges[1]); // subject→reviewer, 1st pass
        s.node_mut("reviewer").unwrap().status = Status::Done;
        s.fire_edge(&mut es, &edges[2]); // LBTM re-arms the subject
        assert_eq!(s.node("subject").unwrap().status, Status::Pending);

        // 2nd pass: the uncapped forward edge re-fires and re-dispatches
        s.node_mut("subject").unwrap().status = Status::Done;
        s.fire_edge(&mut es, &edges[1]);
        assert!(
            es.drain().is_empty(),
            "uncapped forward edge must not be capped"
        );
        assert_eq!(
            s.node("reviewer").unwrap().status,
            Status::Pending,
            "forward edge must re-dispatch the reviewer"
        );
    }

    #[test]
    fn capped_edge_still_stops() {
        let mut s = scheduler(
            r#"
name: capped
agents:
  - {name: a, prompt: x}
  - {name: b, prompt: y}
edges:
  - {from: a, to: b, max_visits: 2}
  - {from: b, to: a, max_visits: 2, loop: true}
"#,
        );
        let edges = s.crew().edges.clone();
        let mut es = EdgeState::new();
        s.fire_edge(&mut es, &edges[0]); // 1st
        s.fire_edge(&mut es, &edges[0]); // 2nd, within cap
        assert!(es.drain().is_empty(), "within cap must not skip");
        s.fire_edge(&mut es, &edges[0]); // 3rd exceeds the cap
        let skips = es.drain();
        assert_eq!(skips.len(), 1);
        assert_eq!(skips[0].kind, "edge_skip");
    }

    #[test]
    fn node_ready_fan_in_and() {
        // P13 BUG-05: fan-in AND — the target waits for ALL incoming edges; one
        // missed edge permanently skips it.
        let mut s = scheduler(
            r#"
name: and-test
agents:
  - {name: x, prompt: x}
  - {name: y, prompt: y}
  - {name: t, prompt: z}
edges:
  - {from: x, to: t}
  - {from: y, to: t}
"#,
        );
        let edges = s.crew().edges.clone();
        let mut es = EdgeState::new();
        s.fire_edge(&mut es, &edges[0]); // x→t only
        assert!(!s.node_ready(&es, "t"), "fan-in AND: t waits for y→t");
        s.fire_edge(&mut es, &edges[1]);
        assert!(s.node_ready(&es, "t"));

        // a missed edge skips the pending target and collects the event
        let mut s2 = scheduler(
            r#"
name: miss-test
agents:
  - {name: e, prompt: e}
  - {name: t, prompt: t}
edges:
  - {from: e, to: t}
"#,
        );
        let edges2 = s2.crew().edges.clone();
        let mut es2 = EdgeState::new();
        s2.miss_edge(&mut es2, &edges2[0], "when not matched");
        assert_eq!(s2.node("t").unwrap().status, Status::Skipped);
        let skips = es2.drain();
        assert_eq!(skips.len(), 1);
        assert_eq!(skips[0].kind, "edge_skip");
        assert_eq!(skips[0].node, "t");
    }

    #[test]
    fn node_ready_cycle_body_waits_for_its_gate() {
        // P19: in x→a→b→a the body edge a→b is a readiness gate — b must wait
        // for a (the old reachability rule marked a→b a back edge and let b run
        // at t0).
        let mut s = scheduler(
            r#"
name: cyc-ready
agents:
  - {name: x, prompt: x}
  - {name: a, prompt: a}
  - {name: b, prompt: b}
edges:
  - {from: x, to: a}
  - {from: a, to: b}
  - {from: b, to: a, max_visits: 2}
"#,
        );
        let edges = s.crew().edges.clone();
        let mut es = EdgeState::new();
        assert!(!s.node_ready(&es, "b"), "b before a→b fires");
        s.node_mut("a").unwrap().status = Status::Done;
        s.fire_edge(&mut es, &edges[1]); // a→b
        assert!(s.node_ready(&es, "b"), "b once a→b fires");
        // the loop-back does not re-gate a: a is satisfied via x→a
        s.fire_edge(&mut es, &edges[0]); // x→a
        assert!(s.node_ready(&es, "a"), "a from its gate edge x→a");
    }

    #[test]
    fn checks_rerun_when_after_node_is_revisited() {
        // P19 follow-up: checks are keyed by visit, so an executor fix round
        // re-proves the artifact and a later pass clears the failure marker.
        let dir = temp_dir();
        let crew = load(
            r#"
name: check-revisit
agents:
  - {name: impl, prompt: write the artifact}
checks:
  - {name: artifact, after: impl, run: "test -f ARTIFACT.md"}
"#,
        )
        .expect("crew validates");
        let mut s = Scheduler::new(
            crew,
            Options {
                cwd: dir.clone(),
                ..Default::default()
            },
        );

        // iteration 1: settled but the artifact is missing → fail
        s.node_mut("impl").unwrap().status = Status::Done;
        s.node_mut("impl").unwrap().visits = 1;
        s.run_due_checks(run_check);
        assert!(s.checks_failed(), "visit 1: missing artifact must fail");

        // a re-check at the SAME visit is a no-op (the flag is per visit)
        s.run_due_checks(run_check);
        assert_eq!(kinds(s.events(), "check_fail"), 1, "same visit re-checked");

        // the fix round creates the artifact; the node settles again (visit 2)
        std::fs::write(std::path::Path::new(&dir).join("ARTIFACT.md"), "ok").unwrap();
        s.node_mut("impl").unwrap().visits = 2;
        s.node_mut("impl").unwrap().status = Status::Done;
        s.run_due_checks(run_check);
        assert!(
            !s.checks_failed(),
            "visit 2 pass must clear the failure marker"
        );
        assert_eq!(kinds(s.events(), "check_pass"), 1);
    }

    #[test]
    fn a_blocked_after_node_is_an_advisory_check_skip() {
        // P13 BUG-25: blocked/skipped after-nodes are designed gate paths, not
        // failures — only a FAILED after-node reddens the exit code.
        let mut s = scheduler(
            r#"
name: check-blocked
agents:
  - {name: impl, prompt: x}
checks:
  - {name: artifact, after: impl, run: "true"}
"#,
        );
        s.node_mut("impl").unwrap().status = Status::Blocked;
        s.run_due_checks(|_, _| panic!("a blocked after-node must not run its check"));
        assert!(!s.checks_failed());
        assert_eq!(kinds(s.events(), "check_skip"), 1);

        // a failed after-node IS a failure, and its check does not run
        let mut s = scheduler(
            r#"
name: check-failed
agents:
  - {name: impl, prompt: x}
checks:
  - {name: artifact, after: impl, run: "true"}
"#,
        );
        s.node_mut("impl").unwrap().status = Status::Failed;
        s.run_due_checks(|_, _| panic!("a failed after-node must not run its check"));
        assert!(s.checks_failed());
        assert_eq!(kinds(s.events(), "check_fail"), 1);
        assert_eq!(s.checks_failed_count(), 1);
    }

    #[test]
    fn run_check_reports_the_exit_code_and_tail() {
        assert!(run_check("true", ".").0);
        let (pass, tail) = run_check("echo oops >&2; exit 3", ".");
        assert!(!pass);
        assert!(tail.contains("exit status 3"), "{tail}");
        assert!(tail.contains("oops"), "{tail}");
    }

    #[test]
    fn run_check_kills_grandchildren_on_timeout() {
        use std::time::Instant;
        let start = Instant::now();
        // The shell child exits at once; its background grandchild holds the
        // inherited pipes. Without the group kill this waits out the budget.
        let (pass, tail) =
            run_check_with_timeout("sleep 30 & exit 0", ".", Duration::from_millis(300));
        assert!(!pass, "should be reported as a timeout: {tail}");
        assert!(tail.contains("timed out"), "{tail}");
        assert!(
            start.elapsed() < Duration::from_secs(10),
            "the grandchild must not outlive the budget: {:?}",
            start.elapsed()
        );
    }

    #[test]
    fn timestamps_are_rfc3339_utc() {
        let ts = now_rfc3339();
        // 2026-10-18T12:34:56.789Z
        assert_eq!(ts.len(), 24, "{ts}");
        assert!(ts.ends_with('Z'), "{ts}");
        assert_eq!(&ts[4..5], "-");
        assert_eq!(&ts[10..11], "T");
        let id = run_id_now();
        assert_eq!(id.len(), 15, "{id}");
        assert!(id.chars().all(|c| c.is_ascii_digit() || c == '-'));
    }

    #[test]
    fn civil_from_days_matches_known_dates() {
        assert_eq!(civil_from_days(0), (1970, 1, 1));
        assert_eq!(civil_from_days(20_000), (2024, 10, 4));
        assert_eq!(civil_from_days(19_000), (2022, 1, 8));
        assert_eq!(civil_from_days(-1), (1969, 12, 31));
        assert_eq!(civil_from_days(11_016), (2000, 2, 29));
    }

    #[test]
    fn settle_edges_fires_matching_and_misses_the_rest() {
        // Pins the skip text the run log carries (`when "re:X" not matched on
        // a output`) and the laya exemption.
        let mut s = scheduler(
            r#"
name: settle
agents:
  - {name: a, prompt: x}
  - {name: yes, prompt: y}
  - {name: no, prompt: z}
edges:
  - {from: a, to: yes, when: "re:READY"}
  - {from: a, to: no, when: "re:NEVER"}
"#,
        );
        let mut es = EdgeState::new();
        s.node_mut("a").unwrap().status = Status::Done;
        s.settle_edges(&mut es, "a", "PLAN_READY");
        assert!(es.fired(&s.crew().edges[0]), "READY must fire yes");
        assert_eq!(s.node("no").unwrap().status, Status::Skipped);
        let skips = es.drain();
        assert_eq!(skips.len(), 1);
        assert_eq!(skips[0].node, "no");
        assert_eq!(
            skips[0].info,
            "a>no when \"re:NEVER\" not matched on a output"
        );
    }

    #[test]
    fn settle_unreachable_sweeps_pending_and_keeps_a_blocked_run() {
        let mut s = scheduler(
            r#"
name: sweep
agents:
  - {name: a, prompt: x}
  - {name: b, prompt: y}
edges:
  - {from: a, to: b}
"#,
        );
        s.node_mut("a").unwrap().status = Status::Blocked;
        assert!(s.settle_unreachable());
        assert_eq!(s.node("b").unwrap().status, Status::Skipped);
        assert!(s.opts().keep_tab && s.opts().keep_worktree);
    }
}
