package main

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"forseti/crew/internal/schema"
)

// P19 TUI: the role form's submit rules are pure so the name/profile/tier
// gates are unit-tested (the form itself only renders them).
func TestBuildRole(t *testing.T) {
	ok, err := buildRole([]string{"architect", "Architect", "decide the shape", "no code", "capable", "Write the design.", "design"})
	if err != nil {
		t.Fatalf("valid role rejected: %v", err)
	}
	if ok.Name != "architect" || ok.Profile != "Architect" || ok.Tier != schema.TierCapable ||
		ok.Goal != "decide the shape" || ok.Writes != "design" || ok.Instruction != "Write the design." {
		t.Fatalf("role fields mangled: %+v", ok)
	}

	// A blank tier is legal (the node's own model:/route: decides). The prose
	// minimum the form enforces is name+profile+goal — a goal-less role
	// renders "your goal is ." into the system prompt, so it is rejected.
	if _, err := buildRole([]string{"architect", "Architect", "decide", "", "", "", ""}); err != nil {
		t.Errorf("minimal role (name+profile+goal) rejected: %v", err)
	}
	if _, err := buildRole([]string{"architect", "Architect", "", "", "", "", ""}); err == nil ||
		!strings.Contains(err.Error(), "goal required") {
		t.Errorf("goal-less role: err = %v, want goal required", err)
	}

	cases := []struct {
		name string
		vals []string
		want string
	}{
		{"bad name", []string{"Architect!", "Architect", "g", "", "", "", ""}, "role name must match"},
		{"empty name", []string{"", "Architect", "g", "", "", "", ""}, "role name must match"},
		{"no profile", []string{"architect", "", "g", "", "", "", ""}, "profile required"},
		{"bad tier", []string{"architect", "Architect", "g", "", "turbo", "", ""}, "tier must be"},
		{"wrong-case tier", []string{"architect", "Architect", "g", "", "Capable", "", ""}, "tier must be"},
	}
	for _, c := range cases {
		if _, err := buildRole(c.vals); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want containing %q", c.name, err, c.want)
		}
	}
}

// P19 TUI: the pattern form's submit rules — the id rule, the kind enum, and
// that criteria/then land in params only when set.
func TestBuildPattern(t *testing.T) {
	p, err := buildPattern([]string{"rev", "reviewer_loop", "draft", "every claim cites a file", "verify"})
	if err != nil {
		t.Fatalf("valid pattern rejected: %v", err)
	}
	if p.ID != "rev" || p.Pattern != schema.PatternReviewerLoop || p.Subject != "draft" {
		t.Fatalf("pattern fields mangled: %+v", p)
	}
	if got := p.Params["criteria"]; got != "every claim cites a file" {
		t.Fatalf("criteria param = %v", got)
	}
	if got := p.Params["then"]; got != "verify" {
		t.Fatalf("then param = %v", got)
	}

	// No criteria/then → no params map at all (the YAML stays minimal).
	bare, err := buildPattern([]string{"sec", "security_review", "draft", "", ""})
	if err != nil {
		t.Fatalf("criteria-less pattern rejected at submit: %v", err)
	}
	if bare.Params != nil {
		t.Fatalf("bare pattern params = %v, want nil", bare.Params)
	}

	for _, c := range []struct {
		name string
		vals []string
		want string
	}{
		{"bad id", []string{"Rev!", "reviewer_loop", "draft", "", ""}, "pattern id must match"},
		{"empty id", []string{"", "reviewer_loop", "draft", "", ""}, "pattern id must match"},
		{"unknown kind", []string{"rev", "code_review", "draft", "", ""}, "pattern kind must be one of"},
		{"empty kind", []string{"rev", "", "draft", "", ""}, "pattern kind must be one of"},
	} {
		if _, err := buildPattern(c.vals); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want containing %q", c.name, err, c.want)
		}
	}
}

// P19 TUI: the build-mode keys l/g open the role/pattern forms (and are inert
// in view mode, where free letters must not mutate the crew).
func TestTUIKeyOpensRoleAndPatternForms(t *testing.T) {
	newModel := func(mode string) *model {
		return &model{crew: &schema.Crew{Name: "x"}, mode: mode, width: 100, height: 40}
	}

	m := newModel("build")
	m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'l'}})
	if m.form == nil || m.form.kind != "role" || len(m.form.fields) != 7 {
		t.Fatalf("l did not open the 7-field role form: %+v", m.form)
	}
	m.form = nil
	m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'g'}})
	if m.form == nil || m.form.kind != "pattern" || len(m.form.fields) != 5 {
		t.Fatalf("g did not open the 5-field pattern form: %+v", m.form)
	}
	if v := m.View(); !strings.Contains(v, "add pattern") {
		t.Fatalf("form view does not name the form:\n%s", v)
	}

	m = newModel("view")
	m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'l'}})
	if m.form != nil {
		t.Fatalf("l opened a form in view mode: %+v", m.form)
	}
}

// submitForm walks a form field-by-field and submits on the last one. Values
// are set directly (textinput.SetValue) — no key events needed for the text.
func submitForm(t *testing.T, m *model, vals []string) {
	t.Helper()
	if m.form == nil || len(m.form.fields) != len(vals) {
		t.Fatalf("form field count mismatch: %+v vs %d values", m.form, len(vals))
	}
	for i := range vals {
		m.form.fields[i].input.SetValue(vals[i])
	}
	for range vals {
		m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	}
	if m.form != nil {
		t.Fatalf("form still open after submit: %+v", m.form)
	}
}

// P19 TUI: the role and pattern forms append to the crew, and the agent form's
// role field rides along.
func TestTUIFormSubmitDeclaresRoleAndPattern(t *testing.T) {
	m := &model{crew: &schema.Crew{Name: "x"}, mode: "build", width: 120, height: 40}

	m.startRoleForm()
	submitForm(t, m, []string{"architect", "Architect", "decide the shape", "no code", "capable", "Write the design.", "design"})
	if len(m.crew.Roles) != 1 || m.crew.Roles[0].Name != "architect" || m.crew.Roles[0].Tier != "capable" {
		t.Fatalf("role not declared: %+v", m.crew.Roles)
	}
	// re-declaring the same name overrides, never duplicates
	m.startRoleForm()
	submitForm(t, m, []string{"architect", "Architect", "decide the shape", "", "efficient", "", ""})
	if len(m.crew.Roles) != 1 || m.crew.Roles[0].Tier != "efficient" {
		t.Fatalf("re-declared role did not override: %+v", m.crew.Roles)
	}

	m.startPatternForm()
	submitForm(t, m, []string{"rev", "reviewer_loop", "draft", "every claim cites a file", ""})
	if len(m.crew.Patterns) != 1 || m.crew.Patterns[0].ID != "rev" {
		t.Fatalf("pattern not declared: %+v", m.crew.Patterns)
	}
	// duplicate id is refused
	m.startPatternForm()
	submitForm(t, m, []string{"rev", "reviewer_loop", "draft", "", ""})
	if len(m.crew.Patterns) != 1 {
		t.Fatalf("duplicate pattern id accepted: %+v", m.crew.Patterns)
	}
	if !strings.Contains(m.buildMsg, "already declared") {
		t.Fatalf("duplicate id did not report: %q", m.buildMsg)
	}

	m.startAgentForm()
	submitForm(t, m, []string{"impl", "opencode-go/beam-free", "implement it", "", "architect"})
	if len(m.crew.Agents) != 1 || m.crew.Agents[0].Role != "architect" {
		t.Fatalf("agent role not carried: %+v", m.crew.Agents)
	}

	// the build view surfaces both
	v := m.View()
	for _, want := range []string{"roles:", "patterns:", "architect (efficient)", "reviewer_loop rev (draft)"} {
		if !strings.Contains(v, want) {
			t.Errorf("build view missing %q:\n%s", want, v)
		}
	}
}

// P19 TUI: what the forms build must round-trip through MarshalSource/Load —
// the declared role survives, and the pattern expands into graph nodes.
func TestTUIRolePatternRoundTrip(t *testing.T) {
	c := &schema.Crew{
		Name: "tui-round-trip",
		Agents: []schema.Agent{
			{Name: "author", Kind: "pi", Model: "opencode-go/beam-free", Prompt: "write the draft"},
		},
		Roles: []schema.Role{
			{Name: "architect", Profile: "Architect", Goal: "decide", Tier: schema.TierEfficient},
		},
		Patterns: []schema.Pattern{
			{ID: "rev", Pattern: schema.PatternReviewerLoop, Subject: "author",
				Params: map[string]any{"criteria": "every claim cites a file"}},
		},
		Models: map[string]string{"efficient": "opencode-go/beam-free", "capable": "opencode-go/longcat-2.5-preview-free"},
	}
	data, err := c.MarshalSource()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	src := string(data)
	if !strings.Contains(src, "architect") || !strings.Contains(src, "reviewer_loop") {
		t.Fatalf("source lost the role/pattern:\n%s", src)
	}
	if strings.Contains(src, "rev-reviewer") {
		t.Fatalf("MarshalSource wrote a synthesized node:\n%s", src)
	}
	fresh, err := schema.Load(data)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if fresh.Agent("rev-reviewer") == nil {
		t.Fatalf("pattern did not expand on load")
	}
	if _, ok := schema.NewRolePack(fresh.Roles).Get("architect"); !ok {
		t.Fatalf("declared role lost on load (roles=%+v)", fresh.Roles)
	}
}
