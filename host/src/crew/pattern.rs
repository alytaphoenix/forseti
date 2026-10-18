//! SOP patterns (Phases 19C / 18D-2): the Rust port of `pattern.go`.
//!
//! A pattern is a named multi-agent procedure (reviewer loop, security review,
//! adversarial panel + executor). It is instantiated into ordinary nodes and
//! edges, so the P13 scheduler (fan-in AND, bounded back edges, phases), the
//! run log, the TUI and the routing layer all work unchanged.
//!
//! The synthesized prompt templates are consumed by the runner (stage 18D-5),
//! but their construction lives here so the two engines cannot drift.

use super::model::{Agent, Check, Crew, Edge, Pattern, Value};
use std::collections::{BTreeSet, HashMap, HashSet};

/// Reviewer loop: subject → reviewer, and back on LBTM.
pub const PATTERN_REVIEWER_LOOP: &str = "reviewer_loop";
/// Security review: the same shape with PASS/FAIL.
pub const PATTERN_SECURITY_REVIEW: &str = "security_review";
/// Adversarial panel: N critics → judge, back on REVISE.
pub const PATTERN_ADVERSARIAL_PANEL: &str = "adversarial_panel";
/// Executor: implement → verify, back on FAIL, with real shell checks.
pub const PATTERN_EXECUTOR: &str = "executor";

/// The instantiable SOP patterns, in the Go engine's order.
pub const KNOWN_PATTERNS: [&str; 4] = [
    PATTERN_REVIEWER_LOOP,
    PATTERN_SECURITY_REVIEW,
    PATTERN_ADVERSARIAL_PANEL,
    PATTERN_EXECUTOR,
];

struct PendingThen {
    from: String,
    to: String,
    when: String,
    pattern: String,
}

fn phase_of_node(phases: &[super::model::Phase], node: &str) -> i32 {
    if node.is_empty() {
        return -1;
    }
    for (i, p) in phases.iter().enumerate() {
        if p.agents.iter().any(|n| n == node) {
            return i as i32;
        }
    }
    -1
}

/// `verdictLabel("VERDICT: LGTM")` → `lgtm` (Go's `badLabel`).
fn verdict_label(line: &str) -> String {
    let line = line.trim();
    let tail = match line.rfind(':') {
        Some(i) => line[i + 1..].trim(),
        None => line,
    };
    tail.to_lowercase()
}

impl Pattern {
    fn str_param(&self, key: &str, def: &str) -> String {
        if let Some(Value::String(s)) = self.params.get(key) {
            let t = s.trim();
            if !t.is_empty() {
                return t.to_string();
            }
        }
        def.to_string()
    }

    /// An integer dial. Go accepts int/int64/float64 from `map[string]any`.
    fn num_param(&self, key: &str, def: i64) -> i64 {
        match self.params.get(key) {
            Some(Value::Number(n)) => n
                .as_i64()
                .or_else(|| n.as_f64().map(|f| f as i64))
                .unwrap_or(def),
            _ => def,
        }
    }

    /// A string list dial. The default stands when the key is absent, empty, or
    /// not a list of non-empty strings (Go's exact behaviour).
    fn list_param(&self, key: &str, def: Vec<String>) -> Vec<String> {
        match self.params.get(key) {
            Some(Value::Sequence(seq)) if !seq.is_empty() => {
                let mut out = Vec::with_capacity(seq.len());
                for item in seq {
                    match item {
                        Value::String(s) if !s.trim().is_empty() => {
                            out.push(s.trim().to_string());
                        }
                        _ => return def,
                    }
                }
                out
            }
            _ => def,
        }
    }

    /// Renders `params.criteria` as a prompt section, or "" when unset. A
    /// verdict gate can always invent a defect unless the SOP states what
    /// "done" means, so the crew file owns the standard (S33c).
    fn criteria_note(&self) -> String {
        let c = self.str_param("criteria", "");
        if c.is_empty() {
            return String::new();
        }
        format!(
            "\n\nAcceptance criteria — judge against these and nothing else; an objection that does not violate them is out of scope: {c}"
        )
    }

    /// Rejects unknown keys and out-of-range dials, so a typo cannot silently
    /// produce a pattern with default behaviour.
    fn validate_params(&self) -> Result<(), String> {
        let allowed: Vec<&str> = match self.pattern.as_str() {
            PATTERN_REVIEWER_LOOP => vec!["max_revisions", "then", "subject_label", "criteria"],
            PATTERN_SECURITY_REVIEW => vec!["max_revisions", "then", "lenses", "criteria"],
            PATTERN_ADVERSARIAL_PANEL => {
                vec!["critics", "lenses", "max_rounds", "then", "criteria"]
            }
            PATTERN_EXECUTOR => vec!["max_fixes", "then", "checks", "task"],
            _ => Vec::new(),
        };
        for k in self.params.keys() {
            if !allowed.contains(&k.as_str()) {
                let keys: BTreeSet<&str> = allowed.iter().copied().collect();
                let joined: Vec<&str> = keys.into_iter().collect();
                return Err(format!(
                    "pattern {:?} ({}): unknown param {k:?} (allowed: {})",
                    self.id,
                    self.pattern,
                    joined.join(", ")
                ));
            }
        }
        match self.pattern.as_str() {
            PATTERN_REVIEWER_LOOP | PATTERN_SECURITY_REVIEW => {
                let n = self.num_param("max_revisions", 2);
                if !(1..=10).contains(&n) {
                    return Err(format!(
                        "pattern {:?}: max_revisions {n} must be 1..10",
                        self.id
                    ));
                }
            }
            PATTERN_ADVERSARIAL_PANEL => {
                let n = self.num_param("critics", 3);
                if !(2..=7).contains(&n) {
                    return Err(format!(
                        "pattern {:?}: critics {n} must be 2..7 (a panel of one is a reviewer, not a judge)",
                        self.id
                    ));
                }
                let r = self.num_param("max_rounds", 2);
                if !(1..=10).contains(&r) {
                    return Err(format!(
                        "pattern {:?}: max_rounds {r} must be 1..10",
                        self.id
                    ));
                }
            }
            PATTERN_EXECUTOR => {
                let n = self.num_param("max_fixes", 2);
                if !(1..=10).contains(&n) {
                    return Err(format!(
                        "pattern {:?}: max_fixes {n} must be 1..10",
                        self.id
                    ));
                }
                if self.subject.trim().is_empty() && self.str_param("task", "").is_empty() {
                    return Err(format!(
                        "pattern {:?}: executor needs a subject (the node that supplies the task) or params.task",
                        self.id
                    ));
                }
            }
            _ => {}
        }
        Ok(())
    }
}

impl Crew {
    /// Instantiates every pattern into agents/checks/edges. Called at the top of
    /// `validate`, so a pattern cannot produce a crew the ordinary graph rules
    /// never see.
    pub fn expand_patterns(&mut self) -> Result<(), String> {
        if self.patterns.is_empty() {
            return Ok(());
        }
        let mut seen: HashSet<String> = HashSet::new();
        let mut entries: HashMap<String, Vec<String>> = HashMap::new();
        let mut pending: Vec<PendingThen> = Vec::new();

        for i in 0..self.patterns.len() {
            let p = self.patterns[i].clone();
            // pattern ids name synthesized nodes: check the rule and leave
            // room for a "-critic-7" style suffix.
            if !crate::crew::valid_name(&p.id) || p.id.len() > 22 {
                return Err(format!(
                    "pattern {i}: id {:?} must match [a-z][a-z0-9_-]{{0,21}} (it prefixes generated node names)",
                    p.id
                ));
            }
            if !seen.insert(p.id.clone()) {
                return Err(format!("duplicate pattern id {:?}", p.id));
            }
            if !KNOWN_PATTERNS.contains(&p.pattern.as_str()) {
                return Err(format!(
                    "pattern {:?}: unknown pattern {:?} (known: {})",
                    p.id,
                    p.pattern,
                    KNOWN_PATTERNS.join(", ")
                ));
            }
            if p.expanded {
                continue;
            }
            p.validate_params()?;
            if !p.subject.is_empty() && self.agent(&p.subject).is_none() {
                return Err(format!(
                    "pattern {:?}: subject {:?} is not a declared agent",
                    p.id, p.subject
                ));
            }
            for (part, node) in &p.members {
                if self.agent(node).is_none() {
                    return Err(format!(
                        "pattern {:?}: member {part}={:?} is not a declared agent",
                        p.id, node
                    ));
                }
            }
            let before = self.agents.len();
            let entry_nodes = self.instantiate(&p, &mut pending)?;
            if !entry_nodes.is_empty() {
                entries.insert(p.id.clone(), entry_nodes);
            }
            // Phased crews: a synthesized node joins its subject's phase; a
            // subject-less executor joins the last phase.
            if !self.phases.is_empty() && self.agents.len() > before {
                let mut ph = phase_of_node(&self.phases, &p.subject);
                if ph < 0 {
                    ph = self.phases.len() as i32 - 1;
                }
                let idx = ph as usize;
                for j in before..self.agents.len() {
                    let n = self.agents[j].name.clone();
                    self.phases[idx].agents.push(n);
                }
            }
            self.patterns[i].expanded = true;
        }

        // Resolve `then:` chains after every pattern expanded: a target may be
        // a declared agent, a synthesized node, or another pattern's id. A
        // pattern id resolves to its ENTRY nodes, so `then` gates every way
        // into the next stage instead of racing one of them (fan-in AND).
        for t in pending {
            if t.to.trim().is_empty() {
                continue;
            }
            let targets: Vec<String> = if self.agent(&t.to).is_some() {
                vec![t.to.clone()]
            } else {
                match entries.get(&t.to) {
                    Some(e) => e.clone(),
                    None => {
                        return Err(format!(
                            "pattern {:?}: then {:?} is neither a declared agent nor a pattern id",
                            t.pattern, t.to
                        ));
                    }
                }
            };
            for to in targets {
                self.add_edge_checked(Edge {
                    from: t.from.clone(),
                    to,
                    when: t.when.clone(),
                    ..Default::default()
                })?;
            }
        }
        Ok(())
    }

    fn instantiate(
        &mut self,
        p: &Pattern,
        pending: &mut Vec<PendingThen>,
    ) -> Result<Vec<String>, String> {
        match p.pattern.as_str() {
            PATTERN_REVIEWER_LOOP => self.instantiate_review_loop(
                p,
                "reviewer",
                "VERDICT: LGTM",
                "VERDICT: LBTM",
                pending,
            ),
            PATTERN_SECURITY_REVIEW => self.instantiate_review_loop(
                p,
                "security_reviewer",
                "VERDICT: PASS",
                "VERDICT: FAIL",
                pending,
            ),
            PATTERN_ADVERSARIAL_PANEL => self.instantiate_panel(p, pending),
            PATTERN_EXECUTOR => self.instantiate_executor(p, pending),
            other => Err(format!("pattern {:?}: unhandled kind {other:?}", p.id)),
        }
    }

    /// A synthesized node must not silently overwrite a declared one.
    fn add_pattern_agent(&mut self, a: Agent) -> Result<(), String> {
        if self.agent(&a.name).is_some() {
            return Err(format!(
                "pattern node {:?} collides with a declared agent (rename the pattern or its id)",
                a.name
            ));
        }
        self.agents.push(a);
        Ok(())
    }

    fn add_edge_checked(&mut self, mut e: Edge) -> Result<(), String> {
        if e.when.is_empty() {
            e.when = "idle".to_string();
        }
        if self.edges.iter().any(|x| x.from == e.from && x.to == e.to) {
            return Err(format!(
                "edge {}→{} already declared (pattern would duplicate it)",
                e.from, e.to
            ));
        }
        self.edges.push(e);
        Ok(())
    }

    /// `subject → reviewer`, and `reviewer → subject` only on the negative
    /// verdict (bounded). The positive verdict optionally continues to `then`.
    fn instantiate_review_loop(
        &mut self,
        p: &Pattern,
        part: &str,
        good: &str,
        bad: &str,
        pending: &mut Vec<PendingThen>,
    ) -> Result<Vec<String>, String> {
        let reviewer = match p.members.get(part) {
            Some(n) => n.clone(),
            None => {
                let reviewer = format!("{}-{}", p.id, part);
                let (role, instruction) = if p.pattern == PATTERN_SECURITY_REVIEW {
                    let lenses = p.list_param(
                        "lenses",
                        vec![
                            "injection".into(),
                            "secrets".into(),
                            "authz".into(),
                            "data exposure".into(),
                            "unsafe defaults".into(),
                        ],
                    );
                    (
                        "security_reviewer",
                        format!(
                            "Work the lens list in order ({}). Each finding: trigger, impact, severity, fix. If a lens is not applicable to this work, say so.",
                            lenses.join(", ")
                        ),
                    )
                } else {
                    (
                        "reviewer",
                        "List defects worst-first, each with what is wrong, why it matters and the fix. If nothing material is wrong, say so.".to_string(),
                    )
                };
                let what = match p.str_param("subject_label", "") {
                    l if !l.is_empty() => l,
                    _ => format!("the output of node {:?}", p.subject),
                };
                let prompt = format!(
                    "The work under review is {what}:\n\n{{{{ node {:?} }}}}\n\n{instruction}{criteria}\n\nEnd with `{good}` or `{bad}`.",
                    p.subject,
                    criteria = p.criteria_note(),
                );
                self.add_pattern_agent(Agent {
                    name: reviewer.clone(),
                    role: role.to_string(),
                    prompt: prompt.trim().to_string(),
                    ..Default::default()
                })?;
                reviewer
            }
        };
        self.add_edge_checked(Edge {
            from: p.subject.clone(),
            to: reviewer.clone(),
            ..Default::default()
        })?;
        self.add_edge_checked(Edge {
            from: reviewer.clone(),
            to: p.subject.clone(),
            when: format!("verdict:{}", verdict_label(bad)),
            max_visits: p.num_param("max_revisions", 2),
            is_loop: true,
            ..Default::default()
        })?;
        pending.push(PendingThen {
            from: reviewer.clone(),
            to: p.str_param("then", ""),
            when: format!("verdict:{}", verdict_label(good)),
            pattern: p.id.clone(),
        });
        Ok(vec![reviewer])
    }

    fn instantiate_panel(
        &mut self,
        p: &Pattern,
        pending: &mut Vec<PendingThen>,
    ) -> Result<Vec<String>, String> {
        let mut lenses = p.list_param(
            "lenses",
            vec![
                "correctness".into(),
                "failure modes".into(),
                "cost and complexity".into(),
            ],
        );
        let n = p.num_param("critics", 3);
        let mut i = lenses.len() as i64;
        while i < n {
            lenses.push(format!("lens {}: your own strongest objection", i + 1));
            i += 1;
        }
        let criteria = p.criteria_note();
        let mut critics: Vec<String> = Vec::with_capacity(n as usize);
        for idx in 0..n {
            let idx = idx as usize;
            let name = format!("{}-critic-{}", p.id, idx + 1);
            critics.push(name.clone());
            let prompt = format!(
                "Artifact under attack (output of node {:?}):\n\n{{{{ node {:?} }}}}\n\nYour lens: {}{criteria}\n\nAttack it from this lens only: give the strongest concrete counter-case (a scenario where it fails) and how you would demonstrate the failure. End with `VERDICT: ACCEPT` or `VERDICT: REVISE`.",
                p.subject, p.subject, lenses[idx],
            );
            self.add_pattern_agent(Agent {
                name: name.clone(),
                role: "adversarial_reviewer".to_string(),
                prompt,
                ..Default::default()
            })?;
            self.add_edge_checked(Edge {
                from: p.subject.clone(),
                to: name,
                ..Default::default()
            })?;
        }
        let judge = match p.members.get("judge") {
            Some(j) => j.clone(),
            None => {
                let judge = format!("{}-judge", p.id);
                let judge_prompt = format!(
                    "Artifact under judgement (output of node {:?}):\n\n{{{{ node {:?} }}}}\n\nYou sit on a panel. Each position below is one critic's independent attack, from a different lens:\n\n{}{criteria}\n\nDecide. For each position state whether it survives scrutiny and why. Name the findings you accept (they must be addressed before shipping) and the ones you reject (with the reason). End with exactly one line: `VERDICT: ACCEPT` (ship as-is) or `VERDICT: REVISE` (the accepted findings must be addressed).",
                    p.subject,
                    p.subject,
                    join_bindings(&critics),
                );
                self.add_pattern_agent(Agent {
                    name: judge.clone(),
                    role: "judge".to_string(),
                    prompt: judge_prompt,
                    ..Default::default()
                })?;
                judge
            }
        };
        for cr in &critics {
            self.add_edge_checked(Edge {
                from: cr.clone(),
                to: judge.clone(),
                ..Default::default()
            })?;
        }
        self.add_edge_checked(Edge {
            from: judge.clone(),
            to: p.subject.clone(),
            when: format!("verdict:{}", verdict_label("REVISE")),
            max_visits: p.num_param("max_rounds", 2),
            is_loop: true,
            ..Default::default()
        })?;
        pending.push(PendingThen {
            from: judge.clone(),
            to: p.str_param("then", ""),
            when: format!("verdict:{}", verdict_label("ACCEPT")),
            pattern: p.id.clone(),
        });
        Ok(critics)
    }

    fn instantiate_executor(
        &mut self,
        p: &Pattern,
        pending: &mut Vec<PendingThen>,
    ) -> Result<Vec<String>, String> {
        let implementer = match p.members.get("implementer") {
            Some(n) => n.clone(),
            None => {
                let name = format!("{}-implement", p.id);
                let mut prompt = p.str_param("task", "");
                if !p.subject.is_empty() {
                    let binding = format!(
                        "The task (output of node {:?}):\n\n{{{{ node {:?} }}}}\n\n",
                        p.subject, p.subject
                    );
                    prompt = format!("{binding}{prompt}");
                }
                if prompt.is_empty() {
                    prompt = "Implement the task.".to_string();
                }
                self.add_pattern_agent(Agent {
                    name: name.clone(),
                    role: "engineer".to_string(),
                    prompt,
                    ..Default::default()
                })?;
                name
            }
        };
        if !p.subject.is_empty() {
            self.add_edge_checked(Edge {
                from: p.subject.clone(),
                to: implementer.clone(),
                ..Default::default()
            })?;
        }
        let checks = p.list_param("checks", Vec::new());
        let verify = format!("{}-verify", p.id);
        let mut vprompt = format!(
            "Verify the work produced by node \"{implementer}\":\n\n{{{{ node \"{implementer}\" }}}}\n\n"
        );
        if checks.is_empty() {
            vprompt
                .push_str("Run whatever checks the work itself names (its build/test command).\n");
        } else {
            vprompt.push_str("Verification commands:\n");
            vprompt.push_str(&bullet(&checks));
            vprompt.push('\n');
        }
        vprompt.push_str("Quote the exact command and its decisive output lines. Treat an unrunnable command as FAIL. End with `VERDICT: PASS` or `VERDICT: FAIL`.");
        self.add_pattern_agent(Agent {
            name: verify.clone(),
            role: "verifier".to_string(),
            prompt: vprompt,
            ..Default::default()
        })?;
        self.add_edge_checked(Edge {
            from: implementer.clone(),
            to: verify.clone(),
            ..Default::default()
        })?;
        self.add_edge_checked(Edge {
            from: verify.clone(),
            to: implementer.clone(),
            when: format!("verdict:{}", verdict_label("FAIL")),
            max_visits: p.num_param("max_fixes", 2),
            is_loop: true,
            ..Default::default()
        })?;
        for (i, cmd) in checks.iter().enumerate() {
            let name = format!("{}-check-{}", p.id, i + 1);
            self.checks.push(Check {
                name,
                after: implementer.clone(),
                run: cmd.clone(),
            });
        }
        pending.push(PendingThen {
            from: verify.clone(),
            to: p.str_param("then", ""),
            when: format!("verdict:{}", verdict_label("PASS")),
            pattern: p.id.clone(),
        });
        Ok(vec![implementer])
    }
}

fn bullet(items: &[String]) -> String {
    let mut out = String::new();
    for s in items {
        out.push_str(&format!("- {s}\n"));
    }
    out
}

fn join_bindings(nodes: &[String]) -> String {
    let mut out = String::new();
    for n in nodes {
        out.push_str(&format!("- {n}:\n\n{{{{ node {:?} }}}}\n\n", n));
    }
    out
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::crew::load;

    /// A crew with both tiers mapped, so a synthesized role node resolves.
    fn crew(body: &str) -> Crew {
        let src = format!("name: t\nmodels:\n  efficient: e\n  capable: c\n{body}");
        load(&src).unwrap_or_else(|e| panic!("load failed: {e}\n{src}"))
    }

    fn edge<'a>(c: &'a Crew, from: &str, to: &str) -> &'a Edge {
        c.edges
            .iter()
            .find(|e| e.from == from && e.to == to)
            .unwrap_or_else(|| panic!("no edge {from}→{to} in {:?}", c.edges))
    }

    #[test]
    fn reviewer_loop_synthesizes_the_reviewer_and_a_bounded_back_edge() {
        let c = crew(
            "agents:\n  - name: a\n    prompt: x\npatterns:\n  - id: rev\n    pattern: reviewer_loop\n    subject: a\n",
        );
        assert_eq!(
            c.agents.iter().map(|a| a.name.as_str()).collect::<Vec<_>>(),
            ["a", "rev-reviewer"]
        );
        let r = c.agents.iter().find(|a| a.name == "rev-reviewer").unwrap();
        assert_eq!(r.role, "reviewer");
        assert!(r.prompt.contains("{{ node \"a\" }}"), "{}", r.prompt);
        assert!(
            r.prompt
                .ends_with("End with `VERDICT: LGTM` or `VERDICT: LBTM`."),
            "{}",
            r.prompt
        );
        assert_eq!(edge(&c, "a", "rev-reviewer").when, "idle");
        let back = edge(&c, "rev-reviewer", "a");
        assert_eq!(back.when, "verdict:lbtm");
        assert_eq!(back.max_visits, 2);
        assert!(back.is_loop);
    }

    #[test]
    fn panel_and_executor_synthesize_their_roster_and_checks() {
        let c = crew(
            "agents:\n  - name: a\n    prompt: x\npatterns:\n  - id: ap\n    pattern: adversarial_panel\n    subject: a\n    params:\n      critics: 2\n      lenses: [one, two]\n  - id: ex\n    pattern: executor\n    subject: a\n    params:\n      checks: [\"echo ok\", \"true\"]\n",
        );
        assert_eq!(
            c.agents.iter().map(|a| a.name.as_str()).collect::<Vec<_>>(),
            [
                "a",
                "ap-critic-1",
                "ap-critic-2",
                "ap-judge",
                "ex-implement",
                "ex-verify"
            ]
        );
        let loopback = edge(&c, "ap-judge", "a");
        assert_eq!(loopback.when, "verdict:revise");
        assert_eq!(loopback.max_visits, 2);
        assert!(loopback.is_loop);
        assert_eq!(edge(&c, "ex-verify", "ex-implement").when, "verdict:fail");
        let checks: Vec<&str> = c.checks.iter().map(|k| k.name.as_str()).collect();
        assert_eq!(checks, ["ex-check-1", "ex-check-2"]);
        assert!(c.checks.iter().all(|k| k.after == "ex-implement"));
        assert_eq!(c.checks[0].run, "echo ok");
    }

    #[test]
    fn then_chains_to_another_patterns_entry_nodes() {
        let c = crew(
            "agents:\n  - name: a\n    prompt: x\npatterns:\n  - id: rev\n    pattern: reviewer_loop\n    subject: a\n    params:\n      then: ex\n  - id: ex\n    pattern: executor\n    subject: a\n",
        );
        // the executor's entry is its implementer, so the verdict gates both
        // ways into the stage (fan-in AND waits for it).
        assert_eq!(
            edge(&c, "rev-reviewer", "ex-implement").when,
            "verdict:lgtm"
        );
    }

    #[test]
    fn a_synthesized_node_joins_its_subjects_phase() {
        let c = crew(
            "phases:\n  - name: work\n    instructions: go\n    agents: [a]\nagents:\n  - name: a\n    prompt: x\npatterns:\n  - id: rev\n    pattern: reviewer_loop\n    subject: a\n",
        );
        assert_eq!(c.phases[0].agents, ["a", "rev-reviewer"]);
    }

    #[test]
    fn a_declared_member_is_reused_instead_of_synthesized() {
        let c = crew(
            "agents:\n  - name: a\n    prompt: x\n  - name: my_judge\n    role: judge\n    prompt: judge it\npatterns:\n  - id: ap\n    pattern: adversarial_panel\n    subject: a\n    members:\n      judge: my_judge\n",
        );
        assert!(!c.agents.iter().any(|a| a.name == "ap-judge"));
        assert_eq!(edge(&c, "my_judge", "a").when, "verdict:revise");
        assert!(c.agents.iter().all(|a| a.name != "ap-judge"));
    }

    #[test]
    fn criteria_is_rendered_into_every_gated_prompt() {
        let c = crew(
            "agents:\n  - name: a\n    prompt: x\npatterns:\n  - id: rev\n    pattern: reviewer_loop\n    subject: a\n    params:\n      criteria: it must name one file\n",
        );
        let r = c.agents.iter().find(|a| a.name == "rev-reviewer").unwrap();
        assert!(
            r.prompt
                .contains("Acceptance criteria — judge against these and nothing else"),
            "{}",
            r.prompt
        );
        assert!(r.prompt.contains("must name one file"), "{}", r.prompt);
    }
}
