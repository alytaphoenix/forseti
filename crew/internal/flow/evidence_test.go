package flow

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMatchGlob(t *testing.T) {
	cases := []struct {
		pattern, name string
		want          bool
	}{
		{"docs/**", "docs/a.md", true},
		{"docs/**", "docs/x/y.md", true},
		{"docs/**", "README.md", false},
		{"**/*.gen.go", "a.gen.go", true},
		{"**/*.gen.go", "pkg/a.gen.go", true},
		{"**/dist/**", "dist/app.js", true},
		{"**/dist/**", "pkg/dist/app.js", true},
		{"*.md", "README.md", true},
		{"*.md", "docs/a.md", false},
	}
	for _, c := range cases {
		if got := matchGlob(c.pattern, c.name); got != c.want {
			t.Errorf("matchGlob(%q,%q)=%v want %v", c.pattern, c.name, got, c.want)
		}
	}
}

func loadFlow(t *testing.T, src string) *Flow {
	t.Helper()
	f, err := Load([]byte(src))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	return f.Flows[0]
}

func TestShellEvidence(t *testing.T) {
	fl := loadFlow(t, `
version: 1
name: t
evidence:
  proof: {kind: shell, run: "true"}
on: {pre_commit: {require: [proof]}}
`)
	ev := NewEvaluator(t.TempDir(), fl, nil)
	ev.Run = func(ctx context.Context, dir, script string) (string, error) { return "all good", nil }
	if r := ev.Eval(context.Background(), "proof"); r.Status != StatusPass {
		t.Fatalf("want pass, got %+v", r)
	}
	ev2 := NewEvaluator(t.TempDir(), fl, nil)
	ev2.Run = func(ctx context.Context, dir, script string) (string, error) { return "boom", errors.New("x") }
	if r := ev2.Eval(context.Background(), "proof"); r.Status != StatusFail || !strings.Contains(r.Detail, "exit 1") {
		t.Fatalf("want fail, got %+v", r)
	}
}

func TestArtifactVerdict(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "out"), 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "out", "review.md")
	fl := loadFlow(t, `
version: 1
name: t
evidence:
  review_lgtm:
    kind: artifact
    file: out/review.md
    verdict: {file: out/review.md, labels: [lgtm]}
on: {pre_commit: {require: [review_lgtm]}}
`)
	ev := NewEvaluator(dir, fl, nil)
	if err := os.WriteFile(file, []byte("looks fine\nVERDICT: LGTM\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if r := ev.Eval(context.Background(), "review_lgtm"); r.Status != StatusPass {
		t.Fatalf("want pass, got %+v", r)
	}
	ev = NewEvaluator(dir, fl, nil)
	if err := os.WriteFile(file, []byte("VERDICT: LBTM\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if r := ev.Eval(context.Background(), "review_lgtm"); r.Status != StatusFail {
		t.Fatalf("want fail, got %+v", r)
	}
}

func TestDiffEvidence(t *testing.T) {
	dir := t.TempDir()
	runGit(t, dir, "init")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", ".")
	runGit(t, dir, "-c", "user.email=t@e", "-c", "user.name=T", "commit", "-m", "init")

	fl := loadFlow(t, `
version: 1
name: t
evidence:
  docs: {kind: diff, changed: ["docs/**"]}
  no_docs: {kind: diff, unchanged: ["docs/**"]}
on: {pre_commit: {require: [docs]}}
`)
	ev := NewEvaluator(dir, fl, nil)
	if r := ev.Eval(context.Background(), "docs"); r.Status != StatusFail {
		t.Fatalf("want fail before docs, got %+v", r)
	}
	if err := os.MkdirAll(filepath.Join(dir, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "docs", "a.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	ev = NewEvaluator(dir, fl, nil)
	if r := ev.Eval(context.Background(), "docs"); r.Status != StatusPass {
		t.Fatalf("want pass after docs, got %+v", r)
	}
	ev = NewEvaluator(dir, fl, nil)
	if r := ev.Eval(context.Background(), "no_docs"); r.Status != StatusFail {
		t.Fatalf("want forbidden match to fail, got %+v", r)
	}
}

func TestEvidenceCache(t *testing.T) {
	dir := t.TempDir()
	fl := loadFlow(t, `
version: 1
name: t
evidence:
  proof: {kind: shell, run: "true", within: 1h}
on: {pre_commit: {require: [proof]}}
`)
	st := &State{Version: Version, Evidence: map[string]Result{}}
	calls := 0
	run := func(ctx context.Context, dir, script string) (string, error) { calls++; return "ok", nil }
	ev := NewEvaluator(dir, fl, st)
	ev.Run = run
	if r := ev.Eval(context.Background(), "proof"); r.Status != StatusPass {
		t.Fatalf("want pass, got %+v", r)
	}
	ev2 := NewEvaluator(dir, fl, st)
	ev2.Run = run
	r := ev2.Eval(context.Background(), "proof")
	if r.Status != StatusPass || !r.Cached || calls != 1 {
		t.Fatalf("want cached pass and one run, got %+v calls=%d", r, calls)
	}
	// A changed definition invalidates the cache.
	fl2 := loadFlow(t, `
version: 1
name: t
evidence:
  proof: {kind: shell, run: "true", within: 1h, timeout: 1m}
on: {pre_commit: {require: [proof]}}
`)
	ev3 := NewEvaluator(dir, fl2, st)
	ev3.Run = run
	if r := ev3.Eval(context.Background(), "proof"); r.Cached || calls != 2 {
		t.Fatalf("want cache miss on definition change, got %+v calls=%d", r, calls)
	}
}

func TestHumanEvidence(t *testing.T) {
	fl := loadFlow(t, `
version: 1
name: t
evidence:
  ship: {kind: human, prompt: "Ship it?"}
on: {pre_push: {require: [ship]}}
`)
	ev := NewEvaluator(t.TempDir(), fl, nil)
	if r := ev.Eval(context.Background(), "ship"); r.Status != StatusSkip {
		t.Fatalf("want skip without a confirmation, got %+v", r)
	}
	ev = NewEvaluator(t.TempDir(), fl, nil)
	ev.Human = map[string]bool{"ship": true}
	if r := ev.Eval(context.Background(), "ship"); r.Status != StatusPass {
		t.Fatalf("want pass, got %+v", r)
	}
	ev = NewEvaluator(t.TempDir(), fl, nil)
	ev.Human = map[string]bool{"ship": false}
	if r := ev.Eval(context.Background(), "ship"); r.Status != StatusFail {
		t.Fatalf("want fail on decline, got %+v", r)
	}
}

func TestComposition(t *testing.T) {
	fl := loadFlow(t, `
version: 1
name: t
evidence:
  a: {kind: shell, run: "a"}
  b: {kind: shell, run: "b"}
  all: {kind: all, of: [a, b]}
  any: {kind: any, of: [a, b]}
on: {pre_commit: {require: [all]}}
`)
	ev := NewEvaluator(t.TempDir(), fl, nil)
	ev.Run = func(ctx context.Context, dir, script string) (string, error) {
		if script == "a" {
			return "", nil
		}
		return "no", errors.New("x")
	}
	if r := ev.Eval(context.Background(), "all"); r.Status != StatusFail {
		t.Fatalf("all: %+v", r)
	}
	if r := ev.Eval(context.Background(), "any"); r.Status != StatusPass {
		t.Fatalf("any: %+v", r)
	}
}

func TestEffectiveSeverity(t *testing.T) {
	fl := loadFlow(t, `
version: 1
name: t
evidence:
  proof: {kind: shell, run: "true", severity: block}
on: {pre_commit: {require: [proof], severity: warn}}
`)
	if got := EffectiveSeverity(fl, "warn", fl.Evidence["proof"]); got != "block" {
		t.Fatalf("evidence severity must raise, got %q", got)
	}
	if got := EffectiveSeverity(fl, "", fl.Evidence["proof"]); got != "block" {
		t.Fatalf("flow default must raise, got %q", got)
	}
}

func TestWaiverExpiry(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	past := now.Add(-time.Minute)
	future := now.Add(time.Hour)
	list := []Waiver{
		{Flow: "t", Stage: "pre_commit", Evidence: "proof", Reason: "old", ExpiresAt: &past},
		{Flow: "t", Stage: "pre_commit", Evidence: "proof", Reason: "new", ExpiresAt: &future},
	}
	if w := ActiveWaiver(list, "t", "pre_commit", "proof", now); w == nil || w.Reason != "new" {
		t.Fatalf("active waiver = %+v", w)
	}
	if w := ActiveWaiver(list, "t", "pre_commit", "proof", now.Add(2*time.Hour)); w != nil {
		t.Fatalf("expired waiver must not apply: %+v", w)
	}
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}
