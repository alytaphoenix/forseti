// Source-form serialization (Phase 19). Validate expands roles/patterns/watch
// into plain agents and edges, so marshaling the in-memory crew would write a
// file that re-expands on Load into a SECOND copy (duplicate nodes). The TUI
// saves and runs through MarshalSource instead: it writes the SOURCE — the
// declared agents/edges, the patterns, the roles — and drops everything
// Validate synthesized.
package schema

import (
	"slices"

	"gopkg.in/yaml.v3"
)

// MarshalSource renders the crew in source form:
//
//   - nodes synthesized by a pattern, edges synthesized by a pattern or by the
//     typed pub/sub derivation, and checks a pattern materialised are omitted
//     (they regenerate identically on Load);
//   - a value a role filled for its node (its tier's model, its writes, its
//     watch) is cleared, so the role stays the single source of truth;
//   - the default gate (`when: idle`) is dropped.
//
// Load(MarshalSource()) is therefore a fixed point of the resolved graph, and a
// TUI edit of a pattern crew never forges a second copy of the synthesized
// nodes.
func (c *Crew) MarshalSource() ([]byte, error) {
	cp := *c
	pack := NewRolePack(c.Roles)
	cp.Agents = make([]Agent, 0, len(c.Agents))
	for i := range c.Agents {
		a := c.Agents[i]
		if c.synthAgents[a.Name] {
			continue
		}
		if a.Role != "" {
			if r, ok := pack.Get(a.Role); ok {
				if a.Writes == r.Writes {
					a.Writes = ""
				}
				if len(r.Watch) > 0 && slices.Equal(a.Watch, r.Watch) {
					a.Watch = nil
				}
				if r.Tier != "" && c.Models[r.Tier] != "" && a.Model == c.Models[r.Tier] {
					a.Model = ""
				}
			}
		}
		cp.Agents = append(cp.Agents, a)
	}
	cp.Edges = make([]Edge, 0, len(c.Edges))
	for _, e := range c.Edges {
		if c.derivedEdges[e.From+">"+e.To] {
			continue
		}
		if e.When == "idle" {
			e.When = ""
		}
		cp.Edges = append(cp.Edges, e)
	}
	cp.Checks = make([]Check, 0, len(c.Checks))
	for _, ch := range c.Checks {
		if c.derivedChecks[ch.After+"\x00"+ch.Name] {
			continue
		}
		cp.Checks = append(cp.Checks, ch)
	}
	return yaml.Marshal(&cp)
}
