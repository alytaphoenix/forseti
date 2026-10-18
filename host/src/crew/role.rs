//! Role archetypes (Phases 19A / 18D-2): the Rust port of `role.go`.
//!
//! Source of the design (MetaGPT is inspiration + attribution — nothing is
//! vendored): a Role is `{name, profile, goal, constraints, actions[]}` and its
//! `Prefix` is the system scaffold installed on every action the role runs. The
//! library of built-in archetypes is byte-identical to the Go engine's; a crew
//! may add or override an archetype by re-declaring its name.
//!
//! The Go engine is the compatibility oracle (`scripts/crew-host-oracle.py`),
//! so every message here is the Go `RolePack.Validate` string.

use super::model::{Crew, Role};
use std::collections::BTreeMap;

/// A model class a role runs on; the crew maps tiers to concrete
/// provider/model strings in its `models:` block.
pub const TIER_EFFICIENT: &str = "efficient";
/// The other tier: capable.
pub const TIER_CAPABLE: &str = "capable";

/// Valid tiers, in the Go engine's order.
pub const VALID_TIERS: [&str; 2] = [TIER_EFFICIENT, TIER_CAPABLE];

/// Valid react modes.
pub const VALID_REACT_MODES: [&str; 2] = ["one_shot", "by_order"];

impl Role {
    /// MetaGPT's role prefix (the system scaffold on every action). `name` is
    /// the concrete node name, not the archetype id.
    pub fn prefix(&self, name: &str) -> String {
        let profile = if self.profile.is_empty() {
            &self.name
        } else {
            &self.profile
        };
        let mut b = String::new();
        b.push_str(&format!(
            "You are a {}, named {}, your goal is {}. ",
            profile,
            name,
            self.goal.trim()
        ));
        let c = self.constraints.trim();
        if !c.is_empty() {
            b.push_str(&format!("the constraint is {}. ", c));
        }
        b
    }
}

fn action(
    name: &str,
    desc: &str,
    instruction: &str,
    expected: &str,
    reads: &[&str],
    writes: &str,
) -> super::model::Action {
    super::model::Action {
        name: name.to_string(),
        desc: desc.to_string(),
        instruction: instruction.to_string(),
        expected: expected.to_string(),
        reads: reads.iter().map(|s| (*s).to_string()).collect(),
        writes: writes.to_string(),
    }
}

/// forseti's own SOP library — the archetypes the patterns and crews
/// instantiate. Transcribed from `role.go` (the Go engine owns the text; this
/// port must not drift, and the runner's live smoke gate is the backstop).
pub fn builtin_roles() -> BTreeMap<String, Role> {
    let mut m: BTreeMap<String, Role> = BTreeMap::new();

    m.insert(
        "planner".into(),
        Role {
            name: "planner".into(),
            profile: "Planner".into(),
            goal: "turn the request into an ordered, verifiable plan".into(),
            constraints: "no implementation, no restating of the request, no prose padding — output a numbered plan whose every step names its verification".into(),
            desc: "Decomposes a goal into steps; the entry role of an SOP.".into(),
            tier: TIER_CAPABLE.into(),
            react_mode: "one_shot".into(),
            writes: "plan".into(),
            actions: vec![action(
                "plan",
                "write the plan",
                "Return a numbered plan. Each item: what to do, and how it will be verified.",
                "list",
                &[],
                "plan",
            )],
            instruction: "Write the plan for the request below. Keep it minimal and ordered; every step must be checkable.".into(),
            ..Default::default()
        },
    );

    m.insert(
        "researcher".into(),
        Role {
            name: "researcher".into(),
            profile: "Researcher".into(),
            goal: "gather and synthesise verified information".into(),
            constraints: "cite the source of each claim; separate fact from inference; state what you could not verify".into(),
            desc: "BY_ORDER: ask questions → search/summarise → synthesise (MetaGPT Researcher).".into(),
            tier: TIER_EFFICIENT.into(),
            react_mode: "by_order".into(),
            writes: "research".into(),
            actions: vec![
                action(
                    "questions",
                    "list the questions worth answering",
                    "List the specific questions this topic raises, most load-bearing first.",
                    "list",
                    &[],
                    "",
                ),
                action(
                    "gather",
                    "gather evidence per question",
                    "For each question above, give the evidence you have and its source. Mark anything uncertain.",
                    "",
                    &["questions"],
                    "",
                ),
                action(
                    "synthesise",
                    "synthesise the findings",
                    "Synthesise the gathered evidence into a short brief: findings, confidence, open questions.",
                    "text",
                    &["gather"],
                    "research",
                ),
            ],
            instruction: "Research the topic below and report findings.".into(),
            ..Default::default()
        },
    );

    m.insert(
        "engineer".into(),
        Role {
            name: "engineer".into(),
            profile: "Engineer".into(),
            goal: "implement exactly what was asked, so that it works".into(),
            constraints: "smallest correct change; no unrequested refactors; say which checks you ran and how they ended".into(),
            desc: "The executor archetype: implement, then hand to a verifier.".into(),
            tier: TIER_CAPABLE.into(),
            react_mode: "one_shot".into(),
            writes: "implementation".into(),
            actions: vec![action(
                "implement",
                "make the change",
                "Make the change. Report the files touched, what you changed, and the exact commands you ran.",
                "",
                &[],
                "implementation",
            )],
            instruction: "Implement the task. Prefer the smallest correct change.".into(),
            ..Default::default()
        },
    );

    m.insert(
        "reviewer".into(),
        Role {
            name: "reviewer".into(),
            profile: "Reviewer".into(),
            goal: "find the real defects before they ship".into(),
            constraints: "be concrete: quote the artifact, name the failure mode, say why it matters — no vague praise; end with a VERDICT line".into(),
            desc: "Critical review pass (MetaGPT WritePRDReview/DesignReview).".into(),
            tier: TIER_CAPABLE.into(),
            react_mode: "one_shot".into(),
            writes: "review".into(),
            actions: vec![action(
                "review",
                "review the artifact",
                "Review the artifact. List defects worst-first (each: what is wrong, why it matters, the fix). If nothing material is wrong, say so plainly. End with exactly one line: `VERDICT: LGTM` or `VERDICT: LBTM`.",
                "verdict",
                &[],
                "review",
            )],
            instruction: "Review the artifact below against what it is supposed to achieve.".into(),
            ..Default::default()
        },
    );

    m.insert(
        "security_reviewer".into(),
        Role {
            name: "security_reviewer".into(),
            profile: "Security reviewer".into(),
            goal: "find weakness an attacker would exploit".into(),
            constraints: "work the lens list in order; each finding needs a concrete trigger and an impact; rate severity (critical/high/medium/low); end with a VERDICT line".into(),
            desc: "Security review pass: injection, secrets, authz, data exposure, unsafe defaults.".into(),
            tier: TIER_CAPABLE.into(),
            react_mode: "one_shot".into(),
            writes: "security_review".into(),
            actions: vec![action(
                "threat_review",
                "review for exploitable weakness",
                "Review the artifact for security defects through these lenses, in order: (1) injection (shell/SQL/path/format), (2) secrets and credentials handling, (3) authentication and authorization, (4) data exposure and logging, (5) unsafe defaults and missing bounds. For each finding give: trigger, impact, severity, fix. Explicitly list what you checked and found clean. End with `VERDICT: PASS` or `VERDICT: FAIL`.",
                "verdict",
                &[],
                "security_review",
            )],
            instruction: "Security-review the artifact below. Assume an adversary reads it as an attack plan.".into(),
            ..Default::default()
        },
    );

    m.insert(
        "adversarial_reviewer".into(),
        Role {
            name: "adversarial_reviewer".into(),
            profile: "Adversary".into(),
            goal: "break the artifact, not bless it".into(),
            constraints: "argue the strongest case AGAINST the artifact; never agree with it without an attempt to falsify it; stay inside your assigned lens; end with a VERDICT line".into(),
            desc: "Adversarial critic for a panel (one lens per instance).".into(),
            tier: TIER_CAPABLE.into(),
            react_mode: "one_shot".into(),
            writes: "critique".into(),
            actions: vec![action(
                "attack",
                "attack the artifact in one lens",
                "Attack the artifact from your assigned lens only. Give the strongest concrete counter-case: a scenario where it fails, plus how you would demonstrate the failure. Do not soften it. End with `VERDICT: ACCEPT` or `VERDICT: REVISE` (ACCEPT = your lens finds nothing that must change before shipping).",
                "verdict",
                &[],
                "critique",
            )],
            instruction: "Governed by the lens stated above. Attack the artifact below.".into(),
            ..Default::default()
        },
    );

    m.insert(
        "judge".into(),
        Role {
            name: "judge".into(),
            profile: "Judge".into(),
            goal: "decide between competing positions on the evidence".into(),
            constraints: "weigh each position's evidence, not its confidence; say what you are rejecting and why; one decision, stated once; end with a VERDICT line".into(),
            desc: "Consolidates a panel of reviews/critiques into one decision.".into(),
            tier: TIER_CAPABLE.into(),
            react_mode: "one_shot".into(),
            writes: "judgement".into(),
            actions: vec![action(
                "decide",
                "decide and justify",
                "Decide. For each position: what it found, whether it survives scrutiny. Name the findings you accept (they must be addressed) and the ones you reject (with the reason). End with exactly one line: `VERDICT: ACCEPT` (ship as-is) or `VERDICT: REVISE` (the accepted findings must be addressed).",
                "verdict",
                &[],
                "judgement",
            )],
            instruction: "Judge the positions below against the artifact they concern.".into(),
            ..Default::default()
        },
    );

    m.insert(
        "verifier".into(),
        Role {
            name: "verifier".into(),
            profile: "Verifier".into(),
            goal: "establish whether the work actually passes its own checks".into(),
            constraints: "run the real commands; report the exit status and the decisive output lines verbatim; do not fix anything; end with a VERDICT line".into(),
            desc: "Executor loop gate: runs the checks and reports PASS/FAIL.".into(),
            tier: TIER_EFFICIENT.into(),
            react_mode: "one_shot".into(),
            writes: "verification".into(),
            actions: vec![action(
                "verify",
                "run the checks",
                "Run the verification commands named above. Quote the exact command and its decisive output. If a command cannot run (missing tool/binary), say so and treat it as FAIL. End with exactly one line: `VERDICT: PASS` or `VERDICT: FAIL`.",
                "verdict",
                &[],
                "verification",
            )],
            instruction: "Verify the work below by running its checks.".into(),
            ..Default::default()
        },
    );

    m
}

/// The resolved archetype set for one crew: built-ins, overlaid by the crew's
/// own `roles:` declarations (same name = override).
pub struct RolePack {
    roles: BTreeMap<String, Role>,
    names: Vec<String>,
}

impl RolePack {
    /// Builds the pack for a crew (built-ins, then inline overrides). A crew
    /// that re-declares a name owns the whole archetype.
    pub fn new(inline: &[Role]) -> Self {
        let mut roles = builtin_roles();
        for r in inline {
            roles.insert(r.name.clone(), r.clone());
        }
        let names = roles.keys().cloned().collect();
        Self { roles, names }
    }

    /// Every archetype in the pack, sorted (built-ins + crew declarations).
    pub fn names(&self) -> &[String] {
        &self.names
    }

    /// Resolves one archetype.
    pub fn get(&self, name: &str) -> Option<&Role> {
        self.roles.get(name)
    }

    /// The "known roles" list for error messages.
    pub fn known(&self) -> String {
        self.names.join(", ")
    }

    /// Resolves the archetype a node runs as (None for a role-less node).
    pub fn role_of(&self, role: &str) -> Option<&Role> {
        if role.is_empty() {
            None
        } else {
            self.roles.get(role)
        }
    }

    /// Renders the node's prompt with the archetype prefix in front. A
    /// role-less node is returned unchanged, so existing crews render exactly
    /// as before (MetaGPT `Role._get_prefix` → `Action.set_prefix`).
    pub fn role_prompt(&self, a: &super::model::Agent, prompt: &str) -> String {
        let Some(r) = self.role_of(&a.role) else {
            return prompt.to_string();
        };
        let body = if prompt.trim().is_empty() {
            r.instruction.as_str()
        } else {
            prompt
        };
        format!("{}{}\n\n{}", r.prefix(&a.name), "", body)
    }

    /// Applies the archetype defaults to every node that declares `role:`, and
    /// rejects unknown archetypes, bad tiers/react modes. Explicit node fields
    /// win.
    pub fn validate(&self, c: &mut Crew) -> Result<(), String> {
        for i in 0..c.agents.len() {
            let name = c.agents[i].name.clone();
            let role = c.agents[i].role.clone();
            if role.is_empty() {
                if !c.agents[i].writes.is_empty() || !c.agents[i].watch.is_empty() {
                    return Err(format!(
                        "agent {name:?}: watch/writes need a role (or move them into the role archetype)"
                    ));
                }
                continue;
            }
            let Some(r) = self.roles.get(&role) else {
                return Err(format!(
                    "agent {name:?}: unknown role {role:?} (known: {})",
                    self.known()
                ));
            };
            if !r.tier.is_empty() {
                if !VALID_TIERS.contains(&r.tier.as_str()) {
                    return Err(format!(
                        "role {:?}: tier {:?} must be efficient or capable",
                        r.name, r.tier
                    ));
                }
                // tier → model via the crew's models map; a node's explicit
                // model/route always wins (MetaGPT action-level llm_name_or_type).
                if c.agents[i].model.is_empty() && c.agents[i].route.is_empty() {
                    let m = c.models.get(&r.tier).map(|s| s.trim()).unwrap_or("");
                    if m.is_empty() {
                        return Err(format!(
                            "agent {name:?}: role {role:?} needs tier {:?} — set models: {{{}: provider/model}} or give the node an explicit model/route",
                            r.tier, r.tier
                        ));
                    }
                    c.agents[i].model = m.to_string();
                }
            }
            if !r.react_mode.is_empty() && !VALID_REACT_MODES.contains(&r.react_mode.as_str()) {
                return Err(format!(
                    "role {:?}: react_mode {:?} must be one_shot or by_order",
                    r.name, r.react_mode
                ));
            }
            if c.agents[i].writes.is_empty() {
                c.agents[i].writes = r.writes.clone();
            }
            if c.agents[i].watch.is_empty() {
                c.agents[i].watch = r.watch.clone();
            }
            for act in &r.actions {
                if !matches!(act.expected.as_str(), "" | "text" | "verdict" | "list") {
                    return Err(format!(
                        "role {:?} action {:?}: expected {:?} must be text, verdict or list",
                        r.name, act.name, act.expected
                    ));
                }
            }
            if c.agents[i].prompt.trim().is_empty() && r.instruction.trim().is_empty() {
                return Err(format!(
                    "agent {:?}: no prompt and role {:?} has no instruction to fall back on",
                    name, role
                ));
            }
        }
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::crew::load;

    fn crew(yaml: &str) -> Crew {
        load(yaml).unwrap_or_else(|e| panic!("should be valid: {e}"))
    }

    fn err(yaml: &str) -> String {
        load(yaml).expect_err("should be invalid")
    }

    #[test]
    fn prefix_is_metagpts_scaffold() {
        let r = builtin_roles().remove("reviewer").unwrap();
        let p = r.prefix("rev-1");
        assert!(p.starts_with(
            "You are a Reviewer, named rev-1, your goal is find the real defects before they ship. "
        ));
        assert!(p.contains("the constraint is be concrete:"));
        assert!(p.ends_with("end with a VERDICT line. "));
    }

    #[test]
    fn an_unknown_role_lists_the_known_ones() {
        let e = err("name: t\nagents:\n  - name: a\n    role: ghost\n    prompt: x\n");
        assert!(
            e.starts_with("agent \"a\": unknown role \"ghost\" (known: "),
            "{e}"
        );
        assert!(e.contains("researcher"), "{e}");
    }

    #[test]
    fn a_role_resolves_its_tier_and_subscriptions() {
        let c = crew(
            "name: t\nmodels:\n  capable: opencode-go/beam-free\nagents:\n  - name: a\n    role: reviewer\n",
        );
        assert_eq!(c.agents[0].model, "opencode-go/beam-free");
        assert_eq!(c.agents[0].writes, "review");
    }

    #[test]
    fn a_tierless_crew_rejects_the_role() {
        let e = err("name: t\nagents:\n  - name: a\n    role: reviewer\n");
        assert!(
            e.starts_with("agent \"a\": role \"reviewer\" needs tier \"capable\" — set models:"),
            "{e}"
        );
    }

    #[test]
    fn an_explicit_model_wins_over_the_tier() {
        let c = crew(
            "name: t\nagents:\n  - name: a\n    role: reviewer\n    model: opencode-go/beam-free\n",
        );
        assert_eq!(c.agents[0].model, "opencode-go/beam-free");
    }

    #[test]
    fn watch_or_writes_without_a_role_is_rejected() {
        let e = err("name: t\nagents:\n  - name: a\n    prompt: x\n    writes: plan\n");
        assert!(
            e.starts_with("agent \"a\": watch/writes need a role"),
            "{e}"
        );
    }

    #[test]
    fn role_errors_mirror_the_go_engine() {
        let tier = err(
            "name: t\nroles:\n  - name: r\n    profile: R\n    tier: fast\nagents:\n  - name: a\n    role: r\n    prompt: x\n",
        );
        assert_eq!(
            tier,
            "role \"r\": tier \"fast\" must be efficient or capable"
        );

        let mode = err(
            "name: t\nroles:\n  - name: r\n    profile: R\n    react_mode: loop\n    instruction: go\nagents:\n  - name: a\n    role: r\n",
        );
        assert_eq!(
            mode,
            "role \"r\": react_mode \"loop\" must be one_shot or by_order"
        );

        let expected = err(
            "name: t\nroles:\n  - name: r\n    profile: R\n    instruction: go\n    actions:\n      - name: step\n        expected: json\nagents:\n  - name: a\n    role: r\n",
        );
        assert_eq!(
            expected,
            "role \"r\" action \"step\": expected \"json\" must be text, verdict or list"
        );

        let nothing = err(
            "name: t\nroles:\n  - name: r\n    profile: R\nagents:\n  - name: a\n    role: r\n",
        );
        assert_eq!(
            nothing,
            "agent \"a\": no prompt and role \"r\" has no instruction to fall back on"
        );
    }
}
