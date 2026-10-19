package flow

import (
	"os"
	"strings"
	"testing"

	"forseti/crew/internal/schema"
)

func loadExample(t *testing.T, name string) *Flow {
	t.Helper()
	data, err := os.ReadFile("../../examples/" + name)
	if err != nil {
		t.Fatal(err)
	}
	f, err := Load(data)
	if err != nil {
		t.Fatal(err)
	}
	return f.Flows[0]
}

func TestCompileAgenticExample(t *testing.T) {
	fl := loadExample(t, "flow-agentic.yaml")
	crew, err := Compile(fl)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, a := range crew.Agents {
		names[a.Name] = true
	}
	for _, want := range []string{"plan", "implement", "review"} {
		if !names[want] {
			t.Fatalf("missing agent %q (agents: %v)", want, names)
		}
	}
	for _, a := range crew.Agents {
		if a.Model == "" && a.Route == "" {
			t.Fatalf("agent %q has no model (compiled crews must be deterministic)", a.Name)
		}
	}
	if names["commit"] {
		t.Fatal("shell phase commit must not compile to an agent")
	}
	var sawBack, sawCommitEdge bool
	for _, e := range crew.Edges {
		if e.From == "review" && e.To == "implement" {
			sawBack = true
			if e.When != "verdict:lbtm" || !e.Loop || e.MaxVisits != 2 {
				t.Fatalf("back edge = %+v", e)
			}
		}
		if e.From == "review" && e.To == "commit" {
			sawCommitEdge = true
		}
	}
	if !sawBack {
		t.Fatal("missing bounded lbtm back edge")
	}
	if sawCommitEdge {
		t.Fatal("verdict edge to a terminal shell phase must be skipped")
	}
	foundCheck := false
	for _, c := range crew.Checks {
		if c.After == "implement" && c.Run == "go test ./..." {
			foundCheck = true
		}
		if c.After == "commit" {
			t.Fatalf("check attached to a shell phase: %+v", c)
		}
	}
	if !foundCheck {
		t.Fatal("missing shell check after implement")
	}
	// The compiled graph re-validates from YAML.
	data, err := Marshal(fl)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := schema.Load(data); err != nil {
		t.Fatalf("compiled crew does not load: %v\n%s", err, data)
	}
}

func TestCompileRewritesTemplate(t *testing.T) {
	fl := loadExample(t, "flow-hybrid.yaml")
	crew, err := Compile(fl)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range crew.Agents {
		if a.Name == "implement" && !strings.Contains(a.Prompt, `{{ node "plan" }}`) {
			t.Fatalf("template not rewritten: %q", a.Prompt)
		}
	}
}

func TestCompileDeterministic(t *testing.T) {
	fl := loadExample(t, "flow-hybrid.yaml")
	a, err := Marshal(fl)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Marshal(fl)
	if err != nil {
		t.Fatal(err)
	}
	if string(a) != string(b) {
		t.Fatal("generated crew is not byte-stable")
	}
}

func TestCompileRepoModeRejected(t *testing.T) {
	fl := loadExample(t, "flow-repo.yaml")
	if _, err := Compile(fl); err == nil || !strings.Contains(err.Error(), "repo mode") {
		t.Fatalf("want repo-mode error, got %v", err)
	}
}

func TestCompileHumanRejected(t *testing.T) {
	fl := loadFlow(t, `
version: 1
name: t
mode: agentic
evidence: {x: {kind: shell, run: "true"}}
phases:
  - id: a
    prompt: "x"
    gate: {require: [x]}
  - id: h
    actor: human
    from: [a]
    prompt: "approve"
`)
	if _, err := Compile(fl); err == nil || !strings.Contains(err.Error(), "human") {
		t.Fatalf("want human error, got %v", err)
	}
}

func TestCompileShellMustBeTerminal(t *testing.T) {
	fl := loadFlow(t, `
version: 1
name: t
mode: agentic
evidence: {x: {kind: shell, run: "true"}}
phases:
  - id: a
    prompt: "x"
    gate: {require: [x]}
  - id: s
    actor: shell
    from: [a]
    run: "true"
  - id: b
    from: [s]
    prompt: "x"
    gate: {require: [x]}
`)
	if _, err := Compile(fl); err == nil || !strings.Contains(err.Error(), "terminal-only") {
		t.Fatalf("want terminal-only error, got %v", err)
	}
}
