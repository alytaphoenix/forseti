//! Human-readable rendering of a resolved crew (Phase 19/18D), ported from
//! `schema.Explain`. Deterministic: declaration order for nodes, edges as
//! declared (derived/pattern edges appended in expansion order).

use super::model::Crew;
use std::collections::HashMap;

/// Renders the resolved graph after validation: each node's archetype,
/// resolved model/route, phase, artifact subscriptions, and the edges with
/// their gates.
pub fn explain(c: &Crew) -> String {
    let mut b = String::new();
    b.push_str(&format!("crew {}\n", c.name));
    if !c.models.is_empty() {
        b.push_str(&format!("models: {}\n", kv(&c.models)));
    }
    if !c.roles.is_empty() {
        b.push_str(&format!(
            "roles (crew-declared): {}\n",
            role_summaries(&c.roles)
        ));
    }

    let mut phase: HashMap<&str, &str> = HashMap::new();
    for p in &c.phases {
        for n in &p.agents {
            phase.insert(n.as_str(), p.name.as_str());
        }
    }
    let mut incoming: HashMap<&str, Vec<String>> = HashMap::new();
    for e in &c.edges {
        incoming
            .entry(e.to.as_str())
            .or_default()
            .push(edge_label(e));
    }

    b.push_str("nodes:\n");
    for a in &c.agents {
        let mut meta: Vec<String> = Vec::new();
        if !a.role.is_empty() {
            meta.push(format!("role={}", a.role));
        }
        if !a.route.is_empty() {
            meta.push(format!("route={}", a.route));
        } else if !a.model.is_empty() {
            meta.push(format!("model={}", a.model));
        }
        if let Some(p) = phase.get(a.name.as_str()) {
            meta.push(format!("phase={p}"));
        }
        let t = a.writes_type();
        if !t.is_empty() {
            meta.push(format!("writes={t}"));
        }
        if !a.watch.is_empty() {
            meta.push(format!("watch={}", a.watch.join(",")));
        }
        if !incoming.contains_key(a.name.as_str()) {
            meta.push("ENTRY".to_string());
        }
        b.push_str(&format!("  {:<24} {}\n", a.name, meta.join(" ")));
        if let Some(labels) = incoming.get(a.name.as_str()) {
            for l in labels {
                b.push_str(&format!("  {:<24}   ← {}\n", "", l));
            }
        }
    }

    if !c.checks.is_empty() {
        b.push_str("checks:\n");
        for ck in &c.checks {
            b.push_str(&format!("  after {:<20} {}\n", ck.after, ck.run));
        }
    }
    if let Some(m) = &c.memory {
        b.push_str(&format!(
            "memory: namespace={} recall_k={} write_max_chars={}\n",
            c.memory_ns(),
            m.recall_k,
            m.write_max_chars
        ));
    }
    b
}

fn edge_label(e: &super::model::Edge) -> String {
    let mut s = format!("{} [{}]", e.from, e.when);
    if e.is_loop {
        s.push_str(" loop");
    }
    if e.max_visits > 0 {
        s.push_str(&format!(" max_visits={}", e.max_visits));
    }
    s
}

fn kv(m: &std::collections::BTreeMap<String, String>) -> String {
    m.iter()
        .map(|(k, v)| format!("{k}={v}"))
        .collect::<Vec<_>>()
        .join(" ")
}

fn role_summaries(roles: &[super::model::Role]) -> String {
    let mut out: Vec<String> = Vec::with_capacity(roles.len());
    for r in roles {
        let mut bits = vec![r.name.clone()];
        if !r.profile.is_empty() {
            bits.push(format!("profile={}", r.profile));
        }
        if !r.tier.is_empty() {
            bits.push(format!("tier={}", r.tier));
        }
        if !r.actions.is_empty() {
            let names = r
                .actions
                .iter()
                .map(|a| a.name.clone())
                .collect::<Vec<_>>()
                .join(",");
            bits.push(format!("actions={names}"));
        }
        out.push(bits.join(" "));
    }
    out.sort();
    out.join("; ")
}
