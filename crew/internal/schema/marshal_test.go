package schema

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestMarshalSourceRoundTripsPatternCrew: saving a resolved pattern crew must
// write its SOURCE, and re-loading it must yield the same resolved graph — the
// TUI saves and runs through MarshalSource, so a duplicated expansion here
// would double every synthesized node.
func TestMarshalSourceRoundTripsPatternCrew(t *testing.T) {
	src := []byte(`
name: sop
models: {capable: c/m, efficient: e/m}
agents:
  - {name: plan, role: planner}
  - name: design
    role: architect
    watch: [plan]
roles:
  - {name: architect, profile: Architect, goal: g, tier: capable, writes: design, instruction: "do it"}
patterns:
  - {id: rev, pattern: reviewer_loop, subject: design, params: {max_revisions: 2, then: run}}
  - {id: run, pattern: executor, subject: design, params: {checks: ["true"]}}
`)
	c, err := Load(src)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	before := len(c.Agents)
	out, err := c.MarshalSource()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// the synthesized nodes are gone from the file; the patterns/roles are not
	for _, gone := range []string{"rev-reviewer", "run-implement", "run-verify"} {
		if strings.Contains(string(out), gone) {
			t.Fatalf("synthesized node %q leaked into the saved source:\n%s", gone, out)
		}
	}
	if !strings.Contains(string(out), "reviewer_loop") || !strings.Contains(string(out), "executor") {
		t.Fatalf("patterns missing from the saved source:\n%s", out)
	}
	// role-filled values are not frozen onto the node
	if strings.Contains(string(out), "model: c/m") {
		t.Fatalf("a tier-resolved model must not be written onto the node:\n%s", out)
	}
	// ...and the round trip resolves to the same graph
	c2, err := Load(out)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if len(c2.Agents) != before {
		t.Fatalf("round trip changed the agent count: %d → %d", before, len(c2.Agents))
	}
	if len(c2.Edges) != len(c.Edges) {
		t.Fatalf("round trip changed the edge count: %d → %d", len(c.Edges), len(c2.Edges))
	}
	if len(c2.Checks) != len(c.Checks) {
		t.Fatalf("round trip changed the check count: %d → %d", len(c.Checks), len(c2.Checks))
	}
	// second save is byte-identical (fixed point)
	out2, err := c2.MarshalSource()
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != string(out2) {
		t.Fatalf("marshal is not a fixed point:\n--- first ---\n%s\n--- second ---\n%s", out, out2)
	}
	// the derived watch edge is not written (it regenerates on Load)
	var raw struct {
		Edges []Edge `yaml:"edges"`
	}
	if err := yaml.Unmarshal(out, &raw); err != nil {
		t.Fatalf("reparse: %v", err)
	}
	if len(raw.Edges) != 0 {
		t.Fatalf("derived edge leaked into the saved source: %+v", raw.Edges)
	}
	found := false
	for _, e := range c2.Edges {
		if e.From == "plan" && e.To == "design" {
			found = true
		}
	}
	if !found {
		t.Fatalf("watch edge was not re-derived: %+v", c2.Edges)
	}
}

func TestMarshalSourceRoundTripsPanelChainedToPattern(t *testing.T) {
	// crew-sop's shape: a panel whose `then` chains to another pattern. The
	// panel-continuation edges are derived (entry-node gated), so they must not
	// be written; the round trip must resolve to the same graph.
	src := []byte(`
name: sop-panel
models: {efficient: e/m, capable: c/m}
roles:
  - {name: architect, profile: Architect, goal: design, tier: capable, writes: design, instruction: design it}
agents:
  - {name: plan, role: planner}
  - {name: design, role: architect, watch: [plan], prompt: "do {{ artifact \"plan\" }}"}
patterns:
  - {id: rev, pattern: reviewer_loop, subject: design, params: {then: ap}}
  - {id: ap, pattern: adversarial_panel, subject: design, params: {critics: 3, then: run}}
  - {id: run, pattern: executor, subject: design, params: {max_fixes: 1}, }
`)
	c, err := Load(src)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	out, err := c.MarshalSource()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(out), "adversarial_panel") {
		t.Fatalf("pattern lost from source:\n%s", out)
	}
	for _, gone := range []string{"ap-critic", "ap-judge", "rev-reviewer", "run-implement"} {
		if strings.Contains(string(out), gone) {
			t.Fatalf("synthesized node %q leaked into the saved source:\n%s", gone, out)
		}
	}
	c2, err := Load(out)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if len(c2.Agents) != len(c.Agents) {
		t.Fatalf("round trip changed the agent count: %d → %d", len(c.Agents), len(c2.Agents))
	}
	if len(c2.Edges) != len(c.Edges) {
		t.Fatalf("round trip changed the edge count: %d → %d", len(c.Edges), len(c2.Edges))
	}
	out2, err := c2.MarshalSource()
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != string(out2) {
		t.Fatalf("not a fixed point:\n--- first ---\n%s\n--- second ---\n%s", out, out2)
	}
}

func TestMarshalSourceKeepsDeclaredEdges(t *testing.T) {
	src := []byte(`
name: plain
agents:
  - {name: a, prompt: x}
  - {name: b, prompt: y}
edges:
  - {from: a, to: b, when: "re:READY"}
`)
	c, err := Load(src)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	out, err := c.MarshalSource()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(out), "re:READY") || !strings.Contains(string(out), "a") {
		t.Fatalf("declared edge lost:\n%s", out)
	}
	c2, err := Load(out)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if len(c2.Edges) != 1 || c2.Edges[0].When != "re:READY" {
		t.Fatalf("declared edge changed: %+v", c2.Edges)
	}
}
