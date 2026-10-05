package schema

import (
	"strings"
	"testing"
)

func TestValidTwoAgent(t *testing.T) {
	c, err := Load([]byte(`
name: t
agents:
  - {name: planner, model: m1, prompt: plan}
  - {name: coder, model: m2, prompt: "do {{ .planner }}"}
edges:
  - {from: planner, to: coder, when: "re:PLAN_READY"}
`))
	if err != nil {
		t.Fatalf("want valid, got %v", err)
	}
	if got := c.Entry(); len(got) != 1 || got[0] != "planner" {
		t.Fatalf("entry = %v", got)
	}
	if c.Agent("coder").Model != "m2" {
		t.Fatalf("lookup broken")
	}
}

func TestNameRule(t *testing.T) {
	_, err := Load([]byte("name: t\nagents:\n  - {name: Bad_Name, prompt: x}\n"))
	if err == nil || !strings.Contains(err.Error(), "herdr rule") {
		t.Fatalf("want name-rule error, got %v", err)
	}
}

func TestUnknownEdgeEndpoint(t *testing.T) {
	_, err := Load([]byte("name: t\nagents:\n  - {name: a, prompt: x}\nedges:\n  - {from: a, to: ghost}\n"))
	if err == nil || !strings.Contains(err.Error(), "unknown to") {
		t.Fatalf("want unknown-to error, got %v", err)
	}
}

func TestUnboundedCycleRejected(t *testing.T) {
	_, err := Load([]byte(`
name: t
agents:
  - {name: c, prompt: entry}
  - {name: a, prompt: x}
  - {name: b, prompt: y}
edges:
  - {from: c, to: a}
  - {from: a, to: b}
  - {from: b, to: a}
`))
	if err == nil || !strings.Contains(err.Error(), "max_visits") {
		t.Fatalf("want max_visits error, got %v", err)
	}
}

func TestBoundedCycleOK(t *testing.T) {
	c, err := Load([]byte(`
name: t
agents:
  - {name: c, prompt: entry}
  - {name: a, prompt: x}
  - {name: b, prompt: y}
edges:
  - {from: c, to: a}
  - {from: a, to: b}
  - {from: b, to: a, max_visits: 2}
`))
	if err != nil {
		t.Fatalf("bounded cycle should be valid, got %v", err)
	}
	if got := c.Entry(); len(got) != 1 || got[0] != "c" {
		t.Fatalf("entry = %v, want [c]", got)
	}
}

func TestNoEntryRejected(t *testing.T) {
	// P13 BUG-03: an all-cycle crew (no agent without incoming edges) can
	// never start — must fail validation, not silently run nothing.
	_, err := Load([]byte(`
name: t
agents:
  - {name: a, prompt: x}
  - {name: b, prompt: y}
edges:
  - {from: a, to: b, max_visits: 2}
  - {from: b, to: a, max_visits: 2}
`))
	if err == nil || !strings.Contains(err.Error(), "no entry node") {
		t.Fatalf("want no-entry error, got %v", err)
	}
}

func TestDuplicateEdgeRejected(t *testing.T) {
	// P13 BUG-12: duplicate (from,to) edges collide on the scheduler's
	// "from>to" key — a guaranteed skip-event/dispatch confusion at runtime.
	_, err := Load([]byte(`
name: t
agents:
  - {name: a, prompt: x}
  - {name: b, prompt: y}
edges:
  - {from: a, to: b}
  - {from: a, to: b}
`))
	if err == nil || !strings.Contains(err.Error(), "duplicate edge") {
		t.Fatalf("want duplicate-edge error, got %v", err)
	}
}

func TestSelfLoopWithMaxVisitsOK(t *testing.T) {
	// P13 BUG-22: the error text always promised self-loops are legal WITH
	// max_visits (and the P13 scheduler re-arm makes them meaningful). A
	// self-loop-only crew still needs an entry (BUG-03) — the loop is a retry
	// mechanism on a node that is also reachable from an entry.
	if _, err := Load([]byte(`
name: t
agents:
  - {name: c, prompt: entry}
  - {name: a, prompt: x}
edges:
  - {from: c, to: a}
  - {from: a, to: a, max_visits: 2}
`)); err != nil {
		t.Fatalf("self-loop with max_visits should be valid, got %v", err)
	}
	_, err := Load([]byte("name: t\nagents:\n  - {name: a, prompt: x}\nedges:\n  - {from: a, to: a}\n"))
	if err == nil || !strings.Contains(err.Error(), "self-loop") {
		t.Fatalf("want self-loop error, got %v", err)
	}
}

func TestCrewNameRule(t *testing.T) {
	// P13 BUG-21: the crew name lands in run-log filenames — path-unsafe or
	// empty names must fail validation.
	_, err := Load([]byte("name: \"bad/name\"\nagents:\n  - {name: a, prompt: x}\n"))
	if err == nil || !strings.Contains(err.Error(), "crew name") {
		t.Fatalf("want crew-name error, got %v", err)
	}
	_, err = Load([]byte("name: \"\"\nagents:\n  - {name: a, prompt: x}\n"))
	if err == nil || !strings.Contains(err.Error(), "crew name") {
		t.Fatalf("want empty-crew-name error, got %v", err)
	}
}

func TestUnrelatedEdgeDoesNotBoundCycle(t *testing.T) {
	// P13 BUG-11: an edge that merely TOUCHES a cycle (one endpoint inside)
	// must not satisfy the cycle's max_visits boundedness requirement.
	_, err := Load([]byte(`
name: t
agents:
  - {name: c, prompt: entry}
  - {name: a, prompt: x}
  - {name: b, prompt: y}
edges:
  - {from: c, to: a, max_visits: 1}
  - {from: a, to: b}
  - {from: b, to: a}
`))
	if err == nil || !strings.Contains(err.Error(), "max_visits") {
		t.Fatalf("want max_visits error (touching edge must not bound the cycle), got %v", err)
	}
}

func TestBadWhenRegex(t *testing.T) {
	_, err := Load([]byte("name: t\nagents:\n  - {name: a, prompt: x}\n  - {name: b, prompt: y}\nedges:\n  - {from: a, to: b, when: \"re:[(\"}\n"))
	if err == nil || !strings.Contains(err.Error(), "bad regex") {
		t.Fatalf("want bad regex error, got %v", err)
	}
}

func TestDefaultKindAndWhen(t *testing.T) {
	c, err := Load([]byte("name: t\nagents:\n  - {name: a, prompt: x}\n  - {name: b, prompt: y}\nedges:\n  - {from: a, to: b}\n"))
	if err != nil {
		t.Fatalf("want valid, got %v", err)
	}
	if c.Agent("a").Kind != "pi" {
		t.Fatalf("kind default not applied")
	}
	if c.Edges[0].When != "idle" {
		t.Fatalf("when default not applied")
	}
}

func TestChecksValid(t *testing.T) {
	c, err := Load([]byte(`
name: t
agents:
  - {name: planner, model: m, prompt: plan}
checks:
  - {after: planner, run: "grep -q PLAN bus/planner.md"}
`))
	if err != nil {
		t.Fatalf("want valid, got %v", err)
	}
	if c.Checks[0].Name != "check-planner-1" {
		t.Fatalf("check name default = %q", c.Checks[0].Name)
	}
}

func TestCheckUnknownAfter(t *testing.T) {
	_, err := Load([]byte("name: t\nagents:\n  - {name: a, prompt: x}\nchecks:\n  - {after: ghost, run: \"true\"}\n"))
	if err == nil || !strings.Contains(err.Error(), "not an agent") {
		t.Fatalf("want unknown-after error, got %v", err)
	}
}

func TestCheckEmptyRun(t *testing.T) {
	_, err := Load([]byte("name: t\nagents:\n  - {name: a, prompt: x}\nchecks:\n  - {after: a, run: \"  \"}\n"))
	if err == nil || !strings.Contains(err.Error(), "empty run") {
		t.Fatalf("want empty-run error, got %v", err)
	}
}

func TestRouteDefaults(t *testing.T) {
	c, err := Load([]byte(`name: t

routes:
  - id: auto-pool
    efficient: halogen/halogen-qwen3.8-flash-next
    capable: opencode-go/glm-5.3-flash
agents:
  - {name: triage, route: auto-pool, prompt: go}
`))
	if err != nil {
		t.Fatalf("want valid, got %v", err)
	}
	r := c.Route("auto-pool")
	if r == nil || r.Type != "stage_router" || r.Picker != "efficient_first" || r.Confidence != 0.5 {
		t.Fatalf("route defaults not applied: %+v", r)
	}
	if c.Agent("triage").Route != "auto-pool" {
		t.Fatalf("agent route not parsed")
	}
}

func TestRouteModelMutuallyExclusive(t *testing.T) {
	_, err := Load([]byte(`name: t

routes:
  - {id: r1, efficient: e, capable: c}
agents:
  - {name: a, model: m, route: r1, prompt: x}
`))
	if err == nil || !strings.Contains(err.Error(), "not both") {
		t.Fatalf("want model/route exclusivity error, got %v", err)
	}
}

func TestRouteUnknownRef(t *testing.T) {
	_, err := Load([]byte("name: t\nagents:\n  - {name: a, route: nosuch, prompt: x}\n"))
	if err == nil || !strings.Contains(err.Error(), "unknown route") {
		t.Fatalf("want unknown-route error, got %v", err)
	}
}

func TestRouteMissingPool(t *testing.T) {
	_, err := Load([]byte("name: t\nroutes:\n  - {id: r1, efficient: e}\nagents:\n  - {name: a, model: m, prompt: x}\n"))
	if err == nil || !strings.Contains(err.Error(), "required") {
		t.Fatalf("want missing-pool error, got %v", err)
	}
}

func TestWatchValidation(t *testing.T) {
	_, err := Load([]byte("name: t\nagents:\n  - {name: a, prompt: x}\nwatch:\n  - {node: ghost, match: \"x\"}\n"))
	if err == nil || !strings.Contains(err.Error(), "not an agent") {
		t.Fatalf("want unknown-node error, got %v", err)
	}
	_, err = Load([]byte("name: t\nagents:\n  - {name: a, prompt: x}\nwatch:\n  - {node: a, match: \"[(\"}\n"))
	if err == nil || !strings.Contains(err.Error(), "bad regex") {
		t.Fatalf("want bad regex error, got %v", err)
	}
	if _, err := Load([]byte("name: t\nagents:\n  - {name: a, prompt: x}\nwatch:\n  - {node: a, match: \"TESTS FAILED\"}\n")); err != nil {
		t.Fatalf("valid watch rejected: %v", err)
	}
}

func TestLayaEdgeGate(t *testing.T) {
	c, err := Load([]byte(`
name: t
agents:
  - {name: triage, model: m, prompt: plan}
  - {name: builder, model: m, prompt: build}
  - {name: docs, model: m, prompt: write}
edges:
  - {from: triage, to: builder, when: "laya:choice:code changes and fixes"}
  - {from: triage, to: docs, when: "laya:choice:documentation work", min_confidence: 0.7}
`))
	if err != nil {
		t.Fatalf("want valid, got %v", err)
	}
	for _, e := range c.Edges {
		if !e.IsLaya() || e.LayaInstructions() == "" {
			t.Fatalf("laya parse broken: %+v", e)
		}
	}
	if c.Edges[0].MinConfidence != 0.5 || c.Edges[1].MinConfidence != 0.7 {
		t.Fatalf("min_confidence defaults/override broken: %v %v", c.Edges[0].MinConfidence, c.Edges[1].MinConfidence)
	}
}

func TestLayaEdgeValidation(t *testing.T) {
	_, err := Load([]byte("name: t\nagents:\n  - {name: a, prompt: x}\n  - {name: b, prompt: y}\nedges:\n  - {from: a, to: b, when: \"laya:choice:\"}\n"))
	if err == nil || !strings.Contains(err.Error(), "empty laya instructions") {
		t.Fatalf("want empty-instructions error, got %v", err)
	}
	_, err = Load([]byte("name: t\nagents:\n  - {name: a, prompt: x}\n  - {name: b, prompt: y}\nedges:\n  - {from: a, to: b, min_confidence: 0.8}\n"))
	if err == nil || !strings.Contains(err.Error(), "only apply to laya") {
		t.Fatalf("want min_confidence-scope error, got %v", err)
	}
	_, err = Load([]byte("name: t\nagents:\n  - {name: a, prompt: x}\n  - {name: b, prompt: y}\nedges:\n  - {from: a, to: b, when: \"laya:choice:ship it\", min_confidence: 2}\n"))
	if err == nil || !strings.Contains(err.Error(), "(0,1]") {
		t.Fatalf("want min_confidence-range error, got %v", err)
	}
}

func TestPhasesValid(t *testing.T) {
	c, err := Load([]byte(`
name: t
agents:
  - {name: explorer, prompt: explore}
  - {name: coder, prompt: "build {{ .explorer }}"}
phases:
  - name: research
    instructions: Read-only. No edits.
    agents: [explorer]
  - name: build
    instructions: Implement what research found.
    agents: [coder]
edges:
  - {from: explorer, to: coder}
`))
	if err != nil {
		t.Fatalf("want valid, got %v", err)
	}
	if got := c.PhaseOf("explorer"); got != 0 {
		t.Fatalf("PhaseOf(explorer) = %d", got)
	}
	if got := c.PhaseOf("coder"); got != 1 {
		t.Fatalf("PhaseOf(coder) = %d", got)
	}
	if c.Phase("build") == nil || c.Phase("research").Instructions == "" {
		t.Fatalf("phase lookup broken")
	}
}

func TestNoPhasesBackcompat(t *testing.T) {
	c, err := Load([]byte("name: t\nagents:\n  - {name: a, prompt: x}\n  - {name: b, prompt: y}\nedges:\n  - {from: a, to: b}\n"))
	if err != nil {
		t.Fatalf("phase-less crew must stay valid, got %v", err)
	}
	if len(c.Phases) != 0 || c.PhaseOf("a") != -1 {
		t.Fatalf("phases should be empty, got %+v", c.Phases)
	}
}

func TestPhaseDuplicateName(t *testing.T) {
	_, err := Load([]byte(`
name: t
agents:
  - {name: a, prompt: x}
  - {name: b, prompt: y}
phases:
  - {name: p, instructions: do, agents: [a]}
  - {name: p, instructions: do, agents: [b]}
`))
	if err == nil || !strings.Contains(err.Error(), "duplicate phase") {
		t.Fatalf("want duplicate-phase error, got %v", err)
	}
}

func TestPhaseNameRule(t *testing.T) {
	_, err := Load([]byte(`
name: t
agents:
  - {name: a, prompt: x}
phases:
  - {name: Bad_Name, instructions: do, agents: [a]}
`))
	if err == nil || !strings.Contains(err.Error(), "herdr rule") {
		t.Fatalf("want name-rule error, got %v", err)
	}
}

func TestPhaseUnknownAgent(t *testing.T) {
	_, err := Load([]byte(`
name: t
agents:
  - {name: a, prompt: x}
phases:
  - {name: p, instructions: do, agents: [ghost]}
`))
	if err == nil || !strings.Contains(err.Error(), "not declared") {
		t.Fatalf("want not-declared error, got %v", err)
	}
}

func TestPhaseDoubleMembership(t *testing.T) {
	_, err := Load([]byte(`
name: t
agents:
  - {name: a, prompt: x}
  - {name: b, prompt: y}
phases:
  - {name: p1, instructions: do, agents: [a, b]}
  - {name: p2, instructions: do, agents: [b]}
`))
	if err == nil || !strings.Contains(err.Error(), "at most one phase") {
		t.Fatalf("want double-membership error, got %v", err)
	}
}

func TestPhaseUnphasedAgentRejected(t *testing.T) {
	_, err := Load([]byte(`
name: t
agents:
  - {name: a, prompt: x}
  - {name: b, prompt: y}
phases:
  - {name: p, instructions: do, agents: [a]}
`))
	if err == nil || !strings.Contains(err.Error(), "not in any phase") {
		t.Fatalf("want unphased-agent error, got %v", err)
	}
}

func TestPhaseEmptyFields(t *testing.T) {
	_, err := Load([]byte("name: t\nagents:\n  - {name: a, prompt: x}\nphases:\n  - {name: p, agents: [a]}\n"))
	if err == nil || !strings.Contains(err.Error(), "empty instructions") {
		t.Fatalf("want empty-instructions error, got %v", err)
	}
	_, err = Load([]byte("name: t\nagents:\n  - {name: a, prompt: x}\nphases:\n  - {name: p, instructions: do}\n"))
	if err == nil || !strings.Contains(err.Error(), "no agents") {
		t.Fatalf("want no-agents error, got %v", err)
	}
}

func TestPhaseBackwardEdge(t *testing.T) {
	_, err := Load([]byte(`
name: t
agents:
  - {name: a, prompt: x}
  - {name: b, prompt: y}
phases:
  - {name: p1, instructions: do, agents: [a]}
  - {name: p2, instructions: do, agents: [b]}
edges:
  - {from: b, to: a}
`))
	if err == nil || !strings.Contains(err.Error(), "goes backward") {
		t.Fatalf("want backward-edge error, got %v", err)
	}
}
