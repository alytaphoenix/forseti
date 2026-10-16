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

// ValidName reports whether name satisfies the shared naming rule (agents,
// phases, crew names — the TUI forms validate with it too).
func ValidName(name string) bool { return nameRule.MatchString(name) }

var validStatuses = map[string]bool{
	"idle": true, "working": true, "blocked": true, "done": true, "unknown": true,
}

type Crew struct {
	Name     string            `yaml:"name"`
	Models   map[string]string `yaml:"models,omitempty"`   // tier → provider/model; roles resolve their tier through it (P19A)
	Roles    []Role            `yaml:"roles,omitempty"`    // archetype overrides/additions (P19A)
	Patterns []Pattern         `yaml:"patterns,omitempty"` // SOP patterns, instantiated into the graph at Validate (P19C)
	Routes   []Route           `yaml:"routes,omitempty"`   // switchyard model-routes (optional, P6-D)
	Agents   []Agent           `yaml:"agents"`
	Phases   []Phase           `yaml:"phases,omitempty"` // ordered work stages with barrier semantics (P11)
	Edges    []Edge            `yaml:"edges,omitempty"`
	Checks   []Check           `yaml:"checks,omitempty"` // shell assertions (optional, 6A-1)
	Watch    []Watcher         `yaml:"watch,omitempty"`  // output regex watchers (optional, 6C-1)
	Memory   *MemorySpec       `yaml:"memory,omitempty"` // shared-memory integration (optional, P14)

	// P19 expansion bookkeeping (never serialized; see MarshalSource).
	pendingThen  []pendingThen       // `then:` chains awaiting pattern resolution
	patternEntry map[string][]string // pattern id → the nodes a `then:` into that pattern should gate
	// P19 provenance: what Validate synthesized, so a saved file keeps the
	// SOURCE (roles + patterns + watch) and re-expands on Load.
	synthAgents   map[string]bool // node names synthesized by a pattern
	derivedEdges  map[string]bool // "from>to" edges synthesized at Validate
	derivedChecks map[string]bool // "after\x00name" checks synthesized at Validate
}

// MemorySpec wires the shared memory service into a crew run (P14). The block
// is opt-in: present = on. The crew's memory namespace is `namespace` (default
// the crew name); rows land in the service namespace `crew-<ns>`, isolated
// from other agents and crews. Auto-recall prepends recalled facts to each
// node's prompt at dispatch; auto-write posts a node's output back on completion.
// Both are advisory — a down service never fails a run.
type MemorySpec struct {
	Namespace     string `yaml:"namespace,omitempty"`       // default: crew name
	AutoRecall    *bool  `yaml:"auto_recall,omitempty"`     // default true
	RecallK       int    `yaml:"recall_k,omitempty"`        // default 3
	AutoWrite     *bool  `yaml:"auto_write,omitempty"`      // default true
	WriteMaxChars int    `yaml:"write_max_chars,omitempty"` // output cap posted (default 4000)
}

// AutoRecallOn / AutoWriteOn resolve the *bool defaults (present block → on).
func (m *MemorySpec) AutoRecallOn() bool { return m != nil && (m.AutoRecall == nil || *m.AutoRecall) }
func (m *MemorySpec) AutoWriteOn() bool  { return m != nil && (m.AutoWrite == nil || *m.AutoWrite) }

// NS returns the crew memory namespace (default = crew name).
func (m *MemorySpec) NS(crewName string) string {
	if m != nil && m.Namespace != "" {
		return m.Namespace
	}
	return crewName
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
	Role  string   `yaml:"role,omitempty"`  // archetype from the role library (P19A)
	Model string   `yaml:"model,omitempty"` // e.g. halogen/halogen-qwen3.8-flash-next
	Route string   `yaml:"route,omitempty"` // switchyard route id (mutually exclusive with Model)
	Args  []string `yaml:"args,omitempty"`  // extra pi argv (appended after --model)
	// Watch lists artifact types this node reacts to; Writes names the type its
	// output publishes (defaults: the role's, else the node name). Declaring a
	// watch derives the incoming edges — the typed pub/sub of MetaGPT's
	// Role._watch, compiled into the graph (P19D).
	Watch  []string `yaml:"watch,omitempty"`
	Writes string   `yaml:"writes,omitempty"`
	// Prompt is a Go text/template; data is map[string]string of
	// upstream node outputs keyed by node name.
	Prompt string `yaml:"prompt"`
	// Memory opts this node out of the crew's memory integration: "off"
	// skips auto-recall AND auto-write for this node (default inherit).
	Memory string `yaml:"memory,omitempty"` // "" (inherit) | "on" | "off"
	// MemoryQuery overrides the auto-recall query for this node (default:
	// the node name + a head of its rendered prompt). Templated like Prompt.
	MemoryQuery string `yaml:"memory_query,omitempty"`
}

// MemoryOff reports whether this node disables the crew memory integration.
func (a *Agent) MemoryOff() bool { return a.Memory == "off" }

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

// Route declares a switchyard routing algorithm that crew agents attach to with
// `route: <id>` instead of `model:`.
//
// forseti emits the typed keys for the tier shapes it can derive from
// efficient/capable and passes Options through verbatim, so every switchyard
// 0.3.0 route type and key is reachable without forseti re-encoding the
// router's schema (which would drift). The router's own `--dry-run` remains the
// authority on semantics — see switchyard.Validate.
type Route struct {
	ID         string         `yaml:"id"`
	Type       string         `yaml:"type,omitempty"`       // switchyard algorithm type; default stage_router
	Efficient  string         `yaml:"efficient,omitempty"`  // provider/model — the cheap tier
	Capable    string         `yaml:"capable,omitempty"`    // provider/model — the strong tier
	Judge      string         `yaml:"judge,omitempty"`      // provider/model — a dedicated judge target (<id>_judge)
	Picker     string         `yaml:"picker,omitempty"`     // stage_router: efficient_first (default) | capable_first
	Confidence float64        `yaml:"confidence,omitempty"` // stage_router: agreement a decisive pick needs (default 0.5)
	Options    map[string]any `yaml:"options,omitempty"`    // raw switchyard route keys, emitted verbatim
}

// KnownRouteTypes mirrors switchyard 0.3.0's AlgorithmSpec variants
// (switchyard-runner/src/algorithm.rs). forseti derives typed keys only for
// the tier shapes below; everything else is option-driven, so a router release
// that adds a type or key needs no forseti change.
var KnownRouteTypes = []string{
	"stage_router", "auto", "plan_execute", "llm_classifier", "passthrough",
	"random", "noop", "composite", "advisor", "prefill_router",
}

// routeTierKeys maps a tiered route type to its (capable, efficient) keys.
var routeTierKeys = map[string][2]string{
	"stage_router": {"capable_target", "efficient_target"},
	// `auto` is switchyard's stage_router preset (efficient_first + 0.5) with
	// no knobs of its own.
	"auto":           {"capable_target", "efficient_target"},
	"plan_execute":   {"capable_target", "efficient_target"},
	"llm_classifier": {"strong_target", "weak_target"},
}

// optionDrivenRouteTypes take no typed keys from forseti — their whole route
// table (nested sub-tables included) comes from `options`.
var optionDrivenRouteTypes = map[string]bool{
	"composite": true, "advisor": true, "prefill_router": true,
}

var validPickers = map[string]bool{"efficient_first": true, "capable_first": true}

func knownRouteType(t string) bool {
	for _, k := range KnownRouteTypes {
		if k == t {
			return true
		}
	}
	return false
}

// EffectiveType is the route type, defaulted to stage_router.
func (r Route) EffectiveType() string {
	if r.Type == "" {
		return "stage_router"
	}
	return r.Type
}

// TargetNames are the switchyard target names forseti generates for this route.
// `options` can reference them to wire a custom route shape.
func (r Route) TargetNames() (efficient, capable string) {
	return r.ID + "_eff", r.ID + "_cap"
}

// JudgeTarget is the target name generated for a route's dedicated judge model.
func (r Route) JudgeTarget() string { return r.ID + "_judge" }

// RouteKeys is the switchyard route table forseti emits: the typed keys of the
// route's type, plus Options merged in. A collision is an error — forseti's
// typed keys are authoritative, and silently overwriting one would produce a
// config that reads differently than the crew file.
func (r Route) RouteKeys() (map[string]any, error) {
	t := r.EffectiveType()
	if !knownRouteType(t) {
		return nil, fmt.Errorf("route %q: unknown switchyard type %q (known: %s)",
			r.ID, t, strings.Join(KnownRouteTypes, ", "))
	}
	eff, cap := r.TargetNames()
	keys := map[string]any{}
	if pair, ok := routeTierKeys[t]; ok {
		keys[pair[0]] = cap
		keys[pair[1]] = eff
	}
	switch t {
	case "stage_router":
		picker := r.Picker
		if picker == "" {
			picker = "efficient_first"
		}
		conf := r.Confidence
		if conf == 0 {
			conf = 0.5
		}
		keys["picker"] = picker
		keys["confidence_threshold"] = conf
	case "passthrough":
		keys["target"] = cap
	case "random":
		keys["targets"] = []any{cap, eff}
	}
	// a dedicated judge model makes llm_classifier ergonomic: the judge target
	// forseti generates is wired in unless the crew wires its own.
	if t == "llm_classifier" {
		if r.Judge != "" {
			if _, wired := r.Options["classifier_target"]; !wired {
				keys["classifier_target"] = r.JudgeTarget()
			}
		}
		if _, set := r.Options["base_threshold"]; !set {
			keys["base_threshold"] = 0.5
		}
	}
	for k, v := range r.Options {
		if k == "id" || k == "type" {
			return nil, fmt.Errorf("route %q: options.%s is set by forseti", r.ID, k)
		}
		if _, taken := keys[k]; taken {
			return nil, fmt.Errorf("route %q: options.%s duplicates the %s field forseti derives", r.ID, k, t)
		}
		keys[k] = v
	}
	return keys, nil
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
	// Loop (P19) marks this edge as the one that CLOSES a cycle: it gates
	// re-dispatch only and never gates initial readiness (so the node it points
	// back at can still be the entry). Unmarked cycles are classified
	// automatically (DFS back edges) when the crew has an entry; an ambiguous
	// cycle — e.g. a two-node review loop with no other way in — needs the
	// marker. A loop edge must be bounded (max_visits).
	Loop bool `yaml:"loop,omitempty"`
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

// crewNSRule bounds a memory namespace so `crew-<ns>` stays within the memory
// service's agent limit (≤ 32 → ns ≤ 26) and its own CREW_RE.
var crewNSRule = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,25}$`)

// layaWhenRule parses `laya:choice:<instructions>` edge gates (instructions
// may be empty at parse time — validation rejects empties explicitly).
var layaWhenRule = regexp.MustCompile(`^laya:choice:(.*)$`)

// verdictWhenRule parses `verdict:<label>` edge gates (P19B): the edge fires
// only when the producing node's output carries that verdict line (see
// ParseVerdict). Labels match [a-z0-9_-]+ and are the lowercased verdict token.
var verdictWhenRule = regexp.MustCompile(`^verdict:([a-z0-9][a-z0-9_-]*)$`)

func (c *Crew) Validate() error {
	// P13 BUG-21: the crew name lands in run-log filenames and herdr view/tab
	// sources — empty or path-unsafe names broke log creation silently.
	if !nameRule.MatchString(c.Name) {
		return fmt.Errorf("crew name %q violates herdr rule [a-z][a-z0-9_-]{0,31} (it names run logs and view sources)", c.Name)
	}
	// P19: patterns instantiate first (they append agents/edges), then the role
	// pack resolves archetypes onto nodes, then the typed pub/sub derived from
	// watch/writes adds the subscription edges — all before the ordinary graph
	// validation below, so nothing a pattern or role produced escapes it.
	if err := c.expandPatterns(); err != nil {
		return err
	}
	roles := NewRolePack(c.Roles)
	if err := roles.Validate(c); err != nil {
		return err
	}
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
		if !knownRouteType(r.Type) {
			return fmt.Errorf("route %q: unknown switchyard type %q (known: %s)",
				r.ID, r.Type, strings.Join(KnownRouteTypes, ", "))
		}
		effOK := strings.TrimSpace(r.Efficient) != ""
		capOK := strings.TrimSpace(r.Capable) != ""
		if _, tiered := routeTierKeys[r.Type]; tiered && (!effOK || !capOK) {
			return fmt.Errorf("route %q: type %s needs both efficient and capable", r.ID, r.Type)
		}
		switch r.Type {
		case "passthrough":
			if !capOK {
				return fmt.Errorf("route %q: type passthrough needs capable (the single target)", r.ID)
			}
		case "llm_classifier":
			if _, ok := r.Options["classifier_target"]; !ok && r.Judge == "" {
				return fmt.Errorf("route %q: type llm_classifier needs a judge (judge: provider/model, or options.classifier_target)", r.ID)
			}
		}
		if optionDrivenRouteTypes[r.Type] && len(r.Options) == 0 {
			return fmt.Errorf("route %q: type %s is option-driven — set options: with its switchyard keys", r.ID, r.Type)
		}
		if r.Picker != "" {
			if r.Type != "stage_router" {
				return fmt.Errorf("route %q: picker only applies to stage_router", r.ID)
			}
			if !validPickers[r.Picker] {
				return fmt.Errorf("route %q: picker %q must be efficient_first or capable_first", r.ID, r.Picker)
			}
		}
		if r.Type == "stage_router" {
			// defaults are applied at emission too (RouteKeys); setting them here
			// keeps a loaded crew self-describing for the builder TUI.
			if r.Picker == "" {
				r.Picker = "efficient_first"
			}
			if r.Confidence == 0 {
				r.Confidence = 0.5
			}
		}
		if r.Confidence != 0 && r.Type != "stage_router" {
			return fmt.Errorf("route %q: confidence only applies to stage_router", r.ID)
		}
		if r.Confidence != 0 && (r.Confidence <= 0 || r.Confidence > 1) {
			return fmt.Errorf("route %q: confidence must be in (0,1]", r.ID)
		}
		if _, err := r.RouteKeys(); err != nil {
			return err
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
		if strings.TrimSpace(a.Prompt) == "" && a.Role == "" {
			return fmt.Errorf("agent %q: empty prompt", a.Name)
		}
		if a.Memory != "" && a.Memory != "on" && a.Memory != "off" {
			return fmt.Errorf("agent %q: memory must be on|off (got %q)", a.Name, a.Memory)
		}
	}
	// P19D: typed pub/sub from watch/writes — after role resolution (which fills
	// defaults), before edge validation (so the derived edges are validated too).
	if err := c.deriveSubscriptions(); err != nil {
		return err
	}
	if c.Memory != nil { // P14: validate + default the memory block
		if ns := c.Memory.NS(c.Name); !crewNSRule.MatchString(ns) {
			return fmt.Errorf("memory namespace %q must match [a-z0-9][a-z0-9_-]{0,25}", ns)
		}
		if c.Memory.RecallK < 0 {
			return fmt.Errorf("memory.recall_k must be >= 0")
		}
		if c.Memory.WriteMaxChars < 0 {
			return fmt.Errorf("memory.write_max_chars must be >= 0")
		}
		if c.Memory.RecallK == 0 {
			c.Memory.RecallK = 3
		}
		if c.Memory.WriteMaxChars == 0 {
			c.Memory.WriteMaxChars = 4000
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
		if e.From == e.To && e.MaxVisits == 0 {
			// P13 BUG-22: a self-loop WITH max_visits is legal (the error text
			// always promised it; the P13 scheduler re-arms make it meaningful)
			return fmt.Errorf("edge %d: self-loop on %q needs max_visits", i, e.From)
		}
		if e.Loop && e.MaxVisits == 0 {
			// P19: a loop edge re-dispatches its target, so it must be bounded.
			return fmt.Errorf("edge %d: loop edge %s→%s needs max_visits (it re-dispatches the target)", i, e.From, e.To)
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
		if !strings.HasPrefix(e.When, "re:") && !validStatuses[e.When] && !layaWhenRule.MatchString(e.When) && !verdictWhenRule.MatchString(e.When) {
			return fmt.Errorf("edge %d: when %q not a status, re:<regex>, laya:choice:<instructions>, or verdict:<label>", i, e.When)
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
	// P13 BUG-12: duplicate (from,to) edges are a typo class that collides on
	// the scheduler's "from>to" key — reject at validation.
	edgeSeen := map[string]bool{}
	for i, e := range c.Edges {
		key := e.From + ">" + e.To
		if edgeSeen[key] {
			return fmt.Errorf("edge %d: duplicate edge %s (same from→to declared twice)", i, key)
		}
		edgeSeen[key] = true
	}
	// P13 BUG-03: a crew whose every node has an incoming edge has NO entry —
	// nothing can ever start (fan-in AND semantics); fail loudly at validation.
	// P19: an explicitly marked loop edge (`loop: true`) gates re-dispatch only,
	// so it does not take away a node's entry status; everything else counts.
	incomingCount := map[string]int{}
	for _, e := range c.Edges {
		if e.Loop {
			continue
		}
		incomingCount[e.To]++
	}
	entry := false
	for _, a := range c.Agents {
		if incomingCount[a.Name] == 0 {
			entry = true
			break
		}
	}
	if !entry {
		return fmt.Errorf("crew has no entry node (every agent has an incoming edge) — nothing can start; an all-cycle crew needs an entry agent with no incoming edges")
	}
	// cycles require at least one bounded edge ON each cycle
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

// inCycle (P13 BUG-11): true only for edges ON the cycle — both endpoints in
// the cycle AND consecutive along it. The old any-endpoint test let an
// unrelated touching edge satisfy the boundedness requirement.
func inCycle(cyc []string, e Edge) bool {
	for i, n := range cyc {
		if e.From == n && e.To == cyc[(i+1)%len(cyc)] {
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

// BackEdges returns "from>to" keys of edges that close a directed cycle (the
// target can reach the source — self-loops included). P13: these are cycle
// RE-ENTRY edges — the scheduler does not require them for INITIAL dispatch
// (strict fan-in AND over a back edge would deadlock every cycle: the target
// would wait for an edge that can only fire after the target itself ran).
// BackEdges lists the edges that gate RE-DISPATCH only — they never gate
// initial readiness, so the node they point back at can still be the entry
// (P13 semantics, refined in P19).
//
// An edge is a back edge when either:
//   - it carries `loop: true` (explicit: "this is the edge that closes a
//     cycle"), or
//   - it is a DFS back edge — an edge to a node on the current DFS stack —
//     discovered from the crew's entry nodes (nodes with no incoming non-loop
//     edge).
//
// The old definition ("target reaches source") marked EVERY edge of a cycle as
// a back edge, which made a cycle's body dispatch out of order (each body node
// saw all its incoming edges ignored and became ready at t0). DFS classification
// marks only the true loop-back, so `x→a→b→a` now runs a → b → a.
//
// A crew where every node has a non-loop incoming edge has no entry: forseti
// rejects it at validation. BackEdges() still answers (falling back to
// reachability) so the scheduler behaves identically on such a graph.
func (c *Crew) BackEdges() map[string]bool {
	out := map[string]bool{}
	adj := map[string][]string{}
	hasNonLoopIncoming := map[string]bool{}
	for _, e := range c.Edges {
		if e.Loop {
			out[e.From+">"+e.To] = true
			continue
		}
		adj[e.From] = append(adj[e.From], e.To)
		hasNonLoopIncoming[e.To] = true
	}
	roots := 0
	for _, a := range c.Agents {
		if hasNonLoopIncoming[a.Name] {
			continue
		}
		roots++
		dfsBackEdges(adj, a.Name, out)
	}
	if roots == 0 {
		// no entry (invalid crew): keep the previous, conservative answer
		for _, e := range c.Edges {
			if reaches(adj, e.To, e.From) {
				out[e.From+">"+e.To] = true
			}
		}
	}
	return out
}

// dfsBackEdges classifies the edges reachable from root as tree/forward/cross
// or back (target on the current path), recording back edges into out.
func dfsBackEdges(adj map[string][]string, root string, out map[string]bool) {
	state := map[string]int{} // 0 unseen, 1 on stack, 2 done
	type frame struct {
		node string
		next int
	}
	state[root] = 1
	stack := []frame{{node: root}}
	for len(stack) > 0 {
		f := &stack[len(stack)-1]
		outs := adj[f.node]
		if f.next >= len(outs) {
			state[f.node] = 2
			stack = stack[:len(stack)-1]
			continue
		}
		to := outs[f.next]
		f.next++
		switch state[to] {
		case 1: // on the current path → the edge closes a cycle
			out[f.node+">"+to] = true
		case 0:
			state[to] = 1
			stack = append(stack, frame{node: to})
		}
	}
}

// reaches reports whether `to` is reachable from `from` (small graphs; the
// trivial self case is true only when a real edge loops back).
func reaches(adj map[string][]string, from, to string) bool {
	seen := map[string]bool{}
	stack := []string{from}
	for len(stack) > 0 {
		n := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		for _, m := range adj[n] {
			if m == to {
				return true
			}
			if !seen[m] {
				seen[m] = true
				stack = append(stack, m)
			}
		}
	}
	return false
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
