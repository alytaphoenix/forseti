package schema

import (
	"strings"
	"testing"
	"text/template"
)

// TestPatternPromptsRender pins the prompt contract the live SOP run broke
// (S32). The runner renders a node's prompt at DISPATCH, so a template bug is
// invisible to Validate and the smoke gate — only a live run hit it:
//
//   - a dot-less `{{ draft }}` is a parse error ("function \"design\" not
//     defined"): templates read a bare identifier as a FUNCTION call;
//   - a hyphenated name cannot be field access at all ({{ .ap-critic-1 }} =
//     "bad character U+002D"), and every synthesized node is <pattern>-<part>;
//   - a prompt may need a NON-predecessor output (the panel's judge renders the
//     artifact it does not edge from), which the incoming-edge map cannot give.
//
// Hence {{ node "name" }} for the generated references, and this test renders
// every node under the runner's binding rules.
func TestPatternPromptsRender(t *testing.T) {
	c, err := Load([]byte(`
name: sop-render
models: {efficient: e/m, capable: c/m}
roles:
  - {name: architect, profile: Architect, goal: design, tier: capable, writes: design, instruction: design it}
agents:
  - {name: plan, role: planner}
  - name: design
    role: architect
    watch: [plan]
    prompt: "Design from this plan:\n\n{{ artifact \"plan\" }}"
patterns:
  - {id: rev, pattern: reviewer_loop, subject: design, params: {then: sec}}
  - {id: sec, pattern: security_review, subject: design, params: {then: ap}}
  - {id: ap, pattern: adversarial_panel, subject: design, params: {critics: 2, then: run}}
  - {id: run, pattern: executor, subject: design, params: {checks: ["true"]}}
`))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(c.Agents) < 9 {
		t.Fatalf("expected the patterns to expand, got %d agents", len(c.Agents))
	}
	funcs := template.FuncMap{
		"artifact": func(string) string { return "ARTIFACT" },
		"memory":   func(string) string { return "" },
	}
	// {{ node "name" }} resolves any declared node's output; the runner leaves a
	// node's own name as "" until it has finished, so the test only needs to
	// flag names that do not exist in the crew.
	declared := map[string]bool{}
	for i := range c.Agents {
		declared[c.Agents[i].Name] = true
	}
	funcs["node"] = func(name string) string {
		if !declared[name] {
			return "UNKNOWN:" + name
		}
		return "UPSTREAM"
	}
	for i := range c.Agents {
		a := &c.Agents[i]
		// bind exactly like the runner: one entry per incoming edge's source.
		data := map[string]string{}
		for _, e := range c.Edges {
			if e.To == a.Name {
				data[e.From] = "UPSTREAM"
			}
		}
		tpl, err := template.New(a.Name).Funcs(funcs).Parse(a.Prompt)
		if err != nil {
			t.Fatalf("agent %q: prompt does not parse (hyphenated {{ .name }}?): %v", a.Name, err)
		}
		var b strings.Builder
		if err := tpl.Execute(&b, data); err != nil {
			t.Fatalf("agent %q: prompt does not render: %v", a.Name, err)
		}
		if strings.Contains(b.String(), "<no value>") {
			t.Fatalf("agent %q: prompt references a node that is not an incoming edge:\n%s", a.Name, b.String())
		}
		if strings.Contains(b.String(), "UNKNOWN:") {
			t.Fatalf("agent %q: prompt names a node that is not an incoming edge:\n%s", a.Name, b.String())
		}
	}
}
