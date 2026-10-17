//! crew.yaml v1 — the Rust port of `crew/internal/schema` (Phase 18D-1).
//!
//! The Go engine (`crew/`, `forseti-crew`) is the compatibility surface while
//! this port is staged: every rule here is checked against the Go engine's
//! output by `scripts/crew-host-oracle.py`, whose goldens live in
//! `host/tests/golden/`. Stage 18D-1 covers the base graph
//! (agents/edges/phases/checks/watch/memory/routes); the Phase-19 layer (roles,
//! SOP patterns, typed pub/sub, verdict edges) parses but is rejected loudly
//! (see `validate::STAGE_2_UNSUPPORTED`) and lands in 18D-2.
//!
//! Deliberate divergences (documented, not oracle-covered): regex-engine error
//! text (Go RE2 vs the Rust `regex` crate) and YAML parse-error text.

pub mod command;
pub mod explain;
pub mod model;
pub mod routes;
pub mod validate;

pub use command::{Outcome, validate};
pub use explain::explain;
pub use model::*;

use regex::Regex;
use std::sync::LazyLock;

/// herdr's name rule: agent names, phase names and crew names.
static NAME_RULE: LazyLock<Regex> =
    LazyLock::new(|| Regex::new(r"^[a-z][a-z0-9_-]{0,31}$").unwrap());
static ROUTE_ID_RULE: LazyLock<Regex> =
    LazyLock::new(|| Regex::new(r"^[a-z0-9][a-z0-9_-]{0,31}$").unwrap());
/// Bounds a memory namespace so `crew-<ns>` stays within the service's limit.
static CREW_NS_RULE: LazyLock<Regex> =
    LazyLock::new(|| Regex::new(r"^[a-z0-9][a-z0-9_-]{0,25}$").unwrap());
static LAYA_WHEN_RULE: LazyLock<Regex> =
    LazyLock::new(|| Regex::new(r"^laya:choice:(.*)$").unwrap());
static VERDICT_WHEN_RULE: LazyLock<Regex> =
    LazyLock::new(|| Regex::new(r"^verdict:([a-z0-9][a-z0-9_-]*)$").unwrap());

/// herdr's node/phase/crew name rule.
pub fn valid_name(name: &str) -> bool {
    NAME_RULE.is_match(name)
}

/// A route id.
pub fn route_id_ok(id: &str) -> bool {
    ROUTE_ID_RULE.is_match(id)
}

/// A memory namespace (`crew-<ns>` must stay within the service's limit).
pub fn crew_ns_ok(ns: &str) -> bool {
    CREW_NS_RULE.is_match(ns)
}

/// Whether an edge's `when` is a laya decision gate.
pub fn is_laya_when(when: &str) -> bool {
    LAYA_WHEN_RULE.is_match(when)
}

/// The choice instructions of a laya gate.
pub fn laya_instructions(when: &str) -> Option<String> {
    LAYA_WHEN_RULE.captures(when).map(|c| c[1].to_string())
}

/// The verdict label a `when: verdict:<label>` edge is gated on.
pub fn verdict_label_of(when: &str) -> Option<String> {
    VERDICT_WHEN_RULE.captures(when).map(|c| c[1].to_string())
}

/// Parses and validates a crew document. Parse errors keep the Go engine's
/// `parse crew.yaml: ` prefix; the message body differs (documented in
/// `validate.rs`).
pub fn load(text: &str) -> Result<Crew, String> {
    let mut crew: Crew =
        serde_norway::from_str(text).map_err(|e| format!("parse crew.yaml: {e}"))?;
    crew.validate()?;
    Ok(crew)
}
