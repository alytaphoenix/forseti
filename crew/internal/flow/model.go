// Package flow defines the forseti flow language v1: a declarative
// per-project workflow. A flow has two surfaces that share one semantic core:
//
//   - repo mode: deterministic lifecycle gates over observable evidence
//     (pre_edit, post_turn, pre_commit, commit_msg, pre_push, pre_pr), and
//   - agentic mode: an ordered phase graph with roles, verdicts and bounded
//     loops that compiles down to the crew engine.
//
// The language is deliberately evidence-shaped: a gate may only require an
// evidence id, never prose. See docs/design.md (Phase 20) for the rationale.
package flow

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"

	"gopkg.in/yaml.v3"
)

// Version is the only accepted flow language version.
const Version = 1

// File is the on-disk document: either one flat flow (name + inline blocks) or
// a list of named flows. Load normalizes both into Flows so callers only ever
// walk the list.
type File struct {
	Version int    `yaml:"version" json:"version"`
	Name    string `yaml:"name,omitempty" json:"name,omitempty"`
	Flow    `yaml:",inline"`
	Flows   []*Flow `yaml:"flows,omitempty" json:"flows,omitempty"`
}

// Flow is one workflow. For a named entry in `flows:` only ID is required;
// for the flat form the document `name` becomes the ID.
type Flow struct {
	ID       string               `yaml:"id,omitempty" json:"id,omitempty"`
	Mode     string               `yaml:"mode,omitempty" json:"mode,omitempty"`       // repo | agentic | hybrid
	Trigger  string               `yaml:"trigger,omitempty" json:"trigger,omitempty"` // session | pre-commit | ... | manual
	Defaults *Defaults            `yaml:"defaults,omitempty" json:"defaults,omitempty"`
	Evidence map[string]*Evidence `yaml:"evidence,omitempty" json:"evidence,omitempty"`
	On       map[string]*Rule     `yaml:"on,omitempty" json:"on,omitempty"`
	Phases   []Phase              `yaml:"phases,omitempty" json:"phases,omitempty"`
	Waivers  *WaiverSpec          `yaml:"waivers,omitempty" json:"waivers,omitempty"`
}

// Defaults are flow-wide dials. Pointer fields distinguish "unset" from an
// explicit zero (max_continues: 0 means "never force another turn").
type Defaults struct {
	Severity     string `yaml:"severity,omitempty" json:"severity,omitempty"` // off | warn | block
	MaxContinues *int   `yaml:"max_continues,omitempty" json:"max_continues,omitempty"`
	MaxCycles    *int   `yaml:"max_cycles,omitempty" json:"max_cycles,omitempty"`
	Timeout      string `yaml:"timeout,omitempty" json:"timeout,omitempty"` // Go duration (default 10m)
}

// Evidence is an observable fact a gate may require. Exactly one kind's fields
// apply; Validate rejects missing/extra shapes per kind.
type Evidence struct {
	Kind      string   `yaml:"kind" json:"kind"` // shell | diff | artifact | review | human | all | any
	Run       string   `yaml:"run,omitempty" json:"run,omitempty"`
	Timeout   string   `yaml:"timeout,omitempty" json:"timeout,omitempty"`
	Within    string   `yaml:"within,omitempty" json:"within,omitempty"` // max age of a cached pass
	Changed   []string `yaml:"changed,omitempty" json:"changed,omitempty"`
	Unchanged []string `yaml:"unchanged,omitempty" json:"unchanged,omitempty"`
	Base      string   `yaml:"base,omitempty" json:"base,omitempty"` // git ref (default HEAD)
	File      string   `yaml:"file,omitempty" json:"file,omitempty"`
	Role      string   `yaml:"role,omitempty" json:"role,omitempty"`
	Criteria  string   `yaml:"criteria,omitempty" json:"criteria,omitempty"`
	Inputs    []string `yaml:"inputs,omitempty" json:"inputs,omitempty"`
	Verdict   *Verdict `yaml:"verdict,omitempty" json:"verdict,omitempty"`
	Model     string   `yaml:"model,omitempty" json:"model,omitempty"`
	Prompt    string   `yaml:"prompt,omitempty" json:"prompt,omitempty"`
	Of        []string `yaml:"of,omitempty" json:"of,omitempty"` // all/any composition
	Severity  string   `yaml:"severity,omitempty" json:"severity,omitempty"`
}

// Verdict names the artifact a review writes and the labels it may carry.
type Verdict struct {
	File   string   `yaml:"file" json:"file"`
	Labels []string `yaml:"labels" json:"labels"`
}

// Rule is a lifecycle gate. Deny is only meaningful for pre_tool; Pattern only
// for commit_msg; every other stage requires Require.
type Rule struct {
	Require  []string   `yaml:"require,omitempty" json:"require,omitempty"`
	Severity string     `yaml:"severity,omitempty" json:"severity,omitempty"`
	Deny     []DenyRule `yaml:"deny,omitempty" json:"deny,omitempty"`
	Pattern  string     `yaml:"pattern,omitempty" json:"pattern,omitempty"`
}

// DenyRule is a substring match against a tool invocation ("bash:git push").
// It is a fast-lane guard only: git hooks and CI are authoritative.
type DenyRule struct {
	Match  string `yaml:"match" json:"match"`
	Reason string `yaml:"reason,omitempty" json:"reason,omitempty"`
}

// Phase is one step of an agentic flow. Declaration order is execution order:
// `from` may only reference earlier phases (verdicts are the only back edges).
type Phase struct {
	ID        string            `yaml:"id" json:"id"`
	Actor     string            `yaml:"actor,omitempty" json:"actor,omitempty"` // agent | reviewer | shell | human
	Role      string            `yaml:"role,omitempty" json:"role,omitempty"`
	Prompt    string            `yaml:"prompt,omitempty" json:"prompt,omitempty"`
	Run       string            `yaml:"run,omitempty" json:"run,omitempty"`
	From      []string          `yaml:"from,omitempty" json:"from,omitempty"`
	Produce   []string          `yaml:"produce,omitempty" json:"produce,omitempty"`
	Inputs    []string          `yaml:"inputs,omitempty" json:"inputs,omitempty"`
	Gate      *Gate             `yaml:"gate,omitempty" json:"gate,omitempty"`
	MaxVisits int               `yaml:"max_visits,omitempty" json:"max_visits,omitempty"`
	Model     string            `yaml:"model,omitempty" json:"model,omitempty"`
	Route     string            `yaml:"route,omitempty" json:"route,omitempty"`
	Criteria  string            `yaml:"criteria,omitempty" json:"criteria,omitempty"`
	Verdict   map[string]string `yaml:"verdict,omitempty" json:"verdict,omitempty"` // label → target phase
}

// Gate is a phase-exit assertion over evidence ids.
type Gate struct {
	Require  []string `yaml:"require,omitempty" json:"require,omitempty"`
	Severity string   `yaml:"severity,omitempty" json:"severity,omitempty"`
}

// WaiverSpec is the escape-hatch policy. Every block can be waived unless the
// evidence id is in Never. The ledger is the audit trail.
type WaiverSpec struct {
	Enabled        *bool    `yaml:"enabled,omitempty" json:"enabled,omitempty"`                 // default true
	ReasonRequired *bool    `yaml:"reason_required,omitempty" json:"reason_required,omitempty"` // default true
	Ledger         string   `yaml:"ledger,omitempty" json:"ledger,omitempty"`                   // default .forseti/waivers.jsonl
	TTL            string   `yaml:"ttl,omitempty" json:"ttl,omitempty"`                         // default 24h
	Never          []string `yaml:"never,omitempty" json:"never,omitempty"`
}

// Load parses and validates a flow document. Unknown keys are errors (typo
// safety), mirroring the crew schema's strictness.
func Load(data []byte) (*File, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var f File
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("parse flow.yaml: %w", err)
	}
	// A second document would silently be ignored by Decode above.
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return nil, errors.New("parse flow.yaml: more than one YAML document")
		}
		return nil, fmt.Errorf("parse flow.yaml: %w", err)
	}
	if err := f.normalize(); err != nil {
		return nil, err
	}
	if err := f.validate(); err != nil {
		return nil, err
	}
	return &f, nil
}

// normalize turns the flat form into the single-element Flows list.
func (f *File) normalize() error {
	if f.Version != Version {
		return fmt.Errorf("version must be %d (got %d)", Version, f.Version)
	}
	if len(f.Flows) == 0 {
		if strings.TrimSpace(f.Name) == "" {
			return errors.New("name is required (or declare flows:)")
		}
		fl := f.Flow
		fl.ID = f.Name
		f.Flows = []*Flow{&fl}
		f.Flow = Flow{}
		f.Name = ""
		return nil
	}
	if f.hasInline() {
		return errors.New("declare either top-level flow fields or flows:, not both")
	}
	if strings.TrimSpace(f.Name) != "" {
		// A document title is harmless but unused; reject it so the file has
		// exactly one meaning.
		return errors.New("name is only allowed with the flat form (drop it or drop flows:)")
	}
	return nil
}

// hasInline reports whether any embedded flat-flow field is set.
func (f *File) hasInline() bool {
	fl := f.Flow
	return fl.ID != "" || fl.Mode != "" || fl.Trigger != "" || fl.Defaults != nil ||
		len(fl.Evidence) > 0 || len(fl.On) > 0 || len(fl.Phases) > 0 || fl.Waivers != nil
}

// FlowByID returns a normalized flow by id.
func (f *File) FlowByID(id string) *Flow {
	for _, fl := range f.Flows {
		if fl.ID == id {
			return fl
		}
	}
	return nil
}

// phaseIndex returns a phase's declaration index (-1 when unknown).
func (fl *Flow) phaseIndex(id string) int {
	for i := range fl.Phases {
		if fl.Phases[i].ID == id {
			return i
		}
	}
	return -1
}

// EffectiveMode resolves the implicit mode: phases mean hybrid, otherwise repo.
func (fl *Flow) EffectiveMode() string {
	if fl.Mode != "" {
		return fl.Mode
	}
	if len(fl.Phases) > 0 {
		return "hybrid"
	}
	return "repo"
}

// EffectiveSeverity is the flow-wide default gate severity.
func (fl *Flow) EffectiveSeverity() string {
	if fl.Defaults != nil && fl.Defaults.Severity != "" {
		return fl.Defaults.Severity
	}
	return "warn"
}

// MaxContinues is the settle-gate continuation bound (default 2).
func (fl *Flow) MaxContinues() int {
	if fl.Defaults != nil && fl.Defaults.MaxContinues != nil {
		return *fl.Defaults.MaxContinues
	}
	return 2
}

// MaxCycles is the verdict-loop bound (default 3).
func (fl *Flow) MaxCycles() int {
	if fl.Defaults != nil && fl.Defaults.MaxCycles != nil {
		return *fl.Defaults.MaxCycles
	}
	return 3
}

// DefaultTimeout is the flow-wide shell/review timeout (default 10m).
func (fl *Flow) DefaultTimeout() string {
	if fl.Defaults != nil && fl.Defaults.Timeout != "" {
		return fl.Defaults.Timeout
	}
	return "10m"
}

// WaiversEnabled resolves the waiver default (present block → on).
func (fl *Flow) WaiversEnabled() bool {
	return fl.Waivers == nil || fl.Waivers.Enabled == nil || *fl.Waivers.Enabled
}

// WaiverLedger is the ledger path (default .forseti/waivers.jsonl).
func (fl *Flow) WaiverLedger() string {
	if fl.Waivers != nil && fl.Waivers.Ledger != "" {
		return fl.Waivers.Ledger
	}
	return ".forseti/waivers.jsonl"
}

// WaiverTTL is the waiver lifetime (default 24h).
func (fl *Flow) WaiverTTL() string {
	if fl.Waivers != nil && fl.Waivers.TTL != "" {
		return fl.Waivers.TTL
	}
	return "24h"
}

// WaiverReasonRequired resolves the reason default (present block → on).
func (fl *Flow) WaiverReasonRequired() bool {
	return fl.Waivers == nil || fl.Waivers.ReasonRequired == nil || *fl.Waivers.ReasonRequired
}
