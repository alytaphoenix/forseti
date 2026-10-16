// Human-readable rendering of a resolved crew (Phase 19): what the roles and
// patterns actually compiled to. Deterministic — declaration order for agents,
// edges as declared (derived/pattern edges appended in expansion order).
package schema

import (
	"fmt"
	"sort"
	"strings"
)

// Explain renders the resolved graph after Validate. Callers pass a validated
// crew; the output names each node, its archetype, resolved model/route, phase,
// artifact subscriptions and the edges (with their gates).
func Explain(c *Crew) string {
	var b strings.Builder
	fmt.Fprintf(&b, "crew %s\n", c.Name)
	if len(c.Models) > 0 {
		fmt.Fprintf(&b, "models: %s\n", kv(c.Models))
	}
	if len(c.Roles) > 0 {
		fmt.Fprintf(&b, "roles (crew-declared): %s\n", roleSummaries(c.Roles))
	}

	phase := map[string]string{}
	for i := range c.Phases {
		for _, n := range c.Phases[i].Agents {
			phase[n] = c.Phases[i].Name
		}
	}
	produced := map[string][]string{}
	for i := range c.Agents {
		a := &c.Agents[i]
		produced[a.WritesType()] = append(produced[a.WritesType()], a.Name)
	}
	incomingBy := map[string][]string{}
	for _, e := range c.Edges {
		incomingBy[e.To] = append(incomingBy[e.To], edgeLabel(e))
	}

	pack := NewRolePack(c.Roles)
	b.WriteString("nodes:\n")
	for i := range c.Agents {
		a := &c.Agents[i]
		meta := []string{}
		if a.Role != "" {
			label := "role=" + a.Role
			if r, ok := pack.Get(a.Role); ok && r.Profile != "" {
				label += "(" + r.Profile + ")"
			}
			meta = append(meta, label)
		}
		switch {
		case a.Route != "":
			meta = append(meta, "route="+a.Route)
		case a.Model != "":
			meta = append(meta, "model="+a.Model)
		}
		if p := phase[a.Name]; p != "" {
			meta = append(meta, "phase="+p)
		}
		if t := a.WritesType(); t != "" {
			meta = append(meta, "writes="+t)
		}
		if len(a.Watch) > 0 {
			meta = append(meta, "watch="+strings.Join(a.Watch, ","))
		}
		if len(incomingBy[a.Name]) == 0 {
			meta = append(meta, "ENTRY")
		}
		fmt.Fprintf(&b, "  %-24s %s\n", a.Name, strings.Join(meta, " "))
		for _, lbl := range incomingBy[a.Name] {
			fmt.Fprintf(&b, "  %-24s   ← %s\n", "", lbl)
		}
	}
	if len(c.Patterns) > 0 {
		b.WriteString("patterns:\n")
		for _, p := range c.Patterns {
			fmt.Fprintf(&b, "  %-10s %s subject=%s\n", p.ID, p.Pattern, p.Subject)
		}
	}
	if len(c.Checks) > 0 {
		b.WriteString("checks:\n")
		for _, ch := range c.Checks {
			fmt.Fprintf(&b, "  after %-20s %s\n", ch.After, ch.Run)
		}
	}
	if c.Memory != nil {
		fmt.Fprintf(&b, "memory: namespace=%s recall_k=%d write_max_chars=%d\n", c.Memory.NS(c.Name), c.Memory.RecallK, c.Memory.WriteMaxChars)
	}
	return b.String()
}

func edgeLabel(e Edge) string {
	s := fmt.Sprintf("%s [%s]", e.From, e.When)
	if e.Loop {
		s += " loop"
	}
	if e.MaxVisits > 0 {
		s += fmt.Sprintf(" max_visits=%d", e.MaxVisits)
	}
	return s
}

func kv(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+m[k])
	}
	return strings.Join(parts, " ")
}

func roleSummaries(roles []Role) string {
	out := make([]string, 0, len(roles))
	for _, r := range roles {
		bits := []string{r.Name}
		if r.Profile != "" {
			bits = append(bits, "profile="+r.Profile)
		}
		if r.Tier != "" {
			bits = append(bits, "tier="+r.Tier)
		}
		if len(r.Actions) > 0 {
			names := make([]string, 0, len(r.Actions))
			for _, a := range r.Actions {
				names = append(names, a.Name)
			}
			bits = append(bits, "actions="+strings.Join(names, ","))
		}
		out = append(out, strings.Join(bits, " "))
	}
	sort.Strings(out)
	return strings.Join(out, "; ")
}
