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
	_, err := Load([]byte("agents:\n  - {name: Bad_Name, prompt: x}\n"))
	if err == nil || !strings.Contains(err.Error(), "herdr rule") {
		t.Fatalf("want name-rule error, got %v", err)
	}
}

func TestUnknownEdgeEndpoint(t *testing.T) {
	_, err := Load([]byte("agents:\n  - {name: a, prompt: x}\nedges:\n  - {from: a, to: ghost}\n"))
	if err == nil || !strings.Contains(err.Error(), "unknown to") {
		t.Fatalf("want unknown-to error, got %v", err)
	}
}

func TestUnboundedCycleRejected(t *testing.T) {
	_, err := Load([]byte(`
agents:
  - {name: a, prompt: x}
  - {name: b, prompt: y}
edges:
  - {from: a, to: b}
  - {from: b, to: a}
`))
	if err == nil || !strings.Contains(err.Error(), "max_visits") {
		t.Fatalf("want max_visits error, got %v", err)
	}
}

func TestBoundedCycleOK(t *testing.T) {
	_, err := Load([]byte(`
agents:
  - {name: a, prompt: x}
  - {name: b, prompt: y}
edges:
  - {from: a, to: b}
  - {from: b, to: a, max_visits: 2}
`))
	if err != nil {
		t.Fatalf("bounded cycle should be valid, got %v", err)
	}
}

func TestBadWhenRegex(t *testing.T) {
	_, err := Load([]byte("agents:\n  - {name: a, prompt: x}\n  - {name: b, prompt: y}\nedges:\n  - {from: a, to: b, when: \"re:[(\"}\n"))
	if err == nil || !strings.Contains(err.Error(), "bad regex") {
		t.Fatalf("want bad regex error, got %v", err)
	}
}

func TestDefaultKindAndWhen(t *testing.T) {
	c, err := Load([]byte("agents:\n  - {name: a, prompt: x}\n  - {name: b, prompt: y}\nedges:\n  - {from: a, to: b}\n"))
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
	_, err := Load([]byte("agents:\n  - {name: a, prompt: x}\nchecks:\n  - {after: ghost, run: \"true\"}\n"))
	if err == nil || !strings.Contains(err.Error(), "not an agent") {
		t.Fatalf("want unknown-after error, got %v", err)
	}
}

func TestCheckEmptyRun(t *testing.T) {
	_, err := Load([]byte("agents:\n  - {name: a, prompt: x}\nchecks:\n  - {after: a, run: \"  \"}\n"))
	if err == nil || !strings.Contains(err.Error(), "empty run") {
		t.Fatalf("want empty-run error, got %v", err)
	}
}

func TestRouteDefaults(t *testing.T) {
	c, err := Load([]byte(`
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
	_, err := Load([]byte(`
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
	_, err := Load([]byte("agents:\n  - {name: a, route: nosuch, prompt: x}\n"))
	if err == nil || !strings.Contains(err.Error(), "unknown route") {
		t.Fatalf("want unknown-route error, got %v", err)
	}
}

func TestRouteMissingPool(t *testing.T) {
	_, err := Load([]byte("routes:\n  - {id: r1, efficient: e}\nagents:\n  - {name: a, model: m, prompt: x}\n"))
	if err == nil || !strings.Contains(err.Error(), "required") {
		t.Fatalf("want missing-pool error, got %v", err)
	}
}

func TestWatchValidation(t *testing.T) {
	_, err := Load([]byte("agents:\n  - {name: a, prompt: x}\nwatch:\n  - {node: ghost, match: \"x\"}\n"))
	if err == nil || !strings.Contains(err.Error(), "not an agent") {
		t.Fatalf("want unknown-node error, got %v", err)
	}
	_, err = Load([]byte("agents:\n  - {name: a, prompt: x}\nwatch:\n  - {node: a, match: \"[(\"}\n"))
	if err == nil || !strings.Contains(err.Error(), "bad regex") {
		t.Fatalf("want bad regex error, got %v", err)
	}
	if _, err := Load([]byte("agents:\n  - {name: a, prompt: x}\nwatch:\n  - {node: a, match: \"TESTS FAILED\"}\n")); err != nil {
		t.Fatalf("valid watch rejected: %v", err)
	}
}
