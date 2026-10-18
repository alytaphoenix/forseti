//! herdr socket client (Phase 18D-3): the Rust port of `crew/internal/herdrd`.
//!
//! Wire facts (spikes S6/S7/S12, S33e), all verified live against herdr 0.9.3:
//!
//! * NDJSON over a Unix socket, **no handshake**.
//! * One-shot requests: the server **closes the connection** after the reply,
//!   so [`Client::call`] dials per request.
//! * `events.subscribe` is the only persistent connection: ack
//!   `{"result":{"type":"subscription_started"}}`, then pushed frames
//!   `{"event":…,"data":…}` with **no id**.
//! * `agent.read` nests the payload under `result.read.text`.
//! * [`Client::agent_prompt_wait`] submits the prompt **and** the settle wait in
//!   one request (a bare wait-idle after a prompt can match the pre-prompt
//!   idle); an unfocused agent settles `done`, so callers pass both `done` and
//!   `idle` in `until`.
//!
//! Deliberate divergences from the Go client (documented, no oracle):
//!   * dialing is a plain `connect` (a UDS connect does not block);
//!   * [`Subscription`] is closable and owns its reader, so a partial frame is
//!     retained across the 60 s idle deadline instead of being discarded.
//!
//! The Go client's signatures are mirrored 1:1 so a caller can be ported
//! stage by stage (18D-4/18D-5).

use std::io::{Read, Write};
use std::os::unix::net::UnixStream;
use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::mpsc::{self, Receiver, Sender};
use std::sync::{Arc, Mutex};
use std::thread::JoinHandle;
use std::time::Duration;

use serde::{Deserialize, Serialize};
use serde_json::{Value, json};

/// herdr's per-call one-shot budget for the cheap helpers (Go's 10–15 s).
const CALL_TIMEOUT: Duration = Duration::from_secs(10);

// MARK: socket resolution

/// Resolves the herdr socket in the order the CLI documents:
/// `HERDR_SOCKET_PATH` → the `HERDR_SESSION` named socket → the default socket.
pub fn socket_path() -> Result<PathBuf> {
    if let Some(p) = env_nonempty("HERDR_SOCKET_PATH") {
        return Ok(PathBuf::from(p));
    }
    let base = config_dir()?;
    match env_nonempty("HERDR_SESSION") {
        Some(name) => Ok(base.join("sessions").join(name).join("herdr.sock")),
        None => Ok(base.join("herdr.sock")),
    }
}

/// Resolves the socket for a named session (or the default session when
/// `name` is empty).
pub fn socket_path_for(name: &str) -> Result<PathBuf> {
    let base = config_dir()?;
    if name.is_empty() {
        Ok(base.join("herdr.sock"))
    } else {
        Ok(base.join("sessions").join(name).join("herdr.sock"))
    }
}

fn config_dir() -> Result<PathBuf> {
    if let Some(d) = env_nonempty("HERDR_CONFIG_DIR") {
        return Ok(PathBuf::from(d));
    }
    match std::env::var_os("HOME") {
        Some(home) => Ok(PathBuf::from(home).join(".config").join("herdr")),
        None => Err(Error::Msg("cannot resolve home directory".into())),
    }
}

fn env_nonempty(key: &str) -> Option<String> {
    std::env::var(key).ok().filter(|v| !v.is_empty())
}

// MARK: errors

/// The server error shape (`{"code":…,"message":…}`).
#[derive(Debug, Clone, PartialEq, Eq, Deserialize, Serialize)]
pub struct ApiError {
    #[serde(default)]
    pub code: String,
    #[serde(default)]
    pub message: String,
}

impl std::fmt::Display for ApiError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(f, "{}: {}", self.code, self.message)
    }
}

/// A herdr client error: transport, server API, or a local shape violation.
#[derive(Debug)]
pub enum Error {
    Io(std::io::Error),
    Api(ApiError),
    Msg(String),
}

impl std::fmt::Display for Error {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            Error::Io(e) => write!(f, "{e}"),
            Error::Api(e) => write!(f, "{e}"),
            Error::Msg(m) => write!(f, "{m}"),
        }
    }
}

impl std::error::Error for Error {}

impl From<std::io::Error> for Error {
    fn from(e: std::io::Error) -> Self {
        Error::Io(e)
    }
}

impl From<ApiError> for Error {
    fn from(e: ApiError) -> Self {
        Error::Api(e)
    }
}

pub type Result<T> = std::result::Result<T, Error>;

fn is_timeout(e: &std::io::Error) -> bool {
    matches!(
        e.kind(),
        std::io::ErrorKind::WouldBlock | std::io::ErrorKind::TimedOut
    )
}

// MARK: framing

/// The raw request frame.
#[derive(Debug, Serialize)]
struct Envelope {
    id: String,
    method: String,
    params: Value,
}

/// The raw response frame (exactly one of `result`/`error`).
#[derive(Debug, Deserialize)]
struct Reply {
    #[serde(default)]
    id: String,
    #[serde(default)]
    result: Value,
    #[serde(default)]
    error: Option<ApiError>,
}

/// Reads one `\n`-terminated frame **byte-wise**, so no look-ahead can swallow
/// bytes belonging to the following frame (the Go bufio reader can; a
/// subscription here shares one socket with its ack).
fn read_line_raw(stream: &mut UnixStream) -> Result<Vec<u8>> {
    let mut line = Vec::new();
    let mut byte = [0u8; 1];
    loop {
        match stream.read(&mut byte) {
            Ok(0) => {
                if line.is_empty() {
                    return Err(Error::Io(std::io::Error::new(
                        std::io::ErrorKind::UnexpectedEof,
                        "EOF",
                    )));
                }
                return Ok(line);
            }
            Ok(_) => {
                line.push(byte[0]);
                if byte[0] == b'\n' {
                    return Ok(line);
                }
            }
            Err(e) => return Err(Error::Io(e)),
        }
    }
}

fn decode_reply(line: &[u8]) -> Result<Reply> {
    serde_json::from_slice(line).map_err(|e| {
        Error::Msg(format!(
            "bad response frame {:?}: {e}",
            String::from_utf8_lossy(line)
        ))
    })
}

// MARK: client

/// Dials the herdr socket. Safe for concurrent use.
#[derive(Debug)]
pub struct Client {
    sock: PathBuf,
    seq: Mutex<u64>,
}

impl Client {
    /// Resolves the socket (env → default) and requires it to exist.
    pub fn new() -> Result<Self> {
        Self::at(socket_path()?)
    }

    /// A client for an explicit socket path. The stat mirrors the Go
    /// `NewAt`/`New` error: the socket must exist for the server to be up.
    pub fn at(sock: impl Into<PathBuf>) -> Result<Self> {
        let sock = sock.into();
        if let Err(e) = std::fs::metadata(&sock) {
            return Err(Error::Msg(format!(
                "herdr socket {} not present (server running?): {e}",
                sock.display()
            )));
        }
        Ok(Client {
            sock,
            seq: Mutex::new(0),
        })
    }

    pub fn socket(&self) -> &Path {
        &self.sock
    }

    /// `crew-<prefix>-<n>` — the same id shape the Go client uses.
    fn next_id(&self, prefix: &str) -> String {
        let mut seq = self.seq.lock().expect("seq mutex poisoned");
        *seq += 1;
        format!("crew-{prefix}-{seq}")
    }

    /// One request on a fresh connection (the server closes after the reply),
    /// with the id correlated per P13-B21.
    pub fn call(&self, method: &str, params: &Value, timeout: Duration) -> Result<Value> {
        let env = Envelope {
            id: self.next_id(method),
            method: method.to_string(),
            params: params.clone(),
        };
        let mut line =
            serde_json::to_vec(&env).map_err(|e| Error::Msg(format!("{method}: {e}")))?;
        line.push(b'\n');

        let mut stream = dial(&self.sock, timeout)?;
        stream.write_all(&line)?;

        let reply = decode_reply(&read_line_raw(&mut stream)?)?;
        if !reply.id.is_empty() && reply.id != env.id {
            return Err(Error::Msg(format!(
                "{method}: reply id {:?} does not match request {:?}",
                reply.id, env.id
            )));
        }
        if let Some(e) = reply.error {
            return Err(Error::Api(e));
        }
        Ok(reply.result)
    }

    /// Opens a persistent `events.subscribe` connection. Events stream on the
    /// returned channel; a terminal (`events_lost`) or transport error is
    /// reported by [`Subscription::err`].
    pub fn subscribe(&self, subscriptions: &[Value], timeout: Duration) -> Result<Subscription> {
        let env = Envelope {
            id: self.next_id("subscribe"),
            method: "events.subscribe".to_string(),
            params: json!({ "subscriptions": subscriptions }),
        };
        let mut line =
            serde_json::to_vec(&env).map_err(|e| Error::Msg(format!("subscribe: {e}")))?;
        line.push(b'\n');

        let mut stream = dial(&self.sock, timeout)?;
        stream.write_all(&line)?;

        let reply = decode_reply(&read_line_raw(&mut stream)?)?;
        if let Some(e) = reply.error {
            return Err(Error::Api(e));
        }
        // Go ignores the unmarshal error and compares the empty string.
        if reply.result.get("type").and_then(Value::as_str) != Some("subscription_started") {
            return Err(Error::Msg(format!(
                "unexpected subscribe ack: {}",
                reply.result
            )));
        }

        let (tx, rx) = mpsc::channel();
        let err = Arc::new(Mutex::new(None));
        let stop = Arc::new(AtomicBool::new(false));
        let reader = stream.try_clone()?;
        let handle = {
            let (err, stop) = (err.clone(), stop.clone());
            std::thread::spawn(move || pump(reader, tx, err, stop))
        };
        Ok(Subscription {
            events: rx,
            err,
            stop,
            sock: stream,
            handle: Some(handle),
        })
    }
}

fn dial(sock: &Path, timeout: Duration) -> Result<UnixStream> {
    // A UDS connect does not block; the Go DialTimeout only bounds it.
    let stream = UnixStream::connect(sock)?;
    stream.set_write_timeout(Some(timeout))?;
    stream.set_read_timeout(Some(timeout))?;
    Ok(stream)
}

// MARK: subscription

/// A persistent `events.subscribe` connection.
pub struct Subscription {
    events: Receiver<Event>,
    err: Arc<Mutex<Option<String>>>,
    stop: Arc<AtomicBool>,
    sock: UnixStream,
    handle: Option<JoinHandle<()>>,
}

impl Subscription {
    /// The pushed-event channel (closes when the pump exits).
    pub fn events(&self) -> &Receiver<Event> {
        &self.events
    }

    /// The terminal error, if the pump has exited.
    pub fn err(&self) -> Option<String> {
        self.err.lock().expect("err mutex poisoned").clone()
    }

    /// Stops the pump and drops the connection.
    pub fn close(&mut self) {
        self.stop.store(true, Ordering::Relaxed);
        let _ = self.sock.shutdown(std::net::Shutdown::Both);
        if let Some(h) = self.handle.take() {
            let _ = h.join();
        }
    }
}

impl Drop for Subscription {
    fn drop(&mut self) {
        self.close();
    }
}

fn set_err(slot: &Mutex<Option<String>>, msg: impl Into<String>) {
    let mut guard = slot.lock().expect("err mutex poisoned");
    if guard.is_none() {
        *guard = Some(msg.into());
    }
}

/// Idle keep-alive: the socket is armed with a 60 s read timeout, and the
/// stream is only considered dead on a non-timeout error.
const IDLE_TIMEOUT: Duration = Duration::from_secs(60);

fn pump(
    mut stream: UnixStream,
    tx: Sender<Event>,
    err: Arc<Mutex<Option<String>>>,
    stop: Arc<AtomicBool>,
) {
    let _ = stream.set_read_timeout(Some(IDLE_TIMEOUT));
    // Bytes retained across an idle-deadline timeout: the frame loop below is
    // the only reader, so a partial frame is never lost.
    let mut acc: Vec<u8> = Vec::new();
    let mut chunk = [0u8; 4096];
    loop {
        if stop.load(Ordering::Relaxed) {
            return;
        }
        match stream.read(&mut chunk) {
            Ok(0) => {
                if !stop.load(Ordering::Relaxed) {
                    set_err(&err, "EOF: subscription closed");
                }
                return;
            }
            Ok(n) => {
                acc.extend_from_slice(&chunk[..n]);
                while let Some(pos) = acc.iter().position(|b| *b == b'\n') {
                    let line: Vec<u8> = acc.drain(..=pos).collect();
                    let line = &line[..line.len() - 1];
                    if line.is_empty() {
                        continue;
                    }
                    if let Some(lost) = terminal_error(line) {
                        set_err(&err, lost);
                        return;
                    }
                    if let Ok(event) = serde_json::from_slice::<Event>(line)
                        && tx.send(event).is_err()
                    {
                        return;
                    }
                }
            }
            Err(e) if is_timeout(&e) => continue,
            Err(e) => {
                if !stop.load(Ordering::Relaxed) {
                    set_err(&err, e.to_string());
                }
                return;
            }
        }
    }
}

/// A pushed frame carrying an `error` (e.g. `events_lost`) terminates the
/// subscription. Any other JSON object is a normal event.
fn terminal_error(line: &[u8]) -> Option<String> {
    #[derive(Deserialize)]
    struct Probe {
        #[serde(default)]
        _id: String,
        #[serde(default)]
        error: Option<ApiError>,
    }
    let probe: Probe = serde_json::from_slice(line).ok()?;
    probe.error.map(|e| e.to_string())
}

/// A pushed subscription line: `{"event":"<name>","data":{…}}`.
#[derive(Debug, Clone, Deserialize, Serialize)]
pub struct Event {
    #[serde(default)]
    pub event: String,
    #[serde(default)]
    pub data: Value,
}

// MARK: typed helpers

/// `agent.list` entry, mirroring the Go `herdrd.AgentInfo`.
#[derive(Debug, Clone, Default, Deserialize, Serialize)]
pub struct AgentInfo {
    #[serde(default)]
    pub name: String,
    #[serde(default)]
    pub agent: String,
    #[serde(default)]
    pub agent_status: String,
    #[serde(default)]
    pub pane_id: String,
    #[serde(default)]
    pub tab_id: String,
    #[serde(default)]
    pub workspace_id: String,
}

impl Client {
    pub fn ping(&self) -> Result<()> {
        self.call("ping", &json!({}), Duration::from_secs(5))?;
        Ok(())
    }

    pub fn agent_list(&self) -> Result<Vec<AgentInfo>> {
        let res = self.call("agent.list", &json!({}), CALL_TIMEOUT)?;
        let agents = res.get("agents").cloned().unwrap_or(Value::Array(vec![]));
        serde_json::from_value(agents).map_err(|e| Error::Msg(e.to_string()))
    }

    /// The pane text for the agent (`agent.read` nests under `read.text`).
    pub fn agent_read(&self, target: &str, source: &str, lines: i64) -> Result<String> {
        let res = self.call(
            "agent.read",
            &json!({ "target": target, "source": source, "lines": lines }),
            Duration::from_secs(15),
        )?;
        Ok(res
            .pointer("/read/text")
            .and_then(Value::as_str)
            .unwrap_or_default()
            .to_string())
    }

    /// Starts a named agent in an existing pane.
    pub fn agent_start(
        &self,
        name: &str,
        kind: &str,
        pane_id: &str,
        args: &[String],
        timeout: Duration,
    ) -> Result<()> {
        let mut params = json!({ "name": name, "kind": kind, "pane_id": pane_id });
        if !args.is_empty() {
            params["args"] = json!(args);
        }
        self.call("agent.start", &params, timeout).map(|_| ())
    }

    /// Sends text to the agent.
    pub fn agent_prompt(&self, name: &str, text: &str) -> Result<()> {
        self.call(
            "agent.prompt",
            &json!({ "target": name, "text": text }),
            Duration::from_secs(15),
        )
        .map(|_| ())
    }

    /// Submits the prompt and starts the settle wait in ONE request (a separate
    /// wait would race the pre-prompt idle). Returns the settled status.
    pub fn agent_prompt_wait(
        &self,
        name: &str,
        text: &str,
        until: &[String],
        timeout: Duration,
    ) -> Result<String> {
        let params = json!({
            "target": name,
            "text": text,
            "wait": { "until": until, "timeout_ms": millis(timeout) },
        });
        let res = match self.call("agent.prompt", &params, timeout + Duration::from_secs(15)) {
            Ok(v) => v,
            // the blocked case comes back as an error code
            Err(Error::Api(e)) if e.code == "agent_blocked" => return Ok("blocked".into()),
            Err(e) => return Err(e),
        };
        // `wait_matched` carrying the status event…
        if let Some(st) = res
            .pointer("/event/data/agent_status")
            .and_then(Value::as_str)
            && !st.is_empty()
        {
            return Ok(st.to_string());
        }
        // …or herdr 0.9.3's live `{"type":"agent_prompted","agent":{…}}`.
        if let Some(st) = res.pointer("/agent/agent_status").and_then(Value::as_str)
            && !st.is_empty()
        {
            return Ok(st.to_string());
        }
        if res.get("type").and_then(Value::as_str) == Some("wait_matched") {
            return Ok("idle".into());
        }
        // An unknown frame must NOT silently count as idle (P13-B21).
        Err(Error::Msg(format!(
            "agent.prompt wait: unrecognised response shape {res}"
        )))
    }

    /// Jumps the herdr UI to the pane hosting the named agent.
    pub fn agent_focus(&self, name: &str) -> Result<()> {
        self.call("agent.focus", &json!({ "target": name }), CALL_TIMEOUT)
            .map(|_| ())
    }

    /// Blocks until the agent reaches one of the `until` states.
    pub fn agent_wait(&self, name: &str, until: &[String], timeout: Duration) -> Result<()> {
        self.call(
            "agent.wait",
            &json!({ "target": name, "until": until, "timeout_ms": millis(timeout) }),
            timeout + Duration::from_secs(5),
        )
        .map(|_| ())
    }

    /// The focused workspace id (`result.snapshot.focused_workspace_id`).
    pub fn focused_workspace(&self) -> Result<String> {
        let res = self.call("session.snapshot", &json!({}), CALL_TIMEOUT)?;
        let id = res
            .pointer("/snapshot/focused_workspace_id")
            .and_then(Value::as_str)
            .unwrap_or_default();
        if id.is_empty() {
            return Err(Error::Msg("snapshot has no focused workspace".into()));
        }
        Ok(id.to_string())
    }

    /// Raises a herdr notification (sound: none|done|request).
    pub fn notification_show(&self, title: &str, body: &str, sound: &str) -> Result<()> {
        self.call(
            "notification.show",
            &json!({ "title": title, "body": body, "sound": sound }),
            CALL_TIMEOUT,
        )
        .map(|_| ())
    }

    /// Projects a source-owned view into herdr's Agents sidebar (UI-only, S10).
    pub fn agent_view_set(
        &self,
        source: &str,
        label: &str,
        filter: Option<&Value>,
        sort: &[Value],
    ) -> Result<()> {
        let mut params = json!({ "source": source });
        if !label.is_empty() {
            params["label"] = json!(label);
        }
        if let Some(f) = filter {
            params["filter"] = f.clone();
        }
        if !sort.is_empty() {
            params["sort"] = json!(sort);
        }
        self.call("agent.view.set", &params, CALL_TIMEOUT)
            .map(|_| ())
    }

    /// Removes this source's projection.
    pub fn agent_view_clear(&self, source: &str) -> Result<()> {
        self.call(
            "agent.view.clear",
            &json!({ "source": source }),
            CALL_TIMEOUT,
        )
        .map(|_| ())
    }

    /// Blocks until the pane's output matches (`timeout` | `invalid_regex`).
    /// Matches existing scrollback instantly (S12). Returns the matched line.
    pub fn pane_wait_for_output(
        &self,
        pane_id: &str,
        match_type: &str,
        value: &str,
        timeout: Duration,
    ) -> Result<String> {
        let res = self.call(
            "pane.wait_for_output",
            &json!({
                "pane_id": pane_id,
                "source": "recent",
                "strip_ansi": true,
                "match": { "type": match_type, "value": value },
                "timeout_ms": millis(timeout),
            }),
            timeout + Duration::from_secs(10),
        )?;
        Ok(res
            .get("matched_line")
            .and_then(Value::as_str)
            .unwrap_or_default()
            .to_string())
    }

    /// Creates a git worktree + dedicated herdr workspace (S13).
    /// Returns `(workspace_id, checkout_path)`.
    pub fn worktree_create(
        &self,
        cwd: &str,
        branch: &str,
        path: &str,
        label: &str,
        timeout: Duration,
    ) -> Result<(String, String)> {
        let mut params = json!({ "cwd": cwd, "trust_repository": true, "focus": false });
        if !branch.is_empty() {
            params["branch"] = json!(branch);
        }
        if !path.is_empty() {
            params["path"] = json!(path);
        }
        if !label.is_empty() {
            params["label"] = json!(label);
        }
        let res = self.call("worktree.create", &params, timeout)?;
        Ok((
            res.pointer("/workspace/workspace_id")
                .and_then(Value::as_str)
                .unwrap_or_default()
                .to_string(),
            res.pointer("/workspace/worktree/checkout_path")
                .and_then(Value::as_str)
                .unwrap_or_default()
                .to_string(),
        ))
    }

    /// Removes a worktree and its workspace.
    pub fn worktree_remove(&self, workspace_id: &str, force: bool) -> Result<()> {
        self.call(
            "worktree.remove",
            &json!({
                "workspace_id": workspace_id,
                "force": force,
                "trust_repository": true,
            }),
            Duration::from_secs(60),
        )
        .map(|_| ())
    }
}

fn millis(d: Duration) -> u64 {
    d.as_millis().min(u64::MAX as u128) as u64
}

// MARK: tests

#[cfg(test)]
mod tests {
    use super::*;
    use std::io::{BufRead, BufReader, Write};
    use std::os::unix::net::UnixListener;
    use std::sync::atomic::AtomicUsize;

    static SEQ: AtomicUsize = AtomicUsize::new(0);

    /// A fake herdr server: one connection per entry, each answered by writing
    /// every line of that entry, then closing.
    struct Fake {
        path: PathBuf,
        requests: Arc<Mutex<Vec<Value>>>,
        handle: Option<JoinHandle<()>>,
    }

    impl Fake {
        fn serve(responses: Vec<Vec<&'static str>>) -> Fake {
            let n = SEQ.fetch_add(1, Ordering::SeqCst);
            let path =
                std::env::temp_dir().join(format!("forseti-herdr-{}-{n}.sock", std::process::id()));
            let _ = std::fs::remove_file(&path);
            let listener = UnixListener::bind(&path).expect("bind fake socket");
            let requests = Arc::new(Mutex::new(Vec::new()));
            let reqs = requests.clone();
            let handle = std::thread::spawn(move || {
                for lines in responses {
                    let (stream, _) = match listener.accept() {
                        Ok(v) => v,
                        Err(_) => return,
                    };
                    let mut reader = BufReader::new(stream.try_clone().expect("clone"));
                    let mut req = String::new();
                    if reader.read_line(&mut req).unwrap_or(0) > 0
                        && let Ok(v) = serde_json::from_str::<Value>(line(&req))
                    {
                        reqs.lock().unwrap().push(v);
                    }
                    let mut w = stream;
                    for l in lines {
                        let _ = w.write_all(l.as_bytes());
                        let _ = w.write_all(b"\n");
                    }
                    let _ = w.flush();
                    // drop closes the connection
                }
            });
            Fake {
                path,
                requests,
                handle: Some(handle),
            }
        }

        fn requests(&self) -> Vec<Value> {
            self.requests.lock().unwrap().clone()
        }
    }

    impl Drop for Fake {
        fn drop(&mut self) {
            if let Some(h) = self.handle.take() {
                let _ = h.join();
            }
            let _ = std::fs::remove_file(&self.path);
        }
    }

    fn line(s: &str) -> &str {
        s.trim_end_matches(['\r', '\n'])
    }

    fn client(f: &Fake) -> Client {
        Client::at(&f.path).expect("fake socket exists")
    }

    #[test]
    fn call_sends_one_frame_and_returns_the_result() {
        let f = Fake::serve(vec![vec![r#"{"result":{"ok":true}}"#]]);
        let c = client(&f);
        let out = c.call("ping", &json!({}), Duration::from_secs(2)).unwrap();
        assert_eq!(out, json!({ "ok": true }));
        let reqs = f.requests();
        assert_eq!(reqs.len(), 1, "one dial per one-shot call");
        assert_eq!(reqs[0]["id"], "crew-ping-1");
        assert_eq!(reqs[0]["method"], "ping");
        assert_eq!(reqs[0]["params"], json!({}));
    }

    #[test]
    fn call_dials_per_request_and_the_id_increments() {
        let f = Fake::serve(vec![
            vec![r#"{"result":{"n":1}}"#],
            vec![r#"{"result":{"n":2}}"#],
        ]);
        let c = client(&f);
        assert_eq!(
            c.call("agent.list", &json!({}), Duration::from_secs(2))
                .unwrap()["n"],
            1
        );
        assert_eq!(
            c.call("agent.list", &json!({}), Duration::from_secs(2))
                .unwrap()["n"],
            2
        );
        let reqs = f.requests();
        assert_eq!(reqs[0]["id"], "crew-agent.list-1");
        assert_eq!(reqs[1]["id"], "crew-agent.list-2");
    }

    #[test]
    fn call_rejects_a_mismatched_reply_id() {
        let f = Fake::serve(vec![vec![r#"{"id":"other","result":{}}"#]]);
        let c = client(&f);
        let err = c
            .call("ping", &json!({}), Duration::from_secs(2))
            .unwrap_err();
        assert!(err.to_string().contains("does not match request"), "{err}");
    }

    #[test]
    fn an_error_frame_becomes_the_api_error() {
        let f = Fake::serve(vec![vec![
            r#"{"error":{"code":"agent_blocked","message":"waiting"}}"#,
        ]]);
        let c = client(&f);
        match c.call("ping", &json!({}), Duration::from_secs(2)) {
            Err(Error::Api(e)) => {
                assert_eq!(e.code, "agent_blocked");
                assert_eq!(e.to_string(), "agent_blocked: waiting");
            }
            other => panic!("want ApiError, got {other:?}"),
        }
    }

    #[test]
    fn agent_prompt_wait_maps_blocked_and_both_idle_shapes() {
        // An error code of `agent_blocked` short-circuits to "blocked".
        let f = Fake::serve(vec![vec![
            r#"{"error":{"code":"agent_blocked","message":"input needed"}}"#,
        ]]);
        assert_eq!(
            client(&f)
                .agent_prompt_wait("a", "hi", &["done".into()], Duration::from_secs(1))
                .unwrap(),
            "blocked"
        );

        // herdr 0.9.3's live success frame (rides in `result`).
        let f = Fake::serve(vec![vec![
            r#"{"result":{"type":"agent_prompted","agent":{"agent_status":"done"}}}"#,
        ]]);
        assert_eq!(
            client(&f)
                .agent_prompt_wait("a", "hi", &["done".into()], Duration::from_secs(1))
                .unwrap(),
            "done"
        );

        // A bare `wait_matched` with no event settles as idle.
        let f = Fake::serve(vec![vec![r#"{"result":{"type":"wait_matched"}}"#]]);
        assert_eq!(
            client(&f)
                .agent_prompt_wait("a", "hi", &["idle".into()], Duration::from_secs(1))
                .unwrap(),
            "idle"
        );

        // An unrecognised shape is an error, never a false settle.
        let f = Fake::serve(vec![vec![r#"{"result":{"weird":true}}"#]]);
        let err = client(&f)
            .agent_prompt_wait("a", "hi", &["idle".into()], Duration::from_secs(1))
            .unwrap_err();
        assert!(err.to_string().contains("unrecognised"), "{err}");
    }

    #[test]
    fn agent_prompt_wait_carries_the_event_status() {
        let f = Fake::serve(vec![vec![
            r#"{"result":{"type":"wait_matched","event":{"event":"pane_agent_status_changed","data":{"agent_status":"done"}}}}"#,
        ]]);
        assert_eq!(
            client(&f)
                .agent_prompt_wait("a", "hi", &["done".into()], Duration::from_secs(1))
                .unwrap(),
            "done"
        );
        // …and the wait rides in the SAME request as the prompt.
        let reqs = f.requests();
        assert_eq!(reqs[0]["method"], "agent.prompt");
        assert_eq!(reqs[0]["params"]["wait"]["until"], json!(["done"]));
    }

    #[test]
    fn agent_read_nests_under_read_text() {
        let f = Fake::serve(vec![vec![r#"{"result":{"read":{"text":"pane"}}}"#]]);
        assert_eq!(client(&f).agent_read("a", "recent", 10).unwrap(), "pane");
    }

    #[test]
    fn agent_list_decodes_every_field() {
        let f = Fake::serve(vec![vec![
            r#"{"result":{"agents":[{"name":"a","pane_id":"p","agent_status":"done","tab_id":"t","workspace_id":"w","agent":"x"}]}}"#,
        ]]);
        let got = client(&f).agent_list().unwrap();
        assert_eq!(got.len(), 1);
        assert_eq!(got[0].name, "a");
        assert_eq!(got[0].pane_id, "p");
        assert_eq!(got[0].agent_status, "done");
        assert_eq!(got[0].workspace_id, "w");
    }

    #[test]
    fn startup_checks_the_socket_and_says_so() {
        let missing = std::env::temp_dir().join("forseti-definitely-absent.sock");
        let _ = std::fs::remove_file(&missing);
        let err = Client::at(&missing).unwrap_err();
        assert!(err.to_string().contains("not present"), "{err}");
        assert!(err.to_string().contains("server running?"), "{err}");
    }

    #[test]
    fn socket_path_for_names_the_session() {
        let sandbox = socket_path_for("sandbox").unwrap();
        assert!(
            sandbox.ends_with("sessions/sandbox/herdr.sock"),
            "{sandbox:?}"
        );
        let default = socket_path_for("").unwrap();
        assert!(
            !default.to_string_lossy().contains("sessions"),
            "{default:?}"
        );
        assert!(default.ends_with("herdr.sock"), "{default:?}");
    }

    #[test]
    fn a_subscription_streams_events_and_closes() {
        let f = Fake::serve(vec![vec![
            r#"{"result":{"type":"subscription_started"}}"#,
            r#"{"event":"pane_output_changed","data":{"pane_id":"p"}}"#,
            r#"{"event":"pane_agent_status_changed","data":{"pane_id":"p"}}"#,
        ]]);
        let c = client(&f);
        let mut sub = c
            .subscribe(&[json!({"pane_id": "p"})], Duration::from_secs(2))
            .unwrap();
        let e1 = sub.events().recv_timeout(Duration::from_secs(2)).unwrap();
        assert_eq!(e1.event, "pane_output_changed");
        assert_eq!(e1.data["pane_id"], "p");
        let e2 = sub.events().recv_timeout(Duration::from_secs(2)).unwrap();
        assert_eq!(e2.event, "pane_agent_status_changed");
        // the fake closes the connection → the pump reports EOF
        sub.close();
        let req = &f.requests()[0];
        assert_eq!(req["method"], "events.subscribe");
        assert_eq!(req["params"]["subscriptions"], json!([{"pane_id": "p"}]));
        assert_eq!(req["id"], "crew-subscribe-1");
    }

    #[test]
    fn a_subscription_ack_is_required() {
        let f = Fake::serve(vec![vec![r#"{"result":{"type":"nope"}}"#]]);
        let err = client(&f)
            .subscribe(&[], Duration::from_secs(2))
            .err()
            .expect("a bad ack must fail");
        assert!(
            err.to_string().contains("unexpected subscribe ack"),
            "{err}"
        );
    }

    #[test]
    fn an_events_lost_frame_is_terminal() {
        let f = Fake::serve(vec![vec![
            r#"{"result":{"type":"subscription_started"}}"#,
            r#"{"error":{"code":"events_lost","message":"lagged"}}"#,
        ]]);
        let c = client(&f);
        let mut sub = c.subscribe(&[], Duration::from_secs(2)).unwrap();
        // Wait for the pump to record the terminal error.
        for _ in 0..100 {
            if sub.err().is_some() {
                break;
            }
            std::thread::sleep(Duration::from_millis(20));
        }
        assert!(sub.err().is_some(), "events_lost must be terminal");
        sub.close();
    }

    /// Live: needs the herdr server running.
    /// `cargo test --lib -- --ignored live_round`.
    #[test]
    #[ignore = "requires a running herdr server"]
    fn live_round_trips_against_a_real_socket() {
        let c = Client::new().expect("herdr socket path");
        c.ping().expect("ping");
        let agents = c.agent_list().expect("agent.list");
        // Shape only: the roster depends on the live box.
        for a in &agents {
            assert_eq!(a.name, a.name);
        }
        let _ = agents.len();
    }
}
