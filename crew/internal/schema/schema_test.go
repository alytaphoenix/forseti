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
