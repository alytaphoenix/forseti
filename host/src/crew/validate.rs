//! Crew validation (Phase 18D-1): the Rust port of `schema.Crew.Validate`.
//!
//! Error strings are byte-identical to the Go engine's — the oracle harness
//! (`scripts/crew-host-oracle.py`) captures them from the Go binary and pins
//! them in `host/tests/golden/invalid/`. Two documented divergences, both
//! *parse-level* and outside the oracle corpus:
//!   * a bad user regex reports the Rust `regex` crate's message (Go reports
//!     RE2's);
//!   - a malformed YAML document reports serde_norway's message (Go's yaml.v3
//!     text differs) — the `parse crew.yaml: ` prefix is preserved.

use super::model::Crew;
use super::routes::{self, KNOWN_ROUTE_TYPES};
use super::{
    crew_ns_ok, is_laya_when, laya_instructions, route_id_ok, valid_name, verdict_label_of,
};
use regex::Regex;
use std::collections::{HashMap, HashSet};

/// Stage 18D-1 covers the base graph (agents, edges, phases, checks, watch,
/// memory, routes). The Phase-19 authoring layer — `roles:`, `patterns:` and a
/// node's `role:` — parses but is rejected loudly here rather than silently
/// ignored, so a crew can never validate differently in the two engines without
/// the divergence being visible. Stage 18D-2 ports it.
pub const STAGE_2_UNSUPPORTED: &str = "roles:/patterns: are not yet supported by the Rust crew engine (stage 18D-2 of the port) — run the Go engine (`forseti-crew`) for this crew";

const VALID_STATUSES: [&str; 5] = ["idle", "working", "blocked", "done", "unknown"];

impl Crew {
    /// Structural validation, in the Go engine's order: validation expands
    /// patterns and resolves roles before the ordinary graph rules, so a crew
    /// that a pattern or role produced can never escape them.
    pub fn validate(&mut self) -> Result<(), String> {
        if !valid_name(&self.name) {
            return Err(format!(
                "crew name {:?} violates herdr rule [a-z][a-z0-9_-]{{0,31}} (it names run logs and view sources)",
                self.name
            ));
        }
        // 18D-1 scope guard (Go: expandPatterns + RolePack.Validate).
        if !self.roles.is_empty()
            || !self.patterns.is_empty()
            || self.agents.iter().any(|a| !a.role.is_empty())
        {
            return Err(STAGE_2_UNSUPPORTED.to_string());
        }
        // The archetype-resolution rule that survives without the role library:
        // watch/writes are role features, so a role-less node declaring either
        // is a validation error (Go: RolePack.Validate).
        for a in &self.agents {
            if a.role.is_empty() && (!a.writes.is_empty() || !a.watch.is_empty()) {
                return Err(format!(
                    "agent {:?}: watch/writes need a role (or move them into the role archetype)",
                    a.name
                ));
            }
        }
        if self.agents.is_empty() {
            return Err("crew has no agents".to_string());
        }

        let mut route_ids: HashSet<String> = HashSet::new();
        for i in 0..self.routes.len() {
            let r = &mut self.routes[i];
            if !route_id_ok(&r.id) {
                return Err(format!(
                    "route {}: id {:?} must match [a-z0-9][a-z0-9_-]{{0,31}}",
                    i, r.id
                ));
            }
            if !route_ids.insert(r.id.clone()) {
                return Err(format!("duplicate route id {:?}", r.id));
            }
            if r.type_.is_empty() {
                r.type_ = "stage_router".to_string();
            }
            if !routes::known_route_type(&r.type_) {
                return Err(format!(
                    "route {:?}: unknown switchyard type {:?} (known: {})",
                    r.id,
                    r.type_,
                    KNOWN_ROUTE_TYPES.join(", ")
                ));
            }
            let eff_ok = !r.efficient.trim().is_empty();
            let cap_ok = !r.capable.trim().is_empty();
            if routes::route_tier_keys(&r.type_).is_some() && (!eff_ok || !cap_ok) {
                return Err(format!(
                    "route {:?}: type {} needs both efficient and capable",
                    r.id, r.type_
                ));
            }
            match r.type_.as_str() {
                "passthrough" if !cap_ok => {
                    return Err(format!(
                        "route {:?}: type passthrough needs capable (the single target)",
                        r.id
                    ));
                }
                "llm_classifier"
                    if !r.options.contains_key("classifier_target") && r.judge.is_empty() =>
                {
                    return Err(format!(
                        "route {:?}: type llm_classifier needs a judge (judge: provider/model, or options.classifier_target)",
                        r.id
                    ));
                }
                _ => {}
            }
            if routes::option_driven(&r.type_) && r.options.is_empty() {
                return Err(format!(
                    "route {:?}: type {} is option-driven — set options: with its switchyard keys",
                    r.id, r.type_
                ));
            }
            if !r.picker.is_empty() {
                if r.type_ != "stage_router" {
                    return Err(format!(
                        "route {:?}: picker only applies to stage_router",
                        r.id
                    ));
                }
                if !routes::valid_picker(&r.picker) {
                    return Err(format!(
                        "route {:?}: picker {:?} must be efficient_first or capable_first",
                        r.id, r.picker
                    ));
                }
            }
            if r.type_ == "stage_router" {
                // defaults are applied at emission too (route_keys); setting
                // them here keeps a loaded crew self-describing.
                if r.picker.is_empty() {
                    r.picker = "efficient_first".to_string();
                }
                if r.confidence == 0.0 {
                    r.confidence = 0.5;
                }
            }
            if r.confidence != 0.0 && r.type_ != "stage_router" {
                return Err(format!(
                    "route {:?}: confidence only applies to stage_router",
                    r.id
                ));
            }
            if r.confidence != 0.0 && (r.confidence <= 0.0 || r.confidence > 1.0) {
                return Err(format!("route {:?}: confidence must be in (0,1]", r.id));
            }
            r.route_keys()?;
        }

        let mut seen: HashSet<String> = HashSet::new();
        for i in 0..self.agents.len() {
            let a = &mut self.agents[i];
            if !valid_name(&a.name) {
                return Err(format!(
                    "agent {}: name {:?} violates herdr rule [a-z][a-z0-9_-]{{0,31}}",
                    i, a.name
                ));
            }
            if !seen.insert(a.name.clone()) {
                return Err(format!("duplicate agent name {:?}", a.name));
            }
            if a.kind.is_empty() {
                a.kind = "pi".to_string();
            }
            if a.kind != "pi" {
                return Err(format!(
                    "agent {:?}: kind {:?} unsupported (v1: pi only)",
                    a.name, a.kind
                ));
            }
            if !a.model.is_empty() && !a.route.is_empty() {
                return Err(format!("agent {:?}: set model or route, not both", a.name));
            }
            if !a.route.is_empty() && !route_ids.contains(&a.route) {
                return Err(format!("agent {:?}: unknown route {:?}", a.name, a.route));
            }
            if a.prompt.trim().is_empty() && a.role.is_empty() {
                return Err(format!("agent {:?}: empty prompt", a.name));
            }
            if !a.memory.is_empty() && a.memory != "on" && a.memory != "off" {
                return Err(format!(
                    "agent {:?}: memory must be on|off (got {:?})",
                    a.name, a.memory
                ));
            }
        }
        // P19D: typed pub/sub from watch/writes — after role resolution (which
        // fills defaults), before edge validation (so derived edges validate).
        self.derive_subscriptions()?;

        if self.memory.is_some() {
            let ns = self.memory_ns();
            if !crew_ns_ok(&ns) {
                return Err(format!(
                    "memory namespace {:?} must match [a-z0-9][a-z0-9_-]{{0,25}}",
                    ns
                ));
            }
        }
        if let Some(m) = self.memory.as_mut() {
            if m.recall_k < 0 {
                return Err("memory.recall_k must be >= 0".to_string());
            }
            if m.write_max_chars < 0 {
                return Err("memory.write_max_chars must be >= 0".to_string());
            }
            if m.recall_k == 0 {
                m.recall_k = 3;
            }
            if m.write_max_chars == 0 {
                m.write_max_chars = 4000;
            }
        }

        let mut phase_seen: HashSet<String> = HashSet::new();
        let mut membership: HashMap<String, String> = HashMap::new();
        for i in 0..self.phases.len() {
            let p = &self.phases[i];
            if !valid_name(&p.name) {
                return Err(format!(
                    "phase {}: name {:?} violates herdr rule [a-z][a-z0-9_-]{{0,31}}",
                    i, p.name
                ));
            }
            if !phase_seen.insert(p.name.clone()) {
                return Err(format!("duplicate phase name {:?}", p.name));
            }
            if p.agents.is_empty() {
                return Err(format!("phase {:?}: no agents", p.name));
            }
            if p.instructions.trim().is_empty() {
                return Err(format!("phase {:?}: empty instructions", p.name));
            }
            for m in &p.agents {
                if !seen.contains(m) {
                    return Err(format!("phase {:?}: agent {:?} is not declared", p.name, m));
                }
                if let Some(prev) = membership.get(m) {
                    return Err(format!(
                        "agent {:?}: already in phase {:?} (each agent lives in at most one phase)",
                        m, prev
                    ));
                }
                membership.insert(m.clone(), p.name.clone());
            }
        }
        if !self.phases.is_empty() {
            for a in &self.agents {
                if !membership.contains_key(&a.name) {
                    return Err(format!(
                        "agent {:?}: not in any phase (phases are declared — add it to one)",
                        a.name
                    ));
                }
            }
        }

        for i in 0..self.checks.len() {
            let ck = &mut self.checks[i];
            if !seen.contains(&ck.after) {
                return Err(format!("check {}: after {:?} is not an agent", i, ck.after));
            }
            if ck.run.trim().is_empty() {
                return Err(format!("check {}: empty run", i));
            }
            if ck.name.is_empty() {
                ck.name = format!("check-{}-{}", ck.after, i);
            }
        }

        for i in 0..self.watch.len() {
            let w = &self.watch[i];
            if !seen.contains(&w.node) {
                return Err(format!("watch {}: node {:?} is not an agent", i, w.node));
            }
            if let Err(e) = Regex::new(&w.match_) {
                return Err(format!("watch {}: bad regex {:?}: {}", i, w.match_, e));
            }
        }

        for i in 0..self.edges.len() {
            let e = &mut self.edges[i];
            if !seen.contains(&e.from) {
                return Err(format!("edge {}: unknown from {:?}", i, e.from));
            }
            if !seen.contains(&e.to) {
                return Err(format!("edge {}: unknown to {:?}", i, e.to));
            }
            if e.from == e.to && e.max_visits == 0 {
                return Err(format!(
                    "edge {}: self-loop on {:?} needs max_visits",
                    i, e.from
                ));
            }
            if e.is_loop && e.max_visits == 0 {
                return Err(format!(
                    "edge {}: loop edge {}→{} needs max_visits (it re-dispatches the target)",
                    i, e.from, e.to
                ));
            }
            if e.when.is_empty() {
                e.when = "idle".to_string();
            }
            if is_laya_when(&e.when) {
                if laya_instructions(&e.when)
                    .map(|s| s.trim().is_empty())
                    .unwrap_or(true)
                {
                    return Err(format!("edge {}: empty laya instructions", i));
                }
                if e.min_confidence == 0.0 {
                    e.min_confidence = 0.5;
                }
                if e.min_confidence <= 0.0 || e.min_confidence > 1.0 {
                    return Err(format!("edge {}: min_confidence must be in (0,1]", i));
                }
            } else if e.min_confidence != 0.0 || !e.state_file.is_empty() {
                return Err(format!(
                    "edge {}: min_confidence/state_file only apply to laya edges",
                    i
                ));
            }
            if !e.when.starts_with("re:")
                && !VALID_STATUSES.contains(&e.when.as_str())
                && !is_laya_when(&e.when)
                && verdict_label_of(&e.when).is_none()
            {
                return Err(format!(
                    "edge {}: when {:?} not a status, re:<regex>, laya:choice:<instructions>, or verdict:<label>",
                    i, e.when
                ));
            }
            if let Some(re) = e.when.strip_prefix("re:") {
                Regex::new(re).map_err(|err| format!("edge {}: bad regex {:?}: {}", i, re, err))?;
            }
            if !self.phases.is_empty() {
                let phase_of = |n: &str| {
                    index_phase_of(
                        &self.phases,
                        membership.get(n).map(|s| s.as_str()).unwrap_or(""),
                    )
                };
                let (fo, fi) = (phase_of(&e.from), phase_of(&e.to));
                if fo > fi {
                    return Err(format!(
                        "edge {}: {:?}→{:?} goes backward (phase {:?} → {:?}) — cross-phase loop-backs unsupported in v1",
                        i,
                        e.from,
                        e.to,
                        membership.get(&e.from).cloned().unwrap_or_default(),
                        membership.get(&e.to).cloned().unwrap_or_default()
                    ));
                }
            }
        }
        // P13 BUG-12: duplicate (from,to) edges collide on the scheduler key.
        let mut edge_seen: HashSet<String> = HashSet::new();
        for i in 0..self.edges.len() {
            let key = format!("{}>{}", self.edges[i].from, self.edges[i].to);
            if !edge_seen.insert(key.clone()) {
                return Err(format!(
                    "edge {}: duplicate edge {} (same from→to declared twice)",
                    i, key
                ));
            }
        }
        // P13 BUG-12: an all-cycle crew has no entry — nothing can start.
        let mut entry = false;
        for a in &self.agents {
            let has_incoming = self.edges.iter().any(|e| !e.is_loop && e.to == a.name);
            if !has_incoming {
                entry = true;
                break;
            }
        }
        if !entry {
            return Err(
                "crew has no entry node (every agent has an incoming edge) — nothing can start; an all-cycle crew needs an entry agent with no incoming edges"
                    .to_string(),
            );
        }
        // cycles require at least one bounded edge ON each cycle.
        for cyc in self.find_cycles() {
            let bounded = self
                .edges
                .iter()
                .any(|e| in_cycle(&cyc, e) && e.max_visits > 0);
            if !bounded {
                return Err(format!(
                    "cycle {} has no edge with max_visits — refusing unbounded loop",
                    cyc.join(" → ")
                ));
            }
        }
        Ok(())
    }
}

/// True only for edges ON the cycle — both endpoints in the cycle AND
/// consecutive along it (P13 BUG-11).
pub fn in_cycle(cyc: &[String], e: &super::model::Edge) -> bool {
    if cyc.is_empty() {
        return false;
    }
    for (i, p) in cyc.iter().enumerate() {
        let next = &cyc[(i + 1) % cyc.len()];
        if &e.from == p && &e.to == next {
            return true;
        }
    }
    false
}

/// The index of the phase with this name, or -1.
pub fn index_phase_of<'a, I>(phases: I, name: &str) -> i32
where
    I: IntoIterator<Item = &'a super::model::Phase>,
{
    for (i, p) in phases.into_iter().enumerate() {
        if p.name == name {
            return i as i32;
        }
    }
    -1
}

#[cfg(test)]
mod tests {
    use super::STAGE_2_UNSUPPORTED;
    use crate::crew::{Crew, load};

    fn err(yaml: &str) -> String {
        load(yaml).expect_err("should be invalid")
    }

    fn ok(yaml: &str) -> Crew {
        load(yaml).unwrap_or_else(|e| panic!("should be valid: {e}"))
    }

    #[test]
    fn stage_2_features_are_rejected_loudly() {
        // A crew file that needs the Phase-19 layer must never validate as if
        // the roles/patterns simply were not there.
        let roles = "name: t\nroles:\n  - name: r\n    profile: R\n    tier: capable\nagents:\n  - name: a\n    prompt: x\n";
        assert_eq!(err(roles), STAGE_2_UNSUPPORTED);

        let patterns = "name: t\npatterns:\n  - id: rl\n    pattern: reviewer_loop\n    subject: a\nagents:\n  - name: a\n    prompt: x\n";
        assert_eq!(err(patterns), STAGE_2_UNSUPPORTED);

        let agent_role = "name: t\nagents:\n  - name: a\n    role: reviewer\n    prompt: x\n";
        assert_eq!(err(agent_role), STAGE_2_UNSUPPORTED);
    }

    #[test]
    fn regex_engine_errors_are_reported_as_errors() {
        // Go RE2 and the Rust regex crate word these differently, so the
        // message body is not oracle-pinned — but the rule must hold.
        let w =
            "name: t\nagents:\n  - name: a\n    prompt: x\nwatch:\n  - node: a\n    match: \"[\"\n";
        let e = err(w);
        assert!(e.starts_with("watch 0: bad regex \"[\": "), "{e}");

        let re = "name: t\nagents:\n  - name: a\n    prompt: x\n  - name: b\n    prompt: y\nedges:\n  - from: a\n    to: b\n    when: \"re:[\"\n";
        let e = err(re);
        assert!(e.starts_with("edge 0: bad regex \"[\": "), "{e}");
    }

    #[test]
    fn valid_when_forms_are_accepted() {
        for w in [
            "idle",
            "working",
            "blocked",
            "done",
            "unknown",
            "re:^READY$",
        ] {
            let y = format!(
                "name: t\nagents:\n  - name: a\n    prompt: x\n  - name: b\n    prompt: y\nedges:\n  - from: a\n    to: b\n    when: {w}\n"
            );
            let c = ok(&y);
            assert_eq!(c.edges[0].when, w);
        }
        // a laya gate defaults min_confidence, a verdict gate is a plain label
        let laya = ok(
            "name: t\nagents:\n  - name: a\n    prompt: x\n  - name: b\n    prompt: y\nedges:\n  - from: a\n    to: b\n    when: \"laya:choice:is it done\"\n",
        );
        assert_eq!(laya.edges[0].min_confidence, 0.5);
        let verdict = ok(
            "name: t\nagents:\n  - name: a\n    prompt: x\n  - name: b\n    prompt: y\nedges:\n  - from: a\n    to: b\n    when: \"verdict:lgtm\"\n",
        );
        assert_eq!(
            crate::crew::verdict_label_of(&verdict.edges[0].when).as_deref(),
            Some("lgtm")
        );
    }

    #[test]
    fn defaults_are_applied_like_the_go_engine() {
        let c = ok(
            "name: t\nagents:\n  - name: a\n    prompt: x\n  - name: b\n    prompt: y\nedges:\n  - from: a\n    to: b\n\nchecks:\n  - after: a\n    run: \"true\"\nmemory: {}\n",
        );
        assert_eq!(c.agents[0].kind, "pi");
        assert_eq!(c.edges[0].when, "idle");
        assert_eq!(c.checks[0].name, "check-a-0");
        assert_eq!(c.memory.as_ref().unwrap().recall_k, 3);
        assert_eq!(c.memory.as_ref().unwrap().write_max_chars, 4000);
        assert_eq!(c.memory_ns(), "t");
    }

    #[test]
    fn self_loop_with_max_visits_is_legal() {
        // A self-loop counts as an incoming edge, so the crew still needs a
        // separate entry node — same as the Go engine.
        let c = ok(
            "name: t\nagents:\n  - name: start\n    prompt: x\n  - name: a\n    prompt: x\nedges:\n  - from: start\n    to: a\n  - from: a\n    to: a\n    max_visits: 2\n",
        );
        assert_eq!(c.edges[1].max_visits, 2);

        // ...and a lone self-loop has no entry at all.
        let e = err(
            "name: t\nagents:\n  - name: a\n    prompt: x\nedges:\n  - from: a\n    to: a\n    max_visits: 2\n",
        );
        assert!(e.starts_with("crew has no entry node"), "{e}");
    }
}
