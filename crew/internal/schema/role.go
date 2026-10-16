// Role archetypes: MetaGPT's Role/Action vocabulary ported as data (Phase 19A).
//
// Source of the design (verified against MetaGPT main, docs/spikes.md S31):
//   - Role{name, profile, goal, constraints, actions[], watch[], react_mode}
//     (metagpt/roles/role.py). A role's prefix —
//     "You are a {profile}, named {name}, your goal is {goal}. " +
//     "the constraint is {constraints}. " — is installed as the SYSTEM prompt of
//     every action the role runs (Role._get_prefix → Action.set_prefix).
//   - Action{name, desc, instruction, expected_type} with a structured
//     ActionNode output (metagpt/actions/action.py, action_node.py).
//   - Roles subscribe to Message tags they care about (Role._watch) and publish
//     with cause_by = the action that produced the message
//     (Role._observe/_act, metagpt/environment/base_env.py).
//
// forseti keeps its own SOP text (MetaGPT is inspiration + attribution, not a
// vendored copy — see docs/design.md §Phase 19). The library here is the
// built-in archetype set; a crew may declare `roles:` to add or override one.
package schema

import (
	"fmt"
	"sort"
	"strings"
)

// Tier names a model class a role runs on. The crew maps tiers to concrete
// provider/model strings in its `models:` block, so role archetypes never pin a
// model id (they churn — halogen→valhalla, S28).
const (
	TierEfficient = "efficient"
	TierCapable   = "capable"
)

// Action is one step of a role's SOP (MetaGPT Action + ActionNode). Expected
// names the shape of the step's output so the runner and the UI can treat it
// uniformly:
//
//	text    — free prose (default)
//	verdict — prose that MUST end with a VERDICT line (see VerdictLabel)
//	list    — a bullet/numbered list (fan-out source)
type Action struct {
	Name        string   `yaml:"name"`
	Desc        string   `yaml:"desc,omitempty"`        // one-line summary (skill manager / UI)
	Instruction string   `yaml:"instruction,omitempty"` // the task scaffold for this step
	Expected    string   `yaml:"expected,omitempty"`    // text | verdict | list
	Reads       []string `yaml:"reads,omitempty"`       // artifact types consumed
	Writes      string   `yaml:"writes,omitempty"`      // artifact type produced
}

// Role is a reusable agent archetype. A crew node references one with `role:`
// and inherits its prefix, tier, actions and subscriptions; the node's own
// prompt (when set) is appended after the prefix.
type Role struct {
	Name        string   `yaml:"name"`                  // archetype id (herdr name rule)
	Profile     string   `yaml:"profile,omitempty"`     // MetaGPT profile, e.g. "Reviewer"
	Goal        string   `yaml:"goal,omitempty"`        // MetaGPT goal
	Constraints string   `yaml:"constraints,omitempty"` // MetaGPT constraints
	Desc        string   `yaml:"desc,omitempty"`        // what this archetype is for (docs/UI)
	Tier        string   `yaml:"tier,omitempty"`        // efficient | capable (resolved via crew models:)
	Watch       []string `yaml:"watch,omitempty"`       // artifact types this role reacts to
	Writes      string   `yaml:"writes,omitempty"`      // artifact type its output publishes (default: node name)
	ReactMode   string   `yaml:"react_mode,omitempty"`  // one_shot (default) | by_order
	Actions     []Action `yaml:"actions,omitempty"`     // the SOP steps (by_order runs them in sequence)
	Instruction string   `yaml:"instruction,omitempty"` // default task text (used when a node sets no prompt)
}

// Prefix is MetaGPT's role prefix: the system-prompt scaffold installed on every
// action of the role. `name` is the concrete node name (Role._get_prefix uses
// the instance name, not the archetype).
func (rl Role) Prefix(name string) string {
	profile := rl.Profile
	if profile == "" {
		profile = rl.Name
	}
	var b strings.Builder
	fmt.Fprintf(&b, "You are a %s, named %s, your goal is %s. ", profile, name, strings.TrimSpace(rl.Goal))
	if c := strings.TrimSpace(rl.Constraints); c != "" {
		fmt.Fprintf(&b, "the constraint is %s. ", c)
	}
	return b.String()
}

// ValidTiers / ValidReactModes pin the enums (validation + TUI).
var (
	ValidTiers      = []string{TierEfficient, TierCapable}
	ValidReactModes = []string{"one_shot", "by_order"}
)

// builtinRoles is forseti's own SOP library — the archetypes the patterns and
// crews instantiate. Each is written to be runnable as a single pi node (one
// shot) while also naming the actions the by-order/pattern forms expand.
var builtinRoles = map[string]Role{
	"planner": {
		Name:        "planner",
		Profile:     "Planner",
		Goal:        "turn the request into an ordered, verifiable plan",
		Constraints: "no implementation, no restating of the request, no prose padding — output a numbered plan whose every step names its verification",
		Desc:        "Decomposes a goal into steps; the entry role of an SOP.",
		Tier:        TierCapable,
		ReactMode:   "one_shot",
		Writes:      "plan",
		Actions: []Action{
			{Name: "plan", Desc: "write the plan", Expected: "list", Writes: "plan",
				Instruction: "Return a numbered plan. Each item: what to do, and how it will be verified."},
		},
		Instruction: "Write the plan for the request below. Keep it minimal and ordered; every step must be checkable.",
	},
	"researcher": {
		Name:        "researcher",
		Profile:     "Researcher",
		Goal:        "gather and synthesise verified information",
		Constraints: "cite the source of each claim; separate fact from inference; state what you could not verify",
		Desc:        "BY_ORDER: ask questions → search/summarise → synthesise (MetaGPT Researcher).",
		Tier:        TierEfficient,
		ReactMode:   "by_order",
		Writes:      "research",
		Actions: []Action{
			{Name: "questions", Desc: "list the questions worth answering", Expected: "list", Reads: []string{},
				Instruction: "List the specific questions this topic raises, most load-bearing first."},
			{Name: "gather", Desc: "gather evidence per question", Reads: []string{"questions"},
				Instruction: "For each question above, give the evidence you have and its source. Mark anything uncertain."},
			{Name: "synthesise", Desc: "synthesise the findings", Expected: "text", Reads: []string{"gather"}, Writes: "research",
				Instruction: "Synthesise the gathered evidence into a short brief: findings, confidence, open questions."},
		},
		Instruction: "Research the topic below and report findings.",
	},
	"engineer": {
		Name:        "engineer",
		Profile:     "Engineer",
		Goal:        "implement exactly what was asked, so that it works",
		Constraints: "smallest correct change; no unrequested refactors; say which checks you ran and how they ended",
		Desc:        "The executor archetype: implement, then hand to a verifier.",
		Tier:        TierCapable,
		ReactMode:   "one_shot",
		Writes:      "implementation",
		Actions: []Action{
			{Name: "implement", Desc: "make the change", Writes: "implementation",
				Instruction: "Make the change. Report the files touched, what you changed, and the exact commands you ran."},
		},
		Instruction: "Implement the task. Prefer the smallest correct change.",
	},
	"reviewer": {
		Name:        "reviewer",
		Profile:     "Reviewer",
		Goal:        "find the real defects before they ship",
		Constraints: "be concrete: quote the artifact, name the failure mode, say why it matters — no vague praise; end with a VERDICT line",
		Desc:        "Critical review pass (MetaGPT WritePRDReview/DesignReview).",
		Tier:        TierCapable,
		ReactMode:   "one_shot",
		Writes:      "review",
		Actions: []Action{
			{Name: "review", Desc: "review the artifact", Expected: "verdict", Writes: "review",
				Instruction: "Review the artifact. List defects worst-first (each: what is wrong, why it matters, the fix). If nothing material is wrong, say so plainly. End with exactly one line: `VERDICT: LGTM` or `VERDICT: LBTM`."},
		},
		Instruction: "Review the artifact below against what it is supposed to achieve.",
	},
	"security_reviewer": {
		Name:        "security_reviewer",
		Profile:     "Security reviewer",
		Goal:        "find weakness an attacker would exploit",
		Constraints: "work the lens list in order; each finding needs a concrete trigger and an impact; rate severity (critical/high/medium/low); end with a VERDICT line",
		Desc:        "Security review pass: injection, secrets, authz, data exposure, unsafe defaults.",
		Tier:        TierCapable,
		ReactMode:   "one_shot",
		Writes:      "security_review",
		Actions: []Action{
			{Name: "threat_review", Desc: "review for exploitable weakness", Expected: "verdict", Writes: "security_review",
				Instruction: "Review the artifact for security defects through these lenses, in order: (1) injection (shell/SQL/path/format), (2) secrets and credentials handling, (3) authentication and authorization, (4) data exposure and logging, (5) unsafe defaults and missing bounds. For each finding give: trigger, impact, severity, fix. Explicitly list what you checked and found clean. End with `VERDICT: PASS` or `VERDICT: FAIL`."},
		},
		Instruction: "Security-review the artifact below. Assume an adversary reads it as an attack plan.",
	},
	"adversarial_reviewer": {
		Name:        "adversarial_reviewer",
		Profile:     "Adversary",
		Goal:        "break the artifact, not bless it",
		Constraints: "argue the strongest case AGAINST the artifact; never agree with it without an attempt to falsify it; stay inside your assigned lens; end with a VERDICT line",
		Desc:        "Adversarial critic for a panel (one lens per instance).",
		Tier:        TierCapable,
		ReactMode:   "one_shot",
		Writes:      "critique",
		Actions: []Action{
			{Name: "attack", Desc: "attack the artifact in one lens", Expected: "verdict", Writes: "critique",
				Instruction: "Attack the artifact from your assigned lens only. Give the strongest concrete counter-case: a scenario where it fails, plus how you would demonstrate the failure. Do not soften it. End with `VERDICT: ACCEPT` or `VERDICT: REVISE` (ACCEPT = your lens finds nothing that must change before shipping)."},
		},
		Instruction: "Governed by the lens stated above. Attack the artifact below.",
	},
	"judge": {
		Name:        "judge",
		Profile:     "Judge",
		Goal:        "decide between competing positions on the evidence",
		Constraints: "weigh each position's evidence, not its confidence; say what you are rejecting and why; one decision, stated once; end with a VERDICT line",
		Desc:        "Consolidates a panel of reviews/critiques into one decision.",
		Tier:        TierCapable,
		ReactMode:   "one_shot",
		Writes:      "judgement",
		Actions: []Action{
			{Name: "decide", Desc: "decide and justify", Expected: "verdict", Writes: "judgement",
				Instruction: "Decide. For each position: what it found, whether it survives scrutiny. Name the findings you accept (they must be addressed) and the ones you reject (with the reason). End with exactly one line: `VERDICT: ACCEPT` (ship as-is) or `VERDICT: REVISE` (the accepted findings must be addressed)."},
		},
		Instruction: "Judge the positions below against the artifact they concern.",
	},
	"verifier": {
		Name:        "verifier",
		Profile:     "Verifier",
		Goal:        "establish whether the work actually passes its own checks",
		Constraints: "run the real commands; report the exit status and the decisive output lines verbatim; do not fix anything; end with a VERDICT line",
		Desc:        "Executor loop gate: runs the checks and reports PASS/FAIL.",
		Tier:        TierEfficient,
		ReactMode:   "one_shot",
		Writes:      "verification",
		Actions: []Action{
			{Name: "verify", Desc: "run the checks", Expected: "verdict", Writes: "verification",
				Instruction: "Run the verification commands named above. Quote the exact command and its decisive output. If a command cannot run (missing tool/binary), say so and treat it as FAIL. Do not modify any file. End with `VERDICT: PASS` or `VERDICT: FAIL`."},
		},
		Instruction: "Verify the work below by running its checks.",
	},
}

// BuiltinRoles returns a copy of the built-in archetype library, sorted by name.
func BuiltinRoles() map[string]Role {
	out := make(map[string]Role, len(builtinRoles))
	for k, v := range builtinRoles {
		out[k] = v
	}
	return out
}

// BuiltinRoleNames lists the built-in archetypes (docs/UI/tests).
func BuiltinRoleNames() []string {
	names := make([]string, 0, len(builtinRoles))
	for k := range builtinRoles {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// RolePack is the resolved archetype set for one crew: built-ins, overlaid by
// the crew's own `roles:` declarations (same name = override, new name = new
// archetype).
type RolePack struct {
	roles map[string]Role
	// used tracks the archetypes a crew actually referenced, for validation
	// error messages.
	names []string
}

// NewRolePack builds the pack for a crew (builtins + inline overrides).
func NewRolePack(inline []Role) *RolePack {
	p := &RolePack{roles: BuiltinRoles()}
	for _, r := range inline {
		p.roles[r.Name] = r
		// an override must not inherit the builtin's prompt content silently:
		// the crew file owns the whole archetype once it re-declares the name.
	}
	p.names = make([]string, 0, len(p.roles))
	for k := range p.roles {
		p.names = append(p.names, k)
	}
	sort.Strings(p.names)
	return p
}

// Names lists every archetype in the pack, sorted.
func (p *RolePack) Names() []string { return p.names }

// Get resolves one archetype.
func (p *RolePack) Get(name string) (Role, bool) {
	r, ok := p.roles[name]
	return r, ok
}

// String is the "known roles" list for error messages.
func (p *RolePack) String() string { return strings.Join(p.names, ", ") }

// Validate applies the archetype defaults to every node that declares `role:`
// and rejects unknown archetypes, bad tiers and bad react modes. It also derives
// each node's watched/produced artifact types from its role (explicit node
// fields win).
func (p *RolePack) Validate(c *Crew) error {
	for i := range c.Agents {
		a := &c.Agents[i]
		if a.Role == "" {
			if a.Writes != "" || len(a.Watch) > 0 {
				return fmt.Errorf("agent %q: watch/writes need a role (or move them into the role archetype)", a.Name)
			}
			continue
		}
		r, ok := p.Get(a.Role)
		if !ok {
			return fmt.Errorf("agent %q: unknown role %q (known: %s)", a.Name, a.Role, p.String())
		}
		if r.Tier != "" {
			if !validTier(r.Tier) {
				return fmt.Errorf("role %q: tier %q must be efficient or capable", r.Name, r.Tier)
			}
			// tier → model via the crew's models: map; a node's explicit
			// model/route always wins (MetaGPT action-level llm_name_or_type).
			if a.Model == "" && a.Route == "" {
				m, ok := c.Models[r.Tier]
				if !ok || strings.TrimSpace(m) == "" {
					return fmt.Errorf("agent %q: role %q needs tier %q — set models: {%s: provider/model} or give the node an explicit model/route",
						a.Name, a.Role, r.Tier, r.Tier)
				}
				a.Model = m
			}
		}
		if r.ReactMode != "" && !validReactMode(r.ReactMode) {
			return fmt.Errorf("role %q: react_mode %q must be one_shot or by_order", r.Name, r.ReactMode)
		}
		if a.Writes == "" {
			a.Writes = r.Writes
		}
		if len(a.Watch) == 0 {
			a.Watch = append([]string(nil), r.Watch...)
		}
		for _, act := range r.Actions {
			switch act.Expected {
			case "", "text", "verdict", "list":
			default:
				return fmt.Errorf("role %q action %q: expected %q must be text, verdict or list", r.Name, act.Name, act.Expected)
			}
		}
		if strings.TrimSpace(a.Prompt) == "" && strings.TrimSpace(r.Instruction) == "" {
			return fmt.Errorf("agent %q: no prompt and role %q has no instruction to fall back on", a.Name, a.Role)
		}
	}
	return nil
}

// RoleOf resolves the archetype a node runs as (zero Role when the node is
// role-less — a plain pi node).
func (p *RolePack) RoleOf(a *Agent) (Role, bool) {
	if a == nil || a.Role == "" {
		return Role{}, false
	}
	return p.Get(a.Role)
}

// RolePrompt renders the node's prompt with the archetype prefix in front
// (MetaGPT Role._get_prefix → Action.set_prefix). A role-less node is returned
// unchanged, so existing crews render exactly as before.
func (p *RolePack) RolePrompt(a *Agent, prompt string) string {
	r, ok := p.RoleOf(a)
	if !ok {
		return prompt
	}
	body := prompt
	if strings.TrimSpace(body) == "" {
		body = r.Instruction
	}
	return r.Prefix(a.Name) + "\n\n" + body
}

func validTier(t string) bool {
	for _, v := range ValidTiers {
		if v == t {
			return true
		}
	}
	return false
}

func validReactMode(m string) bool {
	for _, v := range ValidReactModes {
		if v == m {
			return true
		}
	}
	return false
}
