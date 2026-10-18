//! The per-run switchyard router (Phase 18D-5c-2b): the Rust port of
//! `crew/internal/switchyard/proxy.go`.
//!
//! A crew with `routes:` needs a routing process for the duration of the run.
//! The default router is **this binary** (`forseti-host`), which embeds
//! switchyard as a library; the upstream `switchyard-server` remains the
//! fallback (`FORSETI_CREW_ROUTER=switchyard-server`).
//!
//! What the proxy does, in order:
//!
//! 1. resolve the router (`FORSETI_CREW_ROUTER_BIN` → `host/target/{release,debug}`
//!    → `$PATH` → legacy server), S28.
//! 2. read pi's provider registry (`~/.pi/agent/models.json`) — that is where a
//!    crew's `provider/model` strings resolve (S15).
//! 3. render the routes TOML. **API keys never enter the file**: each
//!    referenced provider's key is resolved once and passed to the router as
//!    `SWITCHYARD_KEY_<PROVIDER>`.
//! 4. dry-run the config with the router's own `--dry-run` so a bad route fails
//!    with the router's message (S28), spawn it on a free loopback port with
//!    stdout/stderr → `router.log`, and wait for `GET /health`.
//! 5. write a `switchyard` provider into pi's models.json (one model id per
//!    route) so `--model switchyard/<route>` works, and return a restore
//!    closure. Restore REMOVES the key (never a byte restore — P13-B9: a crash
//!    mid-run must not leave a dead provider, and a byte restore would clobber
//!    a models.json edit made during the run). A stale entry from a crashed run
//!    is detected by probing its port and swept.
//! 6. tail the routing NDJSON into `route_decision` events (S30 carries route
//!    id/algorithm/tier/session/origin + the token split).

use std::collections::BTreeMap;
use std::io::{Seek, SeekFrom};
use std::path::{Path, PathBuf};
use std::process::{Child, Command, Stdio};
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Mutex};
use std::thread::JoinHandle;
use std::time::{Duration, Instant};

use serde::Deserialize;
use serde_json::{Map, Value, json};

use super::model::Route;
use super::scheduler::Event;

const HEALTH_TIMEOUT: Duration = Duration::from_secs(15);
const HTTP_TIMEOUT: Duration = Duration::from_secs(2);

// MARK: router resolution

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum RouterKind {
    /// Our own host (`forseti-host`), which embeds switchyard.
    Host,
    /// The upstream `switchyard-server` binary.
    Server,
}

impl RouterKind {
    pub fn as_str(self) -> &'static str {
        match self {
            RouterKind::Host => "forseti-host",
            RouterKind::Server => "switchyard-server",
        }
    }
}

#[derive(Debug, Clone)]
pub struct Router {
    pub bin: PathBuf,
    pub kind: RouterKind,
}

impl Router {
    pub fn resolve() -> Result<Router, String> {
        let forced = std::env::var("FORSETI_CREW_ROUTER").unwrap_or_default();
        if let Ok(p) = std::env::var("FORSETI_CREW_ROUTER_BIN")
            && !p.is_empty()
        {
            if !Path::new(&p).exists() {
                return Err(format!("FORSETI_CREW_ROUTER_BIN={p}: not found"));
            }
            let kind = kind_of(&forced, &p);
            return Ok(Router {
                bin: PathBuf::from(p),
                kind,
            });
        }
        if forced != RouterKind::Server.as_str() {
            for p in candidate_host_bins() {
                if p.exists() {
                    return Ok(Router {
                        bin: p,
                        kind: RouterKind::Host,
                    });
                }
            }
            if let Some(p) = look_path(RouterKind::Host.as_str()) {
                return Ok(Router {
                    bin: p,
                    kind: RouterKind::Host,
                });
            }
        }
        match resolve_switchyard_server() {
            Some(bin) => Ok(Router {
                bin,
                kind: RouterKind::Server,
            }),
            None => Err(
                "no router found — build ours (cargo build --release in host/) or cargo install switchyard-server, or set FORSETI_CREW_ROUTER_BIN"
                    .to_string(),
            ),
        }
    }

    /// The router's command line (the host takes `--addr`, the server
    /// `--host/--port`).
    pub fn args(&self, config: &Path, log: &Path, port: u16) -> Vec<String> {
        let config = config.to_string_lossy().into_owned();
        let log = log.to_string_lossy().into_owned();
        match self.kind {
            RouterKind::Host => vec![
                "--config".into(),
                config,
                "--addr".into(),
                format!("127.0.0.1:{port}"),
                "--routing-log".into(),
                log,
            ],
            RouterKind::Server => vec![
                "--config".into(),
                config,
                "--host".into(),
                "127.0.0.1".into(),
                "--port".into(),
                port.to_string(),
                "--routing-log-file".into(),
                log,
            ],
        }
    }

    /// The router's own config check, so a bad routes file fails with the
    /// router's message instead of "exited early".
    pub fn validate(&self, config: &Path, env: &BTreeMap<String, String>) -> Result<(), String> {
        let out = Command::new(&self.bin)
            .arg("--config")
            .arg(config)
            .arg("--dry-run")
            .envs(env)
            .output()
            .map_err(|e| format!("{} --dry-run: {e}", self.kind.as_str()))?;
        if !out.status.success() {
            let mut text = String::from_utf8_lossy(&out.stdout).trim().to_string();
            let err = String::from_utf8_lossy(&out.stderr).trim().to_string();
            if !err.is_empty() {
                if !text.is_empty() {
                    text.push('\n');
                }
                text.push_str(&err);
            }
            return Err(format!(
                "{} --dry-run: {text}: {}",
                self.kind.as_str(),
                out.status
            ));
        }
        Ok(())
    }
}

fn kind_of(forced: &str, path: &str) -> RouterKind {
    match forced {
        "forseti-host" => RouterKind::Host,
        "switchyard-server" => RouterKind::Server,
        _ => {
            if Path::new(path)
                .file_name()
                .and_then(|n| n.to_str())
                .is_some_and(|b| b.contains("switchyard-server"))
            {
                RouterKind::Server
            } else {
                RouterKind::Host
            }
        }
    }
}

fn candidate_host_bins() -> Vec<PathBuf> {
    let mut out = Vec::new();
    if let Ok(p) = std::env::var("FORSETI_HOST_BIN")
        && !p.is_empty()
    {
        out.push(PathBuf::from(p));
    }
    if let Ok(wd) = std::env::current_dir()
        && let Some(root) = find_repo_root(&wd)
    {
        out.push(root.join("host/target/release/forseti-host"));
        out.push(root.join("host/target/debug/forseti-host"));
    }
    out
}

/// Walks up until a dir contains `host/Cargo.toml`.
fn find_repo_root(dir: &Path) -> Option<PathBuf> {
    let mut dir = dir.to_path_buf();
    loop {
        if dir.join("host/Cargo.toml").exists() {
            return Some(dir);
        }
        if !dir.pop() {
            return None;
        }
    }
}

fn resolve_switchyard_server() -> Option<PathBuf> {
    if let Ok(p) = std::env::var("FORSETI_SWITCHYARD_BIN")
        && !p.is_empty()
        && Path::new(&p).exists()
    {
        return Some(PathBuf::from(p));
    }
    if let Some(home) = std::env::var_os("HOME") {
        let p = PathBuf::from(home).join(".cargo/bin/switchyard-server");
        if p.exists() {
            return Some(p);
        }
    }
    look_path("switchyard-server")
}

fn look_path(name: &str) -> Option<PathBuf> {
    let path = std::env::var_os("PATH")?;
    std::env::split_paths(&path)
        .map(|d| d.join(name))
        .find(|p| p.is_file())
}

// MARK: config rendering

#[derive(Debug, Clone, Default, Deserialize)]
pub struct PiProvider {
    #[serde(default)]
    pub name: String,
    #[serde(default, rename = "baseUrl")]
    pub base_url: String,
    #[serde(default, rename = "apiKey")]
    pub api_key: String,
    #[serde(default)]
    pub api: String,
}

fn pi_models_path() -> Result<PathBuf, String> {
    let home = std::env::var_os("HOME").ok_or("cannot resolve home directory")?;
    Ok(PathBuf::from(home).join(".pi/agent/models.json"))
}

fn pi_providers() -> Result<BTreeMap<String, PiProvider>, String> {
    let path = pi_models_path()?;
    let raw = std::fs::read(&path).map_err(|e| format!("read pi models.json: {e}"))?;
    #[derive(Deserialize)]
    struct Registry {
        #[serde(default)]
        providers: BTreeMap<String, PiProvider>,
    }
    let reg: Registry =
        serde_json::from_slice(&raw).map_err(|e| format!("parse pi models.json: {e}"))?;
    Ok(reg.providers)
}

/// Resolves a pi `apiKey` value: `!cmd …` runs through `sh -c` (pi's
/// models.json convention), anything else is literal.
fn api_key_value(v: &str) -> Result<String, String> {
    let Some(cmd) = v.strip_prefix('!') else {
        return Ok(v.to_string());
    };
    let out = Command::new("sh")
        .arg("-c")
        .arg(cmd)
        .output()
        .map_err(|e| format!("resolve !apiKey: {e}"))?;
    if !out.status.success() {
        return Err(format!("resolve !apiKey: {}", out.status));
    }
    Ok(String::from_utf8_lossy(&out.stdout).trim().to_string())
}

/// The sorted, unique pi providers referenced by a crew's routes.
pub fn route_providers(routes: &[Route]) -> Vec<String> {
    let mut seen = std::collections::BTreeSet::new();
    for r in routes {
        for m in [&r.efficient, &r.capable, &r.judge] {
            if m.is_empty() {
                continue;
            }
            seen.insert(provider_of(m));
        }
    }
    seen.into_iter().collect()
}

fn provider_of(model: &str) -> String {
    match model.find('/') {
        Some(i) => model[..i].to_string(),
        None => String::new(),
    }
}

fn model_target(reference: &str) -> (String, String) {
    match reference.find('/') {
        Some(i) => (reference[..i].to_string(), reference[i + 1..].to_string()),
        None => (String::new(), reference.to_string()),
    }
}

/// The env var carrying a provider's key (never the TOML).
pub fn switchyard_key_env(provider: &str) -> String {
    let mut s = String::from("SWITCHYARD_KEY_");
    for c in provider.chars() {
        s.push(match c {
            '-' | '.' => '_',
            other => other.to_ascii_uppercase(),
        });
    }
    s
}

/// Renders the routes TOML + the key env for a run. `key` resolves a pi
/// `apiKey` value (literal or `!cmd`).
pub fn build_config(
    routes: &[Route],
    providers: &BTreeMap<String, PiProvider>,
    key: &mut dyn FnMut(&str) -> Result<String, String>,
) -> Result<(String, BTreeMap<String, String>), String> {
    let mut env = BTreeMap::new();
    let mut clients = String::new();
    for p in route_providers(routes) {
        let Some(prov) = providers.get(&p) else {
            return Err(format!("provider {p:?} not in ~/.pi/agent/models.json"));
        };
        // quoted table keys: a provider id with a '.' must not become a nested
        // table (P13-B22)
        clients.push_str(&format!(
            "\n[llm_clients.{}]\nformat = \"openai_chat\"\nbase_url = {}\n",
            quoted(&p),
            quoted(&prov.base_url)
        ));
        if !prov.api_key.is_empty() {
            let val = key(&prov.api_key).map_err(|e| format!("provider {p}: {e}"))?;
            let k = switchyard_key_env(&p);
            env.insert(k.clone(), val);
            clients.push_str(&format!("api_key_env = {}\n", quoted(&k)));
        }
    }

    let mut targets = String::new();
    let mut routes_t = String::new();
    for r in routes {
        let (eff_name, cap_name) = r.target_names();
        if !r.efficient.is_empty() {
            let (pv, m) = model_target(&r.efficient);
            targets.push_str(&format!(
                "\n[targets.{eff_name}]\nid = {}\nllm_client = {}\n",
                quoted(&m),
                quoted(&pv)
            ));
        }
        if !r.capable.is_empty() {
            let (pv, m) = model_target(&r.capable);
            targets.push_str(&format!(
                "\n[targets.{cap_name}]\nid = {}\nllm_client = {}\n",
                quoted(&m),
                quoted(&pv)
            ));
        }
        if !r.judge.is_empty() {
            let (pv, m) = model_target(&r.judge);
            targets.push_str(&format!(
                "\n[targets.{}]\nid = {}\nllm_client = {}\n",
                r.judge_target(),
                quoted(&m),
                quoted(&pv)
            ));
        }
        let keys = r.route_keys()?;
        routes_t.push_str(&format!(
            "\n[routes.{}]\nid = {}\ntype = {}\n",
            r.id,
            quoted(&r.id),
            quoted(r.effective_type())
        ));
        for (k, v) in &keys {
            // route_keys() carries the YAML value type; normalise to JSON
            let v = serde_json::to_value(v).map_err(|e| format!("route {:?}: {k}: {e}", r.id))?;
            let value = toml_value(&v).map_err(|e| format!("route {:?}: {k}: {e}", r.id))?;
            routes_t.push_str(&format!("{k} = {value}\n"));
        }
    }
    Ok((
        format!("schema_version = 1\n{clients}{targets}{routes_t}"),
        env,
    ))
}

fn quoted(s: &str) -> String {
    serde_json::to_string(s).unwrap_or_else(|_| "\"\"".to_string())
}

/// Go `tomlValue`: scalars, arrays, inline tables for maps (switchyard's
/// nested config reads identically from inline tables).
fn toml_value(v: &Value) -> Result<String, String> {
    match v {
        Value::String(s) => Ok(quoted(s)),
        Value::Bool(b) => Ok(b.to_string()),
        Value::Number(n) => {
            if let Some(i) = n.as_i64() {
                Ok(i.to_string())
            } else if let Some(u) = n.as_u64() {
                Ok(u.to_string())
            } else {
                let f = n.as_f64().unwrap_or(0.0);
                Ok(toml_float(f))
            }
        }
        Value::Null => Err("null is not representable in TOML".to_string()),
        Value::Array(a) => {
            let mut parts = Vec::with_capacity(a.len());
            for item in a {
                parts.push(toml_value(item)?);
            }
            Ok(format!("[{}]", parts.join(", ")))
        }
        Value::Object(m) => Ok(inline_table(m)?),
    }
}

fn inline_table(m: &Map<String, Value>) -> Result<String, String> {
    // sort explicitly: serde_json's Map order depends on the preserve_order
    // feature, which another crate in the tree could switch on
    let mut keys: Vec<&String> = m.keys().collect();
    keys.sort();
    let mut parts = Vec::with_capacity(keys.len());
    for k in keys {
        parts.push(format!("{} = {}", quoted(k), toml_value(&m[k])?));
    }
    Ok(format!("{{{}}}", parts.join(", ")))
}

/// Keeps a float a float: TOML reads a bare `1` as an integer, which serde
/// rejects for an f64 field (e.g. `confidence_threshold = 1`).
fn toml_float(f: f64) -> String {
    let mut s = format!("{f}");
    if !s.contains(['.', 'e', 'E']) {
        s.push_str(".0");
    }
    s
}

// MARK: the proxy

pub struct Proxy {
    pub kind: RouterKind,
    pub port: u16,
    pub config_path: PathBuf,
    pub routing_log: PathBuf,
    pub server_log: PathBuf,
    env: BTreeMap<String, String>,
    child: Option<Child>,
}

impl Proxy {
    /// Generates the TOML, picks a free port, dry-runs, spawns and waits for
    /// `/health`. `routing_log_name` is the per-run JSONL name inside `run_dir`.
    pub fn start(
        routes: &[Route],
        run_dir: &Path,
        routing_log_name: &str,
    ) -> Result<Proxy, String> {
        let router = Router::resolve()?;
        let providers = pi_providers()?;
        let (toml, env) = build_config(routes, &providers, &mut api_key_value)?;
        let port = free_port()?;
        let proxy = Proxy {
            kind: router.kind,
            port,
            config_path: run_dir.join("switchyard.toml"),
            routing_log: run_dir.join(routing_log_name),
            server_log: run_dir.join("router.log"),
            env,
            child: None,
        };
        std::fs::write(&proxy.config_path, &toml)
            .map_err(|e| format!("write switchyard.toml: {e}"))?;
        router.validate(&proxy.config_path, &proxy.env)?;

        let log =
            std::fs::File::create(&proxy.server_log).map_err(|e| format!("router.log: {e}"))?;
        let log_err = log.try_clone().map_err(|e| format!("router.log: {e}"))?;
        let child = Command::new(&router.bin)
            .args(router.args(&proxy.config_path, &proxy.routing_log, port))
            .envs(&proxy.env)
            .stdout(Stdio::from(log))
            .stderr(Stdio::from(log_err))
            .spawn()
            .map_err(|e| format!("spawn {}: {e}", router.kind.as_str()))?;
        let mut proxy = proxy;
        proxy.child = Some(child);
        proxy.await_health()?;
        Ok(proxy)
    }

    fn await_health(&mut self) -> Result<(), String> {
        let http = http_client();
        let deadline = Instant::now() + HEALTH_TIMEOUT;
        loop {
            if !self.alive() {
                return Err(format!(
                    "{} exited early; log: {}",
                    self.kind.as_str(),
                    self.server_log.display()
                ));
            }
            if let Ok(resp) = http.get(format!("{}/health", self.base_url())).send()
                && resp.status() == reqwest::StatusCode::OK
            {
                return Ok(());
            }
            if Instant::now() >= deadline {
                return Err(format!(
                    "{} not healthy within 15s (port {}); log: {}",
                    self.kind.as_str(),
                    self.port,
                    self.server_log.display()
                ));
            }
            std::thread::sleep(Duration::from_millis(150));
        }
    }

    fn alive(&mut self) -> bool {
        match self.child.as_mut() {
            Some(c) => matches!(c.try_wait(), Ok(None)),
            None => false,
        }
    }

    pub fn base_url(&self) -> String {
        format!("http://127.0.0.1:{}", self.port)
    }

    /// The pi models.json provider for this proxy (one model id per route).
    pub fn provider_entry(&self, route_ids: &[String]) -> Value {
        let models: Vec<Value> = route_ids
            .iter()
            .map(|id| {
                json!({
                    "id": id,
                    "name": format!("Switchyard route {id}"),
                    "reasoning": true,
                    "input": ["text"],
                    "cost": {"input": 0, "output": 0, "cacheRead": 0, "cacheWrite": 0},
                    "contextWindow": 131072,
                    "maxTokens": 8192,
                })
            })
            .collect();
        json!({
            "name": "Switchyard Router",
            "baseUrl": format!("{}/v1", self.base_url()),
            "api": "openai-completions",
            "apiKey": "switchyard",
            "compat": {
                "supportsDeveloperRole": false,
                "sendSessionAffinityHeaders": true,
                "sessionAffinityFormat": "openrouter",
            },
            "models": models,
        })
    }

    /// Writes the `switchyard` provider into pi's models.json and returns the
    /// restore closure (key removal, never a byte restore — P13-B9).
    pub fn materialize_provider(&self, route_ids: &[String]) -> Result<Restore, String> {
        let path = pi_models_path()?;
        materialize_at(&path, &self.provider_entry(route_ids), &self.base_url())
    }

    /// `GET /v1/stats`.
    pub fn stats(&self) -> Result<Value, String> {
        let resp = http_client()
            .get(format!("{}/v1/stats", self.base_url()))
            .send()
            .map_err(|e| e.to_string())?;
        resp.json::<Value>().map_err(|e| e.to_string())
    }

    pub fn stop(&mut self) {
        if let Some(mut child) = self.child.take() {
            let _ = child.kill();
            let _ = child.wait();
        }
    }
}

impl Drop for Proxy {
    fn drop(&mut self) {
        self.stop();
    }
}

/// The restore closure a run MUST call at teardown.
pub type Restore = Box<dyn FnOnce() + Send>;

fn materialize_at(path: &Path, entry: &Value, base_url: &str) -> Result<Restore, String> {
    let orig = std::fs::read(path).map_err(|e| format!("read pi models.json: {e}"))?;
    let mut reg: Value =
        serde_json::from_slice(&orig).map_err(|e| format!("parse pi models.json: {e}"))?;
    let Some(providers) = reg.get_mut("providers").and_then(Value::as_object_mut) else {
        return Err("pi models.json has no providers map".to_string());
    };
    if let Some(prev) = providers.get("switchyard") {
        if prev_stale(prev) {
            // a leftover from a crashed run (dead port) — sweep it
            providers.remove("switchyard");
        } else {
            return Err("pi models.json already has a LIVE switchyard provider — another crew run may be active; refusing to stomp it".to_string());
        }
    }
    providers.insert("switchyard".to_string(), entry.clone());
    let out = serde_json::to_vec_pretty(&reg).map_err(|e| e.to_string())?;
    std::fs::write(path, out).map_err(|e| format!("write pi models.json: {e}"))?;

    let path = path.to_path_buf();
    let base = base_url.to_string();
    Ok(Box::new(move || {
        // key-removal restore: re-read (the user may have edited the file during
        // the run), delete only OUR entry (same baseUrl), write back.
        let Ok(fresh) = std::fs::read(&path) else {
            return;
        };
        let Ok(mut reg2) = serde_json::from_slice::<Value>(&fresh) else {
            return;
        };
        let ours = reg2
            .get("providers")
            .and_then(|p| p.get("switchyard"))
            .and_then(|p| p.get("baseUrl"))
            .and_then(Value::as_str)
            .map(|u| u == format!("{base}/v1"))
            .unwrap_or(false);
        if ours {
            if let Some(prov) = reg2.get_mut("providers").and_then(Value::as_object_mut) {
                prov.remove("switchyard");
            }
            if let Ok(b) = serde_json::to_vec_pretty(&reg2) {
                let _ = std::fs::write(&path, b);
            }
            return;
        }
        // not ours any more: leave the user's edit alone
        let _ = std::fs::write(&path, orig);
    }))
}

/// Probes a pre-existing switchyard entry: healthy → false (live owner),
/// unreachable/5xx → true (a husk safe to sweep).
fn prev_stale(prev: &Value) -> bool {
    let Some(base) = prev.get("baseUrl").and_then(Value::as_str) else {
        return true;
    };
    if base.is_empty() {
        return true;
    }
    match http_client()
        .get(format!("{}/models", base.trim_end_matches('/')))
        .send()
    {
        Err(_) => true,
        Ok(r) => r.status().as_u16() >= 500,
    }
}

fn http_client() -> reqwest::blocking::Client {
    reqwest::blocking::Client::builder()
        .timeout(HTTP_TIMEOUT)
        .build()
        .expect("router HTTP client")
}

fn free_port() -> Result<u16, String> {
    let l = std::net::TcpListener::bind("127.0.0.1:0").map_err(|e| e.to_string())?;
    let port = l.local_addr().map_err(|e| e.to_string())?.port();
    Ok(port)
}

// MARK: routing-log tail

/// Polls the router's NDJSON log and turns each record into a `route_decision`
/// event (S30: route id, algorithm, served tier, session, origin, tokens).
pub struct RoutingTailer {
    stop: Arc<AtomicBool>,
    handle: Option<JoinHandle<()>>,
    events: Arc<Mutex<Vec<Event>>>,
}

impl RoutingTailer {
    pub fn spawn(path: PathBuf) -> Self {
        let stop = Arc::new(AtomicBool::new(false));
        let events = Arc::new(Mutex::new(Vec::new()));
        let handle = {
            let stop = Arc::clone(&stop);
            let events = Arc::clone(&events);
            std::thread::spawn(move || tail(&path, stop, events))
        };
        RoutingTailer {
            stop,
            handle: Some(handle),
            events,
        }
    }

    pub fn events(&self) -> Arc<Mutex<Vec<Event>>> {
        Arc::clone(&self.events)
    }

    pub fn stop(&mut self) {
        self.stop.store(true, Ordering::SeqCst);
        if let Some(h) = self.handle.take() {
            let _ = h.join();
        }
    }
}

fn tail(path: &Path, stop: Arc<AtomicBool>, events: Arc<Mutex<Vec<Event>>>) {
    let deadline = Instant::now() + Duration::from_secs(2 * 60 * 60);
    let mut offset: u64 = 0;
    while !stop.load(Ordering::SeqCst) && Instant::now() < deadline {
        poll(path, &mut offset, &events);
        std::thread::sleep(Duration::from_millis(750));
    }
    // final drain: the last record usually lands exactly when the run ends
    poll(path, &mut offset, &events);
}

fn poll(path: &Path, offset: &mut u64, events: &Arc<Mutex<Vec<Event>>>) {
    if let Ok(mut f) = std::fs::File::open(path) {
        let size = f.metadata().map(|m| m.len()).unwrap_or(0);
        if size > *offset && f.seek(SeekFrom::Start(*offset)).is_ok() {
            let mut buf = Vec::new();
            if std::io::Read::read_to_end(&mut f, &mut buf).is_ok() {
                for line in buf.split(|b| *b == b'\n') {
                    if line.is_empty() {
                        continue;
                    }
                    if let Some(ev) = route_event(line)
                        && let Ok(mut v) = events.lock()
                    {
                        v.push(ev);
                    }
                }
                // advance by what was actually read: `size` may already be
                // stale if the router appended while we were reading
                *offset += buf.len() as u64;
            }
        }
    }
}

fn route_event(line: &[u8]) -> Option<Event> {
    // every field is Option: the host writes explicit `null` for unknown
    // fields (origin is null today), and serde rejects null for String/i64
    #[derive(Deserialize)]
    struct Record {
        #[serde(default)]
        ts: Option<String>,
        #[serde(default)]
        route_id: Option<String>,
        #[serde(default)]
        algorithm: Option<String>,
        #[serde(default)]
        tier: Option<String>,
        #[serde(default)]
        session_id: Option<String>,
        #[serde(default)]
        origin: Option<String>,
        #[serde(default)]
        model: Option<String>,
        #[serde(default)]
        prompt_tokens: Option<i64>,
        #[serde(default)]
        completion_tokens: Option<i64>,
        #[serde(default)]
        total_tokens: Option<i64>,
    }
    let rec: Record = serde_json::from_slice(line).ok()?;
    let model = rec.model.unwrap_or_default();
    if model.is_empty() {
        return None;
    }
    let text = |v: Option<String>| v.unwrap_or_default();
    let num = |v: Option<i64>| v.unwrap_or(0);
    let mut parts = vec![format!(
        "{} tokens (in {} / out {})",
        num(rec.total_tokens),
        num(rec.prompt_tokens),
        num(rec.completion_tokens)
    )];
    let route = text(rec.route_id);
    if !route.is_empty() {
        parts.push(format!("route {route}"));
    }
    let algorithm = text(rec.algorithm);
    if !algorithm.is_empty() {
        parts.push(format!("via {algorithm}"));
    }
    let tier = text(rec.tier);
    if !tier.is_empty() {
        parts.push(format!("tier {tier}"));
    }
    let session = text(rec.session_id);
    if !session.is_empty() {
        parts.push(format!("session {session}"));
    }
    let origin = text(rec.origin);
    if !origin.is_empty() {
        parts.push(format!("origin {origin}"));
    }
    let mut ev = Event::new("route_decision");
    ev.ts = super::scheduler::rfc3339_now();
    ev.model = model;
    ev.info = format!("routed call: {} @ {}", parts.join(", "), text(rec.ts));
    Some(ev)
}

/// Renders switchyard stats tallies (`efficient` → `{calls|total_requests,
/// errors}`) as `efficient=3 (errors=1)`.
pub fn fmt_tallies(m: &Map<String, Value>) -> String {
    let mut keys: Vec<&String> = m.keys().collect();
    keys.sort();
    let mut parts = Vec::with_capacity(keys.len());
    for k in keys {
        let v = &m[k];
        let Some(obj) = v.as_object() else {
            parts.push(format!("{k}={v}"));
            continue;
        };
        let mut calls = obj.get("calls").and_then(Value::as_f64).unwrap_or(0.0);
        if calls == 0.0 {
            calls = obj
                .get("total_requests")
                .and_then(Value::as_f64)
                .unwrap_or(0.0);
        }
        let mut s = format!("{k}={}", calls as i64);
        if let Some(errs) = obj.get("errors").and_then(Value::as_f64)
            && errs > 0.0
        {
            s += &format!(" (errors={})", errs as i64);
        }
        parts.push(s);
    }
    parts.join(" ")
}

// MARK: tests

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    fn route(id: &str, ty: &str) -> Route {
        Route {
            id: id.to_string(),
            type_: ty.to_string(),
            confidence: 0.8,
            ..Default::default()
        }
    }

    fn providers() -> BTreeMap<String, PiProvider> {
        let mut p = BTreeMap::new();
        p.insert(
            "opencode-go".to_string(),
            PiProvider {
                name: "opencode-go".into(),
                base_url: "https://opencode.ai/zen/go/v1".into(),
                api_key: "!cat /run/secrets/key".into(),
                api: "openai-completions".into(),
            },
        );
        p.insert(
            "open.ai".to_string(),
            PiProvider {
                base_url: "https://open.ai/v1".into(),
                ..Default::default()
            },
        );
        p
    }

    #[test]
    fn route_providers_are_sorted_and_unique() {
        let mut r = route("auto", "passthrough");
        r.capable = "opencode-go/beam-free".into();
        let mut r2 = route("judge", "passthrough");
        r2.capable = "opencode-go/longcat-2.5-preview-free".into();
        r2.judge = "open.ai/j1".into();
        assert_eq!(route_providers(&[r, r2]), ["open.ai", "opencode-go"]);
    }

    #[test]
    fn a_provider_id_with_a_dot_stays_one_quoted_table() {
        // P13-B22: [llm_clients."open.ai"] must not become a nested table
        let mut r = route("auto", "passthrough");
        r.capable = "open.ai/m1".into();
        let (toml, _) = build_config(&[r], &providers(), &mut |_| Ok("k".into())).unwrap();
        assert!(
            toml.contains("[llm_clients.\"open.ai\"]"),
            "quoted table key missing:\n{toml}"
        );
    }

    #[test]
    fn keys_go_to_the_env_never_the_toml() {
        let mut r = route("auto", "stage_router");
        r.capable = "opencode-go/beam-free".into();
        r.efficient = "opencode-go/beam-fast".into();
        let (toml, env) = build_config(&[r], &providers(), &mut |_| Ok("SECRET".into())).unwrap();
        assert!(!toml.contains("SECRET"));
        assert_eq!(
            env.get("SWITCHYARD_KEY_OPENCODE_GO").map(String::as_str),
            Some("SECRET")
        );
        assert!(toml.contains("api_key_env = \"SWITCHYARD_KEY_OPENCODE_GO\""));
        // the derived target names are <id>_eff / <id>_cap and the route keys
        // point at them (switchyard's own vocabulary)
        assert!(toml.contains("[targets.auto_eff]"), "{toml}");
        assert!(toml.contains("[targets.auto_cap]"), "{toml}");
        assert!(toml.contains("efficient_target = \"auto_eff\""), "{toml}");
        assert!(toml.contains("capable_target = \"auto_cap\""), "{toml}");
        assert!(toml.contains("id = \"beam-fast\""), "{toml}");
        assert!(toml.contains("llm_client = \"opencode-go\""), "{toml}");
        assert!(toml.contains("[routes.auto]"), "{toml}");
        assert!(toml.contains("type = \"stage_router\""), "{toml}");
    }

    #[test]
    fn options_pass_through_as_toml_values() {
        let mut r = route("auto", "stage_router");
        r.capable = "opencode-go/beam-free".into();
        let yaml = |v: serde_json::Value| serde_norway::to_value(v).unwrap();
        // non-derived option keys pass through verbatim; a derived one
        // (confidence_threshold for stage_router) is rejected by route_keys
        r.options = [
            ("weight".to_string(), yaml(json!(1.0))),
            (
                "tool_semantics".to_string(),
                yaml(json!({"enabled": true, "tiers": ["a", "b"]})),
            ),
            ("seed".to_string(), yaml(json!(7))),
        ]
        .into_iter()
        .collect();
        let (toml, _) = build_config(&[r], &providers(), &mut |_| Ok("k".into())).unwrap();
        // a float must stay a float, an int an int, nested maps inline
        assert!(toml.contains("weight = 1.0"), "{toml}");
        assert!(toml.contains("seed = 7"), "{toml}");
        assert!(
            toml.contains("tool_semantics = {\"enabled\" = true, \"tiers\" = [\"a\", \"b\"]}"),
            "{toml}"
        );
    }

    #[test]
    fn a_missing_provider_is_an_error() {
        let mut r = route("auto", "passthrough");
        r.capable = "nope/model".into();
        let err = build_config(&[r], &providers(), &mut |_| Ok("k".into())).unwrap_err();
        assert_eq!(err, "provider \"nope\" not in ~/.pi/agent/models.json");
    }

    #[test]
    fn api_key_values_resolve_only_bang_commands() {
        assert_eq!(api_key_value("literal").unwrap(), "literal");
        assert_eq!(api_key_value("!printf '  pipe\n'").unwrap(), "pipe");
    }

    #[test]
    fn switchyard_key_env_sanitizes_the_provider() {
        assert_eq!(
            switchyard_key_env("opencode-go"),
            "SWITCHYARD_KEY_OPENCODE_GO"
        );
        assert_eq!(switchyard_key_env("open.ai"), "SWITCHYARD_KEY_OPEN_AI");
    }

    #[test]
    fn toml_floats_keep_their_point() {
        assert_eq!(toml_float(1.0), "1.0");
        assert_eq!(toml_float(0.5), "0.5");
        assert_eq!(toml_float(1e-7), "0.0000001");
        assert_eq!(
            toml_value(&Value::Null).unwrap_err(),
            "null is not representable in TOML"
        );
    }

    #[test]
    fn router_args_differ_between_host_and_server() {
        let cfg = Path::new("/x/switchyard.toml");
        let log = Path::new("/x/routing.jsonl");
        let host = Router {
            bin: "/bin/true".into(),
            kind: RouterKind::Host,
        };
        assert_eq!(
            host.args(cfg, log, 9001),
            vec![
                "--config".to_string(),
                "/x/switchyard.toml".to_string(),
                "--addr".to_string(),
                "127.0.0.1:9001".to_string(),
                "--routing-log".to_string(),
                "/x/routing.jsonl".to_string()
            ]
        );
        let server = Router {
            bin: "/bin/true".into(),
            kind: RouterKind::Server,
        };
        assert_eq!(
            server.args(cfg, log, 9001),
            vec![
                "--config".to_string(),
                "/x/switchyard.toml".to_string(),
                "--host".to_string(),
                "127.0.0.1".to_string(),
                "--port".to_string(),
                "9001".to_string(),
                "--routing-log-file".to_string(),
                "/x/routing.jsonl".to_string()
            ]
        );
    }

    #[test]
    fn provider_entry_is_the_pi_contract() {
        let p = Proxy {
            kind: RouterKind::Host,
            port: 9123,
            config_path: PathBuf::new(),
            routing_log: PathBuf::new(),
            server_log: PathBuf::new(),
            env: BTreeMap::new(),
            child: None,
        };
        let entry = p.provider_entry(&["auto".to_string(), "judge".to_string()]);
        assert_eq!(entry["baseUrl"], "http://127.0.0.1:9123/v1");
        assert_eq!(entry["api"], "openai-completions");
        assert_eq!(entry["apiKey"], "switchyard");
        assert_eq!(entry["compat"]["sendSessionAffinityHeaders"], true);
        let models = entry["models"].as_array().unwrap();
        assert_eq!(models.len(), 2);
        assert_eq!(models[0]["id"], "auto");
        assert_eq!(models[1]["name"], "Switchyard route judge");
    }

    fn temp_models(contents: &str) -> PathBuf {
        use std::sync::atomic::{AtomicUsize, Ordering};
        static N: AtomicUsize = AtomicUsize::new(0);
        let n = N.fetch_add(1, Ordering::SeqCst);
        let path =
            std::env::temp_dir().join(format!("forseti-models-{}-{n}.json", std::process::id()));
        std::fs::write(&path, contents).unwrap();
        path
    }

    #[test]
    fn materialize_adds_the_provider_and_restore_removes_only_it() {
        let path = temp_models(r#"{"providers":{"keep":{"baseUrl":"http://x/v1","apiKey":"k"}}}"#);
        let entry = json!({"baseUrl": "http://127.0.0.1:9999/v1", "models": []});
        let restore = materialize_at(&path, &entry, "http://127.0.0.1:9999").unwrap();
        let after: Value = serde_json::from_slice(&std::fs::read(&path).unwrap()).unwrap();
        assert_eq!(
            after["providers"]["switchyard"]["baseUrl"],
            "http://127.0.0.1:9999/v1"
        );
        assert_eq!(after["providers"]["keep"]["apiKey"], "k");

        // a user edit during the run must survive the key-removal restore
        let mut edited = after.clone();
        edited["providers"]["added_during_run"] = json!({"baseUrl": "http://y/v1"});
        std::fs::write(&path, serde_json::to_vec_pretty(&edited).unwrap()).unwrap();

        restore();
        let back: Value = serde_json::from_slice(&std::fs::read(&path).unwrap()).unwrap();
        assert!(
            back["providers"].get("switchyard").is_none(),
            "ours removed"
        );
        assert_eq!(
            back["providers"]["added_during_run"]["baseUrl"],
            "http://y/v1"
        );
        assert_eq!(back["providers"]["keep"]["apiKey"], "k");
    }

    #[test]
    fn a_dead_previous_entry_is_swept() {
        let path =
            temp_models(r#"{"providers":{"switchyard":{"baseUrl":"http://127.0.0.1:1/v1"}}}"#);
        let entry = json!({"baseUrl": "http://127.0.0.1:9999/v1"});
        let _restore =
            materialize_at(&path, &entry, "http://127.0.0.1:9999").expect("stale entry swept");
        let after: Value = serde_json::from_slice(&std::fs::read(&path).unwrap()).unwrap();
        assert_eq!(
            after["providers"]["switchyard"]["baseUrl"],
            "http://127.0.0.1:9999/v1"
        );
    }

    #[test]
    fn a_live_previous_entry_is_refused() {
        // a listener that answers /models with 200 = a live owner
        let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
        let addr = listener.local_addr().unwrap();
        let handle = std::thread::spawn(move || {
            use std::io::{Read, Write};
            let (mut s, _) = listener.accept().unwrap();
            let mut buf = [0u8; 1024];
            let _ = s.read(&mut buf);
            s.write_all(b"HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\n{}")
                .unwrap();
        });
        let path = temp_models(&format!(
            r#"{{"providers":{{"switchyard":{{"baseUrl":"http://{addr}/v1"}}}}}}"#
        ));
        let entry = json!({"baseUrl": "http://127.0.0.1:9999/v1"});
        let err = materialize_at(&path, &entry, "http://127.0.0.1:9999")
            .err()
            .expect("a live entry must be refused");
        assert!(err.contains("LIVE switchyard provider"), "{err}");
        handle.join().unwrap();
    }

    #[test]
    fn a_routing_record_becomes_the_go_event() {
        let ev = route_event(
            br#"{"ts":"T","route_id":"auto","algorithm":"stage_router","tier":"efficient","session_id":"s1","origin":"http","model":"m","prompt_tokens":10,"completion_tokens":5,"total_tokens":15}"#,
        )
        .expect("event");
        assert_eq!(ev.kind, "route_decision");
        assert_eq!(ev.model, "m");
        assert_eq!(
            ev.info,
            "routed call: 15 tokens (in 10 / out 5), route auto, via stage_router, tier efficient, session s1, origin http @ T"
        );
        assert!(
            route_event(br#"{"nope":1}"#).is_none(),
            "a record without a model is ignored"
        );
    }

    #[test]
    fn the_tailer_picks_up_records_written_during_a_run() {
        let path =
            std::env::temp_dir().join(format!("forseti-routing-{}.jsonl", std::process::id()));
        let _ = std::fs::remove_file(&path);
        let mut t = RoutingTailer::spawn(path.clone());
        std::fs::write(
            &path,
            b"{\"ts\":\"T\",\"route_id\":\"auto\",\"algorithm\":\"stage_router\",\"model\":\"m\",\"prompt_tokens\":1,\"completion_tokens\":2,\"total_tokens\":3}\n",
        )
        .unwrap();
        std::thread::sleep(Duration::from_millis(1200));
        let queued = t.events().lock().unwrap().len();
        t.stop();
        let after = t.events().lock().unwrap().len();
        assert_eq!(queued, 1, "record read while running (after stop: {after})");
        let _ = std::fs::remove_file(&path);
    }

    #[test]
    fn tallies_render_like_go() {
        let m: Map<String, Value> = serde_json::from_value(json!({
            "efficient": {"calls": 3, "errors": 1},
            "capable": {"total_requests": 2, "errors": 0},
            "other": 7,
        }))
        .unwrap();
        assert_eq!(fmt_tallies(&m), "capable=2 efficient=3 (errors=1) other=7");
    }
}
