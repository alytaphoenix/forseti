package schema

import (
	"strings"
	"testing"
)

func TestParseVerdict(t *testing.T) {
	cases := []struct {
		name string
		out  string
		want string
		ok   bool
	}{
		{"plain", "looks fine\n\nVERDICT: LGTM", "lgtm", true},
		{"bare LBTM", "the loop guard is wrong\nLBTM", "lbtm", true},
		{"pass/fail", "all green\nVERDICT: PASS", "pass", true},
		{"case folded", "VERDICT: Revise", "revise", true},
		{"markdown quoted", "> VERDICT: ACCEPT", "accept", true},
		{"bullet", "- VERDICT: FAIL", "fail", true},
		{"last one wins (prompt echo first)", "End with `VERDICT: ACCEPT` or `VERDICT: REVISE`.\n...analysis...\nVERDICT: REVISE", "revise", true},
		{"none", "just prose with no decision", "", false},
		{"word in prose is not a verdict", "we should remember the LGTM convention", "", false},
		{"inline mention is not a verdict line", "the token LGTM appears here mid-sentence", "", false},
		{"bare token with indentation", "analysis\n   LGTM", "lgtm", true},
	}
	for _, tc := range cases {
		got, ok := ParseVerdict(tc.out)
		if ok != tc.ok || got != tc.want {
			t.Errorf("%s: ParseVerdict(%q) = (%q,%v), want (%q,%v)", tc.name, tc.out, got, ok, tc.want, tc.ok)
		}
	}
}

func TestParseVerdictLastLineWinsOverPromptEcho(t *testing.T) {
	// the pane echoes the prompt, which names BOTH expected verdicts; the
	// model's own final line must win.
	echo := "End with `VERDICT: ACCEPT` (ship as-is) or `VERDICT: REVISE`.\n\n...analysis...\n\nVERDICT: REVISE"
	if got, _ := ParseVerdict(echo); got != "revise" {
		t.Fatalf("got %q, want revise", got)
	}
}

func TestDeriveSubscriptions(t *testing.T) {
	c := &Crew{
		Name:   "bus",
		Models: map[string]string{TierCapable: "c/m", TierEfficient: "e/m"},
		Agents: []Agent{
			{Name: "planner", Role: "planner", Prompt: ""}, // writes: plan
			{Name: "worker", Role: "engineer", Prompt: "do {{ artifact \"plan\" }}", Watch: []string{"plan"}},
		},
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if e := crewEdge(c, "planner", "worker"); e == nil || e.When != "idle" {
		t.Fatalf("watch must derive the producer edge: %+v", e)
	}
	// idempotent
	if err := c.Validate(); err != nil {
		t.Fatalf("second validate: %v", err)
	}
	n := 0
	for _, e := range c.Edges {
		if e.From == "planner" && e.To == "worker" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("derived edge duplicated: %d", n)
	}
}

func TestDeriveSubscriptionsMissingProducer(t *testing.T) {
	c := &Crew{
		Name:   "bus",
		Models: map[string]string{TierCapable: "c/m"},
		Agents: []Agent{{Name: "a", Role: "engineer", Prompt: "p", Watch: []string{"nowhere"}}},
	}
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "no agent writes it") {
		t.Fatalf("want a missing-producer error, got %v", err)
	}
}

func TestDeriveSubscriptionsExplicitEdgeWins(t *testing.T) {
	c := &Crew{
		Name:   "bus",
		Models: map[string]string{TierCapable: "c/m"},
		Agents: []Agent{
			{Name: "p", Role: "planner", Prompt: "plan"},
			{Name: "w", Role: "engineer", Prompt: "work", Watch: []string{"plan"}},
		},
		Edges: []Edge{{From: "p", To: "w", When: "re:READY"}},
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if e := crewEdge(c, "p", "w"); e.When != "re:READY" {
		t.Fatalf("an explicit edge must not be replaced by the derived one: %+v", e)
	}
	if len(c.Edges) != 1 {
		t.Fatalf("derived edge duplicated an explicit one: %+v", c.Edges)
	}
}

func TestVerdictEdgeLabelsAreValidated(t *testing.T) {
	c := &Crew{
		Name:   "v",
		Agents: []Agent{{Name: "seed", Prompt: "p"}, {Name: "a", Prompt: "p"}, {Name: "b", Prompt: "q"}},
		Edges:  []Edge{{From: "a", To: "b", When: "verdict:LGTM", MaxVisits: 1}},
	}
	if err := c.Validate(); err == nil {
		t.Fatal("uppercase verdict labels must be rejected (labels are lowercased)")
	}
	c.Edges = []Edge{
		{From: "seed", To: "a", When: "idle"},
		{From: "a", To: "b", When: "verdict:lgtm"},
		{From: "b", To: "a", When: "verdict:lbtm", MaxVisits: 2},
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("lowercase verdict labels must validate: %v", err)
	}
}

func TestVerdictEdgeRejectsLayaDials(t *testing.T) {
	c := &Crew{
		Name:   "v",
		Agents: []Agent{{Name: "seed", Prompt: "p"}, {Name: "a", Prompt: "p"}, {Name: "b", Prompt: "q"}},
		Edges:  []Edge{{From: "a", To: "b", When: "verdict:lgtm", MinConfidence: 0.5}},
	}
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "only apply to laya edges") {
		t.Fatalf("want a laya-only error, got %v", err)
	}
}
