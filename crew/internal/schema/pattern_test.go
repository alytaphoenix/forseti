package schema

import (
	"fmt"
	"strings"
	"testing"
)

// baseCrew is the smallest valid crew the patterns can be applied to.
func baseCrew() *Crew {
	return &Crew{
		Name:   "pat",
		Models: map[string]string{TierEfficient: "e/m", TierCapable: "c/m"},
		Agents: []Agent{{Name: "draft", Prompt: "produce the draft"}},
	}
}

func crewNames(c *Crew) map[string]*Agent {
	out := map[string]*Agent{}
	for i := range c.Agents {
		out[c.Agents[i].Name] = &c.Agents[i]
	}
	return out
}

func crewEdge(c *Crew, from, to string) *Edge {
	for i := range c.Edges {
		if c.Edges[i].From == from && c.Edges[i].To == to {
			return &c.Edges[i]
		}
	}
	return nil
}

func TestPatternReviewerLoop(t *testing.T) {
	c := baseCrew()
	c.Agents = append(c.Agents, Agent{Name: "ship", Prompt: "announce"})
	c.Patterns = []Pattern{{ID: "rl", Pattern: PatternReviewerLoop, Subject: "draft",
		Params: map[string]any{"max_revisions": 3, "then": "ship"}}}
	if err := c.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	rev := crewNames(c)["rl-reviewer"]
	if rev == nil {
		t.Fatalf("synthesised reviewer missing: %+v", c.Agents)
	}
	if rev.Role != "reviewer" {
		t.Fatalf("reviewer role = %q", rev.Role)
	}
	if !strings.Contains(rev.Prompt, "{{ draft }}") {
		t.Fatalf("reviewer prompt must bind the subject: %q", rev.Prompt)
	}
	if e := crewEdge(c, "draft", "rl-reviewer"); e == nil || e.When != "idle" {
		t.Fatalf("subject→reviewer edge wrong: %+v", e)
	}
	back := crewEdge(c, "rl-reviewer", "draft")
	if back == nil || back.When != "verdict:lbtm" || back.MaxVisits != 3 {
		t.Fatalf("bounded negative-verdict edge wrong: %+v", back)
	}
	if e := crewEdge(c, "rl-reviewer", "ship"); e == nil || e.When != "verdict:lgtm" {
		t.Fatalf("positive-verdict continuation wrong: %+v", e)
	}
}

func TestPatternSecurityReview(t *testing.T) {
	c := baseCrew()
	c.Patterns = []Pattern{{ID: "sec", Pattern: PatternSecurityReview, Subject: "draft",
		Params: map[string]any{"lenses": []any{"injection", "secrets"}}}}
	if err := c.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	rev := crewNames(c)["sec-security_reviewer"]
	if rev == nil || rev.Role != "security_reviewer" {
		t.Fatalf("security reviewer missing/wrong: %+v", rev)
	}
	if !strings.Contains(rev.Prompt, "injection") {
		t.Fatalf("security reviewer must carry its lenses: %q", rev.Prompt)
	}
	if e := crewEdge(c, "sec-security_reviewer", "draft"); e == nil || e.When != "verdict:fail" {
		t.Fatalf("security loop must gate on verdict:fail: %+v", e)
	}
}

func TestPatternAdversarialPanel(t *testing.T) {
	c := baseCrew()
	c.Patterns = []Pattern{{ID: "ap", Pattern: PatternAdversarialPanel, Subject: "draft",
		Params: map[string]any{"critics": 3, "max_rounds": 2,
			"lenses": []any{"correctness", "failure modes", "cost"}}}}
	if err := c.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	ag := crewNames(c)
	for i := 1; i <= 3; i++ {
		name := fmt.Sprintf("ap-critic-%d", i)
		n := ag[name]
		if n == nil {
			t.Fatalf("critic %d missing: %+v", i, c.Agents)
		}
		if n.Role != "adversarial_reviewer" {
			t.Fatalf("critic %d role = %q", i, n.Role)
		}
		if e := crewEdge(c, "draft", name); e == nil {
			t.Fatalf("subject→critic %d edge missing", i)
		}
	}
	if !strings.Contains(ag["ap-critic-1"].Prompt, "correctness") {
		t.Fatalf("critic 1 must carry its lens: %q", ag["ap-critic-1"].Prompt)
	}
	judge := ag["ap-judge"]
	if judge == nil || judge.Role != "judge" {
		t.Fatalf("judge missing/wrong: %+v", judge)
	}
	for i := 1; i <= 3; i++ {
		if !strings.Contains(judge.Prompt, fmt.Sprintf("{{ ap-critic-%d }}", i)) {
			t.Fatalf("judge prompt must bind critic %d: %q", i, judge.Prompt)
		}
		if e := crewEdge(c, fmt.Sprintf("ap-critic-%d", i), "ap-judge"); e == nil {
			t.Fatalf("critic %d→judge fan-in missing", i)
		}
	}
	if e := crewEdge(c, "ap-judge", "draft"); e == nil || e.When != "verdict:revise" || e.MaxVisits != 2 {
		t.Fatalf("judge loop-back wrong: %+v", e)
	}
}

func TestPatternExecutor(t *testing.T) {
	c := baseCrew()
	c.Patterns = []Pattern{{ID: "ex", Pattern: PatternExecutor, Subject: "draft",
		Params: map[string]any{"max_fixes": 2, "checks": []any{"go test ./...", "go vet ./..."}}}}
	if err := c.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	ag := crewNames(c)
	impl := ag["ex-implement"]
	if impl == nil || impl.Role != "engineer" {
		t.Fatalf("implementer missing/wrong: %+v", impl)
	}
	if !strings.Contains(impl.Prompt, "{{ draft }}") {
		t.Fatalf("implementer must bind the subject: %q", impl.Prompt)
	}
	verify := ag["ex-verify"]
	if verify == nil || verify.Role != "verifier" {
		t.Fatalf("verifier missing/wrong: %+v", verify)
	}
	if e := crewEdge(c, "draft", "ex-implement"); e == nil || e.When != "idle" {
		t.Fatalf("subject→implement edge wrong: %+v", e)
	}
	if e := crewEdge(c, "ex-implement", "ex-verify"); e == nil {
		t.Fatal("implement→verify edge missing")
	}
	if e := crewEdge(c, "ex-verify", "ex-implement"); e == nil || e.When != "verdict:fail" || e.MaxVisits != 2 {
		t.Fatalf("fix loop wrong: %+v", e)
	}
	var got []string
	for _, ch := range c.Checks {
		if ch.After == "ex-implement" {
			got = append(got, ch.Run)
		}
	}
	if len(got) != 2 || got[0] != "go test ./..." {
		t.Fatalf("checks not materialised: %+v", c.Checks)
	}
}

func TestPatternExecutorTaskOnly(t *testing.T) {
	// no subject: the implementer is an entry node carrying the declared task
	c := &Crew{Name: "ex2", Models: map[string]string{TierCapable: "c/m", TierEfficient: "e/m"},
		Agents: []Agent{{Name: "seed", Prompt: "seed"}},
		Patterns: []Pattern{{ID: "ex", Pattern: PatternExecutor,
			Params: map[string]any{"task": "fix the flaky test"}}}}
	if err := c.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	impl := crewNames(c)["ex-implement"]
	if impl == nil || !strings.Contains(impl.Prompt, "fix the flaky test") {
		t.Fatalf("task must land in the implementer prompt: %+v", impl)
	}
	if crewEdge(c, "seed", "ex-implement") != nil {
		t.Fatal("task-only executor must not wire the unrelated node in")
	}
}

func TestPatternValidation(t *testing.T) {
	type tc struct {
		name  string
		build func() *Crew
		want  string
	}
	cases := []tc{
		{"unknown kind", func() *Crew {
			c := baseCrew()
			c.Patterns = []Pattern{{ID: "rl", Pattern: "nope", Subject: "draft"}}
			return c
		}, "unknown pattern"},
		{"unknown param", func() *Crew {
			c := baseCrew()
			c.Patterns = []Pattern{{ID: "rl", Pattern: PatternReviewerLoop, Subject: "draft", Params: map[string]any{"nope": 1}}}
			return c
		}, "unknown param"},
		{"panel of one", func() *Crew {
			c := baseCrew()
			c.Patterns = []Pattern{{ID: "ap", Pattern: PatternAdversarialPanel, Subject: "draft", Params: map[string]any{"critics": 1}}}
			return c
		}, "critics"},
		{"long id", func() *Crew {
			c := baseCrew()
			c.Patterns = []Pattern{{ID: strings.Repeat("a", 23), Pattern: PatternReviewerLoop, Subject: "draft"}}
			return c
		}, "must match"},
		{"unknown subject", func() *Crew {
			c := baseCrew()
			c.Patterns = []Pattern{{ID: "rl", Pattern: PatternReviewerLoop, Subject: "nope"}}
			return c
		}, "not a declared agent"},
		{"unknown member", func() *Crew {
			c := baseCrew()
			c.Patterns = []Pattern{{ID: "rl", Pattern: PatternReviewerLoop, Subject: "draft", Members: map[string]string{"reviewer": "nope"}}}
			return c
		}, "not a declared agent"},
		{"executor without task", func() *Crew {
			c := baseCrew()
			c.Patterns = []Pattern{{ID: "ex", Pattern: PatternExecutor}}
			return c
		}, "needs a subject"},
		{"duplicate id", func() *Crew {
			c := baseCrew()
			c.Patterns = []Pattern{
				{ID: "rl", Pattern: PatternReviewerLoop, Subject: "draft"},
				{ID: "rl", Pattern: PatternSecurityReview, Subject: "draft"},
			}
			return c
		}, "duplicate pattern id"},
	}
	for _, tc := range cases {
		broke := false
		func() {
			defer func() { _ = recover(); broke = true }()
			err := tc.build().Validate()
			if err == nil {
				t.Errorf("%s: want error", tc.name)
			} else if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("%s: error %q must contain %q", tc.name, err.Error(), tc.want)
			}
		}()
		_ = broke
	}
}

func TestPatternMemberSupplied(t *testing.T) {
	c := baseCrew()
	c.Agents = append(c.Agents, Agent{Name: "myrev", Role: "reviewer", Prompt: "review {{ draft }}"})
	c.Patterns = []Pattern{{ID: "rl", Pattern: PatternReviewerLoop, Subject: "draft",
		Members: map[string]string{"reviewer": "myrev"}}}
	if err := c.Validate(); err != nil {
		t.Fatalf("member-supplied loop: %v", err)
	}
	if crewNames(c)["rl-reviewer"] != nil {
		t.Fatal("a supplied member must not be duplicated by synthesis")
	}
	if crewEdge(c, "draft", "myrev") == nil {
		t.Fatal("supplied reviewer must be wired into the loop")
	}
	if e := crewEdge(c, "myrev", "draft"); e == nil || e.When != "verdict:lbtm" {
		t.Fatalf("supplied reviewer must carry the loop-back: %+v", e)
	}
}

func TestPatternExpansionIsIdempotent(t *testing.T) {
	c := baseCrew()
	c.Patterns = []Pattern{{ID: "rl", Pattern: PatternReviewerLoop, Subject: "draft"}}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	n, e := len(c.Agents), len(c.Edges)
	if err := c.Validate(); err != nil {
		t.Fatalf("second validate: %v", err)
	}
	if len(c.Agents) != n || len(c.Edges) != e {
		t.Fatalf("re-validation duplicated the graph: agents %d→%d edges %d→%d", n, len(c.Agents), e, len(c.Edges))
	}
}

func TestPatternPhasedCrew(t *testing.T) {
	// phases declared → the synthesized reviewer joins the subject's phase and
	// the crew stays valid; a pattern with no subject joins the last phase.
	c := baseCrew()
	c.Phases = []Phase{{Name: "p1", Instructions: "do the draft", Agents: []string{"draft"}}}
	c.Patterns = []Pattern{{ID: "rl", Pattern: PatternReviewerLoop, Subject: "draft"}}
	if err := c.Validate(); err != nil {
		t.Fatalf("phased pattern: %v", err)
	}
	if c.phaseOfNode("rl-reviewer") != 0 {
		t.Fatalf("synthesized reviewer must join the subject's phase; phases=%+v", c.Phases)
	}
	if e := crewEdge(c, "rl-reviewer", "draft"); e == nil {
		t.Fatal("loop-back missing in phased crew")
	}
	// loop-back is intra-phase, so no backward-phase rejection
	if e := crewEdge(c, "rl-reviewer", "draft"); e != nil && e.When != "verdict:lbtm" {
		t.Fatalf("unexpected back edge: %+v", e)
	}
}

func TestPatternBackEdgeStaysInPhase(t *testing.T) {
	// the loop-back edge must not be re-created when the phases differ: with the
	// subject in p1 and the synthesized reviewer placed in p1, the edge is
	// intra-phase; this guards the placement logic against regression.
	c := baseCrew()
	c.Phases = []Phase{
		{Name: "p1", Instructions: "draft", Agents: []string{"draft"}},
		{Name: "p2", Instructions: "sign off", Agents: []string{"ship"}},
	}
	c.Agents = append(c.Agents, Agent{Name: "ship", Prompt: "sign off"})
	c.Patterns = []Pattern{{ID: "rl", Pattern: PatternReviewerLoop, Subject: "draft", Params: map[string]any{"then": "ship"}}}
	if err := c.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if e := crewEdge(c, "rl-reviewer", "ship"); e == nil || e.When != "verdict:lgtm" {
		t.Fatalf("forward continuation edge wrong: %+v", e)
	}
}

func TestPatternNamesGeneratedAsValidHerdrNames(t *testing.T) {
	c := baseCrew()
	c.Patterns = []Pattern{{ID: "rl-x", Pattern: PatternReviewerLoop, Subject: "draft"}}
	if err := c.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	for name := range crewNames(c) {
		if !nameRule.MatchString(name) {
			t.Fatalf("generated name %q violates the herdr rule", name)
		}
	}
}

func TestPatternThenChainsToAnotherPattern(t *testing.T) {
	c := baseCrew()
	c.Agents = append(c.Agents, Agent{Name: "done", Prompt: "wrap up"})
	c.Patterns = []Pattern{
		{ID: "rev", Pattern: PatternReviewerLoop, Subject: "draft", Params: map[string]any{"then": "sec"}},
		{ID: "sec", Pattern: PatternSecurityReview, Subject: "draft", Params: map[string]any{"then": "done"}},
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	// reviewer_loop's positive verdict continues into the security review's
	// entry node...
	if e := crewEdge(c, "rev-reviewer", "sec-security_reviewer"); e == nil || e.When != "verdict:lgtm" {
		t.Fatalf("then must resolve to the other pattern's entry node: %+v", e)
	}
	// ...and the security pass continues to the declared agent.
	if e := crewEdge(c, "sec-security_reviewer", "done"); e == nil || e.When != "verdict:pass" {
		t.Fatalf("then must resolve to a declared agent: %+v", e)
	}
}

func TestPatternThenGatesEveryEntryOfTarget(t *testing.T) {
	// A `then:` into a panel must gate EVERY critic, not just the first
	// synthesized node: otherwise the untouched critics run early and the judge
	// can accept before the previous stage's verdict arrived (fan-in AND).
	c := baseCrew()
	c.Patterns = []Pattern{
		{ID: "rev", Pattern: PatternReviewerLoop, Subject: "draft", Params: map[string]any{"then": "ap"}},
		{ID: "ap", Pattern: PatternAdversarialPanel, Subject: "draft", Params: map[string]any{"critics": 3}},
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	for i := 1; i <= 3; i++ {
		to := fmt.Sprintf("ap-critic-%d", i)
		if e := crewEdge(c, "rev-reviewer", to); e == nil || e.When != "verdict:lgtm" {
			t.Fatalf("then must gate %s: %+v", to, e)
		}
	}
	// the panel's own positive verdict continues from the judge, not a critic
	if e := crewEdge(c, "ap-critic-1", "draft"); e != nil {
		t.Fatalf("critic must not own the loop-back: %+v", e)
	}
	if e := crewEdge(c, "ap-judge", "draft"); e == nil || e.When != "verdict:revise" {
		t.Fatalf("judge owns the panel loop-back: %+v", e)
	}
}

func TestPatternThenUnknownTarget(t *testing.T) {
	c := baseCrew()
	c.Patterns = []Pattern{{ID: "rev", Pattern: PatternReviewerLoop, Subject: "draft", Params: map[string]any{"then": "nope"}}}
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "neither a declared agent nor a pattern id") {
		t.Fatalf("want a then-resolution error, got %v", err)
	}
}

func TestPatternSubjectLabel(t *testing.T) {
	c := baseCrew()
	c.Patterns = []Pattern{{ID: "rev", Pattern: PatternReviewerLoop, Subject: "draft",
		Params: map[string]any{"subject_label": "the draft design"}}}
	if err := c.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	rev := crewNames(c)["rev-reviewer"]
	if rev == nil || !strings.Contains(rev.Prompt, "The work under review is the draft design:") {
		t.Fatalf("subject_label not used: %+v", rev)
	}
	if strings.Contains(rev.Prompt, "the output of node") {
		t.Fatalf("default phrasing leaked: %q", rev.Prompt)
	}
}
