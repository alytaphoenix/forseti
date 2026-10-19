package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"forseti/crew/internal/flow"
)

func newTestBuilder(t *testing.T, src string) (builderModel, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "flow.yaml")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := flow.Load([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	return newBuilderModel(path, []byte(src), f), path
}

func setField(f *formModel, label, value string) {
	for _, fld := range f.fields {
		if fld.label == label {
			fld.value = value
			return
		}
	}
	panic("no field " + label)
}

func TestBuilderOverviewSave(t *testing.T) {
	m, path := newTestBuilder(t, `
version: 1
name: t
evidence: {x: {kind: shell, run: "true"}}
on: {pre_commit: {require: [x]}}
`)
	m.openOverview()
	setField(m.form, "severity", "block")
	if err := m.form.onSave(m.form.values()); err != nil {
		t.Fatal(err)
	}
	if m.flow().EffectiveSeverity() != "block" {
		t.Fatalf("severity = %q", m.flow().EffectiveSeverity())
	}
	m.save()
	f, err := flow.Load(readFile(t, path))
	if err != nil {
		t.Fatal(err)
	}
	if f.Flows[0].EffectiveSeverity() != "block" {
		t.Fatalf("saved severity = %q", f.Flows[0].EffectiveSeverity())
	}
}

func TestBuilderEvidenceSave(t *testing.T) {
	m, path := newTestBuilder(t, `
version: 1
name: t
evidence: {x: {kind: shell, run: "true"}}
on: {pre_commit: {require: [x]}}
`)
	m.openEvidence("new-evidence")
	setField(m.form, "id", "new-evidence")
	setField(m.form, "kind", "shell")
	setField(m.form, "run", "test -f OK")
	if err := m.form.onSave(m.form.values()); err != nil {
		t.Fatal(err)
	}
	if e := m.flow().Evidence["new-evidence"]; e == nil || e.Run != "test -f OK" {
		t.Fatalf("evidence = %+v", e)
	}
	m.save()
	f, err := flow.Load(readFile(t, path))
	if err != nil {
		t.Fatal(err)
	}
	if f.Flows[0].Evidence["new-evidence"].Run != "test -f OK" {
		t.Fatal("saved evidence missing")
	}
}

func TestBuilderStageSave(t *testing.T) {
	m, _ := newTestBuilder(t, `
version: 1
name: t
evidence: {x: {kind: shell, run: "true"}}
on: {post_turn: {require: [x]}}
`)
	m.openStage("pre_commit")
	setField(m.form, "require", "x")
	if err := m.form.onSave(m.form.values()); err != nil {
		t.Fatal(err)
	}
	if m.flow().On["pre_commit"] == nil {
		t.Fatal("stage not added")
	}
}

func TestBuilderPhaseSave(t *testing.T) {
	m, path := newTestBuilder(t, `
version: 1
name: t
mode: agentic
evidence: {x: {kind: shell, run: "true"}}
phases:
  - id: a
    prompt: "x"
    gate: {require: [x]}
`)
	m.openPhase(1)
	setField(m.form, "id", "b")
	setField(m.form, "actor", "agent")
	setField(m.form, "prompt", "do b")
	setField(m.form, "from", "a")
	setField(m.form, "gate_require", "x")
	if err := m.form.onSave(m.form.values()); err != nil {
		t.Fatal(err)
	}
	if len(m.flow().Phases) != 2 || m.flow().Phases[1].ID != "b" {
		t.Fatalf("phases = %+v", m.flow().Phases)
	}
	m.save()
	if _, err := flow.Load(readFile(t, path)); err != nil {
		t.Fatal(err)
	}
}

func TestBuilderFlatMarshal(t *testing.T) {
	m, _ := newTestBuilder(t, `
version: 1
name: t
evidence: {x: {kind: shell, run: "true"}}
on: {pre_commit: {require: [x]}}
`)
	data, err := m.marshal()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "flows:") || !strings.Contains(string(data), "name: t") {
		t.Fatalf("flat form not preserved:\n%s", data)
	}
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
