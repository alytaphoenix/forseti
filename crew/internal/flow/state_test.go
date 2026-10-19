package flow

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStateRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".forseti", "flow", "state.json")
	s := &State{Version: Version, Evidence: map[string]Result{}}
	s.Set("t", "proof", Result{ID: "proof", Status: StatusPass, At: time.Now(), InputHash: "abc"})
	if err := s.Save(path); err != nil {
		t.Fatal(err)
	}
	got, err := LoadState(path)
	if err != nil {
		t.Fatal(err)
	}
	r, ok := got.Get("t", "proof")
	if !ok || r.Status != StatusPass || r.InputHash != "abc" {
		t.Fatalf("round trip: %+v ok=%v", r, ok)
	}
}

func TestMergeSaveKeepsOtherRuns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	base := &State{Version: Version, Evidence: map[string]Result{}}
	base.Set("t", "a", Result{ID: "a", Status: StatusPass})
	if err := base.Save(path); err != nil {
		t.Fatal(err)
	}
	other := &State{Version: Version, Evidence: map[string]Result{}}
	other.Set("t", "b", Result{ID: "b", Status: StatusFail})
	if err := other.MergeSave(path); err != nil {
		t.Fatal(err)
	}
	got, err := LoadState(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got.Get("t", "a"); !ok {
		t.Fatal("lost a")
	}
	if _, ok := got.Get("t", "b"); !ok {
		t.Fatal("lost b")
	}
}

func TestWithLockStale(t *testing.T) {
	dir := t.TempDir()
	lock := filepath.Join(dir, "state.lock")
	if err := os.WriteFile(lock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Minute)
	if err := os.Chtimes(lock, old, old); err != nil {
		t.Fatal(err)
	}
	called := false
	if err := withLock(lock, func() error { called = true; return nil }); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("fn not called")
	}
	if _, err := os.Stat(lock); !os.IsNotExist(err) {
		t.Fatalf("lock left behind: %v", err)
	}
}
