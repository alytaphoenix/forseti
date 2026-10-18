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

use std::collections::BTreeMap;
use std::path::{Path, PathBuf};
use std::time::{Duration, Instant};

use serde::{Deserialize, Serialize};
use serde_json::{Value, json};

use super::runner::PaneBackend;

pub const DEFAULT_EXEC_URL: &str = "http://127.0.0.1:4242/exec";
pub const BRIDGE_COMMAND: &str = "Forseti: Agent Bridge";
const RESPONSE_TIMEOUT: Duration = Duration::from_secs(10);
const START_TIMEOUT: Duration = Duration::from_secs(10);

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
    seq: u64,
    http: reqwest::blocking::Client,
}

impl TttBridge {
    pub fn new(exec_url: &str, plugin_dir: PathBuf, status_dir: PathBuf) -> Self {
        TttBridge {
            exec_url: exec_url.to_string(),
            plugin_dir,
            status_dir,
            seq: 0,
            http: reqwest::blocking::Client::builder()
                .timeout(Duration::from_secs(15))
                .build()
                .expect("ttt bridge HTTP client"),
        }
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
        let req_path = self.plugin_dir.join("agent-req.json");
        let res_path = self.plugin_dir.join("agent-res.json");
        std::fs::create_dir_all(&self.plugin_dir)
            .map_err(|e| format!("ttt plugin dir {}: {e}", self.plugin_dir.display()))?;
        // atomic request: the plugin never reads a half-written file
        let tmp = self.plugin_dir.join(format!(".agent-req.{seq}.tmp"));
        std::fs::write(&tmp, serde_json::to_vec(&req).map_err(|e| e.to_string())?)
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
        if !resp.status().is_success() {
            return Err(format!("ttt exec: HTTP {}", resp.status().as_u16()));
        }

        let deadline = Instant::now() + RESPONSE_TIMEOUT;
        loop {
            if let Ok(text) = std::fs::read_to_string(&res_path)
                && let Ok(v) = serde_json::from_str::<Value>(&text)
                && v.get("seq").and_then(Value::as_u64) == Some(seq)
            {
                if v.get("ok").and_then(Value::as_bool) != Some(true) {
                    return Err(v
                        .get("error")
                        .and_then(Value::as_str)
                        .unwrap_or("ttt bridge error")
                        .to_string());
                }
                return Ok(v);
            }
            if Instant::now() >= deadline {
                return Err(format!(
                    "ttt bridge: no response to seq {seq} — is the forseti plugin loaded (Plugins: Reload All)?"
                ));
            }
            std::thread::sleep(Duration::from_millis(20));
        }
    }

    /// Opens a pi tab for one node; `FORSETI_AGENT_NAME` turns on the status
    /// channel (S51).
    pub fn spawn(&mut self, name: &str, args: &[String], cwd: &str) -> Result<String, String> {
        let res = self.call(json!({
            "op": "spawn",
            "shell": "pi",
            "args": args,
            "env": {"FORSETI_AGENT_NAME": name},
            "cwd": cwd,
            "label": name,
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

    pub fn focus(&mut self, id: &str) -> Result<(), String> {
        self.call(json!({"op": "focus", "id": id}))?;
        Ok(())
    }

    /// Reads the pi status channel for `name` (None = no file yet).
    pub fn status(&self, name: &str) -> Option<StatusRecord> {
        let text = std::fs::read_to_string(self.status_dir.join(format!("{name}.json"))).ok()?;
        serde_json::from_str(&text).ok()
    }

    /// Waits until the agent has started (status `idle` or `working`).
    pub fn await_ready(&self, name: &str, timeout: Duration) -> Result<(), String> {
        let deadline = Instant::now() + timeout;
        loop {
            if let Some(rec) = self.status(name)
                && matches!(rec.state.as_str(), "idle" | "working")
            {
                return Ok(());
            }
            if Instant::now() >= deadline {
                return Err(format!(
                    "ttt agent {name:?} did not report a status within {}s (is pi running in the tab?)",
                    timeout.as_secs()
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
    fn wait_settled(&mut self, name: &str, timeout: Duration) -> Result<String, String> {
        let id = self.id(name)?;
        let baseline = self.bridge.read(&id, 0).unwrap_or_default();
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
        // pi's TUI submits on CR; a LF is not Enter (verified live)
        self.bridge.send(&id, &format!("{text}\r"))?;
        self.wait_settled(target, timeout)
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
                Some(']') => {
                    // OSC: terminated by BEL or ESC \
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
                        let req: Value = std::fs::read_to_string(dir.join("agent-req.json"))
                            .ok()
                            .and_then(|t| serde_json::from_str(&t).ok())
                            .unwrap_or(Value::Null);
                        if let Ok(mut v) = requests.lock() {
                            v.push(req.clone());
                        }
                        let op = req.get("op").and_then(Value::as_str).unwrap_or("");
                        let mut res = responses.get(op).cloned().unwrap_or_else(|| json!({"ok": false, "error": format!("no canned response for {op}")}));
                        res["seq"] = req.get("seq").cloned().unwrap_or(json!(0));
                        let _ = std::fs::write(
                            dir.join("agent-res.json"),
                            serde_json::to_vec(&res).unwrap(),
                        );
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
        let fake = FakeTtt::new(dir.clone(), responses);
        let mut b = bridge(&dir, &fake);
        b.send("terminal-1", "hello\n").expect("send");
        assert_eq!(b.read("terminal-1", 4000).expect("read"), "pane text");
        b.focus("terminal-1").expect("focus");
        let reqs = fake.requests();
        assert_eq!(reqs[0]["op"], "send");
        assert_eq!(reqs[0]["text"], "hello\n");
        assert_eq!(reqs[1]["op"], "read");
        assert_eq!(reqs[1]["n"], 4000);
        assert_eq!(reqs[2]["op"], "focus");
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
    fn await_ready_waits_for_the_status_file() {
        let dir = temp_dir();
        let fake = FakeTtt::new(dir.clone(), BTreeMap::new());
        let b = TttBridge::new(&fake.url, dir.clone(), dir.join("status"));
        std::fs::create_dir_all(dir.join("status")).unwrap();
        let writer = {
            let path = dir.join("status/a.json");
            std::thread::spawn(move || {
                std::thread::sleep(Duration::from_millis(200));
                std::fs::write(path, r#"{"name":"a","state":"idle"}"#).unwrap();
            })
        };
        b.await_ready("a", Duration::from_secs(5)).expect("ready");
        writer.join().unwrap();
        assert_eq!(b.status("a").map(|r| r.state), Some("idle".to_string()));
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
    fn the_backend_refuses_an_unregistered_node() {
        let dir = temp_dir();
        let fake = FakeTtt::new(dir.clone(), BTreeMap::new());
        let mut backend = TttBackend::new(bridge(&dir, &fake));
        let err = backend.read("ghost", "recent", 10).unwrap_err();
        assert!(err.contains("no terminal registered"), "{err}");
    }
}
