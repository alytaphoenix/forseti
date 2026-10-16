package schema

import (
	"strings"
	"testing"
)

func TestBackEdgesOnlyMarksTheLoopBack(t *testing.T) {
	// x→a→b→a: only b→a closes the cycle. The old reachability rule marked
	// EVERY edge of the cycle, which made b dispatch at t0 (its only incoming
	// edge was "ignored"). P19 classifies with a DFS from the entry.
	c, err := Load([]byte(`
name: cyc
agents:
  - {name: x, prompt: x}
  - {name: a, prompt: a}
  - {name: b, prompt: b}
edges:
  - {from: x, to: a}
  - {from: a, to: b}
  - {from: b, to: a, max_visits: 2}
`))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	back := c.BackEdges()
	if !back["b>a"] {
		t.Fatalf("b→a must be the back edge: %v", back)
	}
	for _, k := range []string{"x>a", "a>b"} {
		if back[k] {
			t.Fatalf("%s must not be a back edge — the body would dispatch out of order: %v", k, back)
		}
	}
}

func TestBackEdgesExplicitLoopMarker(t *testing.T) {
	// a two-node loop with no other way in: the marker names the loop-back, so
	// the node it points at stays the entry.
	c, err := Load([]byte(`
name: loop2
agents:
  - {name: draft, prompt: write}
  - name: rev
    prompt: "review {{ draft }}"
edges:
  - {from: draft, to: rev}
  - {from: rev, to: draft, when: "verdict:lbtm", max_visits: 2, loop: true}
`))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	back := c.BackEdges()
	if !back["rev>draft"] {
		t.Fatalf("the marked edge must be the back edge: %v", back)
	}
	if back["draft>rev"] {
		t.Fatalf("the forward edge must not be a back edge: %v", back)
	}
}

func TestLoopMarkerNeedsMaxVisits(t *testing.T) {
	_, err := Load([]byte(`
name: cyc
agents:
  - {name: a, prompt: a}
  - {name: b, prompt: b}
edges:
  - {from: a, to: b}
  - {from: b, to: a, loop: true}
`))
	if err == nil || !strings.Contains(err.Error(), "needs max_visits") {
		t.Fatalf("want a max_visits error for a loop edge, got %v", err)
	}
}

func TestSelfLoopStillClassified(t *testing.T) {
	c, err := Load([]byte(`
name: self
agents:
  - {name: a, prompt: a}
  - {name: b, prompt: b}
edges:
  - {from: a, to: b}
  - {from: b, to: b, max_visits: 3}
`))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !c.BackEdges()["b>b"] {
		t.Fatalf("self-loop must be a back edge (re-dispatch only): %v", c.BackEdges())
	}
	// a→b is the gate that starts b
	if c.BackEdges()["a>b"] {
		t.Fatal("a→b must remain a readiness gate")
	}
}

func TestTwoNodeCycleWithoutMarkerStillRejected(t *testing.T) {
	// the degenerate case the entry rule exists for: no loop marker, no way in.
	_, err := Load([]byte(`
name: deg
agents:
  - {name: a, prompt: a}
  - {name: b, prompt: b}
edges:
  - {from: a, to: b, max_visits: 2}
  - {from: b, to: a, max_visits: 2}
`))
	if err == nil || !strings.Contains(err.Error(), "no entry node") {
		t.Fatalf("want no-entry error, got %v", err)
	}
}
