//! crew.yaml v1 model (Phases 5 / 11 / 14 / 19), ported from the Go engine.
//!
//! Field names, YAML keys and defaulting semantics follow
//! `crew/internal/schema` exactly — the Go engine is the compatibility oracle
//! for this crate (`scripts/crew-host-oracle.py`, goldens in
//! `host/tests/golden/`). Graph *behaviour* (cycle classification, entry
//! detection) is ported here too, because validation and the scheduler share it.

use serde::{Deserialize, Serialize};
use std::collections::{BTreeMap, HashMap, HashSet};

/// An open-ended YAML value (`params:`, route `options:`). Untyped until a later
/// 18D stage needs the values.
pub type Value = serde_norway::Value;

/// Crew is a deterministic agent graph plus the Phase-19 authoring sugar
/// (roles, SOP patterns) that compiles into it at validate time.
#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct Crew {
    #[serde(default)]
    pub name: String,
    /// tier → provider/model; roles resolve their tier through it (P19A).
    #[serde(default)]
    pub models: BTreeMap<String, String>,
    /// archetype overrides/additions (P19A).
    #[serde(default)]
    pub roles: Vec<Role>,
    /// SOP patterns, instantiated into the graph at validate (P19C).
    #[serde(default)]
    pub patterns: Vec<Pattern>,
    /// switchyard model-routes (optional, P6-D).
    #[serde(default)]
    pub routes: Vec<Route>,
    #[serde(default)]
    pub agents: Vec<Agent>,
    /// ordered work stages with barrier semantics (P11).
    #[serde(default)]
    pub phases: Vec<Phase>,
    #[serde(default)]
    pub edges: Vec<Edge>,
    /// shell assertions (optional, 6A-1).
    #[serde(default)]
    pub checks: Vec<Check>,
    /// output regex watchers (optional, 6C-1).
    #[serde(default)]
    pub watch: Vec<Watcher>,
    /// shared-memory integration (optional, P14).
    #[serde(default)]
    pub memory: Option<MemorySpec>,
}

/// MemorySpec wires the shared memory service into a crew run (P14). Present
/// block = on; auto-recall prefixes recalled facts to each node's prompt, and
/// auto-write posts a node's output back on completion.
#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct MemorySpec {
    #[serde(default)]
    pub namespace: String,
    #[serde(default)]
    pub auto_recall: Option<bool>,
    #[serde(default)]
    pub recall_k: i64,
    #[serde(default)]
    pub auto_write: Option<bool>,
    #[serde(default)]
    pub write_max_chars: i64,
}

impl MemorySpec {
    pub fn auto_recall_on(&self) -> bool {
        self.auto_recall.unwrap_or(true)
    }
    pub fn auto_write_on(&self) -> bool {
        self.auto_write.unwrap_or(true)
    }
}

/// Phase is an ordered stage of work; its agents dispatch only after every node
/// in earlier phases is terminal. Instructions prefix member prompts.
#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct Phase {
    #[serde(default)]
    pub name: String,
    #[serde(default)]
    pub instructions: String,
    #[serde(default)]
    pub agents: Vec<String>,
}

#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct Agent {
    #[serde(default)]
    pub name: String,
    /// currently only "pi".
    #[serde(default)]
    pub kind: String,
    /// archetype from the role library (P19A).
    #[serde(default)]
    pub role: String,
    /// e.g. opencode-go/beam-free.
    #[serde(default)]
    pub model: String,
    /// switchyard route id (mutually exclusive with model).
    #[serde(default)]
    pub route: String,
    /// extra pi argv (appended after --model).
    #[serde(default)]
    pub args: Vec<String>,
    /// artifact types this node reacts to; declaring a watch derives the
    /// incoming edges (typed pub/sub, P19D).
    #[serde(default)]
    pub watch: Vec<String>,
    /// the artifact type this node's output publishes.
    #[serde(default)]
    pub writes: String,
    /// Go text/template: data is the map of upstream node outputs by node name.
    #[serde(default)]
    pub prompt: String,
    /// "off" skips auto-recall AND auto-write for this node (default inherit).
    #[serde(default)]
    pub memory: String,
    /// overrides the auto-recall query for this node.
    #[serde(default)]
    pub memory_query: String,
}

impl Agent {
    pub fn memory_off(&self) -> bool {
        self.memory == "off"
    }

    /// The artifact type a node publishes: the explicit `writes:`, the role's
    /// `writes:` (resolved into `writes` before validation), else the node name.
    pub fn writes_type(&self) -> &str {
        let t = self.writes.trim();
        if t.is_empty() { &self.name } else { t }
    }
}

/// Check is a shell assertion run after its `after` node settles. exit 0 = pass.
#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct Check {
    /// optional; defaults to check-<after>-<i>.
    #[serde(default)]
    pub name: String,
    #[serde(default)]
    pub after: String,
    #[serde(default)]
    pub run: String,
}

/// Watcher arms a pane output regex on a node's pane (advisory).
#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct Watcher {
    #[serde(default)]
    pub node: String,
    #[serde(default, rename = "match")]
    pub match_: String,
}

/// Route declares a switchyard routing algorithm that crew agents attach to with
/// `route: <id>` instead of `model:`.
#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct Route {
    #[serde(default)]
    pub id: String,
    /// switchyard algorithm type; default stage_router.
    #[serde(default, rename = "type")]
    pub type_: String,
    #[serde(default)]
    pub efficient: String,
    #[serde(default)]
    pub capable: String,
    #[serde(default)]
    pub judge: String,
    #[serde(default)]
    pub picker: String,
    #[serde(default)]
    pub confidence: f64,
    /// raw switchyard route keys, emitted verbatim.
    #[serde(default)]
    pub options: BTreeMap<String, Value>,
}

#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct Edge {
    #[serde(default)]
    pub from: String,
    #[serde(default)]
    pub to: String,
    /// "idle" (default), "re:<regex>", "laya:choice:<instructions>", or
    /// "verdict:<label>".
    #[serde(default)]
    pub when: String,
    /// bounds cycles; required for any edge participating in a cycle.
    #[serde(default)]
    pub max_visits: i64,
    /// gates laya edges (default 0.5).
    #[serde(default)]
    pub min_confidence: f64,
    /// when set, the laya gate judges THIS file instead of the pane transcript.
    #[serde(default)]
    pub state_file: String,
    /// marks this edge as the one that CLOSES a cycle (P19).
    #[serde(default, rename = "loop")]
    pub is_loop: bool,
}

impl Edge {
    pub fn is_laya(&self) -> bool {
        crate::crew::is_laya_when(&self.when)
    }
    pub fn laya_instructions(&self) -> String {
        crate::crew::laya_instructions(&self.when).unwrap_or_default()
    }
    pub fn verdict_label_of(&self) -> Option<String> {
        crate::crew::verdict_label_of(&self.when)
    }
}

/// Action is one step of a role's SOP (P19A).
#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct Action {
    #[serde(default)]
    pub name: String,
    #[serde(default)]
    pub desc: String,
    #[serde(default)]
    pub instruction: String,
    /// text | verdict | list
    #[serde(default)]
    pub expected: String,
    /// artifact types consumed
    #[serde(default)]
    pub reads: Vec<String>,
    /// artifact type produced
    #[serde(default)]
    pub writes: String,
}

/// Role is a reusable agent archetype (P19A). A node references one with `role:`
/// and inherits its prefix, tier, actions and subscriptions.
#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct Role {
    #[serde(default)]
    pub name: String,
    #[serde(default)]
    pub profile: String,
    #[serde(default)]
    pub goal: String,
    #[serde(default)]
    pub constraints: String,
    #[serde(default)]
    pub desc: String,
    /// efficient | capable (resolved via the crew `models:` map).
    #[serde(default)]
    pub tier: String,
    #[serde(default)]
    pub watch: Vec<String>,
    #[serde(default)]
    pub writes: String,
    #[serde(default)]
    pub react_mode: String,
    #[serde(default)]
    pub actions: Vec<Action>,
    #[serde(default)]
    pub instruction: String,
}

/// Pattern instantiates a multi-agent SOP over an existing crew (P19C).
#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct Pattern {
    #[serde(default)]
    pub id: String,
    #[serde(default)]
    pub pattern: String,
    #[serde(default)]
    pub subject: String,
    /// part name → declared node
    #[serde(default)]
    pub members: BTreeMap<String, String>,
    /// kind-specific dials
    #[serde(default)]
    pub params: BTreeMap<String, Value>,
    /// `validate` is not re-entrant for expansion (P19C, Go's `expanded`).
    #[serde(skip)]
    pub expanded: bool,
}

impl Crew {
    /// Looks up an agent by name.
    pub fn agent(&self, name: &str) -> Option<&Agent> {
        self.agents.iter().find(|a| a.name == name)
    }

    /// Looks up a route by id.
    pub fn route(&self, id: &str) -> Option<&Route> {
        self.routes.iter().find(|r| r.id == id)
    }

    /// The crew memory namespace (default = the crew name).
    pub fn memory_ns(&self) -> String {
        match &self.memory {
            Some(m) if !m.namespace.is_empty() => m.namespace.clone(),
            _ => self.name.clone(),
        }
    }

    /// Maps every published artifact type to the nodes that write it.
    pub fn producers(&self) -> HashMap<String, Vec<String>> {
        let mut out: HashMap<String, Vec<String>> = HashMap::new();
        for a in &self.agents {
            out.entry(a.writes_type().to_string())
                .or_default()
                .push(a.name.clone());
        }
        out
    }

    /// Adds one edge (producer → watcher) for every artifact type a node
    /// watches; idempotent — an edge that already exists is left alone (P19D).
    pub fn derive_subscriptions(&mut self) -> Result<(), String> {
        if !self.agents.iter().any(|a| !a.watch.is_empty()) {
            return Ok(());
        }
        let producers = self.producers();
        let mut exists: HashSet<String> = self
            .edges
            .iter()
            .map(|e| format!("{}>{}", e.from, e.to))
            .collect();
        let mut added: Vec<Edge> = Vec::new();
        for i in 0..self.agents.len() {
            let a = &self.agents[i];
            for t in &a.watch {
                let t = t.trim();
                if t.is_empty() {
                    return Err(format!("agent {:?}: empty watch artifact type", a.name));
                }
                let ps = producers.get(t).cloned().unwrap_or_default();
                if ps.is_empty() {
                    return Err(format!(
                        "agent {:?}: watches artifact type {:?} but no agent writes it (add `writes: {}` to the producer, or drop the watch)",
                        a.name, t, t
                    ));
                }
                for p in ps {
                    if p == a.name {
                        continue; // a node does not need an edge to itself
                    }
                    let key = format!("{p}>{}", a.name);
                    if !exists.insert(key) {
                        continue;
                    }
                    added.push(Edge {
                        from: p,
                        to: a.name.clone(),
                        when: "idle".to_string(),
                        ..Default::default()
                    });
                }
            }
        }
        self.edges.extend(added);
        Ok(())
    }

    /// Nodes with no incoming edges (the run's starting wave). Unlike the
    /// validation entry check this does NOT ignore `loop:` edges — it is the
    /// runner's entry, which only ever sees a valid crew.
    pub fn entry(&self) -> Vec<String> {
        let incoming: HashSet<&str> = self.edges.iter().map(|e| e.to.as_str()).collect();
        self.agents
            .iter()
            .filter(|a| !incoming.contains(a.name.as_str()))
            .map(|a| a.name.clone())
            .collect()
    }

    /// Edges leaving a node.
    pub fn out_edges(&self, name: &str) -> Vec<Edge> {
        self.edges
            .iter()
            .filter(|e| e.from == name)
            .cloned()
            .collect()
    }

    /// Edges entering a node (the scheduler's fan-in AND set).
    pub fn in_edges(&self, name: &str) -> Vec<Edge> {
        self.edges
            .iter()
            .filter(|e| e.to == name)
            .cloned()
            .collect()
    }

    /// The phase position of an agent, or -1 when the crew declares no phases
    /// (or the agent is unphased — validation rejects that when phases exist).
    pub fn phase_of(&self, agent: &str) -> i32 {
        if self.phases.is_empty() {
            return -1;
        }
        for (i, p) in self.phases.iter().enumerate() {
            if p.agents.iter().any(|m| m == agent) {
                return i as i32;
            }
        }
        -1
    }

    /// The directed cycles in the graph (P13 BUG-11): DFS from the entry nodes
    /// in declaration order, recording the path slice that closes each cycle.
    pub fn find_cycles(&self) -> Vec<Vec<String>> {
        let mut adj: HashMap<&str, Vec<&str>> = HashMap::new();
        for e in &self.edges {
            adj.entry(e.from.as_str()).or_default().push(e.to.as_str());
        }
        let mut color: HashMap<&str, u8> = HashMap::new();
        let mut cycles: Vec<Vec<String>> = Vec::new();
        let mut path: Vec<&str> = Vec::new();

        fn dfs<'a>(
            n: &'a str,
            adj: &HashMap<&'a str, Vec<&'a str>>,
            color: &mut HashMap<&'a str, u8>,
            path: &mut Vec<&'a str>,
            cycles: &mut Vec<Vec<String>>,
        ) {
            color.insert(n, 1);
            path.push(n);
            if let Some(ms) = adj.get(n) {
                for m in ms {
                    match color.get(m).copied().unwrap_or(0) {
                        0 => dfs(m, adj, color, path, cycles),
                        1 => {
                            for (i, p) in path.iter().enumerate() {
                                if p == m {
                                    cycles
                                        .push(path[i..].iter().map(|s| (*s).to_string()).collect());
                                    break;
                                }
                            }
                        }
                        _ => {}
                    }
                }
            }
            path.pop();
            color.insert(n, 2);
        }

        for a in &self.agents {
            if color.get(a.name.as_str()).copied().unwrap_or(0) == 0 {
                dfs(a.name.as_str(), &adj, &mut color, &mut path, &mut cycles);
            }
        }
        cycles
    }

    /// "from>to" keys of edges that close a directed cycle: an explicit
    /// `loop: true`, or a DFS back edge discovered from the crew's entry nodes
    /// (P19's refinement of P13's "target reaches source").
    pub fn back_edges(&self) -> HashSet<String> {
        let mut out: HashSet<String> = HashSet::new();
        let mut adj: HashMap<&str, Vec<&str>> = HashMap::new();
        for e in &self.edges {
            if e.is_loop {
                out.insert(format!("{}>{}", e.from, e.to));
            } else {
                adj.entry(e.from.as_str()).or_default().push(e.to.as_str());
            }
        }
        let roots: Vec<&str> = self
            .agents
            .iter()
            .filter(|a| !self.edges.iter().any(|e| !e.is_loop && e.to == a.name))
            .map(|a| a.name.as_str())
            .collect();
        if !roots.is_empty() {
            for r in roots {
                dfs_back_edges(&adj, r, &mut out);
            }
        } else {
            // no entry (invalid crew): keep the previous, conservative answer.
            for e in &self.edges {
                if reaches(&adj, e.to.as_str(), e.from.as_str()) {
                    out.insert(format!("{}>{}", e.from, e.to));
                }
            }
        }
        out
    }
}

/// Classifies the edges reachable from `root` as tree/forward/cross or back
/// (target on the current path), recording back edges into `out`.
fn dfs_back_edges<'a>(
    adj: &HashMap<&'a str, Vec<&'a str>>,
    root: &'a str,
    out: &mut HashSet<String>,
) {
    fn visit<'a>(
        n: &'a str,
        adj: &HashMap<&'a str, Vec<&'a str>>,
        state: &mut HashMap<&'a str, u8>,
        out: &mut HashSet<String>,
    ) {
        state.insert(n, 1);
        if let Some(ms) = adj.get(n) {
            for m in ms {
                match state.get(m).copied().unwrap_or(0) {
                    1 => {
                        out.insert(format!("{n}>{m}"));
                    }
                    0 => visit(m, adj, state, out),
                    _ => {}
                }
            }
        }
        state.insert(n, 2);
    }
    let mut state: HashMap<&'a str, u8> = HashMap::new();
    visit(root, adj, &mut state, out);
}

/// `reaches` reports whether `to` is reachable from `from` (small graphs).
fn reaches<'a>(adj: &HashMap<&'a str, Vec<&'a str>>, from: &'a str, to: &str) -> bool {
    let mut seen: HashSet<&'a str> = HashSet::new();
    let mut stack: Vec<&'a str> = vec![from];
    while let Some(n) = stack.pop() {
        if let Some(ms) = adj.get(n) {
            for m in ms {
                if *m == to {
                    return true;
                }
                if seen.insert(m) {
                    stack.push(m);
                }
            }
        }
    }
    false
}

#[cfg(test)]
mod tests {
    use super::Crew;

    fn crew(yaml: &str) -> Crew {
        serde_norway::from_str(yaml).expect("parse")
    }

    const DIAMOND: &str = "name: t\nagents:\n  - name: c\n    prompt: x\n  - name: a\n    prompt: x\n  - name: b\n    prompt: x\nedges:\n  - from: c\n    to: a\n  - from: a\n    to: b\n  - from: b\n    to: a\n";

    #[test]
    fn find_cycles_reports_the_cycle_path() {
        let c = crew(DIAMOND);
        assert_eq!(
            c.find_cycles(),
            vec![vec!["a".to_string(), "b".to_string()]]
        );
    }

    #[test]
    fn back_edges_are_dfs_classified_not_all_cycle_edges() {
        // The old "target reaches source" rule marked both cycle edges; DFS from
        // the entry marks only the edge that actually closes the cycle.
        let c = crew(DIAMOND);
        assert_eq!(
            c.back_edges().into_iter().collect::<Vec<_>>(),
            vec!["b>a".to_string()]
        );
    }

    #[test]
    fn explicit_loop_edges_are_back_edges() {
        let c = crew(
            "name: t\nagents:\n  - name: a\n    prompt: x\n  - name: b\n    prompt: x\nedges:\n  - from: a\n    to: b\n  - from: b\n    to: a\n    loop: true\n",
        );
        assert!(c.back_edges().contains("b>a"));
        // Entry() mirrors the Go runner's helper, which counts loop edges as
        // incoming; the *validation* entry check ignores them (see
        // `validate`), which is why the loop edge alone is not a back edge.
        assert!(c.entry().is_empty());
    }

    #[test]
    fn entry_is_declaration_order() {
        let c = crew(DIAMOND);
        assert_eq!(c.entry(), vec!["c".to_string()]);
    }

    #[test]
    fn derive_subscriptions_links_producers_to_watchers() {
        let mut c = crew(
            "name: t\nagents:\n  - name: producer\n    prompt: x\n    writes: plan\n  - name: watcher\n    prompt: y\n    watch: [plan]\n",
        );
        c.derive_subscriptions().expect("link");
        assert_eq!(c.edges.len(), 1);
        assert_eq!(c.edges[0].from, "producer");
        assert_eq!(c.edges[0].to, "watcher");
        assert_eq!(c.edges[0].when, "idle");
        // idempotent
        c.derive_subscriptions().expect("relink");
        assert_eq!(c.edges.len(), 1);
    }

    #[test]
    fn derive_subscriptions_rejects_an_unwritten_type() {
        let mut c =
            crew("name: t\nagents:\n  - name: watcher\n    prompt: y\n    watch: [ghost]\n");
        let e = c.derive_subscriptions().expect_err("ghost type");
        assert!(e.contains("watches artifact type \"ghost\""), "{e}");
    }

    #[test]
    fn writes_type_falls_back_to_the_node_name() {
        let c = crew(
            "name: t\nagents:\n  - name: a\n    prompt: x\n  - name: b\n    prompt: x\n    writes: \" plan \"\n",
        );
        assert_eq!(c.agents[0].writes_type(), "a");
        assert_eq!(c.agents[1].writes_type(), "plan");
    }
}
