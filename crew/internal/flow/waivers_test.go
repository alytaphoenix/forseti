package flow

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWaiversRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".forseti", "waivers.jsonl")
	now := time.Now()
	exp := now.Add(time.Hour)
	if err := AppendWaiver(path, Waiver{At: now, Flow: "t", Stage: "pre_commit", Evidence: "proof", Reason: "first"}); err != nil {
		t.Fatal(err)
	}
	if err := AppendWaiver(path, Waiver{At: now, Flow: "t", Stage: "pre_commit", Evidence: "proof", Reason: "second", ExpiresAt: &exp}); err != nil {
		t.Fatal(err)
	}
	list, err := ReadWaivers(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("rows = %d", len(list))
	}
	if w := ActiveWaiver(list, "t", "pre_commit", "proof", now); w == nil || w.Reason != "second" {
		t.Fatalf("latest active = %+v", w)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("ledger mode = %o", st.Mode().Perm())
	}
}

func TestWaiversMalformed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "waivers.jsonl")
	if err := os.WriteFile(path, []byte("not json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadWaivers(path); err == nil || !strings.Contains(err.Error(), ":1:") {
		t.Fatalf("want line error, got %v", err)
	}
}

func TestWaiverAllowed(t *testing.T) {
	fl := loadFlow(t, `
version: 1
name: t
evidence:
  proof: {kind: shell, run: "true"}
  frozen: {kind: shell, run: "true"}
on: {pre_commit: {require: [proof, frozen]}}
waivers: {never: [frozen]}
`)
	if err := WaiverAllowed(fl, "proof"); err != nil {
		t.Fatal(err)
	}
	if err := WaiverAllowed(fl, "frozen"); err == nil || !strings.Contains(err.Error(), "never") {
		t.Fatalf("want never error, got %v", err)
	}
	off := loadFlow(t, `
version: 1
name: t
evidence: {proof: {kind: shell, run: "true"}}
on: {pre_commit: {require: [proof]}}
waivers: {enabled: false}
`)
	if err := WaiverAllowed(off, "proof"); err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Fatalf("want disabled error, got %v", err)
	}
}
