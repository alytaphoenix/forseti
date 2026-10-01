// Package schema defines crew.yaml v1: a deterministic agent graph.
//
// Design (docs/design.md §Phase 5): nodes are pi agents with per-node model +
// prompt; edges carry the trigger condition (`when`) and optional loop bound.
// No LLM routing — the runner is a plain data-driven DAG executor.
package schema

import (
	"fmt"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// nameRule mirrors herdr's live-agent naming: [a-z][a-z0-9_-]{0,31}.
var nameRule = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)

var validStatuses = map[string]bool{
	"idle": true, "working": true, "blocked": true, "done": true, "unknown": true,
}

type Crew struct {
	Name   string    `yaml:"name"`
	Routes []Route   `yaml:"routes,omitempty"` // switchyard model-routes (optional, P6-D)
	Agents []Agent   `yaml:"agents"`
	Edges  []Edge    `yaml:"edges,omitempty"`
	Checks []Check   `yaml:"checks,omitempty"` // shell assertions (optional, 6A-1)
	Watch  []Watcher `yaml:"watch,omitempty"`  // output regex watchers (optional, 6C-1)
}

type Agent struct {
	Name  string   `yaml:"name"`
	Kind  string   `yaml:"kind,omitempty"`  // currently only "pi"
	Model string   `yaml:"model,omitempty"` // e.g. halogen/halogen-qwen3.8-flash-next
	Route string   `yaml:"route,omitempty"` // switchyard route id (mutually exclusive with Model)
	Args  []string `yaml:"args,omitempty"`  // extra pi argv (appended after --model)
	// Prompt is a Go text/template; data is map[string]string of
	// upstream node outputs keyed by node name.
	Prompt string `yaml:"prompt"`
}

// Check is a shell assertion run after its `after` node settles.
// exit 0 = pass; failures flip the run's exit code (crew.yaml as E2E harness).
type Check struct {
	Name  string `yaml:"name,omitempty"`  // optional; defaults to check-<after>-<n>
	After string `yaml:"after"`           // node whose completion triggers this check
	Run   string `yaml:"run"`             // shell command, executed in the crew cwd
}

// Watcher arms a pane.wait_for_output regex on a node's pane (advisory:
// emits pattern_matched events, never fails the run).
type Watcher struct {
	Node  string `yaml:"node"`
	Match string `yaml:"match"` // regex (Rust regex crate semantics server-side)
}

// Route declares a switchyard stage_router over an efficient/capable model pool.
// Agents attach with `route: <id>` instead of `model:`.
type Route struct {
	ID         string  `yaml:"id"`
	Type       string  `yaml:"type,omitempty"` // v1: stage_router (the "auto" preset shape)
	Efficient  string  `yaml:"efficient"`
	Capable    string  `yaml:"capable"`
	Picker     string  `yaml:"picker,omitempty"`     // efficient_first (default) | capable_first
	Confidence float64 `yaml:"confidence,omitempty"` // default 0.5
}

type Edge struct {
	From string `yaml:"from"`
	To   string `yaml:"to"`
	// When: "idle" (default) or "re:<regex>" gating on upstream output.
	When string `yaml:"when,omitempty"`
	// MaxVisits bounds cycles; required for any edge participating in a cycle.
	MaxVisits int `yaml:"max_visits,omitempty"`
}

// Load parses and validates crew.yaml content.
func Load(data []byte) (*Crew, error) {
	var c Crew
	if err := yaml.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parse crew.yaml: %w", err)
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

var routeIDRule = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

var validRouteTypes = map[string]bool{"stage_router": true}
var validPickers = map[string]bool{"efficient_first": true, "capable_first": true}

func (c *Crew) Validate() error {
	if len(c.Agents) == 0 {
		return fmt.Errorf("crew has no agents")
	}
	routes := map[string]bool{}
	for i := range c.Routes {
		r := &c.Routes[i]
		if !routeIDRule.MatchString(r.ID) {
			return fmt.Errorf("route %d: id %q must match [a-z0-9][a-z0-9_-]{0,31}", i, r.ID)
		}
		if routes[r.ID] {
			return fmt.Errorf("duplicate route id %q", r.ID)
		}
		routes[r.ID] = true
		if r.Type == "" {
			r.Type = "stage_router"
		}
		if !validRouteTypes[r.Type] {
			return fmt.Errorf("route %q: type %q unsupported (v1: stage_router)", r.ID, r.Type)
		}
		if strings.TrimSpace(r.Efficient) == "" || strings.TrimSpace(r.Capable) == "" {
			return fmt.Errorf("route %q: efficient and capable are required", r.ID)
		}
		if r.Picker == "" {
			r.Picker = "efficient_first"
		}
		if !validPickers[r.Picker] {
			return fmt.Errorf("route %q: picker %q invalid", r.ID, r.Picker)
		}
		if r.Confidence == 0 {
			r.Confidence = 0.5
		}
		if r.Confidence <= 0 || r.Confidence > 1 {
			return fmt.Errorf("route %q: confidence must be in (0,1]", r.ID)
		}
	}
	seen := map[string]bool{}
	for i := range c.Agents {
		a := &c.Agents[i]
		if !nameRule.MatchString(a.Name) {
			return fmt.Errorf("agent %d: name %q violates herdr rule [a-z][a-z0-9_-]{0,31}", i, a.Name)
		}
		if seen[a.Name] {
			return fmt.Errorf("duplicate agent name %q", a.Name)
		}
		seen[a.Name] = true
		if a.Kind == "" {
			a.Kind = "pi"
		}
		if a.Kind != "pi" {
			return fmt.Errorf("agent %q: kind %q unsupported (v1: pi only)", a.Name, a.Kind)
		}
		if a.Model != "" && a.Route != "" {
			return fmt.Errorf("agent %q: set model or route, not both", a.Name)
		}
		if a.Route != "" && !routes[a.Route] {
			return fmt.Errorf("agent %q: unknown route %q", a.Name, a.Route)
		}
		if strings.TrimSpace(a.Prompt) == "" {
			return fmt.Errorf("agent %q: empty prompt", a.Name)
		}
	}
	for i := range c.Checks {
		ch := &c.Checks[i]
		if !seen[ch.After] {
			return fmt.Errorf("check %d: after %q is not an agent", i, ch.After)
		}
		if strings.TrimSpace(ch.Run) == "" {
			return fmt.Errorf("check %d: empty run", i)
		}
		if ch.Name == "" {
			ch.Name = fmt.Sprintf("check-%s-%d", ch.After, i+1)
		}
	}
	for i := range c.Watch {
		w := &c.Watch[i]
		if !seen[w.Node] {
			return fmt.Errorf("watch %d: node %q is not an agent", i, w.Node)
		}
		if _, err := regexp.Compile(w.Match); err != nil {
			return fmt.Errorf("watch %d: bad regex %q: %w", i, w.Match, err)
		}
	}
	for i := range c.Edges {
		e := &c.Edges[i]
		if !seen[e.From] {
			return fmt.Errorf("edge %d: unknown from %q", i, e.From)
		}
		if !seen[e.To] {
			return fmt.Errorf("edge %d: unknown to %q", i, e.To)
		}
		if e.From == e.To {
			return fmt.Errorf("edge %d: self-loop on %q needs max_visits", i, e.From)
		}
		if e.When == "" {
			e.When = "idle"
		}
		if !strings.HasPrefix(e.When, "re:") && !validStatuses[e.When] {
			return fmt.Errorf("edge %d: when %q not a status or re:<regex>", i, e.When)
		}
		if strings.HasPrefix(e.When, "re:") {
			if _, err := regexp.Compile(e.When[3:]); err != nil {
				return fmt.Errorf("edge %d: bad regex %q: %w", i, e.When[3:], err)
			}
		}
	}
	// cycles require at least one bounded edge inside each cycle
	for _, cyc := range c.findCycles() {
		bounded := false
		for _, e := range c.Edges {
			if inCycle(cyc, e) && e.MaxVisits > 0 {
				bounded = true
				break
			}
		}
		if !bounded {
			return fmt.Errorf("cycle %s has no edge with max_visits — refusing unbounded loop", strings.Join(cyc, " → "))
		}
	}
	return nil
}

func inCycle(cyc []string, e Edge) bool {
	for _, n := range cyc {
		if n == e.From || n == e.To {
			return true
		}
	}
	return false
}

// findCycles returns node lists of cycles via DFS (small graphs).
func (c *Crew) findCycles() [][]string {
	adj := map[string][]string{}
	for _, e := range c.Edges {
		adj[e.From] = append(adj[e.From], e.To)
	}
	var cycles [][]string
	color := map[string]int{} // 0 white 1 gray 2 black
	var path []string
	var dfs func(string)
	dfs = func(n string) {
		color[n] = 1
		path = append(path, n)
		for _, m := range adj[n] {
			switch color[m] {
			case 0:
				dfs(m)
			case 1:
				for i, p := range path {
					if p == m {
						cycles = append(cycles, append([]string{}, path[i:]...))
						break
					}
				}
			}
		}
		path = path[:len(path)-1]
		color[n] = 2
	}
	for _, a := range c.Agents {
		if color[a.Name] == 0 {
			dfs(a.Name)
		}
	}
	return cycles
}

// Entry returns nodes with no incoming edges (the run's starting wave).
func (c *Crew) Entry() []string {
	incoming := map[string]bool{}
	for _, e := range c.Edges {
		incoming[e.To] = true
	}
	var out []string
	for _, a := range c.Agents {
		if !incoming[a.Name] {
			out = append(out, a.Name)
		}
	}
	return out
}

// Agent looks up an agent by name.
func (c *Crew) Agent(name string) *Agent {
	for i := range c.Agents {
		if c.Agents[i].Name == name {
			return &c.Agents[i]
		}
	}
	return nil
}

// Route looks up a route by id.
func (c *Crew) Route(id string) *Route {
	for i := range c.Routes {
		if c.Routes[i].ID == id {
			return &c.Routes[i]
		}
	}
	return nil
}

// OutEdges returns edges leaving a node.
func (c *Crew) OutEdges(name string) []Edge {
	var out []Edge
	for _, e := range c.Edges {
		if e.From == name {
			out = append(out, e)
		}
	}
	return out
}
