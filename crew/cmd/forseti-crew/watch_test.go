package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"forseti/crew/internal/runner"
)

func newWatchModel(path string) *watchModel {
	return &watchModel{path: path, follow: false,
		status: map[string]string{}, started: map[string]time.Time{},
		ended: map[string]time.Time{}, cost: map[string]float64{},
		phase: map[string]string{}, verdict: map[string]string{}}
}

// P19B: the verdict event carries "<label> → <artifact path>"; the row tag must
// be the label only, and a negative verdict must read as a warning marker.
func TestWatchFoldVerdict(t *testing.T) {
	m := newWatchModel("run.jsonl")
	m.fold(runner.Event{Type: "node_start", Node: "rev-reviewer"})
	m.fold(runner.Event{Type: "verdict", Node: "rev-reviewer", Info: "lbtm → /p/design.md"})
	m.fold(runner.Event{Type: "node_done", Node: "rev-reviewer", CostUSD: 0})
	if got := m.verdict["rev-reviewer"]; got != "lbtm" {
		t.Fatalf("verdict = %q, want lbtm", got)
	}
	if v := m.View(); !strings.Contains(v, "rev-reviewer") || !strings.Contains(v, "✗lbtm") {
		t.Fatalf("row must carry the verdict tag:\n%s", v)
	}
	// a later positive verdict replaces it (the node ran again — a loop pass)
	m.fold(runner.Event{Type: "verdict", Node: "rev-reviewer", Info: "lgtm"})
	if got := m.verdict["rev-reviewer"]; got != "lgtm" {
		t.Fatalf("verdict = %q, want lgtm after the loop pass", got)
	}
	if !strings.Contains(m.View(), "✓lgtm") {
		t.Fatalf("positive verdict must render with the neutral marker:\n%s", m.View())
	}
}

func TestWatchReplayRebuildsState(t *testing.T) {
	// P13-B1 / P19: a follow tick re-folds the whole file; state (incl. the
	// checks list and the verdicts) must be rebuilt, not appended.
	dir := t.TempDir()
	p := filepath.Join(dir, "run.jsonl")
	lines := `{"type":"node_start","node":"ex-implement"}
{"type":"verdict","node":"ex-implement","info":"pass"}
{"type":"check_pass","node":"ex-check-1","info":"exit 0"}
{"type":"run_end","info":"done"}
`
	if err := os.WriteFile(p, []byte(lines), 0o644); err != nil {
		t.Fatal(err)
	}
	m := newWatchModel(p)
	m.replay()
	m.replay() // a second tick
	if len(m.checks) != 1 {
		t.Fatalf("checks grew across replays: %v", m.checks)
	}
	if m.verdict["ex-implement"] != "pass" {
		t.Fatalf("verdict lost on replay: %v", m.verdict)
	}
}

func TestVerdictTag(t *testing.T) {
	for _, tc := range []struct{ label, want string }{
		{"", ""},
		{"lgtm", " ✓lgtm"},
		{"pass", " ✓pass"},
		{"accept", " ✓accept"},
		{"lbtm", " ✗lbtm"},
		{"fail", " ✗fail"},
		{"revise", " ✗revise"},
	} {
		if got := verdictTag(tc.label); got != tc.want {
			t.Fatalf("verdictTag(%q) = %q, want %q", tc.label, got, tc.want)
		}
	}
}
