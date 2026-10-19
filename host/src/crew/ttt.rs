//! The ttt `PaneBackend` (Phase 21): run a crew with pi inside ttt instead of
//! herdr.
//!
//! ttt exposes no socket API for agents, but it has three surfaces this bridge
//! composes:
//!
//! 1. **`POST /exec`** on `127.0.0.1:4242` (`ttt --listen`) runs a palette
//!    command, here `Forseti: Agent Bridge` from the forseti ttt plugin.
//! 2. **Hand-off files** in the plugin dir: this backend writes
//!    `agent-req.json` (`{seq, op: spawn|send|read|focus, …}`), the command acts
//!    through `ttt.terminal.*` and writes `agent-res.json` (`{seq, ok, id|tail|
//!    error}`). The seq rejects stale responses; a missing response means the
//!    plugin is not loaded (the error says so).
//! 3. **The pi status channel** (S51): the spawned agent writes
//!    `.forseti/agent-status/<name>.json` (`{state: idle|working, pid, ts}`), so
//!    `prompt_wait` can gate on the same idle boundary herdr provides.
//!
//! The bridge needs the forseti ttt fork (`ttt.terminal`, `docs/
//! ttt-terminal-api.md`). On upstream ttt the command answers with a clear
//! "unavailable" error instead of hanging.

use std::collections::{BTreeMap, HashSet};
use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};
use std::sync::{Arc, Mutex};
use std::thread::JoinHandle;
use std::time::{Duration, Instant};

use serde::{Deserialize, Serialize};
use serde_json::{Value, json};

use super::model::Crew;
use super::runner::PaneBackend;

pub const DEFAULT_EXEC_URL: &str = "http://127.0.0.1:4242/exec";
pub const BRIDGE_COMMAND: &str = "Forseti: Agent Bridge";
const RESPONSE_TIMEOUT: Duration = Duration::from_secs(10);
const START_TIMEOUT: Duration = Duration::from_secs(10);
/// A status record is written during session init; the TUI draws after it.
const READY_GRACE: Duration = Duration::from_millis(750);

static BRIDGE_TAG: AtomicU64 = AtomicU64::new(0);

/// One status-channel record (S51).
#[derive(Debug, Clone, Default, Deserialize, Serialize)]
pub struct StatusRecord {
    #[serde(default)]
    pub name: String,
    #[serde(default)]
    pub state: String,
    #[serde(default)]
    pub pid: u64,
    #[serde(default)]
    pub ts: String,
    #[serde(default)]
    pub tool: String,
}

/// The request/response bridge to the forseti ttt plugin.
pub struct TttBridge {
    exec_url: String,
    plugin_dir: PathBuf,
    status_dir: PathBuf,
    /// Per-instance request/response namespace, so concurrent crews (or a
    /// manual bridge call) never share a file.
    tag: String,
    /// Extra env for every spawned tab (memory namespace, run id, …).
    extra_env: BTreeMap<String, String>,
    seq: u64,
    http: reqwest::blocking::Client,
}

impl TttBridge {
    pub fn new(exec_url: &str, plugin_dir: PathBuf, status_dir: PathBuf) -> Self {
        TttBridge {
            exec_url: exec_url.to_string(),
            plugin_dir,
            status_dir,
            tag: format!(
                "{}-{}",
                std::process::id(),
                BRIDGE_TAG.fetch_add(1, Ordering::SeqCst)
            ),
            extra_env: BTreeMap::new(),
            seq: 0,
            http: reqwest::blocking::Client::builder()
                .timeout(Duration::from_secs(15))
                .build()
                .expect("ttt bridge HTTP client"),
        }
    }

    /// Overrides the status directory (per-run under `.forseti/agent-status`).
    pub fn with_status_dir(mut self, dir: PathBuf) -> Self {
        self.status_dir = dir;
        self
    }

    /// Adds an env var for every spawned tab (e.g. `FORSETI_MEMORY_CREW`).
    pub fn with_env(mut self, key: &str, value: &str) -> Self {
        self.extra_env.insert(key.to_string(), value.to_string());
        self
    }

    /// A second bridge with its own request namespace, for the live monitor
    /// (the backend keeps polling the status while the monitor reads panes).
    pub fn fork(&self) -> TttBridge {
        TttBridge::new(
            &self.exec_url,
            self.plugin_dir.clone(),
            self.status_dir.clone(),
        )
    }

    /// `FORSETI_TTT_EXEC_URL`, `FORSETI_TTT_PLUGIN_DIR`,
    /// `FORSETI_STATUS_DIR`; defaults are ttt's hardcoded exec URL, the forseti
    /// plugin dir, and `<cwd>/.forseti/agent-status`.
    pub fn from_env(cwd: &str) -> Self {
        let exec_url = env_nonempty("FORSETI_TTT_EXEC_URL").unwrap_or(DEFAULT_EXEC_URL.to_string());
        let plugin_dir = env_nonempty("FORSETI_TTT_PLUGIN_DIR")
            .map(PathBuf::from)
            .unwrap_or_else(|| {
                let home = std::env::var_os("HOME").unwrap_or_default();
                PathBuf::from(home).join(".config/ttt/plugins/forseti")
            });
        let status_dir = env_nonempty("FORSETI_STATUS_DIR")
            .map(PathBuf::from)
            .unwrap_or_else(|| Path::new(cwd).join(".forseti/agent-status"));
        Self::new(&exec_url, plugin_dir, status_dir)
    }

    pub fn status_dir(&self) -> &Path {
        &self.status_dir
    }

    fn call(&mut self, mut req: Value) -> Result<Value, String> {
        self.seq += 1;
        let seq = self.seq;
        req["seq"] = json!(seq);
        let req_path = self.plugin_dir.join(format!("agent-req-{}.json", self.tag));
        let res_path = self.plugin_dir.join(format!("agent-res-{}.json", self.tag));
        std::fs::create_dir_all(&self.plugin_dir)
            .map_err(|e| format!("ttt plugin dir {}: {e}", self.plugin_dir.display()))?;
        // atomic request: the plugin never reads a half-written file
        let tmp = self
            .plugin_dir
            .join(format!(".agent-req-{}-{seq}.tmp", self.tag));
        super::fsx::write_private(&tmp, &serde_json::to_vec(&req).map_err(|e| e.to_string())?)
            .map_err(|e| format!("write {}: {e}", tmp.display()))?;
        std::fs::rename(&tmp, &req_path)
            .map_err(|e| format!("write {}: {e}", req_path.display()))?;
        let _ = std::fs::remove_file(&res_path);

        let resp = self
            .http
            .post(&self.exec_url)
            .header("Content-Type", "text/plain")
            .body(format!("exec {BRIDGE_COMMAND:?}"))
            .send()
            .map_err(|e| format!("ttt exec unreachable ({}): {e}", self.exec_url))?;
        // the plugin reads the request during the POST (ttt runs it inline)
        let _ = std::fs::remove_file(&req_path);
        if !resp.status().is_success() {
            let _ = std::fs::remove_file(&res_path);
            return Err(format!("ttt exec: HTTP {}", resp.status().as_u16()));
        }

        let deadline = Instant::now() + RESPONSE_TIMEOUT;
        let outcome = loop {
            if let Ok(text) = std::fs::read_to_string(&res_path)
                && let Ok(v) = serde_json::from_str::<Value>(&text)
                && v.get("seq").and_then(Value::as_u64) == Some(seq)
            {
                if v.get("ok").and_then(Value::as_bool) != Some(true) {
                    break Err(v
                        .get("error")
                        .and_then(Value::as_str)
                        .unwrap_or("ttt bridge error")
                        .to_string());
                }
                break Ok(v);
            }
            if Instant::now() >= deadline {
                break Err(format!(
                    "ttt bridge: no response to seq {seq} — is the forseti plugin loaded (Plugins: Reload All)?"
                ));
            }
            std::thread::sleep(Duration::from_millis(20));
        };
        // never leave pane text (prompts + answers) behind
        let _ = std::fs::remove_file(&res_path);
        outcome
    }

    /// Opens a pi tab for one node; `FORSETI_AGENT_NAME` turns on the status
    /// channel (S51).
    pub fn spawn(&mut self, name: &str, args: &[String], cwd: &str) -> Result<String, String> {
        let mut env = self.extra_env.clone();
        env.insert("FORSETI_AGENT_NAME".to_string(), name.to_string());
        env.insert(
            "FORSETI_STATUS_DIR".to_string(),
            self.status_dir.to_string_lossy().into_owned(),
        );
        self.spawn_with_env("pi", args, cwd, name, &env)
    }

    /// Spawns any program in a tab (no crew env); used by `--review` for
    /// lazygit.
    pub fn spawn_shell(
        &mut self,
        shell: &str,
        args: &[String],
        cwd: &str,
        label: &str,
    ) -> Result<String, String> {
        self.spawn_with_env(shell, args, cwd, label, &BTreeMap::new())
    }

    fn spawn_with_env(
        &mut self,
        shell: &str,
        args: &[String],
        cwd: &str,
        label: &str,
        env: &BTreeMap<String, String>,
    ) -> Result<String, String> {
        let res = self.call(json!({
            "op": "spawn",
            "shell": shell,
            "args": args,
            "env": env,
            "cwd": cwd,
            "label": label,
        }))?;
        res.get("id")
            .and_then(Value::as_str)
            .filter(|s| !s.is_empty())
            .map(str::to_string)
            .ok_or_else(|| "ttt spawn returned no terminal id".to_string())
    }

    pub fn send(&mut self, id: &str, text: &str) -> Result<(), String> {
        self.call(json!({"op": "send", "id": id, "text": text}))?;
        Ok(())
    }

    pub fn read(&mut self, id: &str, max_bytes: i64) -> Result<String, String> {
        let res = self.call(json!({"op": "read", "id": id, "n": max_bytes}))?;
        Ok(res
            .get("tail")
            .and_then(Value::as_str)
            .unwrap_or_default()
            .to_string())
    }

    /// The visible screen as plain lines (the fork's `ttt.terminal.screen`) —
    /// real rows for `watch:` matching, unlike the raw PTY tail.
    pub fn screen(&mut self, id: &str) -> Result<String, String> {
        let res = self.call(json!({"op": "screen", "id": id}))?;
        Ok(res
            .get("text")
            .and_then(Value::as_str)
            .unwrap_or_default()
            .to_string())
    }

    pub fn focus(&mut self, id: &str) -> Result<(), String> {
        self.call(json!({"op": "focus", "id": id}))?;
        Ok(())
    }

    /// Kills the terminal's PTY and removes its tab (the ttt fork's
    /// `ttt.terminal.close`, `terminal.control`).
    pub fn close(&mut self, id: &str) -> Result<(), String> {
        self.call(json!({"op": "close", "id": id}))?;
        Ok(())
    }

    /// Reads the pi status channel for `name` (None = no file yet).
    pub fn status(&self, name: &str) -> Option<StatusRecord> {
        let text = std::fs::read_to_string(self.status_dir.join(format!("{name}.json"))).ok()?;
        serde_json::from_str(&text).ok()
    }

    /// Waits until the agent has started AND its TUI has drawn (status `idle`
    /// or `working`, then `READY_GRACE`, then a non-empty pane tail). The status
    /// hook fires during session init, before pi's input reader exists; sending
    /// a prompt into that window drops it (S33e), so the tail is the real gate.
    pub fn await_ready(&mut self, name: &str, id: &str, timeout: Duration) -> Result<(), String> {
        let deadline = Instant::now() + timeout;
        let mut seen: Option<Instant> = None;
        loop {
            if let Some(rec) = self.status(name)
                && matches!(rec.state.as_str(), "idle" | "working")
            {
                let first = *seen.get_or_insert_with(Instant::now);
                if first.elapsed() >= READY_GRACE {
                    let tail = self.read(id, 0).unwrap_or_default();
                    if !tail.trim().is_empty()
                        && self
                            .status(name)
                            .is_some_and(|r| matches!(r.state.as_str(), "idle" | "working"))
                    {
                        return Ok(());
                    }
                }
            } else {
                seen = None;
            }
            if Instant::now() >= deadline {
                let tail = self.read(id, 0).unwrap_or_default();
                return Err(format!(
                    "ttt agent {name:?} not ready within {}s (status {:?}, pane {} bytes) — is pi still running in the tab?",
                    timeout.as_secs(),
                    self.status(name).map(|r| r.state).unwrap_or_default(),
                    tail.len()
                ));
            }
            std::thread::sleep(Duration::from_millis(100));
        }
    }
}

fn env_nonempty(key: &str) -> Option<String> {
    std::env::var(key).ok().filter(|v| !v.is_empty())
}

// MARK: the backend

/// `PaneBackend` over the ttt bridge. `nodes` maps crew node names to the
/// terminal ids the plugin returned.
pub struct TttBackend {
    pub bridge: TttBridge,
    pub nodes: BTreeMap<String, String>,
}

impl TttBackend {
    pub fn new(bridge: TttBridge) -> Self {
        TttBackend {
            bridge,
            nodes: BTreeMap::new(),
        }
    }

    pub fn register(&mut self, name: &str, id: &str) {
        self.nodes.insert(name.to_string(), id.to_string());
    }

    fn id(&self, name: &str) -> Result<String, String> {
        self.nodes
            .get(name)
            .cloned()
            .ok_or_else(|| format!("ttt: no terminal registered for node {name:?}"))
    }

    /// Sends the prompt, then waits for the status channel to leave `idle`.
    /// Falls back to "the tail grew" for a turn that finishes between polls.
    fn wait_settled(
        &mut self,
        name: &str,
        baseline: &str,
        timeout: Duration,
    ) -> Result<String, String> {
        let id = self.id(name)?;
        let started = Instant::now();
        let mut saw_working = false;
        while started.elapsed() < START_TIMEOUT {
            if let Some(rec) = self.bridge.status(name) {
                if rec.state == "working" {
                    saw_working = true;
                    break;
                }
                if rec.state == "blocked" {
                    return Ok("blocked".to_string());
                }
            }
            std::thread::sleep(Duration::from_millis(100));
        }
        if !saw_working {
            // the turn may have completed inside one poll: accept a changed tail
            if self.bridge.read(&id, 0).unwrap_or_default() != baseline {
                return Ok("idle".to_string());
            }
            return Err(format!(
                "ttt agent {name:?}: prompt produced no working state within {}s",
                START_TIMEOUT.as_secs()
            ));
        }
        let deadline = Instant::now() + timeout;
        loop {
            if let Some(rec) = self.bridge.status(name) {
                if rec.state == "idle" {
                    return Ok("idle".to_string());
                }
                if rec.state == "blocked" {
                    return Ok("blocked".to_string());
                }
            }
            if Instant::now() >= deadline {
                return Err(format!(
                    "ttt agent {name:?}: no idle within {}s (a stuck `working` usually means an approval prompt)",
                    timeout.as_secs()
                ));
            }
            std::thread::sleep(Duration::from_millis(200));
        }
    }
}

impl PaneBackend for TttBackend {
    fn read(&mut self, target: &str, _source: &str, lines: i64) -> Result<String, String> {
        let id = self.id(target)?;
        let raw = self.bridge.read(&id, 0)?;
        let text = strip_ansi(&raw);
        Ok(tail_lines(&text, lines.max(0) as usize))
    }

    fn prompt(&mut self, target: &str, text: &str) -> Result<(), String> {
        let id = self.id(target)?;
        // pi's TUI submits on CR; a LF is not Enter (verified live)
        self.bridge.send(&id, &format!("{text}\r"))
    }

    fn prompt_wait(
        &mut self,
        target: &str,
        text: &str,
        _until: &[String],
        timeout: Duration,
    ) -> Result<String, String> {
        let id = self.id(target)?;
        // snapshot BEFORE the prompt: a turn that finishes between polls must
        // still show a grown tail (the prompt echo alone does not count)
        let baseline = self.bridge.read(&id, 0).unwrap_or_default();
        // pi's TUI submits on CR; a LF is not Enter (verified live)
        self.bridge.send(&id, &format!("{text}\r"))?;
        self.wait_settled(target, &baseline, timeout)
    }

    fn focus(&mut self, target: &str) -> Result<(), String> {
        let id = self.id(target)?;
        self.bridge.focus(&id)
    }

    fn status(&mut self, target: &str) -> Result<String, String> {
        Ok(self
            .bridge
            .status(target)
            .map(|r| r.state)
            .unwrap_or_default())
    }
}

// MARK: text helpers

/// Strips ANSI escape sequences (CSI, OSC, and two-byte ESC forms) and `\r`, so
/// a raw PTY tail reads like herdr's `recent_unwrapped` pane text.
pub fn strip_ansi(s: &str) -> String {
    let mut out = String::with_capacity(s.len());
    let mut chars = s.chars().peekable();
    while let Some(c) = chars.next() {
        match c {
            '\u{1b}' => match chars.next() {
                Some('[') => {
                    // CSI: parameter/intermediate bytes, then a final byte @-~
                    for n in chars.by_ref() {
                        if ('\u{40}'..='\u{7e}').contains(&n) {
                            break;
                        }
                    }
                }
                Some(']') | Some('P') | Some('_') | Some('^') | Some('X') => {
                    // OSC / DCS / APC / PM / SOS: terminated by BEL or ST (ESC \)
                    let mut prev = '\0';
                    for n in chars.by_ref() {
                        if n == '\u{07}' || (prev == '\u{1b}' && n == '\\') {
                            break;
                        }
                        prev = n;
                    }
                }
                Some(_) | None => {}
            },
            '\r' => {}
            _ => out.push(c),
        }
    }
    out
}

/// The last `lines` lines of `text` (0 = all).
pub fn tail_lines(text: &str, lines: usize) -> String {
    if lines == 0 {
        return text.to_string();
    }
    let all: Vec<&str> = text.split_inclusive('\n').collect();
    if all.len() <= lines {
        return text.to_string();
    }
    all[all.len() - lines..].concat()
}

// MARK: the live monitor (Phase 22B)

/// The writer a live monitor shares with the run: the JSONL log (written
/// immediately, marked `written` so the regular flush does not duplicate it),
/// the queue the flush drains into `outcome.events`, and the bridge projector.
pub struct LiveReporter {
    log: Arc<Mutex<std::fs::File>>,
    headless: bool,
    queued: Arc<Mutex<Vec<super::scheduler::Event>>>,
    bridge_lock: Arc<Mutex<()>>,
    cwd: String,
    total: usize,
}

impl LiveReporter {
    pub fn new(
        log: Arc<Mutex<std::fs::File>>,
        headless: bool,
        queued: Arc<Mutex<Vec<super::scheduler::Event>>>,
        bridge_lock: Arc<Mutex<()>>,
        cwd: &str,
        total: usize,
    ) -> Self {
        LiveReporter {
            log,
            headless,
            queued,
            bridge_lock,
            cwd: cwd.to_string(),
            total,
        }
    }

    fn emit(&self, mut ev: super::scheduler::Event) {
        if ev.ts.is_empty() {
            ev.ts = super::scheduler::rfc3339_now();
        }
        if let Ok(mut log) = self.log.lock() {
            if let Ok(line) = serde_json::to_string(&ev) {
                if self.headless {
                    println!("{line}");
                }
                let _ = std::io::Write::write_all(&mut *log, format!("{line}\n").as_bytes());
                let _ = std::io::Write::flush(&mut *log);
            }
            ev.written = true;
        }
        if let Ok(mut q) = self.queued.lock() {
            q.push(ev);
        }
    }

    /// Updates the node rows + counts of the existing crew-status.json. The
    /// run's own flush owns the full projection; this only keeps the badge
    /// live between flushes (serialized by the shared bridge lock).
    fn patch_bridge(&self, statuses: &BTreeMap<String, String>) {
        if self.cwd.is_empty() || statuses.is_empty() {
            return;
        }
        let _guard = self.bridge_lock.lock();
        let path = Path::new(&self.cwd)
            .join(".forseti")
            .join("crew-status.json");
        let Ok(text) = std::fs::read_to_string(&path) else {
            return;
        };
        let Ok(mut v) = serde_json::from_str::<Value>(&text) else {
            return;
        };
        let Some(nodes) = v.get_mut("nodes").and_then(Value::as_array_mut) else {
            return;
        };
        let mut changed = false;
        for row in nodes.iter_mut() {
            let Some(name) = row.get("name").and_then(Value::as_str) else {
                continue;
            };
            // only `blocked` changes the projection: `idle/working` are pi's
            // live states, while the node is still RUNNING in the scheduler
            // until the runner settles it. Overwriting with them would break
            // the sidebar's status glyphs.
            if statuses.get(name).map(String::as_str) == Some("blocked")
                && row.get("status").and_then(Value::as_str) != Some("blocked")
            {
                row["status"] = json!("blocked");
                changed = true;
            }
        }
        if !changed {
            return;
        }
        let mut counts: BTreeMap<String, usize> = BTreeMap::new();
        for row in nodes.iter() {
            if let Some(s) = row.get("status").and_then(Value::as_str) {
                *counts.entry(s.to_string()).or_insert(0) += 1;
            }
        }
        for (key, status) in [
            ("done", "done"),
            ("running", "running"),
            ("blocked", "blocked"),
            ("failed", "failed"),
        ] {
            v[key] = json!(counts.get(status).copied().unwrap_or(0));
        }
        v["total"] = json!(self.total);
        v["ts"] = json!(super::scheduler::rfc3339_now());
        let tmp = path.with_extension(format!("json.tmp-{}", std::process::id()));
        if super::fsx::write_private(&tmp, v.to_string().as_bytes()).is_ok() {
            let _ = std::fs::rename(&tmp, &path);
        }
    }
}

/// Polls the status channel + watched screens for the ttt host (ttt has no
/// subscription surface; the files and the visible terminal are the signals).
pub struct TttLive {
    stop: Arc<AtomicBool>,
    handle: Option<JoinHandle<()>>,
}

impl TttLive {
    pub fn start(
        mut bridge: TttBridge,
        crew: &Crew,
        ids: &BTreeMap<String, String>,
        reporter: LiveReporter,
    ) -> TttLive {
        let stop = Arc::new(AtomicBool::new(false));
        let crews_stop = Arc::clone(&stop);
        let nodes: Vec<(String, String)> =
            ids.iter().map(|(n, i)| (n.clone(), i.clone())).collect();
        let watches: Vec<(String, regex::Regex, String)> = crew
            .watch
            .iter()
            .filter_map(|w| {
                regex::Regex::new(&w.match_)
                    .ok()
                    .map(|re| (w.node.clone(), re, w.match_.clone()))
            })
            .collect();
        let handle = std::thread::spawn(move || {
            let mut last: BTreeMap<String, String> = BTreeMap::new();
            let mut seen: HashSet<(String, String)> = HashSet::new();
            while !crews_stop.load(Ordering::SeqCst) {
                let mut statuses = BTreeMap::new();
                for (node, _id) in &nodes {
                    let state = bridge.status(node).map(|r| r.state).unwrap_or_default();
                    if state.is_empty() {
                        continue;
                    }
                    statuses.insert(node.clone(), state.clone());
                    if last.get(node) != Some(&state) {
                        let mut ev = super::scheduler::Event::on_node("node_status", node);
                        ev.info = state.clone();
                        ev.ts = super::scheduler::rfc3339_now();
                        reporter.emit(ev);
                        last.insert(node.clone(), state);
                    }
                }
                for (node, re, pattern) in &watches {
                    let Some((_, id)) = nodes.iter().find(|(n, _)| n == node) else {
                        continue;
                    };
                    let Ok(text) = bridge.screen(id) else {
                        continue;
                    };
                    for line in text.lines() {
                        let line = line.trim();
                        if !line.is_empty()
                            && re.is_match(line)
                            && seen.insert((node.clone(), line.to_string()))
                        {
                            reporter.emit(super::live::watch_event(node, pattern, line));
                        }
                    }
                }
                reporter.patch_bridge(&statuses);
                std::thread::sleep(Duration::from_millis(300));
            }
        });
        TttLive {
            stop,
            handle: Some(handle),
        }
    }

    pub fn stop(&mut self) {
        self.stop.store(true, Ordering::SeqCst);
        if let Some(h) = self.handle.take() {
            let _ = h.join();
        }
    }
}

// MARK: tests

#[cfg(test)]
mod tests {
    use super::*;
    use std::io::{Read, Write};
    use std::net::TcpListener;
    use std::sync::atomic::{AtomicBool, Ordering};
    use std::sync::{Arc, Mutex};

    /// A fake `ttt --listen` + forseti plugin: each POST reads the request file
    /// and writes the response the test scripted for that op.
    struct FakeTtt {
        url: String,
        requests: Arc<Mutex<Vec<Value>>>,
        stop: Arc<AtomicBool>,
        handle: Option<std::thread::JoinHandle<()>>,
        dir: PathBuf,
    }

    impl FakeTtt {
        fn new(dir: PathBuf, responses: BTreeMap<&'static str, Value>) -> FakeTtt {
            let listener = TcpListener::bind("127.0.0.1:0").expect("bind");
            let addr = listener.local_addr().expect("addr");
            let requests = Arc::new(Mutex::new(Vec::new()));
            let stop = Arc::new(AtomicBool::new(false));
            let handle = {
                let requests = Arc::clone(&requests);
                let stop = Arc::clone(&stop);
                let dir = dir.clone();
                std::thread::spawn(move || {
                    for stream in listener.incoming() {
                        if stop.load(Ordering::SeqCst) {
                            return;
                        }
                        let Ok(mut stream) = stream else { return };
                        let mut chunk = [0u8; 4096];
                        let _ = stream.read(&mut chunk);
                        // the request is already in agent-req.json (written
                        // before the POST), so simulate the plugin from it
                        // the bridge namespaces its files per instance; find
                        // this POST's request and answer into its response
                        let mut req = Value::Null;
                        let mut res_path = dir.join("agent-res.json");
                        if let Ok(rd) = std::fs::read_dir(&dir) {
                            for entry in rd.flatten() {
                                let name = entry.file_name().to_string_lossy().into_owned();
                                if name.starts_with("agent-req")
                                    && name.ends_with(".json")
                                    && let Ok(t) = std::fs::read_to_string(entry.path())
                                    && let Ok(v) = serde_json::from_str::<Value>(&t)
                                {
                                    req = v;
                                    res_path = dir.join(name.replacen("agent-req", "agent-res", 1));
                                }
                            }
                        }
                        if let Ok(mut v) = requests.lock() {
                            v.push(req.clone());
                        }
                        let op = req.get("op").and_then(Value::as_str).unwrap_or("");
                        let mut res = responses.get(op).cloned().unwrap_or_else(|| json!({"ok": false, "error": format!("no canned response for {op}")}));
                        res["seq"] = req.get("seq").cloned().unwrap_or(json!(0));
                        let _ = std::fs::write(res_path, serde_json::to_vec(&res).unwrap());
                        let _ = stream.write_all(
                            b"HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\n{}",
                        );
                        let _ = stream.flush();
                    }
                })
            };
            FakeTtt {
                url: format!("http://{addr}/exec"),
                requests,
                stop,
                handle: Some(handle),
                dir,
            }
        }

        fn requests(&self) -> Vec<Value> {
            self.requests.lock().unwrap().clone()
        }
    }

    impl Drop for FakeTtt {
        fn drop(&mut self) {
            self.stop.store(true, Ordering::SeqCst);
            // unblock accept()
            let _ = std::net::TcpStream::connect(
                self.url
                    .trim_start_matches("http://")
                    .trim_end_matches("/exec"),
            );
            if let Some(h) = self.handle.take() {
                let _ = h.join();
            }
            let _ = std::fs::remove_dir_all(&self.dir);
        }
    }

    fn temp_dir() -> PathBuf {
        use std::sync::atomic::AtomicUsize;
        static N: AtomicUsize = AtomicUsize::new(0);
        let n = N.fetch_add(1, Ordering::SeqCst);
        let dir = std::env::temp_dir().join(format!("forseti-ttt-{}-{n}", std::process::id()));
        let _ = std::fs::remove_dir_all(&dir);
        std::fs::create_dir_all(&dir).expect("temp dir");
        dir
    }

    fn bridge(dir: &Path, fake: &FakeTtt) -> TttBridge {
        TttBridge::new(&fake.url, dir.to_path_buf(), dir.join("status"))
    }

    #[test]
    fn spawn_sends_the_spec_and_returns_the_id() {
        let dir = temp_dir();
        let mut responses = BTreeMap::new();
        responses.insert("spawn", json!({"ok": true, "id": "terminal-7"}));
        let fake = FakeTtt::new(dir.clone(), responses);
        let mut b = bridge(&dir, &fake);
        let id = b
            .spawn("planner", &["--model".to_string(), "m".to_string()], "/w")
            .expect("spawn");
        assert_eq!(id, "terminal-7");
        let req = &fake.requests()[0];
        assert_eq!(req["op"], "spawn");
        assert_eq!(req["shell"], "pi");
        assert_eq!(req["args"], json!(["--model", "m"]));
        assert_eq!(req["env"]["FORSETI_AGENT_NAME"], "planner");
        assert_eq!(
            req["env"]["FORSETI_STATUS_DIR"],
            dir.join("status").to_string_lossy().as_ref()
        );
        assert_eq!(req["cwd"], "/w");
        assert_eq!(req["label"], "planner");
        assert_eq!(req["seq"], 1);
    }

    #[test]
    fn send_read_focus_round_trip() {
        let dir = temp_dir();
        let mut responses = BTreeMap::new();
        responses.insert("send", json!({"ok": true}));
        responses.insert("read", json!({"ok": true, "tail": "pane text"}));
        responses.insert("focus", json!({"ok": true}));
        responses.insert("close", json!({"ok": true}));
        let fake = FakeTtt::new(dir.clone(), responses);
        let mut b = bridge(&dir, &fake);
        b.send("terminal-1", "hello\n").expect("send");
        assert_eq!(b.read("terminal-1", 4000).expect("read"), "pane text");
        b.focus("terminal-1").expect("focus");
        b.close("terminal-1").expect("close");
        let reqs = fake.requests();
        assert_eq!(reqs[0]["op"], "send");
        assert_eq!(reqs[0]["text"], "hello\n");
        assert_eq!(reqs[1]["op"], "read");
        assert_eq!(reqs[1]["n"], 4000);
        assert_eq!(reqs[2]["op"], "focus");
        assert_eq!(reqs[3]["op"], "close");
    }

    #[test]
    fn a_failed_bridge_response_is_an_error() {
        let dir = temp_dir();
        let mut responses = BTreeMap::new();
        responses.insert(
            "spawn",
            json!({"ok": false, "error": "ttt.terminal unavailable"}),
        );
        let fake = FakeTtt::new(dir.clone(), responses);
        let err = bridge(&dir, &fake).spawn("a", &[], "/w").unwrap_err();
        assert_eq!(err, "ttt.terminal unavailable");
    }

    #[test]
    fn an_unreachable_exec_server_says_so() {
        let dir = temp_dir();
        let mut b = TttBridge::new("http://127.0.0.1:1/exec", dir.clone(), dir.join("status"));
        let err = b.spawn("a", &[], "/w").unwrap_err();
        assert!(err.contains("ttt exec unreachable"), "{err}");
    }

    #[test]
    fn await_ready_waits_for_the_status_file_and_a_drawn_tui() {
        let dir = temp_dir();
        let mut responses = BTreeMap::new();
        responses.insert("read", json!({"ok": true, "tail": "pi ready"}));
        let fake = FakeTtt::new(dir.clone(), responses);
        let mut b = TttBridge::new(&fake.url, dir.clone(), dir.join("status"));
        std::fs::create_dir_all(dir.join("status")).unwrap();
        let writer = {
            let path = dir.join("status/a.json");
            std::thread::spawn(move || {
                std::thread::sleep(Duration::from_millis(200));
                std::fs::write(path, r#"{"name":"a","state":"idle"}"#).unwrap();
            })
        };
        let t0 = Instant::now();
        b.await_ready("a", "terminal-1", Duration::from_secs(5))
            .expect("ready");
        // the status hook beats the TUI; readiness waits the grace AND a tail
        assert!(t0.elapsed() >= READY_GRACE, "grace not applied");
        writer.join().unwrap();
        assert_eq!(b.status("a").map(|r| r.state), Some("idle".to_string()));
    }

    #[test]
    fn two_bridges_never_share_a_request_file() {
        let dir = temp_dir();
        let mut responses = BTreeMap::new();
        responses.insert("focus", json!({"ok": true}));
        let fake = FakeTtt::new(dir.clone(), responses);
        let mut a = TttBridge::new(&fake.url, dir.clone(), dir.join("status"));
        let mut b = TttBridge::new(&fake.url, dir.clone(), dir.join("status"));
        a.focus("terminal-1").expect("a");
        b.focus("terminal-2").expect("b");
        let files: Vec<String> = std::fs::read_dir(&dir)
            .unwrap()
            .flatten()
            .map(|e| e.file_name().to_string_lossy().into_owned())
            .filter(|n| n.starts_with("agent-"))
            .collect();
        assert!(
            files.is_empty(),
            "request/response files are consumed: {files:?}"
        );
    }

    #[test]
    fn prompt_wait_gates_on_working_then_idle() {
        let dir = temp_dir();
        let mut responses = BTreeMap::new();
        responses.insert("send", json!({"ok": true}));
        responses.insert("read", json!({"ok": true, "tail": "before"}));
        let fake = FakeTtt::new(dir.clone(), responses);
        let b = bridge(&dir, &fake);
        let mut backend = TttBackend::new(b);
        backend.register("a", "terminal-1");
        // the "agent": working shortly after the prompt, idle later
        let status_dir = dir.join("status");
        std::fs::create_dir_all(&status_dir).unwrap();
        let agent = {
            let p = status_dir.join("a.json");
            std::thread::spawn(move || {
                std::thread::sleep(Duration::from_millis(150));
                std::fs::write(&p, r#"{"name":"a","state":"working"}"#).unwrap();
                std::thread::sleep(Duration::from_millis(400));
                std::fs::write(&p, r#"{"name":"a","state":"idle"}"#).unwrap();
            })
        };
        let t0 = Instant::now();
        let state = backend
            .prompt_wait("a", "do it", &["idle".into()], Duration::from_secs(5))
            .expect("settled");
        assert_eq!(state, "idle");
        assert!(
            t0.elapsed() >= Duration::from_millis(400),
            "must wait for idle"
        );
        agent.join().unwrap();
    }

    #[test]
    fn screen_returns_the_visible_rows() {
        let dir = temp_dir();
        let mut responses = BTreeMap::new();
        responses.insert("screen", json!({"ok": true, "text": "line one\nDONE\n"}));
        let fake = FakeTtt::new(dir.clone(), responses);
        let mut b = bridge(&dir, &fake);
        assert_eq!(b.screen("terminal-1").expect("screen"), "line one\nDONE\n");
        assert_eq!(fake.requests()[0]["op"], "screen");
    }

    #[test]
    fn read_strips_ansi_and_keeps_the_last_lines() {
        let dir = temp_dir();
        let mut responses = BTreeMap::new();
        responses.insert(
            "read",
            json!({"ok": true, "tail": "\u{1b}[31mred\u{1b}[0m\r\nline2\nline3\n"}),
        );
        let fake = FakeTtt::new(dir.clone(), responses);
        let mut backend = TttBackend::new(bridge(&dir, &fake));
        backend.register("a", "terminal-1");
        let text = backend.read("a", "recent", 2).expect("read");
        assert_eq!(text, "line2\nline3\n");
    }

    #[test]
    fn strip_ansi_handles_csi_osc_and_carriage_returns() {
        assert_eq!(strip_ansi("\u{1b}[31mred\u{1b}[0m"), "red");
        assert_eq!(strip_ansi("a\r\nb"), "a\nb");
        assert_eq!(strip_ansi("x\u{1b}]0;title\u{07}y"), "xy");
        assert_eq!(strip_ansi("plain"), "plain");
    }

    #[test]
    fn tail_lines_zero_returns_everything() {
        assert_eq!(tail_lines("a\nb\n", 0), "a\nb\n");
        assert_eq!(tail_lines("a\nb\nc\n", 2), "b\nc\n");
        assert_eq!(tail_lines("a\n", 5), "a\n");
    }

    #[test]
    fn spawn_shell_launches_a_review_program() {
        let dir = temp_dir();
        let mut responses = BTreeMap::new();
        responses.insert("spawn", json!({"ok": true, "id": "terminal-9"}));
        let fake = FakeTtt::new(dir.clone(), responses);
        let mut b = bridge(&dir, &fake);
        let id = b
            .spawn_shell(
                "lazygit",
                &["-p".to_string(), "/wt".to_string()],
                "/wt",
                "lg",
            )
            .expect("spawn lazygit");
        assert_eq!(id, "terminal-9");
        let req = &fake.requests()[0];
        assert_eq!(req["shell"], "lazygit");
        assert_eq!(req["args"], json!(["-p", "/wt"]));
        assert_eq!(req["label"], "lg");
        assert!(
            req["env"]
                .as_object()
                .map(|m| m.is_empty())
                .unwrap_or(false)
        );
    }

    #[test]
    fn the_bridge_only_flags_blocked_in_the_live_bridge() {
        let dir = temp_dir();
        let path = dir.join(".forseti/crew-status.json");
        std::fs::create_dir_all(path.parent().unwrap()).unwrap();
        std::fs::write(
            &path,
            r#"{"crew":"c","total":2,"done":0,"running":2,"blocked":0,"failed":0,"nodes":[{"name":"a","status":"running"},{"name":"b","status":"running"}]}"#,
        )
        .unwrap();
        let reporter = LiveReporter::new(
            std::sync::Arc::new(std::sync::Mutex::new(
                std::fs::File::create(dir.join("run.jsonl")).unwrap(),
            )),
            false,
            std::sync::Arc::new(std::sync::Mutex::new(Vec::new())),
            std::sync::Arc::new(std::sync::Mutex::new(())),
            &dir.to_string_lossy(),
            2,
        );
        let mut statuses = BTreeMap::new();
        statuses.insert("a".to_string(), "blocked".to_string());
        statuses.insert("b".to_string(), "idle".to_string());
        reporter.patch_bridge(&statuses);
        let after: Value = serde_json::from_str(&std::fs::read_to_string(&path).unwrap()).unwrap();
        assert_eq!(after["nodes"][0]["status"], "blocked");
        assert_eq!(
            after["nodes"][1]["status"], "running",
            "idle must not overwrite"
        );
        assert_eq!(after["blocked"], 1);
        assert_eq!(after["running"], 1);
    }

    #[test]
    fn the_backend_refuses_an_unregistered_node() {
        let dir = temp_dir();
        let fake = FakeTtt::new(dir.clone(), BTreeMap::new());
        let mut backend = TttBackend::new(bridge(&dir, &fake));
        let err = backend.read("ghost", "recent", 10).unwrap_err();
        assert!(err.contains("no terminal registered"), "{err}");
    }
}
