// Structured builder TUI (F5): section navigation + field forms over the flow
// model, with a raw-YAML mode for anything the forms do not expose (and for
// preserving comments). Saving always validates the whole document first.
package main

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
	"gopkg.in/yaml.v3"

	"forseti/crew/internal/flow"
)

const (
	screenOverview = iota
	screenEvidence
	screenStages
	screenPhases
	screenWaivers
	screenRaw
)

type formField struct {
	label   string
	value   string
	options []string // non-empty = cycle field
}

type formModel struct {
	title  string
	fields []*formField
	idx    int
	onSave func([]string) error
	err    string
}

func (f *formModel) values() []string {
	out := make([]string, len(f.fields))
	for i, x := range f.fields {
		out[i] = x.value
	}
	return out
}

func (f *formModel) update(msg tea.KeyMsg) (keep bool) {
	switch msg.String() {
	case "esc":
		return false
	case "ctrl+s":
		if err := f.onSave(f.values()); err != nil {
			f.err = err.Error()
			return true
		}
		return false
	case "up":
		if f.idx > 0 {
			f.idx--
		}
	case "down":
		if f.idx < len(f.fields)-1 {
			f.idx++
		}
	case "left", "right":
		fld := f.fields[f.idx]
		if len(fld.options) > 0 {
			at := 0
			for i, o := range fld.options {
				if o == fld.value {
					at = i
				}
			}
			if msg.String() == "left" {
				at = (at - 1 + len(fld.options)) % len(fld.options)
			} else {
				at = (at + 1) % len(fld.options)
			}
			fld.value = fld.options[at]
		}
	case "backspace":
		fld := f.fields[f.idx]
		if len(fld.value) > 0 {
			fld.value = fld.value[:len(fld.value)-1]
		}
	case "enter":
		if f.idx < len(f.fields)-1 {
			f.idx++
			return true
		}
		if err := f.onSave(f.values()); err != nil {
			f.err = err.Error()
			return true
		}
		return false
	default:
		if len(msg.Runes) > 0 {
			f.fields[f.idx].value += string(msg.Runes)
		}
	}
	return true
}

func (f *formModel) view(height int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "  %s   (ctrl+s save · esc cancel)\n\n", f.title)
	start, end := 0, len(f.fields)
	if height > 6 && len(f.fields) > height-4 {
		start = f.idx - (height-5)/2
		if start < 0 {
			start = 0
		}
		end = start + height - 4
		if end > len(f.fields) {
			end = len(f.fields)
		}
	}
	for i := start; i < end; i++ {
		fld := f.fields[i]
		mark := "  "
		if i == f.idx {
			mark = "▸ "
		}
		val := fld.value
		if len(fld.options) > 0 {
			val = "‹ " + fld.value + " ›"
		}
		fmt.Fprintf(&b, "%s%-18s %s\n", mark, fld.label+":", val)
	}
	if f.err != "" {
		fmt.Fprintf(&b, "\n  ! %s\n", f.err)
	}
	return b.String()
}

// ---------------------------------------------------------------------------

type builderModel struct {
	path   string
	file   *flow.File
	flat   bool
	screen int
	cursor int
	form   *formModel
	raw    textarea.Model
	status string
	width  int
	height int
}

func newBuilderModel(path string, data []byte, f *flow.File) builderModel {
	m := builderModel{
		path: path, file: f, flat: true, raw: textarea.New(),
		status: "1 overview · 2 evidence · 3 stages · 4 phases · 5 waivers · r raw · v validate · ctrl+s save · esc quit",
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "flows:") {
			m.flat = false
		}
	}
	return m
}

func (m builderModel) flow() *flow.Flow {
	if len(m.file.Flows) == 0 {
		return nil
	}
	return m.file.Flows[0]
}

func (m builderModel) Init() tea.Cmd { return nil }

func (m builderModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.raw.SetWidth(max(20, msg.Width-2))
		m.raw.SetHeight(max(5, msg.Height-4))
		return m, nil
	case tea.KeyMsg:
		if m.form != nil {
			if !m.form.update(msg) {
				m.form = nil
				m.status = "form closed"
			}
			return m, nil
		}
		switch m.screen {
		case screenRaw:
			return m.updateRaw(msg)
		default:
			return m.updateList(msg)
		}
	}
	return m, nil
}

func (m builderModel) updateRaw(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.screen = screenOverview
		m.status = "left raw mode"
		return m, nil
	case "ctrl+s":
		text := m.raw.Value()
		if _, err := flow.Load([]byte(text)); err != nil {
			m.status = "NOT SAVED: " + err.Error()
			return m, nil
		}
		if err := os.WriteFile(m.path, []byte(text), 0o644); err != nil {
			m.status = "write failed: " + err.Error()
			return m, nil
		}
		if f, err := flow.Load([]byte(text)); err == nil {
			m.file = f
		}
		m.status = "saved " + m.path + " (raw)"
		return m, nil
	}
	var cmd tea.Cmd
	m.raw, cmd = m.raw.Update(msg)
	return m, cmd
}

func (m builderModel) updateList(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		if m.screen == screenOverview {
			return m, tea.Quit
		}
		m.screen, m.cursor = screenOverview, 0
		m.status = "overview"
		return m, nil
	case "1":
		m.screen, m.cursor = screenOverview, 0
	case "2":
		m.screen, m.cursor = screenEvidence, 0
	case "3":
		m.screen, m.cursor = screenStages, 0
	case "4":
		m.screen, m.cursor = screenPhases, 0
	case "5":
		m.screen, m.cursor = screenWaivers, 0
	case "r":
		m.raw = textarea.New()
		m.raw.SetValue(m.rawText())
		m.raw.Focus()
		m.raw.CharLimit = 0
		m.screen = screenRaw
		m.status = "raw YAML (ctrl+s save · esc back)"
	case "v":
		if err := m.validate(); err != nil {
			m.status = "INVALID: " + err.Error()
		} else {
			m.status = "valid"
		}
	case "ctrl+s", "s":
		m.save()
	case "up":
		if m.cursor > 0 {
			m.cursor--
		}
	case "down":
		m.cursor++
	case "enter":
		m.openSelected()
	case "a":
		m.add()
	case "d":
		m.del()
	}
	return m, nil
}

func (m builderModel) rawText() string {
	data, err := m.marshal()
	if err != nil {
		return ""
	}
	return string(data)
}

func (m *builderModel) validate() error {
	data, err := m.marshal()
	if err != nil {
		return err
	}
	_, err = flow.Load(data)
	return err
}

func (m *builderModel) save() {
	data, err := m.marshal()
	if err != nil {
		m.status = "NOT SAVED: " + err.Error()
		return
	}
	if _, err := flow.Load(data); err != nil {
		m.status = "NOT SAVED: " + err.Error()
		return
	}
	if err := os.WriteFile(m.path, data, 0o644); err != nil {
		m.status = "write failed: " + err.Error()
		return
	}
	m.status = "saved " + m.path + " (comments are not preserved in form mode; use raw)"
}

func (m *builderModel) marshal() ([]byte, error) {
	if len(m.file.Flows) == 0 {
		return nil, fmt.Errorf("no flow")
	}
	if len(m.file.Flows) == 1 && m.flat {
		fl := *m.file.Flows[0]
		name := fl.ID
		fl.ID = ""
		out := flow.File{Version: flow.Version, Name: name, Flow: fl}
		return yaml.Marshal(&out)
	}
	return yaml.Marshal(m.file)
}

// ---------------------------------------------------------------------------
// Screens

func (m builderModel) View() string {
	var b strings.Builder
	if m.form != nil {
		fmt.Fprintf(&b, "%s\n\n%s", m.status, m.form.view(m.height))
		return b.String()
	}
	fmt.Fprintf(&b, "%s\n\n", m.status)
	if m.screen == screenRaw {
		b.WriteString(m.raw.View())
		b.WriteString("\nctrl+s save · esc back")
		return b.String()
	}
	switch m.screen {
	case screenOverview:
		b.WriteString(m.viewOverview())
	case screenEvidence:
		b.WriteString(m.viewEvidence())
	case screenStages:
		b.WriteString(m.viewStages())
	case screenPhases:
		b.WriteString(m.viewPhases())
	case screenWaivers:
		b.WriteString(m.viewWaivers())
	}
	return b.String()
}

func (m builderModel) viewOverview() string {
	fl := m.flow()
	if fl == nil {
		return "no flow"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "flow %s (%s)\n", fl.ID, fl.EffectiveMode())
	fmt.Fprintf(&b, "severity=%s max_continues=%d max_cycles=%d timeout=%s\n",
		fl.EffectiveSeverity(), fl.MaxContinues(), fl.MaxCycles(), fl.DefaultTimeout())
	fmt.Fprintf(&b, "evidence=%d stages=%d phases=%d\n\n", len(fl.Evidence), len(fl.On), len(fl.Phases))
	b.WriteString("enter edit overview · 2 evidence · 3 stages · 4 phases · 5 waivers · r raw · v validate · ctrl+s save · esc quit\n")
	return b.String()
}

func (m builderModel) viewEvidence() string {
	fl := m.flow()
	var b strings.Builder
	cursor := clamp(m.cursor, len(fl.Evidence))
	fmt.Fprintf(&b, "evidence:\n")
	for i, id := range sortedEvidence(fl) {
		e := fl.Evidence[id]
		mark := "  "
		if i == cursor {
			mark = "▸ "
		}
		kind := "?"
		if e != nil {
			kind = e.Kind
		}
		fmt.Fprintf(&b, "%s%-20s %-8s %s\n", mark, id, kind, evidenceLine(e))
	}
	b.WriteString("\nenter edit · a add · d delete · esc back")
	return b.String()
}

func (m builderModel) viewStages() string {
	fl := m.flow()
	var b strings.Builder
	cursor := clamp(m.cursor, len(flow.StageOrder))
	b.WriteString("stages:\n")
	for i, st := range flow.StageOrder {
		mark := "  "
		if i == cursor {
			mark = "▸ "
		}
		box, state := "[ ]", "·"
		if r, ok := fl.On[st]; ok {
			box = "[x]"
			if len(r.Require) > 0 {
				state = strings.Join(r.Require, ",")
			}
		}
		fmt.Fprintf(&b, "%s%s %-11s %s\n", mark, box, st, state)
	}
	b.WriteString("\nenter edit/toggle · a add · d remove · esc back")
	return b.String()
}

func (m builderModel) viewPhases() string {
	fl := m.flow()
	var b strings.Builder
	cursor := clamp(m.cursor, len(fl.Phases))
	b.WriteString("phases:\n")
	for i := range fl.Phases {
		p := &fl.Phases[i]
		mark := "  "
		if i == cursor {
			mark = "▸ "
		}
		from := ""
		if len(p.From) > 0 {
			from = " ← " + strings.Join(p.From, ",")
		}
		fmt.Fprintf(&b, "%s%-14s %-9s%s\n", mark, p.ID, p.Actor, from)
	}
	b.WriteString("\nenter edit · a add · d delete · esc back")
	return b.String()
}

func (m builderModel) viewWaivers() string {
	fl := m.flow()
	var b strings.Builder
	fmt.Fprintf(&b, "waivers: enabled=%v reason_required=%v\n", fl.WaiversEnabled(), fl.WaiverReasonRequired())
	fmt.Fprintf(&b, "ledger=%s ttl=%s never=%s\n\n", fl.WaiverLedger(), fl.WaiverTTL(), strings.Join(neverOf(fl), ","))
	b.WriteString("enter edit · esc back")
	return b.String()
}

// ---------------------------------------------------------------------------
// Forms

func (m *builderModel) openSelected() {
	fl := m.flow()
	switch m.screen {
	case screenOverview:
		m.openOverview()
	case screenEvidence:
		ids := sortedEvidence(fl)
		if len(ids) == 0 {
			return
		}
		m.openEvidence(ids[clamp(m.cursor, len(ids))])
	case screenStages:
		m.openStage(flow.StageOrder[clamp(m.cursor, len(flow.StageOrder))])
	case screenPhases:
		if m.cursor < len(fl.Phases) {
			m.openPhase(m.cursor)
		}
	case screenWaivers:
		m.openWaivers()
	}
}

func (m *builderModel) add() {
	fl := m.flow()
	switch m.screen {
	case screenEvidence:
		id := "new-evidence"
		for i := 2; fl.Evidence[id] != nil; i++ {
			id = fmt.Sprintf("new-evidence-%d", i)
		}
		m.openEvidence(id)
	case screenStages:
		for _, st := range flow.StageOrder {
			if _, ok := fl.On[st]; !ok {
				m.openStage(st)
				return
			}
		}
	case screenPhases:
		m.openPhase(len(fl.Phases))
	case screenWaivers:
		m.openWaivers()
	}
}

func (m *builderModel) del() {
	fl := m.flow()
	switch m.screen {
	case screenEvidence:
		ids := sortedEvidence(fl)
		if len(ids) == 0 {
			return
		}
		delete(fl.Evidence, ids[clamp(m.cursor, len(ids))])
		m.status = "deleted evidence (ctrl+s to persist)"
	case screenStages:
		if m.cursor < len(flow.StageOrder) {
			st := flow.StageOrder[m.cursor]
			delete(fl.On, st)
			m.status = "removed stage " + st + " (ctrl+s to persist)"
		}
	case screenPhases:
		if m.cursor < len(fl.Phases) {
			id := fl.Phases[m.cursor].ID
			fl.Phases = append(fl.Phases[:m.cursor], fl.Phases[m.cursor+1:]...)
			m.status = "deleted phase " + id + " (ctrl+s to persist)"
		}
	}
}

func (m *builderModel) openOverview() {
	fl := m.flow()
	d := fl.Defaults
	if d == nil {
		d = &flow.Defaults{}
	}
	m.form = &formModel{
		title: "overview",
		fields: []*formField{
			{label: "name", value: fl.ID},
			{label: "mode", value: fl.EffectiveMode(), options: []string{"repo", "agentic", "hybrid"}},
			{label: "severity", value: d.Severity, options: []string{"", "off", "warn", "block"}},
			{label: "max_continues", value: intPtr(d.MaxContinues)},
			{label: "max_cycles", value: intPtr(d.MaxCycles)},
			{label: "timeout", value: d.Timeout},
		},
		onSave: func(v []string) error {
			if strings.TrimSpace(v[0]) == "" {
				return fmt.Errorf("name is required")
			}
			fl.ID = v[0]
			fl.Mode = v[1]
			d.Severity = v[2]
			mc, err := parsePtrInt(v[3])
			if err != nil {
				return fmt.Errorf("max_continues: %w", err)
			}
			cy, err := parsePtrInt(v[4])
			if err != nil {
				return fmt.Errorf("max_cycles: %w", err)
			}
			d.MaxContinues, d.MaxCycles, d.Timeout = mc, cy, v[5]
			fl.Defaults = d
			return m.validate()
		},
	}
}

func (m *builderModel) openEvidence(id string) {
	fl := m.flow()
	if fl.Evidence == nil {
		fl.Evidence = map[string]*flow.Evidence{}
	}
	e := fl.Evidence[id]
	if e == nil {
		e = &flow.Evidence{Kind: "shell"}
	}
	changed, unchanged, of := "", "", ""
	if e.Changed != nil {
		changed = strings.Join(e.Changed, ",")
	}
	if e.Unchanged != nil {
		unchanged = strings.Join(e.Unchanged, ",")
	}
	if e.Of != nil {
		of = strings.Join(e.Of, ",")
	}
	verdictFile, verdictLabels := "", ""
	if e.Verdict != nil {
		verdictFile = e.Verdict.File
		verdictLabels = strings.Join(e.Verdict.Labels, ",")
	}
	m.form = &formModel{
		title: "evidence " + id,
		fields: []*formField{
			{label: "id", value: id},
			{label: "kind", value: e.Kind, options: []string{"shell", "diff", "artifact", "review", "human", "all", "any"}},
			{label: "run", value: e.Run},
			{label: "file", value: e.File},
			{label: "changed", value: changed},
			{label: "unchanged", value: unchanged},
			{label: "base", value: e.Base},
			{label: "prompt (human)", value: e.Prompt},
			{label: "role (review)", value: e.Role},
			{label: "criteria (review)", value: e.Criteria},
			{label: "verdict_file", value: verdictFile},
			{label: "verdict_labels", value: verdictLabels},
			{label: "model", value: e.Model},
			{label: "within", value: e.Within},
			{label: "timeout", value: e.Timeout},
			{label: "severity", value: e.Severity, options: []string{"", "off", "warn", "block"}},
			{label: "of (all/any)", value: of},
		},
		onSave: func(v []string) error {
			newID := strings.TrimSpace(v[0])
			if newID == "" {
				return fmt.Errorf("id is required")
			}
			ne := &flow.Evidence{
				Kind: v[1], Run: v[2], File: v[3],
				Changed: list(v[4]), Unchanged: list(v[5]), Base: v[6],
				Prompt: v[7], Role: v[8], Criteria: v[9],
				Model: v[12], Within: v[13], Timeout: v[14], Severity: v[15], Of: list(v[16]),
			}
			if v[10] != "" || v[11] != "" {
				ne.Verdict = &flow.Verdict{File: v[10], Labels: list(v[11])}
			}
			if newID != id {
				delete(fl.Evidence, id)
			}
			fl.Evidence[newID] = ne
			return m.validate()
		},
	}
}

func (m *builderModel) openStage(stage string) {
	fl := m.flow()
	if fl.On == nil {
		fl.On = map[string]*flow.Rule{}
	}
	r := fl.On[stage]
	if r == nil {
		r = &flow.Rule{}
	}
	deny := ""
	for i, d := range r.Deny {
		if i > 0 {
			deny += "; "
		}
		deny += d.Match
		if d.Reason != "" {
			deny += "|" + d.Reason
		}
	}
	m.form = &formModel{
		title: "stage " + stage,
		fields: []*formField{
			{label: "stage", value: stage, options: flow.StageOrder},
			{label: "severity", value: r.Severity, options: []string{"", "off", "warn", "block"}},
			{label: "require", value: strings.Join(r.Require, ",")},
			{label: "pattern (msg)", value: r.Pattern},
			{label: "deny (match|why;…)", value: deny},
		},
		onSave: func(v []string) error {
			ns := v[0]
			nr := &flow.Rule{Severity: v[1], Require: list(v[2]), Pattern: v[3]}
			if v[4] != "" {
				for _, part := range strings.Split(v[4], ";") {
					part = strings.TrimSpace(part)
					if part == "" {
						continue
					}
					match, reason := part, ""
					if i := strings.Index(part, "|"); i >= 0 {
						match, reason = strings.TrimSpace(part[:i]), strings.TrimSpace(part[i+1:])
					}
					nr.Deny = append(nr.Deny, flow.DenyRule{Match: match, Reason: reason})
				}
			}
			if ns != stage {
				delete(fl.On, stage)
			}
			fl.On[ns] = nr
			return m.validate()
		},
	}
}

func (m *builderModel) openPhase(idx int) {
	fl := m.flow()
	p := &flow.Phase{Actor: "agent"}
	if idx < len(fl.Phases) {
		copy := fl.Phases[idx]
		p = &copy
	}
	verdict := ""
	labels := make([]string, 0, len(p.Verdict))
	for l := range p.Verdict {
		labels = append(labels, l)
	}
	sort.Strings(labels)
	for i, l := range labels {
		if i > 0 {
			verdict += ","
		}
		verdict += l + "=" + p.Verdict[l]
	}
	gate := ""
	if p.Gate != nil {
		gate = strings.Join(p.Gate.Require, ",")
	}
	m.form = &formModel{
		title: "phase " + p.ID,
		fields: []*formField{
			{label: "id", value: p.ID},
			{label: "actor", value: p.Actor, options: []string{"agent", "reviewer", "shell", "human"}},
			{label: "role", value: p.Role},
			{label: "from", value: strings.Join(p.From, ",")},
			{label: "prompt", value: p.Prompt},
			{label: "run (shell)", value: p.Run},
			{label: "gate_require", value: gate},
			{label: "gate_severity", value: gateSeverity(p), options: []string{"", "off", "warn", "block"}},
			{label: "max_visits", value: itoa(p.MaxVisits)},
			{label: "criteria", value: p.Criteria},
			{label: "verdict (l=target,…)", value: verdict},
			{label: "model", value: p.Model},
			{label: "route", value: p.Route},
			{label: "produce", value: strings.Join(p.Produce, ",")},
			{label: "inputs", value: strings.Join(p.Inputs, ",")},
		},
		onSave: func(v []string) error {
			np := &flow.Phase{
				ID: v[0], Actor: v[1], Role: v[2], From: list(v[3]), Prompt: v[4], Run: v[5],
				MaxVisits: 0, Criteria: v[9], Model: v[11], Route: v[12],
				Produce: list(v[13]), Inputs: list(v[14]),
			}
			mv, err := strconv.Atoi(strings.TrimSpace(v[8]))
			if v[8] != "" && err != nil {
				return fmt.Errorf("max_visits: %w", err)
			}
			np.MaxVisits = mv
			if v[6] != "" || v[7] != "" {
				np.Gate = &flow.Gate{Require: list(v[6]), Severity: v[7]}
			}
			if v[10] != "" {
				np.Verdict = map[string]string{}
				for _, part := range strings.Split(v[10], ",") {
					part = strings.TrimSpace(part)
					if part == "" {
						continue
					}
					i := strings.Index(part, "=")
					if i < 0 {
						return fmt.Errorf("verdict %q must be label=target", part)
					}
					np.Verdict[strings.TrimSpace(part[:i])] = strings.TrimSpace(part[i+1:])
				}
			}
			if idx < len(fl.Phases) {
				fl.Phases[idx] = *np
			} else {
				fl.Phases = append(fl.Phases, *np)
			}
			return m.validate()
		},
	}
}

func (m *builderModel) openWaivers() {
	fl := m.flow()
	w := fl.Waivers
	if w == nil {
		w = &flow.WaiverSpec{}
	}
	m.form = &formModel{
		title: "waivers",
		fields: []*formField{
			{label: "enabled", value: boolPtr(w.Enabled), options: []string{"", "true", "false"}},
			{label: "reason_required", value: boolPtr(w.ReasonRequired), options: []string{"", "true", "false"}},
			{label: "ledger", value: w.Ledger},
			{label: "ttl", value: w.TTL},
			{label: "never", value: strings.Join(w.Never, ",")},
		},
		onSave: func(v []string) error {
			en, err := parsePtrBool(v[0])
			if err != nil {
				return fmt.Errorf("enabled: %w", err)
			}
			rr, err := parsePtrBool(v[1])
			if err != nil {
				return fmt.Errorf("reason_required: %w", err)
			}
			w.Enabled, w.ReasonRequired, w.Ledger, w.TTL, w.Never = en, rr, v[2], v[3], list(v[4])
			fl.Waivers = w
			return m.validate()
		},
	}
}

// ---------------------------------------------------------------------------
// helpers

func sortedEvidence(fl *flow.Flow) []string {
	ids := make([]string, 0, len(fl.Evidence))
	for id := range fl.Evidence {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func evidenceLine(e *flow.Evidence) string {
	if e == nil {
		return ""
	}
	switch e.Kind {
	case "shell":
		return e.Run
	case "diff":
		return "changed=" + strings.Join(e.Changed, ",") + " unchanged=" + strings.Join(e.Unchanged, ",")
	case "artifact":
		if e.File != "" {
			return e.File
		}
		if e.Verdict != nil {
			return e.Verdict.File
		}
	case "review":
		return "criteria=" + e.Criteria
	case "human":
		return e.Prompt
	}
	return ""
}

func gateSeverity(p *flow.Phase) string {
	if p.Gate == nil {
		return ""
	}
	return p.Gate.Severity
}

func neverOf(fl *flow.Flow) []string {
	if fl.Waivers == nil {
		return nil
	}
	return fl.Waivers.Never
}

func list(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func intPtr(v *int) string {
	if v == nil {
		return ""
	}
	return itoa(*v)
}

func boolPtr(v *bool) string {
	if v == nil {
		return ""
	}
	if *v {
		return "true"
	}
	return "false"
}

func parsePtrInt(s string) (*int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return nil, err
	}
	return &v, nil
}

func parsePtrBool(s string) (*bool, error) {
	switch strings.TrimSpace(s) {
	case "":
		return nil, nil
	case "true":
		v := true
		return &v, nil
	case "false":
		v := false
		return &v, nil
	}
	return nil, fmt.Errorf("must be true or false")
}

func itoa(v int) string {
	if v == 0 {
		return ""
	}
	return strconv.Itoa(v)
}

func clamp(v, n int) int {
	if n == 0 {
		return 0
	}
	if v < 0 {
		return 0
	}
	if v >= n {
		return n - 1
	}
	return v
}

// runFlowBuilder opens the structured builder.
func runFlowBuilder(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	f, err := flow.Load(data)
	if err != nil {
		// An invalid file cannot be represented by the forms; the raw editor
		// can still fix it (and validate on save).
		return runFlowEditor(path)
	}
	_, err = tea.NewProgram(newBuilderModel(path, data, f), tea.WithAltScreen()).Run()
	return err
}
