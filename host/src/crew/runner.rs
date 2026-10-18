//! The crew runner core (Phase 18D-5): pane-side execution behind a
//! [`PaneBackend`] trait, so the same node path runs on herdr today and inside
//! ttt later (S38) without a second runner.
//!
//! This module owns the parts of `crew/internal/runner/runner.go` that are not
//! orchestration:
//!
//! * [`PaneBackend`] — `read` / `prompt` / `prompt_wait` / `focus` / `status`.
//!   [`HerdrBackend`] adapts the 18D-3 socket client; a `FakeBackend` (tests)
//!   and a future `TttBackend` are the same shape.
//! * [`NodeExecutor`] — the Go `runNode` path: phase instructions + role prefix,
//!   template render (18D-5's [`super::template`]), auto-recall, prompt+settle in
//!   ONE call, the `blocked` outcome, the reasoning-model empty-settle retry,
//!   cost/context capture from pi's status line, the bus + typed-artifact
//!   writes, `node_start`/`node_done`/`node_failed`/`node_retry`/`node_blocked`/
//!   `verdict` events, and auto-write.
//! * The pure helpers the Go suite pins: `parsePiStatus`, `paneDelta`,
//!   `emptySettleProne`, `truncateUTF8`.
//!
//! Not here (next stages): the herdr tab/pane orchestration, status
//! subscription, run log, status bridge, teardown, switchyard/worktree/memory
//! clients. They compose on top.

use std::collections::HashMap;
use std::path::Path;
use std::sync::LazyLock;
use std::time::{Duration, SystemTime};

use regex::Regex;

use super::herdr;
use super::model::Agent;
use super::role::RolePack;
use super::scheduler::{Event, Scheduler, Status};
use super::template;

// MARK: backend

/// The pane operations a crew run needs. One impl per host (herdr now, ttt
/// later); the scheduler never sees which one it is.
pub trait PaneBackend {
    /// The pane text (`recent_unwrapped`, last `lines`) for an agent.
    fn read(&mut self, target: &str, source: &str, lines: i64) -> Result<String, String>;

    /// Sends text without waiting (prompt injection outside the settle path).
    fn prompt(&mut self, target: &str, text: &str) -> Result<(), String>;

    /// Sends the prompt AND waits for settle in one request. Returns the settled
    /// status (`idle` / `done` / `blocked`).
    fn prompt_wait(
        &mut self,
        target: &str,
        text: &str,
        until: &[String],
        timeout: Duration,
    ) -> Result<String, String>;

    /// Jumps the host UI to the agent's pane.
    fn focus(&mut self, target: &str) -> Result<(), String>;

    /// The agent's live status (`idle|working|blocked|done|unknown`).
    fn status(&mut self, target: &str) -> Result<String, String>;
}

/// The herdr implementation (18D-3).
pub struct HerdrBackend {
    client: herdr::Client,
}

impl HerdrBackend {
    pub fn new(client: herdr::Client) -> Self {
        HerdrBackend { client }
    }

    pub fn client(&self) -> &herdr::Client {
        &self.client
    }
}

impl PaneBackend for HerdrBackend {
    fn read(&mut self, target: &str, source: &str, lines: i64) -> Result<String, String> {
        self.client
            .agent_read(target, source, lines)
            .map_err(|e| e.to_string())
    }

    fn prompt(&mut self, target: &str, text: &str) -> Result<(), String> {
        self.client
            .agent_prompt(target, text)
            .map_err(|e| e.to_string())
    }

    fn prompt_wait(
        &mut self,
        target: &str,
        text: &str,
        until: &[String],
        timeout: Duration,
    ) -> Result<String, String> {
        self.client
            .agent_prompt_wait(target, text, until, timeout)
            .map_err(|e| e.to_string())
    }

    fn focus(&mut self, target: &str) -> Result<(), String> {
        self.client.agent_focus(target).map_err(|e| e.to_string())
    }

    fn status(&mut self, target: &str) -> Result<String, String> {
        let agents = self.client.agent_list().map_err(|e| e.to_string())?;
        Ok(agents
            .into_iter()
            .find(|a| a.name == target)
            .map(|a| a.agent_status)
            .unwrap_or_default())
    }
}

// MARK: memory link

/// One recalled memory row (the service's id/ts/agent/text/score shape).
#[derive(Debug, Clone, Default)]
pub struct MemoryRow {
    pub ts: String,
    pub text: String,
}

/// The memory service surface the runner uses (implemented by the HTTP client in
/// the next stage; a null link keeps every call advisory).
pub trait MemoryLink {
    fn recall(
        &mut self,
        query: &str,
        node: &str,
        k: i64,
        namespace: &str,
    ) -> Result<Vec<MemoryRow>, String>;

    fn write(
        &mut self,
        text: &str,
        namespace: &str,
        run_id: &str,
        source: &str,
        tags: &[String],
    ) -> Result<i64, String>;
}

// MARK: pure helpers (Go runner_test.go ports)

static PI_COST: LazyLock<Regex> =
    LazyLock::new(|| Regex::new(r"\$([0-9]+(?:\.[0-9]+)?)").expect("cost regex"));
static PI_CTX: LazyLock<Regex> =
    LazyLock::new(|| Regex::new(r"([0-9]+(?:\.[0-9]+)?)%/").expect("ctx regex"));

/// Cost (USD) + context % from pi's status footer — the LAST match wins (the
/// pane echoes earlier prompts, which may contain a `$`).
pub fn parse_pi_status(text: &str) -> (f64, f64) {
    let cost = PI_COST
        .captures_iter(text)
        .last()
        .and_then(|c| c[1].parse::<f64>().ok())
        .unwrap_or(0.0);
    let ctx = PI_CTX
        .captures_iter(text)
        .last()
        .and_then(|c| c[1].parse::<f64>().ok())
        .unwrap_or(0.0);
    (cost, ctx)
}

/// How much the pane grew between reads: a real answer adds the prompt echo plus
/// its own text; an empty-content settle grows by the echo alone (6A-3).
pub fn pane_delta(baseline: &str, after: &str) -> i64 {
    after.trim().len() as i64 - baseline.trim().len() as i64
}

/// Whether a resolved provider/model can settle with EMPTY content because the
/// whole budget went to reasoning. Only these retry — a false positive pays for
/// every short answer twice (P13 BUG-23). Covers the LAN box (halogen→valhalla)
/// and OpenCode Go's free reasoning ids.
pub fn empty_settle_prone(resolved: &str) -> bool {
    ["halogen", "valhalla", "beam-free", "longcat"]
        .iter()
        .any(|m| resolved.contains(m))
}

/// Cuts `s` to at most `n` BYTES without splitting a rune (Go `truncateUTF8`).
pub fn truncate_utf8(s: &str, n: i64) -> String {
    if n <= 0 || s.len() <= n as usize {
        return s.to_string();
    }
    let mut end = n as usize;
    while end > 0 && !s.is_char_boundary(end) {
        end -= 1;
    }
    s[..end].to_string()
}

// MARK: prompt functions

fn format_memory_rows(rows: &[MemoryRow]) -> String {
    let mut out = String::new();
    for row in rows {
        let mut ts = row.ts.clone();
        if ts.len() > 10 {
            ts.truncate(10);
        }
        if row.text.is_empty() {
            continue;
        }
        out.push_str(&format!("- [{ts}] {}\n", row.text));
    }
    if out.len() > 2000 {
        return truncate_utf8(&out, 2000);
    }
    out
}

// MARK: the node executor

/// Runs one node on a backend, mutating the scheduler's node state and queueing
/// events. Structurally the Go `runNode`; the split is the backend trait.
pub struct NodeExecutor<'a, B: PaneBackend> {
    pub sched: &'a mut Scheduler,
    pub roles: &'a RolePack,
    pub backend: &'a mut B,
    pub memory: Option<&'a mut dyn MemoryLink>,
    pub timeout: Duration,
}

impl<'a, B: PaneBackend> NodeExecutor<'a, B> {
    /// Executes `node` once. Status/retries/events are the result; a failure is
    /// recorded on the node and emitted, exactly as in Go (no Err to thread).
    pub fn run(&mut self, node: &str) {
        let Some(agent) = self.sched.crew().agent(node).cloned() else {
            self.sched
                .emit(Event::info("node_failed", format!("unknown node {node:?}")));
            return;
        };
        let resolved = self.resolved_model(&agent);
        let phase_idx = self.sched.node(node).map(|st| st.phase_idx).unwrap_or(-1);

        {
            let st = self.sched.node_mut(node).expect("node exists");
            st.status = Status::Running;
            st.started = Some(SystemTime::now());
            st.visits += 1;
        }
        let pane_id = self
            .sched
            .node(node)
            .map(|st| st.pane_id.clone())
            .unwrap_or_default();
        let mut start = Event::on_node("node_start", node);
        start.model = resolved.clone();
        start.info = format!("pane={pane_id}");
        if phase_idx >= 0
            && let Some(p) = self.sched.crew().phases.get(phase_idx as usize)
        {
            start.phase = p.name.clone();
        }
        self.sched.emit(start);

        // base prompt: phase instructions + the node's own prompt, then the role
        // prefix (MetaGPT's scaffold); instructions render through the same
        // template as the prompt.
        let mut base = agent.prompt.clone();
        if phase_idx >= 0
            && let Some(p) = self.sched.crew().phases.get(phase_idx as usize)
        {
            let ins = p.instructions.trim();
            if !ins.is_empty() {
                base = format!("{ins}\n\n{}", agent.prompt);
            }
        }
        base = self.roles.role_prompt(&agent, &base);

        // upstream outputs bind unconditionally — a skipped/empty upstream is
        // "", never "<no value>" (P13 BUG-06).
        let mut data: HashMap<String, String> = HashMap::new();
        for e in self.sched.crew().in_edges(node) {
            if let Some(up) = self.sched.node(&e.from) {
                data.insert(e.from.clone(), up.output.clone());
            }
        }
        let retries = self.sched.node(node).map(|st| st.retries).unwrap_or(0);

        let parts = match template::parse(node, &base, &mut |n| {
            matches!(n, "node" | "artifact" | "memory")
        }) {
            Ok(p) => p,
            Err(e) => {
                self.fail(node, &format!("prompt template: {e}"));
                return;
            }
        };
        // Resolve every call OCCURRENCE with short sequential borrows; Go
        // evaluates them during Execute, so repeats are repeats (two
        // {{ memory "q" }} = two recalls, two events).
        let mut values: Vec<Option<String>> = vec![None; parts.len()];
        for (i, part) in parts.iter().enumerate() {
            let template::Part::Call { name, arg, .. } = part else {
                continue;
            };
            values[i] = Some(match name.as_str() {
                "node" => self
                    .sched
                    .node(arg.trim())
                    .map(|st| st.output.clone())
                    .unwrap_or_default(),
                "artifact" => self.artifact(arg),
                "memory" => self.recall(node, arg),
                _ => String::new(),
            });
        }
        let prompt = template::render_resolved(&parts, &data, &values);
        let prompt = self.with_auto_recall(&agent, prompt);

        // baseline for the empty-settle guard; a read failure is ignored like
        // the Go `baseline, _ :=`
        let baseline = self
            .backend
            .read(node, "recent_unwrapped", 400)
            .unwrap_or_default();
        let until = ["idle", "done", "blocked"].map(String::from);
        let status = match self
            .backend
            .prompt_wait(node, &prompt, &until, self.timeout)
        {
            Ok(s) => s,
            Err(e) => {
                self.fail(node, &format!("agent.prompt+wait: {e}"));
                return;
            }
        };
        if status == "blocked" {
            self.block(node);
            return;
        }
        let mut text = match self.backend.read(node, "recent_unwrapped", 400) {
            Ok(t) => t,
            Err(e) => {
                self.fail(node, &format!("agent.read: {e}"));
                return;
            }
        };
        if empty_settle_prone(&resolved) && pane_delta(&baseline, &text) < 150 && retries == 0 {
            let delta = pane_delta(&baseline, &text);
            self.sched.node_mut(node).expect("node exists").retries += 1;
            self.sched.emit(
                Event::on_node("node_retry", node)
                    .with_info(format!("settle delta {delta} chars — re-prompting once")),
            );
            let status = match self
                .backend
                .prompt_wait(node, &prompt, &until, self.timeout)
            {
                Ok(s) => s,
                Err(e) => {
                    self.fail(node, &format!("agent.prompt+wait (retry): {e}"));
                    return;
                }
            };
            if status == "blocked" {
                self.block(node);
                return;
            }
            text = match self.backend.read(node, "recent_unwrapped", 400) {
                Ok(t) => t,
                Err(e) => {
                    self.fail(node, &format!("agent.read (retry): {e}"));
                    return;
                }
            };
        }

        let (cost, ctx_pct) = parse_pi_status(&text);
        let bus = Path::new(self.sched.effective_cwd())
            .join(".forseti")
            .join("bus")
            .join(format!("{}.md", agent.name));
        if let Err(e) = std::fs::write(&bus, &text) {
            self.sched
                .emit(Event::on_node("node_failed", node).with_info(format!("bus write: {e}")));
            self.sched.node_mut(node).expect("node exists").status = Status::Failed;
            return;
        }

        // publish under the node's artifact type too (P19D)
        let mut artifact_path = String::new();
        let writes = agent.writes_type();
        if !writes.is_empty() && writes != agent.name {
            let dir = Path::new(self.sched.effective_cwd())
                .join(".forseti")
                .join("bus")
                .join("typed");
            let _ = std::fs::create_dir_all(&dir);
            let path = dir.join(format!("{writes}.md"));
            if std::fs::write(&path, &text).is_ok() {
                artifact_path = path.to_string_lossy().into_owned();
            }
        }

        {
            let st = self.sched.node_mut(node).expect("node exists");
            st.output = text.clone();
            st.status = Status::Done;
            st.ended = Some(SystemTime::now());
            st.cost_usd = cost;
            st.ctx_pct = ctx_pct;
        }
        self.sched.emit(
            Event::on_node("node_done", node)
                .with_info(format!("{} chars → {}", text.len(), bus.display()))
                .with_model(&resolved)
                .with_metrics(cost, ctx_pct),
        );
        if let Some(label) = super::bus::parse_verdict(&text) {
            let info = if artifact_path.is_empty() {
                label
            } else {
                format!("{label} → {artifact_path}")
            };
            self.sched
                .emit(Event::on_node("verdict", node).with_info(info));
        }
        self.auto_write(&agent, &text);
    }

    /// `{{ artifact "type" }}` (P19D): the latest in-run output published under
    /// that type, else the typed file the previous run left behind.
    fn artifact(&mut self, t: &str) -> String {
        let t = t.trim();
        if t.is_empty() {
            return String::new();
        }
        for a in &self.sched.crew().agents {
            if a.writes_type() != t {
                continue;
            }
            if let Some(st) = self.sched.node(&a.name)
                && st.status == Status::Done
                && !st.output.is_empty()
            {
                return st.output.clone();
            }
        }
        let path = Path::new(self.sched.effective_cwd())
            .join(".forseti")
            .join("bus")
            .join("typed")
            .join(format!("{t}.md"));
        match std::fs::read_to_string(path) {
            Ok(s) => s,
            Err(_) => {
                self.sched.emit(Event::info(
                    "artifact_miss",
                    format!("no artifact of type {t:?} in this run"),
                ));
                String::new()
            }
        }
    }

    /// `{{ memory "query" }}`: advisory recall at render time. A down or empty
    /// service yields ""; the prompt is never changed by a failure.
    fn recall(&mut self, node: &str, query: &str) -> String {
        let ns = self.sched.crew().memory_ns();
        let Some(link) = self.memory.as_mut() else {
            return String::new();
        };
        let rec = link.recall(query, node, 3, &ns);
        match rec {
            Err(e) => {
                self.sched.emit(
                    Event::on_node("memory_recall", node).with_info(format!("unavailable: {e}")),
                );
                String::new()
            }
            Ok(rows) => {
                self.sched.emit(
                    Event::on_node("memory_recall", node)
                        .with_info(format!("{} entries for {query:?}", rows.len())),
                );
                format_memory_rows(&rows)
            }
        }
    }

    /// Go `effectiveModel` + the route rewrite: a route node always addresses
    /// its switchyard pool, so the outage override never touches it.
    fn resolved_model(&self, agent: &Agent) -> String {
        if !agent.route.is_empty() {
            return format!("switchyard/{}", agent.route);
        }
        let opts = self.sched.opts();
        if !opts.model_override.is_empty() && !agent.model.is_empty() {
            return opts.model_override.clone();
        }
        agent.model.clone()
    }

    /// Prepend the crew-memory block (P14). Advisory: any failure or empty
    /// result → the prompt unchanged.
    fn with_auto_recall(&mut self, agent: &Agent, prompt: String) -> String {
        let Some(mem) = self.sched.crew().memory.clone() else {
            return prompt;
        };
        if !mem.auto_recall_on() || agent.memory_off() {
            return prompt;
        }
        let k = if mem.recall_k <= 0 { 3 } else { mem.recall_k };
        let q = if agent.memory_query.is_empty() {
            format!("{} {}", agent.name, truncate_utf8(&prompt, 200))
        } else {
            agent.memory_query.clone()
        };
        let ns = self.sched.crew().memory_ns();
        let Some(link) = self.memory.as_mut() else {
            return prompt;
        };
        let rec = link.recall(&q, &agent.name, k, &ns);
        match rec {
            Err(e) => {
                self.sched.emit(
                    Event::on_node("memory_recall", &agent.name)
                        .with_info(format!("auto: unavailable: {e}")),
                );
                prompt
            }
            Ok(rows) if rows.is_empty() => {
                self.sched.emit(
                    Event::on_node("memory_recall", &agent.name)
                        .with_info(format!("auto: 0 entries for {:?}", truncate_utf8(&q, 60))),
                );
                prompt
            }
            Ok(rows) => {
                self.sched.emit(
                    Event::on_node("memory_recall", &agent.name).with_info(format!(
                        "auto: {} entries for {:?}",
                        rows.len(),
                        truncate_utf8(&q, 60)
                    )),
                );
                let mut block =
                    String::from("## Shared memory (from the crew; trust but verify)\n");
                for row in &rows {
                    let mut ts = row.ts.clone();
                    if ts.len() > 10 {
                        ts.truncate(10);
                    }
                    if row.text.is_empty() {
                        continue;
                    }
                    block.push_str(&format!("- [{ts}] {}\n", row.text));
                }
                let mut block = block.trim().to_string();
                if block.len() > 2000 {
                    block = truncate_utf8(&block, 2000);
                }
                format!("{block}\n\n---\n\n{prompt}")
            }
        }
    }

    /// Post a finished node's output to the crew namespace (P14). Errors only
    /// emit an event; the run never fails on memory.
    fn auto_write(&mut self, agent: &Agent, text: &str) {
        let Some(mem) = self.sched.crew().memory.clone() else {
            return;
        };
        if !mem.auto_write_on() || agent.memory_off() {
            return;
        }
        let text = truncate_utf8(text, mem.write_max_chars);
        if text.trim().is_empty() {
            return;
        }
        let ns = self.sched.crew().memory_ns();
        let run_id = self.sched.run_id().to_string();
        let source = format!("node:{}", agent.name);
        let tags = vec!["crew".to_string(), self.sched.crew().name.clone()];
        let Some(link) = self.memory.as_mut() else {
            return;
        };
        let written = link.write(&text, &ns, &run_id, &source, &tags);
        match written {
            Ok(id) => self.sched.emit(
                Event::on_node("memory_write", &agent.name)
                    .with_info(format!("#{id} -> crew-{ns}")),
            ),
            Err(e) => self.sched.emit(
                Event::on_node("memory_write", &agent.name).with_info(format!("unavailable: {e}")),
            ),
        }
    }

    fn fail(&mut self, node: &str, msg: &str) {
        if let Some(st) = self.sched.node_mut(node) {
            st.status = Status::Failed;
            st.ended = Some(SystemTime::now());
        }
        self.sched
            .emit(Event::on_node("node_failed", node).with_info(msg));
    }

    fn block(&mut self, node: &str) {
        if let Some(st) = self.sched.node_mut(node) {
            st.status = Status::Blocked;
            st.ended = Some(SystemTime::now());
        }
        self.sched.emit(
            Event::on_node("node_blocked", node)
                .with_info("approval/question UI detected — needs you in the pane"),
        );
    }
}

// MARK: tests

#[cfg(test)]
mod tests {
    use super::*;
    use crate::crew::{load, scheduler::Options};
    use std::collections::VecDeque;

    // ---- pure helpers (the Go runner_test.go ports) ----

    #[test]
    fn pi_status_parses_cost_and_context() {
        let text = "some answer text\n\
                    ~/repos/forseti (main)\n\
                    ↑5.9k ↓308 R23k CH94.8% $0.002 2.5%/200k (auto)          (opencode-go) glm-5.3-flash";
        let (cost, ctx) = parse_pi_status(text);
        assert!((cost - 0.002).abs() < 1e-9, "cost = {cost}");
        assert!((ctx - 2.5).abs() < 1e-9, "ctx = {ctx}");
    }

    #[test]
    fn pi_status_handles_free_ids_and_no_footer() {
        let (cost, ctx) =
            parse_pi_status("answer\n$0.000 0.0%/200k (auto)   opencode-go/beam-free");
        assert_eq!((cost, ctx), (0.0, 0.0));
        assert_eq!(parse_pi_status("just text, no status line"), (0.0, 0.0));
    }

    #[test]
    fn pi_status_takes_the_last_match() {
        // the pane echoes earlier prompts/answers, so the footer must win
        let (cost, ctx) = parse_pi_status("$9.999 99.0%/200k\nanswer\n$0.001 3.0%/200k");
        assert!((cost - 0.001).abs() < 1e-9, "cost = {cost}");
        assert!((ctx - 3.0).abs() < 1e-9, "ctx = {ctx}");
    }

    #[test]
    fn empty_settle_prone_covers_the_known_reasoners_only() {
        for m in [
            "halogen/halogen-qwen3.8-flash-next",
            "valhalla/valhalla-flash-next",
            "opencode-go/beam-free",
            "opencode-go/longcat-2.5-preview-free",
        ] {
            assert!(empty_settle_prone(m), "{m} must be guarded");
        }
        for m in [
            "opencode-go/glm-5.3-flash",
            "anthropic/claude-opus-4-7",
            "switchyard/forseti-auto",
            "",
        ] {
            assert!(!empty_settle_prone(m), "{m} must not be guarded");
        }
    }

    #[test]
    fn truncate_utf8_never_splits_a_rune() {
        assert_eq!(truncate_utf8("abc", 0), "abc");
        assert_eq!(truncate_utf8("abc", 2), "ab");
        // "é" is 2 bytes: a cut at 1 byte must back off entirely
        assert_eq!(truncate_utf8("é", 1), "");
        assert_eq!(truncate_utf8("aé", 2), "a");
    }

    // ---- node execution over a fake backend ----

    struct FakeBackend {
        reads: VecDeque<Result<String, String>>,
        waits: VecDeque<Result<String, String>>,
        prompts: Vec<String>,
        read_calls: usize,
        wait_calls: usize,
    }

    impl FakeBackend {
        fn new(reads: Vec<Result<String, String>>, waits: Vec<Result<String, String>>) -> Self {
            FakeBackend {
                reads: reads.into(),
                waits: waits.into(),
                prompts: Vec::new(),
                read_calls: 0,
                wait_calls: 0,
            }
        }
    }

    impl PaneBackend for FakeBackend {
        fn read(&mut self, _t: &str, _s: &str, _l: i64) -> Result<String, String> {
            self.read_calls += 1;
            self.reads.pop_front().unwrap_or(Ok(String::new()))
        }
        fn prompt(&mut self, _t: &str, text: &str) -> Result<(), String> {
            self.prompts.push(text.to_string());
            Ok(())
        }
        fn prompt_wait(
            &mut self,
            _t: &str,
            text: &str,
            _u: &[String],
            _timeout: Duration,
        ) -> Result<String, String> {
            self.wait_calls += 1;
            self.prompts.push(text.to_string());
            self.waits.pop_front().unwrap_or(Ok("done".to_string()))
        }
        fn focus(&mut self, _t: &str) -> Result<(), String> {
            Ok(())
        }
        fn status(&mut self, _t: &str) -> Result<String, String> {
            Ok("idle".to_string())
        }
    }

    struct NullMemory;

    impl MemoryLink for NullMemory {
        fn recall(
            &mut self,
            _q: &str,
            _n: &str,
            _k: i64,
            _ns: &str,
        ) -> Result<Vec<MemoryRow>, String> {
            Ok(Vec::new())
        }
        fn write(
            &mut self,
            _t: &str,
            _ns: &str,
            _r: &str,
            _s: &str,
            _tags: &[String],
        ) -> Result<i64, String> {
            Ok(1)
        }
    }

    fn temp_dir() -> std::path::PathBuf {
        use std::sync::atomic::{AtomicUsize, Ordering};
        static N: AtomicUsize = AtomicUsize::new(0);
        let n = N.fetch_add(1, Ordering::SeqCst);
        let dir = std::env::temp_dir().join(format!("forseti-runner-{}-{n}", std::process::id()));
        let _ = std::fs::remove_dir_all(&dir);
        std::fs::create_dir_all(dir.join(".forseti/bus")).expect("bus dir");
        dir
    }

    fn setup(yaml: &str) -> (Scheduler, RolePack, std::path::PathBuf) {
        let crew = load(yaml).expect("crew validates");
        let roles = RolePack::new(&crew.roles);
        let dir = temp_dir();
        let sched = Scheduler::new(
            crew,
            Options {
                cwd: dir.to_string_lossy().into_owned(),
                ..Default::default()
            },
        );
        (sched, roles, dir)
    }

    fn kinds(evs: &[Event], kind: &str) -> usize {
        evs.iter().filter(|e| e.kind == kind).count()
    }

    #[test]
    fn run_records_output_cost_and_verdict() {
        let (mut sched, roles, dir) = setup(
            r#"
name: one
roles:
  - {name: producer, profile: producer, goal: ship, instruction: "Write it.", writes: artifact}
agents:
  - {name: impl, prompt: "Do it. {{ .plan }}", role: producer, model: opencode-go/beam-free}
  - {name: plan, prompt: plan}
edges:
  - {from: plan, to: impl}
"#,
        );
        // the upstream has already run
        sched.node_mut("plan").unwrap().status = Status::Done;
        sched.node_mut("plan").unwrap().output = "THE PLAN".to_string();

        // a real answer clears the empty-settle guard's 150-char delta
        let answer = format!(
            "{}\n\nVERDICT: LGTM\n$0.004 7.5%/200k (auto)",
            "work ".repeat(40)
        );
        let mut backend = FakeBackend::new(
            vec![Ok("before".into()), Ok(answer.clone())],
            vec![Ok("idle".into())],
        );
        let mut mem = NullMemory;
        let mut ex = NodeExecutor {
            sched: &mut sched,
            roles: &roles,
            backend: &mut backend,
            memory: Some(&mut mem),
            timeout: Duration::from_secs(5),
        };
        ex.run("impl");

        // the upstream output was templated in, phase-free
        assert_eq!(backend.prompts.len(), 1);
        assert!(
            backend.prompts[0].ends_with("Do it. THE PLAN"),
            "{}",
            backend.prompts[0]
        );

        let st = sched.node("impl").unwrap();
        assert_eq!(st.status, Status::Done);
        assert_eq!(st.visits, 1);
        assert_eq!(st.output, answer);
        assert!((st.cost_usd - 0.004).abs() < 1e-9);
        assert!((st.ctx_pct - 7.5).abs() < 1e-9);

        // bus file + typed artifact
        let bus = dir.join(".forseti/bus/impl.md");
        assert_eq!(std::fs::read_to_string(&bus).unwrap(), answer);
        let typed = dir.join(".forseti/bus/typed/artifact.md");
        assert_eq!(std::fs::read_to_string(&typed).unwrap(), answer);

        let evs = sched.events();
        assert_eq!(kinds(evs, "node_start"), 1);
        assert_eq!(kinds(evs, "node_done"), 1);
        assert_eq!(kinds(evs, "verdict"), 1);
        let done = evs.iter().find(|e| e.kind == "node_done").unwrap();
        assert_eq!(done.model, "opencode-go/beam-free");
        assert!(done.info.contains("chars → "), "{}", done.info);
    }

    #[test]
    fn a_blocked_settle_records_blocked_and_does_not_write() {
        let (mut sched, roles, dir) = setup(
            r#"
name: block
agents:
  - {name: a, prompt: x}
"#,
        );
        let mut backend = FakeBackend::new(vec![Ok("before".into())], vec![Ok("blocked".into())]);
        let mut ex = NodeExecutor {
            sched: &mut sched,
            roles: &roles,
            backend: &mut backend,
            memory: None,
            timeout: Duration::from_secs(5),
        };
        ex.run("a");

        assert_eq!(sched.node("a").unwrap().status, Status::Blocked);
        assert_eq!(kinds(sched.events(), "node_blocked"), 1);
        assert!(!dir.join(".forseti/bus/a.md").exists());
    }

    #[test]
    fn the_empty_settle_guard_retries_a_reasoner_once() {
        let (mut sched, roles, _dir) = setup(
            r#"
name: retry
agents:
  - {name: a, prompt: x, model: opencode-go/beam-free}
"#,
        );
        // baseline, tiny first read, then a real answer
        let mut backend = FakeBackend::new(
            vec![
                Ok("pane".into()),
                Ok("pane\r\n> x".into()),
                Ok(format!("pane\r\n> x\r\n{}", "answer ".repeat(60))),
            ],
            vec![Ok("idle".into()), Ok("idle".into())],
        );
        let mut ex = NodeExecutor {
            sched: &mut sched,
            roles: &roles,
            backend: &mut backend,
            memory: None,
            timeout: Duration::from_secs(5),
        };
        ex.run("a");

        assert_eq!(sched.node("a").unwrap().retries, 1);
        assert_eq!(sched.node("a").unwrap().status, Status::Done);
        assert_eq!(backend.wait_calls, 2, "one retry, one settle");
        assert_eq!(kinds(sched.events(), "node_retry"), 1);
    }

    #[test]
    fn a_healthy_model_is_not_retried_for_a_short_answer() {
        let (mut sched, roles, _dir) = setup(
            r#"
name: no-retry
agents:
  - {name: a, prompt: x, model: opencode-go/glm-5.3-flash}
"#,
        );
        let mut backend = FakeBackend::new(
            vec![Ok("pane".into()), Ok("pane\r\n> x\r\nok".into())],
            vec![Ok("idle".into())],
        );
        let mut ex = NodeExecutor {
            sched: &mut sched,
            roles: &roles,
            backend: &mut backend,
            memory: None,
            timeout: Duration::from_secs(5),
        };
        ex.run("a");

        assert_eq!(sched.node("a").unwrap().retries, 0);
        assert_eq!(backend.wait_calls, 1);
        assert_eq!(kinds(sched.events(), "node_retry"), 0);
    }

    #[test]
    fn a_template_error_fails_the_node_with_go_s_message() {
        let (mut sched, roles, _dir) = setup(
            r#"
name: bad-tpl
agents:
  - {name: rev-reviewer, prompt: "line\nline\n{{ draft }}"}
"#,
        );
        let mut backend = FakeBackend::new(vec![], vec![]);
        let mut ex = NodeExecutor {
            sched: &mut sched,
            roles: &roles,
            backend: &mut backend,
            memory: None,
            timeout: Duration::from_secs(5),
        };
        ex.run("rev-reviewer");

        assert_eq!(sched.node("rev-reviewer").unwrap().status, Status::Failed);
        let ev = sched
            .events()
            .iter()
            .find(|e| e.kind == "node_failed")
            .expect("node_failed emitted");
        assert_eq!(
            ev.info,
            "prompt template: template: rev-reviewer:3: function \"draft\" not defined"
        );
        assert_eq!(backend.wait_calls, 0, "never dispatched");
    }

    #[test]
    fn a_read_error_after_settle_fails_the_node() {
        let (mut sched, roles, _dir) = setup(
            r#"
name: read-fail
agents:
  - {name: a, prompt: x}
"#,
        );
        let mut backend = FakeBackend::new(
            vec![Ok("pane".into()), Err("pane gone".into())],
            vec![Ok("idle".into())],
        );
        let mut ex = NodeExecutor {
            sched: &mut sched,
            roles: &roles,
            backend: &mut backend,
            memory: None,
            timeout: Duration::from_secs(5),
        };
        ex.run("a");

        assert_eq!(sched.node("a").unwrap().status, Status::Failed);
        let ev = sched
            .events()
            .iter()
            .find(|e| e.kind == "node_failed")
            .unwrap();
        assert_eq!(ev.info, "agent.read: pane gone");
    }

    #[test]
    fn phase_instructions_and_roles_prefix_the_prompt() {
        let (mut sched, roles, _dir) = setup(
            r#"
name: phased
roles:
  - {name: planner, profile: planner, goal: ship it, instruction: "Write the plan."}
agents:
  - {name: p, prompt: "request: x", role: planner}
  - {name: q, prompt: y}
phases:
  - {name: one, instructions: "First, {{ .seed }}", agents: [p]}
  - {name: two, instructions: second, agents: [q]}
"#,
        );
        let mut backend = FakeBackend::new(
            vec![Ok(String::new()), Ok("out".into())],
            vec![Ok("idle".into())],
        );
        let mut ex = NodeExecutor {
            sched: &mut sched,
            roles: &roles,
            backend: &mut backend,
            memory: None,
            timeout: Duration::from_secs(5),
        };
        ex.run("p");

        let prompt = &backend.prompts[0];
        // role scaffold first (MetaGPT prefix), then instructions + prompt
        assert!(
            prompt.starts_with("You are a planner, named p, your goal is ship it."),
            "{prompt}"
        );
        assert!(prompt.contains("First, <no value>"), "{prompt}");
        assert!(prompt.contains("request: x"), "{prompt}");
        // node_start carries the phase
        let start = sched
            .events()
            .iter()
            .find(|e| e.kind == "node_start")
            .unwrap();
        assert_eq!(start.phase, "one");
    }

    #[test]
    fn auto_recall_prepends_the_block_and_auto_write_posts_it() {
        let (mut sched, roles, _dir) = setup(
            r#"
name: mem
memory:
  namespace: memtest
agents:
  - {name: a, prompt: x}
"#,
        );
        struct Mem {
            writes: usize,
        }
        impl MemoryLink for Mem {
            fn recall(
                &mut self,
                _q: &str,
                _n: &str,
                _k: i64,
                _ns: &str,
            ) -> Result<Vec<MemoryRow>, String> {
                Ok(vec![MemoryRow {
                    ts: "2026-10-01T12:00:00Z".into(),
                    text: "remember this".into(),
                }])
            }
            fn write(
                &mut self,
                _t: &str,
                _ns: &str,
                _r: &str,
                _s: &str,
                _tags: &[String],
            ) -> Result<i64, String> {
                self.writes += 1;
                Ok(7)
            }
        }
        let mut mem = Mem { writes: 0 };
        let mut backend = FakeBackend::new(
            vec![Ok(String::new()), Ok("answer ".repeat(40))],
            vec![Ok("idle".into())],
        );
        let mut ex = NodeExecutor {
            sched: &mut sched,
            roles: &roles,
            backend: &mut backend,
            memory: Some(&mut mem),
            timeout: Duration::from_secs(5),
        };
        ex.run("a");

        let prompt = &backend.prompts[0];
        assert!(
            prompt.starts_with("## Shared memory (from the crew; trust but verify)"),
            "{prompt}"
        );
        assert!(prompt.contains("- [2026-10-01] remember this"), "{prompt}");
        assert!(prompt.contains("\n\n---\n\nx"), "{prompt}");
        assert_eq!(mem.writes, 1, "auto-write posts the node output");
        let ev = sched
            .events()
            .iter()
            .find(|e| e.kind == "memory_write")
            .unwrap();
        assert_eq!(ev.info, "#7 -> crew-memtest");
    }
}
