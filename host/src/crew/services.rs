//! The loopback services the runner uses (Phase 18D-5c): shared memory and the
//! laya decision endpoint.
//!
//! Both are JSON-over-HTTP on 127.0.0.1 and both are **advisory**:
//!
//! * memory (`FORSETI_MEMORY_URL`, default `http://127.0.0.1:8752`) — a dead
//!   service costs a `memory_recall`/`memory_write` event, never the run.
//! * laya (`FORSETI_LAYA_URL`, default `http://127.0.0.1:8751`) — a dead or
//!   ambiguous endpoint fail-safes to skipping every laya-gated edge, so the
//!   graph never hangs on a decision (S19).
//!
//! The payload shapes are the Go runner's (`crew/internal/runner/laya.go`),
//! including the error text, because the services and their evals
//! (`scripts/memory-eval.sh`, `scripts/laya-eval.sh`) are already pinned.

use std::time::{Duration, Instant};

use serde_json::{Map, Value, json};

use super::runner::{MemoryLink, MemoryRow, truncate_utf8};

const MEMORY_TIMEOUT: Duration = Duration::from_secs(10);
const LAYA_TIMEOUT: Duration = Duration::from_secs(15);

fn env_url(key: &str, default: &str) -> String {
    let raw = std::env::var(key).unwrap_or_default();
    let raw = if raw.is_empty() { default } else { &raw };
    raw.trim_end_matches('/').to_string()
}

fn client(timeout: Duration) -> reqwest::blocking::Client {
    reqwest::blocking::Client::builder()
        .timeout(timeout)
        .build()
        .expect("loopback HTTP client")
}

/// POST a JSON body and decode a JSON reply. Error text mirrors the Go
/// clients (`<label> endpoint unreachable (<base>): …`, `<label> endpoint N`).
fn post(
    http: &reqwest::blocking::Client,
    base: &str,
    path: &str,
    body: &Value,
    label: &str,
) -> Result<Value, String> {
    let resp = http
        .post(format!("{base}{path}"))
        .json(body)
        .send()
        .map_err(|e| format!("{label} endpoint unreachable ({base}): {e}"))?;
    if resp.status() != reqwest::StatusCode::OK {
        return Err(format!("{label} endpoint {}", resp.status().as_u16()));
    }
    resp.json::<Value>()
        .map_err(|e| format!("{label} decode: {e}"))
}

// MARK: memory

/// The shared-memory consumer (P10-2/P14): recall for prompts, write for node
/// outputs. Namespace semantics are the service's (`crew-<ns>` rows).
pub struct MemoryClient {
    base: String,
    http: reqwest::blocking::Client,
}

impl MemoryClient {
    pub fn new() -> Self {
        Self::at(&env_url("FORSETI_MEMORY_URL", "http://127.0.0.1:8752"))
    }

    pub fn at(base: &str) -> Self {
        MemoryClient {
            base: base.trim_end_matches('/').to_string(),
            http: client(MEMORY_TIMEOUT),
        }
    }

    pub fn base(&self) -> &str {
        &self.base
    }
}

impl Default for MemoryClient {
    fn default() -> Self {
        Self::new()
    }
}

impl MemoryLink for MemoryClient {
    fn recall(
        &mut self,
        query: &str,
        node: &str,
        k: i64,
        namespace: &str,
    ) -> Result<Vec<MemoryRow>, String> {
        let k = if k <= 0 { 3 } else { k };
        let mut payload = json!({"query": query, "agent": node, "k": k});
        if !namespace.is_empty() {
            // crew widens visibility to the crew namespace (empty = personal
            // + shared only)
            payload["crew"] = json!(namespace);
        }
        let body = post(&self.http, &self.base, "/recall", &payload, "memory")?;
        let rows = body.as_array().cloned().unwrap_or_default();
        Ok(rows
            .iter()
            .map(|r| MemoryRow {
                ts: r
                    .get("ts")
                    .and_then(Value::as_str)
                    .unwrap_or_default()
                    .to_string(),
                text: r
                    .get("text")
                    .and_then(Value::as_str)
                    .unwrap_or_default()
                    .to_string(),
            })
            .collect())
    }

    fn write(
        &mut self,
        text: &str,
        namespace: &str,
        run_id: &str,
        source: &str,
        tags: &[String],
    ) -> Result<i64, String> {
        // the service caps a row at 4000 bytes (Go slices there too)
        let text = truncate_utf8(text, 4000);
        let payload = json!({
            "agent": format!("crew-{namespace}"),
            "crew": namespace,
            "text": text,
            "run": run_id,
            "source": source,
            "tags": tags,
        });
        let body = post(&self.http, &self.base, "/write", &payload, "memory")?;
        Ok(body.get("id").and_then(Value::as_i64).unwrap_or(0))
    }
}

// MARK: laya

/// One candidate target and its edge's instructions.
#[derive(Debug, Clone)]
pub struct LayaChoice {
    pub target: String,
    pub instructions: String,
}

/// The outcome of one bounded decision.
#[derive(Debug, Clone, Default)]
pub struct LayaDecision {
    /// The chosen target ("" = explicit "other" abstention).
    pub chosen: String,
    /// The CALIBRATED confidence (temperature) — the gate signal, not the
    /// top-option share.
    pub confidence: f64,
    /// The routing checkpoint (event `model`).
    pub checkpoint: String,
    pub latency_ms: u128,
}

/// The decision endpoint. `decide` is one HTTP call: the argmax is already
/// picked by the service; the runner only applies the confidence dial.
pub trait LayaDecider {
    fn decide(&mut self, state: &str, choices: &[LayaChoice]) -> Result<LayaDecision, String>;
}

/// A decider that always fails — every laya gate then skips (fail-safe).
pub struct NoLaya;

impl LayaDecider for NoLaya {
    fn decide(&mut self, _state: &str, _choices: &[LayaChoice]) -> Result<LayaDecision, String> {
        Err("laya disabled".to_string())
    }
}

pub struct LayaClient {
    base: String,
    http: reqwest::blocking::Client,
}

impl LayaClient {
    pub fn new() -> Self {
        Self::at(&env_url("FORSETI_LAYA_URL", "http://127.0.0.1:8751"))
    }

    pub fn at(base: &str) -> Self {
        LayaClient {
            base: base.trim_end_matches('/').to_string(),
            http: client(LAYA_TIMEOUT),
        }
    }

    pub fn base(&self) -> &str {
        &self.base
    }
}

impl Default for LayaClient {
    fn default() -> Self {
        Self::new()
    }
}

impl LayaDecider for LayaClient {
    fn decide(&mut self, state: &str, choices: &[LayaChoice]) -> Result<LayaDecision, String> {
        let mut criteria = Map::new();
        for c in choices {
            criteria.insert(c.target.clone(), Value::String(c.instructions.clone()));
        }
        // the implicit escape hatch: no activation
        criteria.insert(
            "other".to_string(),
            Value::String("none of the options fit this output".to_string()),
        );
        let question = json!({
            "type": "choice",
            "instructions": "Which option best fits this output? Choose 'other' if none fit.",
            "criteria": criteria,
        });
        let body = json!({
            "state": {"output": state},
            "questions": {"route": question},
        });
        let t0 = Instant::now();
        let out = post(&self.http, &self.base, "/v1/systemone", &body, "laya")?;
        let ans = out
            .pointer("/answers/route")
            .ok_or_else(|| "laya answer missing 'route' key".to_string())?;
        let choice = ans
            .get("choice")
            .and_then(Value::as_str)
            .unwrap_or_default();
        Ok(LayaDecision {
            chosen: if choice == "other" {
                String::new()
            } else {
                choice.to_string()
            },
            confidence: ans.get("confidence").and_then(Value::as_f64).unwrap_or(0.0),
            checkpoint: out
                .pointer("/routing/model")
                .and_then(Value::as_str)
                .unwrap_or_default()
                .to_string(),
            latency_ms: t0.elapsed().as_millis(),
        })
    }
}

// MARK: tests

#[cfg(test)]
mod tests {
    use super::*;
    use std::io::{Read, Write};
    use std::net::TcpListener;
    use std::thread::JoinHandle;

    /// A one-shot HTTP/1.1 server: replies with `body` and returns the raw
    /// request it received.
    fn serve_once(body: &'static str) -> (String, JoinHandle<Vec<u8>>) {
        let listener = TcpListener::bind("127.0.0.1:0").expect("bind");
        let addr = listener.local_addr().expect("addr");
        let handle = std::thread::spawn(move || {
            let (mut stream, _) = listener.accept().expect("accept");
            let mut req = Vec::new();
            let mut buf = [0u8; 4096];
            let mut header_end = None;
            while header_end.is_none() {
                let n = stream.read(&mut buf).expect("read");
                if n == 0 {
                    break;
                }
                req.extend_from_slice(&buf[..n]);
                header_end = req.windows(4).position(|w| w == b"\r\n\r\n");
            }
            let hend = header_end.expect("headers");
            let headers = String::from_utf8_lossy(&req[..hend]).to_ascii_lowercase();
            let len: usize = headers
                .lines()
                .find_map(|l| l.strip_prefix("content-length:"))
                .and_then(|v| v.trim().parse().ok())
                .unwrap_or(0);
            while req.len() < hend + 4 + len {
                let n = stream.read(&mut buf).expect("read body");
                if n == 0 {
                    break;
                }
                req.extend_from_slice(&buf[..n]);
            }
            let reply = format!(
                "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{}",
                body.len(),
                body
            );
            stream.write_all(reply.as_bytes()).expect("write");
            let _ = stream.flush();
            req
        });
        (format!("http://{addr}"), handle)
    }

    fn body_of(req: &[u8]) -> Value {
        let pos = req
            .windows(4)
            .position(|w| w == b"\r\n\r\n")
            .expect("headers");
        serde_json::from_slice(&req[pos + 4..]).expect("json body")
    }

    #[test]
    fn recall_posts_the_go_payload_and_reads_rows() {
        let (base, handle) = serve_once(
            r#"[{"id":1,"ts":"2026-10-01T00:00:00Z","agent":"a","text":"remembered","score":0.9}]"#,
        );
        let mut mem = MemoryClient::at(&base);
        let rows = mem.recall("q", "node-a", 3, "crew-ns").expect("recall");
        assert_eq!(rows.len(), 1);
        assert_eq!(rows[0].text, "remembered");
        assert_eq!(rows[0].ts, "2026-10-01T00:00:00Z");
        let req = handle.join().unwrap();
        let sent = body_of(&req);
        assert_eq!(sent["query"], "q");
        assert_eq!(sent["agent"], "node-a");
        assert_eq!(sent["k"], 3);
        assert_eq!(sent["crew"], "crew-ns");
        // the request line carries the path
        assert!(String::from_utf8_lossy(&req).starts_with("POST /recall "));
    }

    #[test]
    fn recall_omits_the_crew_key_without_a_namespace() {
        let (base, handle) = serve_once("[]");
        let mut mem = MemoryClient::at(&base);
        let rows = mem.recall("q", "a", 0, "").expect("recall");
        assert!(rows.is_empty());
        let sent = body_of(&handle.join().unwrap());
        assert_eq!(sent["k"], 3, "k <= 0 defaults to 3");
        assert!(sent.get("crew").is_none());
    }

    #[test]
    fn write_posts_the_crew_agent_and_reads_the_id() {
        let (base, handle) = serve_once(r#"{"id":42}"#);
        let mut mem = MemoryClient::at(&base);
        let id = mem
            .write(
                "output",
                "memtest",
                "20261018-120000",
                "node:a",
                &["crew".to_string(), "demo".to_string()],
            )
            .expect("write");
        assert_eq!(id, 42);
        let req = handle.join().unwrap();
        let sent = body_of(&req);
        assert!(String::from_utf8_lossy(&req).starts_with("POST /write "));
        assert_eq!(sent["agent"], "crew-memtest");
        assert_eq!(sent["crew"], "memtest");
        assert_eq!(sent["run"], "20261018-120000");
        assert_eq!(sent["source"], "node:a");
        assert_eq!(sent["tags"], json!(["crew", "demo"]));
    }

    #[test]
    fn a_non_200_is_an_error_with_the_go_text() {
        let listener = TcpListener::bind("127.0.0.1:0").unwrap();
        let addr = listener.local_addr().unwrap();
        let handle = std::thread::spawn(move || {
            let (mut s, _) = listener.accept().unwrap();
            let mut buf = [0u8; 1024];
            let _ = s.read(&mut buf);
            s.write_all(
                b"HTTP/1.1 503 Service Unavailable\r\nContent-Length: 0\r\nConnection: close\r\n\r\n",
            )
            .unwrap();
        });
        let mut mem = MemoryClient::at(&format!("http://{addr}"));
        let err = mem.recall("q", "a", 1, "").unwrap_err();
        assert_eq!(err, "memory endpoint 503");
        handle.join().unwrap();
    }

    #[test]
    fn laya_builds_the_choice_question_and_reads_the_calibrated_confidence() {
        let (base, handle) = serve_once(
            r#"{"model":"laya","answers":{"route":{"choice":"b","confidence":0.71,"answer_confidence":0.5,"probabilities":{"a":0.2,"b":0.5,"other":0.3}}},"routing":{"model":"checkpoint-x"}}"#,
        );
        let mut laya = LayaClient::at(&base);
        let decision = laya
            .decide(
                "the state",
                &[
                    LayaChoice {
                        target: "a".into(),
                        instructions: "why a".into(),
                    },
                    LayaChoice {
                        target: "b".into(),
                        instructions: "why b".into(),
                    },
                ],
            )
            .expect("decide");
        assert_eq!(decision.chosen, "b");
        assert!((decision.confidence - 0.71).abs() < 1e-9);
        assert_eq!(decision.checkpoint, "checkpoint-x");
        let sent = body_of(&handle.join().unwrap());
        assert_eq!(sent["state"]["output"], "the state");
        let q = &sent["questions"]["route"];
        assert_eq!(q["type"], "choice");
        assert_eq!(q["criteria"]["a"], "why a");
        assert_eq!(q["criteria"]["b"], "why b");
        assert_eq!(
            q["criteria"]["other"],
            "none of the options fit this output"
        );
    }

    #[test]
    fn laya_maps_other_to_no_activation() {
        let (base, _h) =
            serve_once(r#"{"answers":{"route":{"choice":"other","confidence":0.2}},"routing":{}}"#);
        let mut laya = LayaClient::at(&base);
        let d = laya
            .decide(
                "s",
                &[LayaChoice {
                    target: "a".into(),
                    instructions: "x".into(),
                }],
            )
            .expect("decide");
        assert_eq!(d.chosen, "");
        assert!((d.confidence - 0.2).abs() < 1e-9);
    }

    #[test]
    fn laya_missing_route_answer_is_an_error() {
        let (base, _h) = serve_once(r#"{"answers":{},"routing":{}}"#);
        let mut laya = LayaClient::at(&base);
        assert_eq!(
            laya.decide("s", &[]).unwrap_err(),
            "laya answer missing 'route' key"
        );
    }
}
