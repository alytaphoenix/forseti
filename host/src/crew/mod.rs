//! crew.yaml v1 — the Rust port of `crew/internal/schema` (Phase 18D).
//!
//! The Go engine (`crew/`, `forseti-crew`) is the compatibility surface while
//! this port is staged: every rule here is checked against the Go engine's
//! output by `scripts/crew-host-oracle.py`, whose goldens live in
//! `host/tests/golden/`.
//!
//! * **18D-1** — the base graph (agents/edges/phases/checks/watch/memory/
//!   routes): `model`, `routes`, `validate`, `explain`, `command`.
//! * **18D-2** — the Phase-19 authoring layer: `role` (the archetype library
//!   and `RolePack.Validate`), `pattern` (SOP patterns compiled into the
//!   graph), `bus` (verdict parsing). `Crew::validate` runs both at the top,
//!   exactly as the Go engine does.
//! * **18D-3** — `herdr`, the herdr JSON socket client (NDJSON over a Unix
//!   socket: dial-per-call one-shots, the one persistent `events.subscribe`
//!   stream, and every typed helper the runner needs).
//! * **18D-4** — `scheduler`, the P13/P19 wave scheduler: fan-in AND, cycle
//!   back edges + `max_visits`, phase barriers, verdict gates, per-visit
//!   checks. Pure decisions — no herdr, no threads; 18D-5 drives it.
//! * **18D-5a** — `template` (the Go `text/template` subset crews use, with the
//!   S32 parse errors verbatim) and `runner` (the `PaneBackend` trait + herdr
//!   impl, and the Go `runNode` path: render → prompt+settle → retry guard →
//!   bus/artifact → events).
//! * **18D-5b** — `run`, the orchestration: the crew tab/pane spine, agent
//!   start + readiness, the wave loop over a `PaneBackend`, the JSONL run log,
//!   the crew-status bridge, teardown, and `forseti-host crew run`. The live
//!   status stream and `watch:` waiters are UI-only and land with the TUI
//!   (18D-6).
//!
//! Deliberate divergences (documented, not oracle-covered): regex-engine error
//! text (Go RE2 vs the Rust `regex` crate) and YAML parse-error text.

pub mod bus;
pub mod command;
pub mod explain;
pub mod herdr;
pub mod live;
pub mod model;
pub mod pattern;
pub mod proxy;
pub mod role;
pub mod routes;
pub mod run;
pub mod runner;
pub mod scheduler;
pub mod services;
pub mod template;
pub mod ttt;
pub mod validate;

pub use command::{Outcome, validate};
pub use explain::explain;
pub use model::*;
pub use run::{Run, RunOutcome};
pub use scheduler::Options;

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
