package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"forseti/crew/internal/flow"
)

func TestDraftPresetsValidate(t *testing.T) {
	for _, preset := range []string{"repo", "review-loop", "docs-required", "release"} {
		dir := t.TempDir()
		doc := draftFlow(dir, preset)
		f, err := flow.Load([]byte(doc))
		if err != nil {
			t.Fatalf("%s: %v\n%s", preset, err, doc)
		}
		if len(f.Flows) != 1 {
			t.Fatalf("%s: flows = %d", preset, len(f.Flows))
		}
		if preset == "review-loop" && f.Flows[0].EffectiveMode() != "hybrid" {
			t.Fatalf("review-loop mode = %s", f.Flows[0].EffectiveMode())
		}
	}
}

func TestDraftMultiModuleTests(t *testing.T) {
	dir := t.TempDir()
	for _, sub := range []string{"cli", "crew"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, sub, "go.mod"), []byte("module x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	doc := draftFlow(dir, "repo")
	if !strings.Contains(doc, "(cd cli && go test ./...)") || !strings.Contains(doc, "(cd crew && go test ./...)") {
		t.Fatalf("multi-module test command missing:\n%s", doc)
	}
	if _, err := flow.Load([]byte(doc)); err != nil {
		t.Fatalf("draft invalid: %v\n%s", err, doc)
	}
}

func TestFlowNameFor(t *testing.T) {
	cases := map[string]string{
		"My Project": "my-project",
		"forseti":    "forseti",
		"123":        "repo-123",
	}
	for base, want := range cases {
		got := flowNameFor(filepath.Join(t.TempDir(), base))
		if got != want {
			t.Errorf("flowNameFor(%q)=%q want %q", base, got, want)
		}
	}
}

func TestInstallHookIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pre-commit")
	block := "# >>> forseti flow (generated) >>>\necho check\n# <<< forseti flow (generated) <<<"
	if err := installHookBlock(path, block, false); err != nil {
		t.Fatal(err)
	}
	if err := installHookBlock(path, block, false); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(data), ">>> forseti flow"); n != 1 {
		t.Fatalf("marker count = %d\n%s", n, data)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o755 {
		t.Fatalf("mode = %o", st.Mode().Perm())
	}
}

func TestInstallHookForceAppend(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pre-commit")
	if err := os.WriteFile(path, []byte("#!/bin/sh\necho existing\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := installHookBlock(path, "block", false); err == nil {
		t.Fatal("want refusal without --force")
	}
	if err := installHookBlock(path, "block", true); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "echo existing") || !strings.Contains(string(data), "block") {
		t.Fatalf("force append lost content:\n%s", data)
	}
}
