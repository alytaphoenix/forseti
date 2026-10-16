// Typed message bus (Phase 19D) and verdict edges (Phase 19B).
//
// MetaGPT's roles are message-driven: a Role subscribes to the message tags it
// cares about (Role._watch) and every message carries cause_by — the action
// that produced it (metagpt/environment/base_env.py). forseti keeps the graph
// as the source of truth and *derives* the subscription edges from the declared
// artifact types, so pub/sub is compiled rather than interpreted: the P13
// scheduler, phases, bounded back edges and the run log all apply unchanged.
//
// Verdict edges are the other half: a review loop only loops if the reviewer's
// verdict is machine-readable. `when: verdict:lbtm` fires only when the
// producer's output ends with that verdict (MetaGPT's LGTM/LBTM from
// WriteReview — docs/spikes.md S31).
package schema

import (
	"fmt"
	"regexp"
	"strings"
)

// verdictLine matches a verdict line anywhere in an output block. Anchored to
// its own line so prose that merely mentions the word does not count (the
// prompt echo does match, which is why ParseVerdict takes the LAST one — see
// the doc comment there).
var verdictLine = regexp.MustCompile(`(?im)^[ \t>*-]*VERDICT[ \t]*:[ \t]*([A-Za-z0-9][A-Za-z0-9_-]*)`)

// bareVerdict matches MetaGPT's LGTM/LBTM convention: the bare token on its own
// line (WriteReview's ActionNode output IS the bare token). Prose that merely
// mentions the word does not count.
var bareVerdict = regexp.MustCompile(`(?im)^[ \t>*-]*(LGTM|LBTM)[ \t]*$`)

// ParseVerdict extracts the verdict label a node's output carries, lowercased.
//
// The LAST verdict line wins: pi panes echo the prompt (which names the
// expected verdicts) before the model's answer, so the final line is the
// model's own decision. A bare LGTM/LBTM (MetaGPT's convention) is accepted as
// a fallback and normalised to lgtm/lbtm.
func ParseVerdict(output string) (string, bool) {
	if m := verdictLine.FindAllStringSubmatch(output, -1); len(m) > 0 {
		return strings.ToLower(m[len(m)-1][1]), true
	}
	if m := bareVerdict.FindAllStringSubmatch(output, -1); len(m) > 0 {
		return strings.ToLower(m[len(m)-1][1]), true
	}
	return "", false
}

// WritesType is the artifact type a node publishes: the explicit `writes:`, the
// role's `writes:`, else the node's own name (so a node is addressable as an
// artifact type without extra ceremony).
func (a *Agent) WritesType() string {
	if a == nil {
		return ""
	}
	if strings.TrimSpace(a.Writes) != "" {
		return strings.TrimSpace(a.Writes)
	}
	return a.Name
}

// Producers maps every published artifact type to the nodes that write it.
func (c *Crew) Producers() map[string][]string {
	out := map[string][]string{}
	for i := range c.Agents {
		a := &c.Agents[i]
		t := a.WritesType()
		out[t] = append(out[t], a.Name)
	}
	return out
}

// deriveSubscriptions adds one edge (producer → watcher) for every artifact
// type a node watches. Idempotent: an edge that already exists (declared or
// derived on an earlier Validate) is left alone.
func (c *Crew) deriveSubscriptions() error {
	watchers := false
	for i := range c.Agents {
		if len(c.Agents[i].Watch) > 0 {
			watchers = true
			break
		}
	}
	if !watchers {
		return nil
	}
	producers := c.Producers()
	edgeExists := map[string]bool{}
	for _, e := range c.Edges {
		edgeExists[e.From+">"+e.To] = true
	}
	for i := range c.Agents {
		a := &c.Agents[i]
		for _, t := range a.Watch {
			t = strings.TrimSpace(t)
			if t == "" {
				return fmt.Errorf("agent %q: empty watch artifact type", a.Name)
			}
			ps := producers[t]
			if len(ps) == 0 {
				return fmt.Errorf("agent %q: watches artifact type %q but no agent writes it (add `writes: %s` to the producer, or drop the watch)", a.Name, t, t)
			}
			for _, p := range ps {
				if p == a.Name {
					continue // a node does not need an edge to itself
				}
				key := p + ">" + a.Name
				if edgeExists[key] {
					continue
				}
				edgeExists[key] = true
				c.Edges = append(c.Edges, Edge{From: p, To: a.Name, When: "idle"})
				if c.derivedEdges == nil {
					c.derivedEdges = map[string]bool{}
				}
				c.derivedEdges[key] = true
			}
		}
	}
	return nil
}

// hasAgent reports whether a node name is declared (used by pattern expansion).
func (c *Crew) hasAgent(name string) bool { return c.Agent(name) != nil }

// VerdictLabelOf returns the verdict an edge is gated on ("" when it is not a
// verdict edge).
func (e Edge) VerdictLabelOf() string {
	if m := verdictWhenRule.FindStringSubmatch(e.When); m != nil {
		return m[1]
	}
	return ""
}
