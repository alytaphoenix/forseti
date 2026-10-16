// SOP patterns: MetaGPT-shaped workflows compiled into the existing agent graph
// (Phase 19C).
//
// A pattern is a named multi-agent procedure the user asked for (reviewer loop,
// security review, adversarial panel + judge, executor). It is instantiated
// into ordinary nodes and edges, so the P13 scheduler (fan-in AND, bounded back
// edges, phases), the run log, the TUI and the routing layer all work unchanged.
// MetaGPT anchors (docs/spikes.md S31): Role._set_react_mode(BY_ORDER) for a
// fixed action sequence, WriteReview's LGTM/LBTM verdict, QA Engineer's
// test→run→debug fix loop, and Engineer's is_pass/summarize loop.
//
// Verdict edges (Phase 19B) give the loops their teeth: `when: verdict:lbtm`
// fires only when the producing node's output carries that verdict line.
package schema

import (
	"fmt"
	"strings"
)

// Pattern kinds.
const (
	PatternReviewerLoop     = "reviewer_loop"
	PatternSecurityReview   = "security_review"
	PatternAdversarialPanel = "adversarial_panel"
	PatternExecutor         = "executor"
)

// KnownPatterns lists the instantiable SOP patterns.
var KnownPatterns = []string{
	PatternReviewerLoop, PatternSecurityReview, PatternAdversarialPanel, PatternExecutor,
}

// Pattern instantiates a multi-agent SOP over an existing crew.
//
//	members: which declared nodes play the pattern's named parts (author,
//	         reviewer, implementer, judge, ...). A missing member is synthesized
//	         as a new node named "<pattern id>-<part>" with the matching role.
//	params:  kind-specific dials (max_revisions, critics, lenses, checks, task,
//	         then), validated per kind.
type Pattern struct {
	ID      string            `yaml:"id"`                // herdr name rule; prefixes synthesized node names
	Pattern string            `yaml:"pattern"`           // one of KnownPatterns
	Subject string            `yaml:"subject,omitempty"` // declared node whose output the SOP works on
	Members map[string]string `yaml:"members,omitempty"` // part name → declared node
	Params  map[string]any    `yaml:"params,omitempty"`  // kind-specific dials

	expanded bool // yaml:"-" — Validate is not re-entrant for expansion
}

// pendingThen is a `then:` continuation resolved after all patterns expand (it
// may name a declared agent, a synthesized node, or another pattern's id).
type pendingThen struct {
	From    string
	To      string
	When    string
	Pattern string // the pattern that declared it (error messages)
}

// phaseOfNode returns the index of the phase containing node, or -1.
func (c *Crew) phaseOfNode(node string) int {
	if node == "" {
		return -1
	}
	for i := range c.Phases {
		for _, n := range c.Phases[i].Agents {
			if n == node {
				return i
			}
		}
	}
	return -1
}

// params accessors (typed, with defaults) --------------------------------

func (p *Pattern) str(key, def string) string {
	if v, ok := p.Params[key]; ok {
		if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return def
}

func (p *Pattern) num(key string, def int) int {
	if v, ok := p.Params[key]; ok {
		switch n := v.(type) {
		case int:
			return n
		case int64:
			return int(n)
		case float64:
			return int(n)
		}
	}
	return def
}

func (p *Pattern) list(key string, def []string) []string {
	v, ok := p.Params[key]
	if !ok {
		return def
	}
	raw, ok := v.([]any)
	if !ok || len(raw) == 0 {
		return def
	}
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		s, ok := item.(string)
		if !ok || strings.TrimSpace(s) == "" {
			return def
		}
		out = append(out, strings.TrimSpace(s))
	}
	return out
}

// validateParams rejects unknown keys and out-of-range dials, so a typo cannot
// silently produce a pattern with default behaviour.
func (p *Pattern) validateParams() error {
	allowed := map[string]map[string]bool{
		PatternReviewerLoop:     {"max_revisions": true, "then": true, "subject_label": true},
		PatternSecurityReview:   {"max_revisions": true, "then": true, "lenses": true},
		PatternAdversarialPanel: {"critics": true, "lenses": true, "max_rounds": true, "then": true},
		PatternExecutor:         {"max_fixes": true, "then": true, "checks": true, "task": true},
	}[p.Pattern]
	for k := range p.Params {
		if !allowed[k] {
			keys := make([]string, 0, len(allowed))
			for a := range allowed {
				keys = append(keys, a)
			}
			return fmt.Errorf("pattern %q (%s): unknown param %q (allowed: %s)", p.ID, p.Pattern, k, strings.Join(sortedStrings(keys), ", "))
		}
	}
	switch p.Pattern {
	case PatternReviewerLoop, PatternSecurityReview:
		if n := p.num("max_revisions", 2); n < 1 || n > 10 {
			return fmt.Errorf("pattern %q: max_revisions %d must be 1..10", p.ID, n)
		}
	case PatternAdversarialPanel:
		if n := p.num("critics", 3); n < 2 || n > 7 {
			return fmt.Errorf("pattern %q: critics %d must be 2..7 (a panel of one is a reviewer, not a judge)", p.ID, n)
		}
		if n := p.num("max_rounds", 2); n < 1 || n > 10 {
			return fmt.Errorf("pattern %q: max_rounds %d must be 1..10", p.ID, n)
		}
	case PatternExecutor:
		if n := p.num("max_fixes", 2); n < 1 || n > 10 {
			return fmt.Errorf("pattern %q: max_fixes %d must be 1..10", p.ID, n)
		}
		if strings.TrimSpace(p.Subject) == "" && p.str("task", "") == "" {
			return fmt.Errorf("pattern %q: executor needs a subject (the node that supplies the task) or params.task", p.ID)
		}
	}
	return nil
}

func sortedStrings(in []string) []string {
	out := append([]string(nil), in...)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// expand instantiates every pattern into agents/checks/edges. Called at the top
// of Validate (expansion is followed by the ordinary graph validation, so a
// pattern cannot produce an invalid crew).
func (c *Crew) expandPatterns() error {
	if len(c.Patterns) == 0 {
		return nil
	}
	seenID := map[string]bool{}
	for i := range c.Patterns {
		p := &c.Patterns[i]
		// pattern ids name synthesized nodes: check the rule + leave room for
		// a "-critic-7" style suffix.
		if !nameRule.MatchString(p.ID) || len(p.ID) > 22 {
			return fmt.Errorf("pattern %d: id %q must match [a-z][a-z0-9_-]{0,21} (it prefixes generated node names)", i, p.ID)
		}
		if seenID[p.ID] {
			return fmt.Errorf("duplicate pattern id %q", p.ID)
		}
		seenID[p.ID] = true
		known := false
		for _, k := range KnownPatterns {
			if k == p.Pattern {
				known = true
			}
		}
		if !known {
			return fmt.Errorf("pattern %q: unknown pattern %q (known: %s)", p.ID, p.Pattern, strings.Join(KnownPatterns, ", "))
		}
		if p.expanded {
			continue
		}
		if err := p.validateParams(); err != nil {
			return err
		}
		if p.Subject != "" && !c.hasAgent(p.Subject) {
			return fmt.Errorf("pattern %q: subject %q is not a declared agent", p.ID, p.Subject)
		}
		for part, node := range p.Members {
			if !c.hasAgent(node) {
				return fmt.Errorf("pattern %q: member %s=%q is not a declared agent", p.ID, part, node)
			}
		}
		before := len(c.Agents)
		entries, err := c.instantiate(p)
		if err != nil {
			return err
		}
		if len(entries) > 0 {
			if c.patternEntry == nil {
				c.patternEntry = map[string][]string{}
			}
			c.patternEntry[p.ID] = entries // the pattern's entry nodes
		}
		// Phased crews: a synthesized node belongs to the phase its subject
		// lives in (the reviewer of a phase's artifact is that phase's work).
		// Without a subject (an executor entry) the nodes join the last phase.
		if len(c.Phases) > 0 && len(c.Agents) > before {
			phase := c.phaseOfNode(p.Subject)
			if phase < 0 {
				phase = len(c.Phases) - 1
			}
			for i := before; i < len(c.Agents); i++ {
				c.Phases[phase].Agents = append(c.Phases[phase].Agents, c.Agents[i].Name)
			}
		}
		p.expanded = true
	}
	// Resolve `then:` chains after every pattern expanded: a target may be a
	// declared agent, a synthesized node name, or another pattern's id. A
	// pattern id resolves to its ENTRY nodes (the critics of a panel, the
	// implementer of an executor, the reviewer of a review loop), so `then`
	// gates every way into the next stage instead of racing one of them —
	// fan-in AND makes the stage wait for the verdict edge.
	for _, t := range c.pendingThen {
		if strings.TrimSpace(t.To) == "" {
			continue
		}
		targets := []string{t.To}
		if c.Agent(t.To) == nil {
			entries, ok := c.patternEntry[t.To]
			if !ok {
				return fmt.Errorf("pattern %q: then %q is neither a declared agent nor a pattern id", t.Pattern, t.To)
			}
			targets = entries
		}
		for _, to := range targets {
			if err := c.addEdgeChecked(Edge{From: t.From, To: to, When: t.When}); err != nil {
				return err
			}
		}
	}
	c.pendingThen = nil
	return nil
}

// instantiate dispatches on the pattern kind. Each arm appends agents and edges
// (and checks for executor) to the crew and returns the pattern's ENTRY nodes —
// the nodes another pattern's `then:` edge should gate when it chains here.
func (c *Crew) instantiate(p *Pattern) ([]string, error) {
	switch p.Pattern {
	case PatternReviewerLoop:
		return c.instantiateReviewLoop(p, "reviewer", "VERDICT: LGTM", "VERDICT: LBTM")
	case PatternSecurityReview:
		return c.instantiateReviewLoop(p, "security_reviewer", "VERDICT: PASS", "VERDICT: FAIL")
	case PatternAdversarialPanel:
		return c.instantiatePanel(p)
	case PatternExecutor:
		return c.instantiateExecutor(p)
	}
	return nil, fmt.Errorf("pattern %q: unhandled kind %q", p.ID, p.Pattern)
}

// addAgent appends a synthesized node, rejecting a name collision (a pattern
// must not silently overwrite a declared node).
func (c *Crew) addAgent(a Agent) error {
	if c.Agent(a.Name) != nil {
		return fmt.Errorf("pattern node %q collides with a declared agent (rename the pattern or its id)", a.Name)
	}
	c.Agents = append(c.Agents, a)
	if c.synthAgents == nil {
		c.synthAgents = map[string]bool{}
	}
	c.synthAgents[a.Name] = true
	return nil
}

// instantiateReviewLoop: subject → reviewer, and reviewer → subject only on the
// negative verdict (bounded). The positive verdict optionally continues to
// params.then. This is MetaGPT's review→revise loop with an explicit gate.
func (c *Crew) instantiateReviewLoop(p *Pattern, part, good, bad string) ([]string, error) {
	reviewer, ok := p.Members[part]
	if !ok {
		reviewer = p.ID + "-" + part
		role := "reviewer"
		instruction := "List defects worst-first, each with what is wrong, why it matters and the fix. If nothing material is wrong, say so. End with"
		subject := p.Subject
		if p.Pattern == PatternSecurityReview {
			role = "security_reviewer"
			instruction = "Work the lens list in order (" + strings.Join(p.list("lenses", []string{"injection", "secrets", "authz", "data exposure", "unsafe defaults"}), ", ") + "). Each finding: trigger, impact, severity, fix. End with"
			subject = p.Subject
		}
		// subject_label names the artifact in prose ("the design"); the default
		// points the reviewer at the producing node.
		what := fmt.Sprintf("the output of node %q", subject)
		if l := strings.TrimSpace(p.str("subject_label", "")); l != "" {
			what = l
		}
		prompt := fmt.Sprintf("The work under review is %s:\n\n{{ %s }}\n\n%s `%s` or `%s`.",
			what, subject, instruction, good, bad)
		if err := c.addAgent(Agent{
			Name:   reviewer,
			Role:   role,
			Prompt: strings.TrimSpace(prompt),
		}); err != nil {
			return nil, err
		}
	}
	// the revise pass re-enters the subject: bounded by max_revisions
	if err := c.addEdgeChecked(Edge{From: p.Subject, To: reviewer}); err != nil {
		return nil, err
	}
	if err := c.addEdgeChecked(Edge{From: reviewer, To: p.Subject, When: "verdict:" + verdictLabel(bad), MaxVisits: p.num("max_revisions", 2), Loop: true}); err != nil {
		return nil, err
	}
	// then is resolved after every pattern has expanded (it may name another
	// pattern's node, or another pattern id outright).
	c.pendingThen = append(c.pendingThen, pendingThen{From: reviewer, To: p.str("then", ""), When: "verdict:" + verdictLabel(good), Pattern: p.ID})
	return []string{reviewer}, nil
}

// instantiatePanel: subject → N lens critics (fan-in AND) → judge; the judge's
// negative verdict loops back to the subject (bounded). The "adversarial
// reviews + judges" pattern.
func (c *Crew) instantiatePanel(p *Pattern) ([]string, error) {
	lenses := p.list("lenses", []string{"correctness", "failure modes", "cost and complexity"})
	n := p.num("critics", 3)
	if n > len(lenses) {
		// extend with generic lenses so the declared count is honoured
		for i := len(lenses); i < n; i++ {
			lenses = append(lenses, fmt.Sprintf("lens %d: your own strongest objection", i+1))
		}
	}
	critics := make([]string, 0, n)
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("%s-critic-%d", p.ID, i+1)
		critics = append(critics, name)
		prompt := fmt.Sprintf("Artifact under attack (output of node %q):\n\n{{ %s }}\n\nYour lens: %s\n\nAttack it from this lens only: give the strongest concrete counter-case (a scenario where it fails) and how you would demonstrate the failure. End with `VERDICT: ACCEPT` or `VERDICT: REVISE`.",
			p.Subject, p.Subject, lenses[i])
		if err := c.addAgent(Agent{Name: name, Role: "adversarial_reviewer", Prompt: prompt}); err != nil {
			return nil, err
		}
		if err := c.addEdgeChecked(Edge{From: p.Subject, To: name}); err != nil {
			return nil, err
		}
	}
	judge := p.Members["judge"]
	if judge == "" {
		judge = p.ID + "-judge"
		judgePrompt := fmt.Sprintf("Artifact under judgement (output of node %q):\n\n{{ %s }}\n\nYou sit on a panel. Each position below is one critic's independent attack, from a different lens:\n\n%sDecide. For each position state whether it survives scrutiny and why. Name the findings you accept (they must be addressed before shipping) and the ones you reject (with the reason). End with exactly one line: `VERDICT: ACCEPT` (ship as-is) or `VERDICT: REVISE` (the accepted findings must be addressed).",
			p.Subject, p.Subject, joinBindings(critics))
		if err := c.addAgent(Agent{Name: judge, Role: "judge", Prompt: judgePrompt}); err != nil {
			return nil, err
		}
	}
	for _, cr := range critics {
		if err := c.addEdgeChecked(Edge{From: cr, To: judge}); err != nil {
			return nil, err
		}
	}
	if err := c.addEdgeChecked(Edge{From: judge, To: p.Subject, When: "verdict:revise", MaxVisits: p.num("max_rounds", 2), Loop: true}); err != nil {
		return nil, err
	}
	c.pendingThen = append(c.pendingThen, pendingThen{From: judge, To: p.str("then", ""), When: "verdict:accept", Pattern: p.ID})
	return critics, nil
}

// instantiateExecutor: implement → verify, and verify → implement only on
// VERDICT: FAIL (bounded); params.checks become real shell checks after the
// implementer (objective evidence in the run log/exit code).
func (c *Crew) instantiateExecutor(p *Pattern) ([]string, error) {
	impl, ok := p.Members["implementer"]
	if !ok {
		impl = p.ID + "-implement"
		prompt := strings.TrimSpace(p.str("task", ""))
		if p.Subject != "" {
			binding := fmt.Sprintf("The task (output of node %q):\n\n{{ %s }}\n\n", p.Subject, p.Subject)
			prompt = binding + prompt
		}
		if prompt == "" {
			prompt = "Implement the task."
		}
		if err := c.addAgent(Agent{Name: impl, Role: "engineer", Prompt: prompt}); err != nil {
			return nil, err
		}
	}
	if p.Subject != "" {
		if err := c.addEdgeChecked(Edge{From: p.Subject, To: impl}); err != nil {
			return nil, err
		}
	}
	checks := p.list("checks", nil)
	verify := p.ID + "-verify"
	vprompt := "Verify the work produced by node \"" + impl + "\":\n\n{{ " + impl + " }}\n\n"
	if len(checks) > 0 {
		vprompt += "Verification commands:\n" + bullet(checks) + "\n"
	} else {
		vprompt += "Run whatever checks the work itself names (its build/test command).\n"
	}
	vprompt += "Quote the exact command and its decisive output lines. Treat an unrunnable command as FAIL. End with `VERDICT: PASS` or `VERDICT: FAIL`."
	if err := c.addAgent(Agent{Name: verify, Role: "verifier", Prompt: vprompt}); err != nil {
		return nil, err
	}
	if err := c.addEdgeChecked(Edge{From: impl, To: verify}); err != nil {
		return nil, err
	}
	if err := c.addEdgeChecked(Edge{From: verify, To: impl, When: "verdict:fail", MaxVisits: p.num("max_fixes", 2), Loop: true}); err != nil {
		return nil, err
	}
	for i, cmd := range checks {
		name := fmt.Sprintf("%s-check-%d", p.ID, i+1)
		c.Checks = append(c.Checks, Check{Name: name, After: impl, Run: cmd})
		if c.derivedChecks == nil {
			c.derivedChecks = map[string]bool{}
		}
		c.derivedChecks[impl+"\x00"+name] = true
	}
	c.pendingThen = append(c.pendingThen, pendingThen{From: verify, To: p.str("then", ""), When: "verdict:pass", Pattern: p.ID})
	return []string{impl}, nil
}

// addEdgeChecked appends an edge and rejects a duplicate (from,to) — the same
// rule the graph validator enforces, applied at synthesis time so the error
// names the pattern.
func (c *Crew) addEdgeChecked(e Edge) error {
	if e.When == "" {
		e.When = "idle"
	}
	for _, ex := range c.Edges {
		if ex.From == e.From && ex.To == e.To {
			return fmt.Errorf("edge %s→%s already declared (pattern would duplicate it)", e.From, e.To)
		}
	}
	c.Edges = append(c.Edges, e)
	if c.derivedEdges == nil {
		c.derivedEdges = map[string]bool{}
	}
	c.derivedEdges[e.From+">"+e.To] = true
	return nil
}

// verdictLabel maps "VERDICT: LGTM" → "lgtm".
func verdictLabel(line string) string { return badLabel(line) }

func badLabel(line string) string {
	line = strings.TrimSpace(line)
	if i := strings.LastIndex(line, ":"); i >= 0 {
		line = strings.TrimSpace(line[i+1:])
	}
	return strings.ToLower(line)
}

func bullet(items []string) string {
	var b strings.Builder
	for _, s := range items {
		b.WriteString("- " + s + "\n")
	}
	return b.String()
}

func joinBindings(nodes []string) string {
	var b strings.Builder
	for _, n := range nodes {
		b.WriteString(fmt.Sprintf("- %s:\n\n{{ %s }}\n\n", n, n))
	}
	return b.String()
}
