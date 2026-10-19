package flow

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

var (
	flowNameRule     = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)
	evidenceIDRule   = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)
	verdictLabelRule = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)
)

// StageOrder is the canonical lifecycle order. Explain and validation both use
// it, so a file's declaration order never changes the rendered output.
var StageOrder = []string{
	"pre_edit", "pre_tool", "post_edit", "post_turn",
	"pre_commit", "commit_msg", "pre_push", "pre_pr",
}

var validModes = map[string]bool{"repo": true, "agentic": true, "hybrid": true}

var validTriggers = map[string]bool{
	"session": true, "pre-commit": true, "commit-msg": true,
	"pre-push": true, "pre-pr": true, "manual": true,
}

var validSeverities = map[string]bool{"off": true, "warn": true, "block": true}

// Only statuses the pi runtime can actually observe (plus the runner-only
// "unknown" for a crashed agent).
var validEvidenceKinds = map[string]bool{
	"shell": true, "diff": true, "artifact": true, "review": true,
	"human": true, "all": true, "any": true,
}

var validActors = map[string]bool{
	"agent": true, "reviewer": true, "shell": true, "human": true,
}

func (f *File) validate() error {
	seen := map[string]bool{}
	for i, fl := range f.Flows {
		if fl == nil {
			return fmt.Errorf("flow %d: empty definition", i)
		}
		if !flowNameRule.MatchString(fl.ID) {
			return fmt.Errorf("flow %q: id must match [a-z][a-z0-9_-]{0,31}", fl.ID)
		}
		if seen[fl.ID] {
			return fmt.Errorf("duplicate flow id %q", fl.ID)
		}
		seen[fl.ID] = true
		if err := fl.validate(); err != nil {
			return fmt.Errorf("flow %q: %w", fl.ID, err)
		}
	}
	return nil
}

func (fl *Flow) validate() error {
	mode := fl.EffectiveMode()
	if !validModes[mode] {
		return fmt.Errorf("mode %q must be repo, agentic, or hybrid", mode)
	}
	if fl.Trigger != "" && !validTriggers[fl.Trigger] {
		return fmt.Errorf("trigger %q is not one of %s", fl.Trigger, strings.Join(sortedKeys(validTriggers), ", "))
	}
	if err := fl.validateDefaults(); err != nil {
		return err
	}
	if err := fl.validateEvidence(); err != nil {
		return err
	}
	if err := fl.validateRules(); err != nil {
		return err
	}
	if err := fl.validatePhases(); err != nil {
		return err
	}
	if err := fl.validateWaivers(); err != nil {
		return err
	}
	if len(fl.On) == 0 && len(fl.Phases) == 0 {
		return fmt.Errorf("flow needs on: gates or phases:")
	}
	return nil
}

func (fl *Flow) validateDefaults() error {
	d := fl.Defaults
	if d == nil {
		return nil
	}
	if d.Severity != "" && !validSeverities[d.Severity] {
		return fmt.Errorf("defaults.severity %q must be off, warn, or block", d.Severity)
	}
	if d.MaxContinues != nil && *d.MaxContinues < 0 {
		return fmt.Errorf("defaults.max_continues must be >= 0")
	}
	if d.MaxCycles != nil && *d.MaxCycles < 0 {
		return fmt.Errorf("defaults.max_cycles must be >= 0")
	}
	if d.Timeout != "" {
		if _, err := time.ParseDuration(d.Timeout); err != nil {
			return fmt.Errorf("defaults.timeout %q is not a duration: %w", d.Timeout, err)
		}
	}
	return nil
}

func (fl *Flow) validateEvidence() error {
	ids := fl.evidenceIDs()
	for _, id := range ids {
		if !evidenceIDRule.MatchString(id) {
			return fmt.Errorf("evidence %q: id must match [a-z][a-z0-9_-]{0,63}", id)
		}
		if err := validateEvidence(id, fl.Evidence[id]); err != nil {
			return err
		}
	}
	// Composition references need every definition parsed first.
	for _, id := range ids {
		for _, ref := range fl.Evidence[id].Of {
			if _, ok := fl.Evidence[ref]; !ok {
				return fmt.Errorf("evidence %q: of references unknown evidence %q", id, ref)
			}
		}
	}
	if cyc := evidenceCycle(fl); len(cyc) > 0 {
		return fmt.Errorf("evidence cycle: %s", strings.Join(cyc, " → "))
	}
	return nil
}

func validateEvidence(id string, e *Evidence) error {
	if e == nil {
		return fmt.Errorf("evidence %q: empty definition", id)
	}
	kind := strings.TrimSpace(e.Kind)
	if kind == "" {
		return fmt.Errorf("evidence %q: kind is required (shell|diff|artifact|review|human|all|any)", id)
	}
	if !validEvidenceKinds[kind] {
		return fmt.Errorf("evidence %q: kind %q is not one of %s", id, kind, strings.Join(sortedKeys(validEvidenceKinds), ", "))
	}
	if e.Severity != "" && !validSeverities[e.Severity] {
		return fmt.Errorf("evidence %q: severity %q must be off, warn, or block", id, e.Severity)
	}
	if err := parseOptionalDuration("evidence "+id, "timeout", e.Timeout); err != nil {
		return err
	}
	if err := parseOptionalDuration("evidence "+id, "within", e.Within); err != nil {
		return err
	}
	switch kind {
	case "shell":
		if strings.TrimSpace(e.Run) == "" {
			return fmt.Errorf("evidence %q: shell requires run", id)
		}
	case "diff":
		if len(e.Changed) == 0 && len(e.Unchanged) == 0 {
			return fmt.Errorf("evidence %q: diff requires changed or unchanged globs", id)
		}
		for _, g := range append(append([]string{}, e.Changed...), e.Unchanged...) {
			if strings.TrimSpace(g) == "" {
				return fmt.Errorf("evidence %q: empty glob", id)
			}
		}
	case "artifact":
		if strings.TrimSpace(e.File) == "" && (e.Verdict == nil || strings.TrimSpace(e.Verdict.File) == "") {
			return fmt.Errorf("evidence %q: artifact requires file (or verdict.file)", id)
		}
	case "review":
		if strings.TrimSpace(e.Role) == "" {
			return fmt.Errorf("evidence %q: review requires role", id)
		}
		// A reviewer without an acceptance standard always finds an objection
		// (S33c), so criteria is mandatory.
		if strings.TrimSpace(e.Criteria) == "" {
			return fmt.Errorf("evidence %q: review requires criteria (the acceptance standard)", id)
		}
		if e.Verdict == nil || strings.TrimSpace(e.Verdict.File) == "" {
			return fmt.Errorf("evidence %q: review requires verdict.file", id)
		}
		if len(e.Verdict.Labels) == 0 {
			return fmt.Errorf("evidence %q: review requires at least one verdict label", id)
		}
		for _, l := range e.Verdict.Labels {
			if !verdictLabelRule.MatchString(l) {
				return fmt.Errorf("evidence %q: verdict label %q must match [a-z][a-z0-9_-]*", id, l)
			}
		}
	case "human":
		if strings.TrimSpace(e.Prompt) == "" {
			return fmt.Errorf("evidence %q: human requires prompt", id)
		}
	case "all", "any":
		if len(e.Of) == 0 {
			return fmt.Errorf("evidence %q: %s requires of", id, kind)
		}
	}
	return nil
}

func (fl *Flow) validateRules() error {
	known := map[string]bool{}
	for _, s := range StageOrder {
		known[s] = true
	}
	for _, stage := range sortedKeys(fl.On) {
		if !known[stage] {
			return fmt.Errorf("on.%s: unknown stage (known: %s)", stage, strings.Join(StageOrder, ", "))
		}
		r := fl.On[stage]
		if r == nil {
			return fmt.Errorf("on.%s: empty rule", stage)
		}
		sev := r.Severity
		if sev == "" {
			sev = fl.EffectiveSeverity()
		}
		if !validSeverities[sev] {
			return fmt.Errorf("on.%s: severity %q must be off, warn, or block", stage, sev)
		}
		switch stage {
		case "pre_tool":
			if len(r.Deny) == 0 && len(r.Require) == 0 {
				return fmt.Errorf("on.pre_tool: deny or require is required")
			}
			for i, d := range r.Deny {
				if strings.TrimSpace(d.Match) == "" {
					return fmt.Errorf("on.pre_tool: deny[%d].match is required", i)
				}
				if strings.Contains(d.Match, "-re:") {
					re := d.Match[strings.Index(d.Match, "-re:")+4:]
					if _, err := regexp.Compile(re); err != nil {
						return fmt.Errorf("on.pre_tool: deny[%d] bad regex %q: %w", i, re, err)
					}
				}
			}
		case "commit_msg":
			if strings.TrimSpace(r.Pattern) == "" {
				return fmt.Errorf("on.commit_msg: pattern is required")
			}
			if _, err := regexp.Compile(r.Pattern); err != nil {
				return fmt.Errorf("on.commit_msg: bad pattern %q: %w", r.Pattern, err)
			}
		default:
			if len(r.Require) == 0 {
				return fmt.Errorf("on.%s: require is required", stage)
			}
		}
		for _, id := range r.Require {
			if _, ok := fl.Evidence[id]; !ok {
				return fmt.Errorf("on.%s: unknown evidence %q", stage, id)
			}
		}
	}
	return nil
}

func (fl *Flow) validatePhases() error {
	mode := fl.EffectiveMode()
	switch mode {
	case "repo":
		if len(fl.Phases) > 0 {
			return fmt.Errorf("mode repo cannot declare phases (use agentic or hybrid)")
		}
		return nil
	case "agentic", "hybrid":
		if len(fl.Phases) == 0 {
			return fmt.Errorf("mode %s requires phases", mode)
		}
	}
	idx := map[string]int{}
	for i := range fl.Phases {
		p := &fl.Phases[i]
		if !flowNameRule.MatchString(p.ID) {
			return fmt.Errorf("phase %d: id %q must match [a-z][a-z0-9_-]{0,31}", i, p.ID)
		}
		if _, dup := idx[p.ID]; dup {
			return fmt.Errorf("duplicate phase id %q", p.ID)
		}
		idx[p.ID] = i
		if p.Actor == "" {
			p.Actor = "agent"
		}
		if !validActors[p.Actor] {
			return fmt.Errorf("phase %q: actor %q must be agent, reviewer, shell, or human", p.ID, p.Actor)
		}
	}
	entries := 0
	for i := range fl.Phases {
		p := &fl.Phases[i]
		for _, from := range p.From {
			j, ok := idx[from]
			if !ok {
				return fmt.Errorf("phase %q: from references unknown phase %q", p.ID, from)
			}
			if j >= i {
				return fmt.Errorf("phase %q: from %q must reference an earlier phase (verdict is the only back edge)", p.ID, from)
			}
		}
		if len(p.From) == 0 {
			entries++
		}
		if p.Model != "" && p.Route != "" {
			return fmt.Errorf("phase %q: set model or route, not both", p.ID)
		}
		for _, t := range append(append([]string{}, p.Produce...), p.Inputs...) {
			if !evidenceIDRule.MatchString(t) {
				return fmt.Errorf("phase %q: artifact type %q must match [a-z][a-z0-9_-]{0,63}", p.ID, t)
			}
		}
		if p.Gate != nil {
			sev := p.Gate.Severity
			if sev == "" {
				sev = fl.EffectiveSeverity()
			}
			if !validSeverities[sev] {
				return fmt.Errorf("phase %q: gate severity %q must be off, warn, or block", p.ID, sev)
			}
			if len(p.Gate.Require) == 0 {
				return fmt.Errorf("phase %q: gate requires at least one evidence id", p.ID)
			}
			for _, id := range p.Gate.Require {
				if _, ok := fl.Evidence[id]; !ok {
					return fmt.Errorf("phase %q: gate requires unknown evidence %q", p.ID, id)
				}
			}
		}
		switch p.Actor {
		case "agent":
			if strings.TrimSpace(p.Prompt) == "" {
				return fmt.Errorf("phase %q: agent requires prompt", p.ID)
			}
		case "reviewer":
			if strings.TrimSpace(p.Criteria) == "" {
				return fmt.Errorf("phase %q: reviewer requires criteria (the acceptance standard)", p.ID)
			}
			if len(p.Verdict) == 0 {
				return fmt.Errorf("phase %q: reviewer requires verdict labels", p.ID)
			}
			for label, target := range p.Verdict {
				if !verdictLabelRule.MatchString(label) {
					return fmt.Errorf("phase %q: verdict label %q must match [a-z][a-z0-9_-]*", p.ID, label)
				}
				j, ok := idx[target]
				if !ok {
					return fmt.Errorf("phase %q: verdict %q targets unknown phase %q", p.ID, label, target)
				}
				if j <= i && p.MaxVisits <= 0 {
					return fmt.Errorf("phase %q: verdict %q targets %q (not after it) — set max_visits to bound the loop", p.ID, label, target)
				}
			}
		case "shell":
			if strings.TrimSpace(p.Run) == "" {
				return fmt.Errorf("phase %q: shell requires run", p.ID)
			}
		case "human":
			if strings.TrimSpace(p.Prompt) == "" {
				return fmt.Errorf("phase %q: human requires prompt", p.ID)
			}
		}
	}
	if entries == 0 {
		return fmt.Errorf("no entry phase (every phase has from:) — nothing can start")
	}
	return nil
}

func (fl *Flow) validateWaivers() error {
	w := fl.Waivers
	if w == nil {
		return nil
	}
	if w.TTL != "" {
		if _, err := time.ParseDuration(w.TTL); err != nil {
			return fmt.Errorf("waivers.ttl %q is not a duration: %w", w.TTL, err)
		}
	}
	for _, id := range w.Never {
		if _, ok := fl.Evidence[id]; !ok {
			return fmt.Errorf("waivers.never: unknown evidence %q", id)
		}
	}
	return nil
}

// EvidenceIDs returns the evidence ids in stable order (the explain/simulate
// surface).
func (fl *Flow) EvidenceIDs() []string { return fl.evidenceIDs() }

// evidenceIDs returns the evidence ids in stable order.
func (fl *Flow) evidenceIDs() []string {
	ids := make([]string, 0, len(fl.Evidence))
	for id := range fl.Evidence {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// evidenceCycle returns one cycle path (or nil). Composition is a DAG today;
// a cycle would make evaluation non-terminating.
func evidenceCycle(fl *Flow) []string {
	state := map[string]int{} // 0 unseen, 1 on stack, 2 done
	var stack []string
	var found []string
	var visit func(string) bool
	visit = func(id string) bool {
		state[id] = 1
		stack = append(stack, id)
		for _, ref := range fl.Evidence[id].Of {
			switch state[ref] {
			case 0:
				if visit(ref) {
					return true
				}
			case 1:
				for i, s := range stack {
					if s == ref {
						found = append(append([]string{}, stack[i:]...), ref)
						return true
					}
				}
			}
		}
		stack = stack[:len(stack)-1]
		state[id] = 2
		return false
	}
	for _, id := range fl.evidenceIDs() {
		if state[id] == 0 && visit(id) {
			return found
		}
	}
	return nil
}

func parseOptionalDuration(scope, field, raw string) error {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	if _, err := time.ParseDuration(raw); err != nil {
		return fmt.Errorf("%s: %s %q is not a duration: %w", scope, field, raw, err)
	}
	return nil
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
