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
	Phases []Phase   `yaml:"phases,omitempty"` // ordered work stages with barrier semantics (P11)
	Edges  []Edge    `yaml:"edges,omitempty"`
	Checks []Check   `yaml:"checks,omitempty"` // shell assertions (optional, 6A-1)
	Watch  []Watcher `yaml:"watch,omitempty"`  // output regex watchers (optional, 6C-1)
}

// Phase is an ordered stage of work: its agents only dispatch after every
// node in earlier phases has reached a terminal state (done/skipped/failed).
// Instructions prefix each member agent's prompt at dispatch (same template
// engine: upstream outputs + {{ memory }} work inside instructions).
// A crew without phases behaves exactly as before (one implicit phase).
type Phase struct {
	Name         string   `yaml:"name"`         // herdr name rule
	Instructions string   `yaml:"instructions"` // template prepended to member prompts
	Agents       []string `yaml:"agents"`       // member node names; each agent lives in at most one phase
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
	Name  string `yaml:"name,omitempty"` // optional; defaults to check-<after>-<n>
	After string `yaml:"after"`          // node whose completion triggers this check
	Run   string `yaml:"run"`            // shell command, executed in the crew cwd
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
	// When: "idle" (default), "re:<regex>" (output gate), or
	// "laya:choice:<instructions>" — a bounded decision gate: the runner asks
	// the local Laya endpoint a choice question whose options are the
	// laya-gated targets of the same from-node (+ implicit "other"); the
	// argmax above min_confidence activates exactly that edge.
	When string `yaml:"when,omitempty"`
	// MaxVisits bounds cycles; required for any edge participating in a cycle.
	MaxVisits int `yaml:"max_visits,omitempty"`
	// MinConfidence gates laya edges (default 0.5): below it the edge
	// abstains (skipped with the distribution recorded) rather than guess.
	MinConfidence float64 `yaml:"min_confidence,omitempty"`
	// StateFile: when set, the laya gate judges THIS file's content (relative
	// to the workdir) instead of the pane transcript — the artifact the agent
	// wrote is the review target, not the terminal scrollback.
	StateFile string `yaml:"state_file,omitempty"`
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

// layaWhenRule parses `laya:choice:<instructions>` edge gates (instructions
// may be empty at parse time — validation rejects empties explicitly).
var layaWhenRule = regexp.MustCompile(`^laya:choice:(.*)$`)

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
	phaseSeen := map[string]bool{}
	membership := map[string]string{} // agent name → phase name
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
	for i := range c.Phases {
		p := &c.Phases[i]
		if !nameRule.MatchString(p.Name) {
			return fmt.Errorf("phase %d: name %q violates herdr rule [a-z][a-z0-9_-]{0,31}", i, p.Name)
		}
		if phaseSeen[p.Name] {
			return fmt.Errorf("duplicate phase name %q", p.Name)
		}
		phaseSeen[p.Name] = true
		if len(p.Agents) == 0 {
			return fmt.Errorf("phase %q: no agents", p.Name)
		}
		if strings.TrimSpace(p.Instructions) == "" {
			return fmt.Errorf("phase %q: empty instructions", p.Name)
		}
		for _, m := range p.Agents {
			if !seen[m] {
				return fmt.Errorf("phase %q: agent %q is not declared", p.Name, m)
			}
			if membership[m] != "" {
				return fmt.Errorf("agent %q: already in phase %q (each agent lives in at most one phase)", m, membership[m])
			}
			membership[m] = p.Name
		}
	}
	// with phases declared, every agent must be a member (explicit beats implicit)
	if len(c.Phases) > 0 {
		for _, a := range c.Agents {
			if membership[a.Name] == "" {
				return fmt.Errorf("agent %q: not in any phase (phases are declared — add it to one)", a.Name)
			}
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
		if m := layaWhenRule.FindStringSubmatch(e.When); m != nil {
			if strings.TrimSpace(m[1]) == "" {
				return fmt.Errorf("edge %d: empty laya instructions", i)
			}
			if e.MinConfidence == 0 {
				e.MinConfidence = 0.5
			}
			if e.MinConfidence <= 0 || e.MinConfidence > 1 {
				return fmt.Errorf("edge %d: min_confidence must be in (0,1]", i)
			}
		} else if e.MinConfidence != 0 || e.StateFile != "" {
			return fmt.Errorf("edge %d: min_confidence/state_file only apply to laya edges", i)
		}
		if !strings.HasPrefix(e.When, "re:") && !validStatuses[e.When] && !layaWhenRule.MatchString(e.When) {
			return fmt.Errorf("edge %d: when %q not a status, re:<regex>, or laya:choice:<instructions>", i, e.When)
		}
		if strings.HasPrefix(e.When, "re:") {
			if _, err := regexp.Compile(e.When[3:]); err != nil {
				return fmt.Errorf("edge %d: bad regex %q: %w", i, e.When[3:], err)
			}
		}
		// backward phase edges rejected (v1: time flows forward between phases;
		// cross-phase loop-backs are a documented future item)
		if len(c.Phases) > 0 {
			fo, fi := indexPhaseOf(c.Phases, membership[e.From]), indexPhaseOf(c.Phases, membership[e.To])
			if fo > fi {
				return fmt.Errorf("edge %d: %q→%q goes backward (phase %q → %q) — cross-phase loop-backs unsupported in v1", i, e.From, e.To, membership[e.From], membership[e.To])
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

// IsLaya reports whether the edge's when is a laya decision gate.
func (e Edge) IsLaya() bool { return layaWhenRule.MatchString(e.When) }

// LayaInstructions extracts the choice instructions from a laya gate.
func (e Edge) LayaInstructions() string {
	if m := layaWhenRule.FindStringSubmatch(e.When); m != nil {
		return m[1]
	}
	return ""
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

// Phase looks up a phase by name.
func (c *Crew) Phase(name string) *Phase {
	for i := range c.Phases {
		if c.Phases[i].Name == name {
			return &c.Phases[i]
		}
	}
	return nil
}

// PhaseOf returns the phase index an agent belongs to (0-based; -1 when the
// crew declares no phases or the agent is unphased).
func (c *Crew) PhaseOf(agent string) int {
	if len(c.Phases) == 0 {
		return -1
	}
	for i := range c.Phases {
		for _, m := range c.Phases[i].Agents {
			if m == agent {
				return i
			}
		}
	}
	return -1
}

// indexPhaseOf maps a phase name to its index (-1 for "" or unknown); used by
// validation, which reasons over the membership map.
func indexPhaseOf(phases []Phase, name string) int {
	for i := range phases {
		if phases[i].Name == name {
			return i
		}
	}
	return -1
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
