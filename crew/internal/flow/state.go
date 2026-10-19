// Evidence cache and the lock that guards it. State is deliberately disposable:
// deleting it only costs re-running checks.
package flow

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// State is the on-disk evidence cache, keyed "<flow>/<evidence>".
type State struct {
	Version  int               `json:"version"`
	Evidence map[string]Result `json:"evidence"`
}

// StatePath is the shared cache location for a checkout.
func StatePath(root string) string {
	return filepath.Join(root, ".forseti", "flow", "state.json")
}

// LoadState reads the cache; a missing file is an empty state.
func LoadState(path string) (*State, error) {
	s := &State{Version: Version, Evidence: map[string]Result{}}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return s, nil
		}
		return nil, err
	}
	if err := json.Unmarshal(data, s); err != nil {
		return nil, fmt.Errorf("state %s: %w", path, err)
	}
	if s.Evidence == nil {
		s.Evidence = map[string]Result{}
	}
	if s.Version != Version {
		return nil, fmt.Errorf("state %s: version %d (want %d)", path, s.Version, Version)
	}
	return s, nil
}

// Get returns a cached result for flow/evidence.
func (s *State) Get(flow, id string) (Result, bool) {
	r, ok := s.Evidence[flow+"/"+id]
	return r, ok
}

// Set stores a result for flow/evidence.
func (s *State) Set(flow, id string, r Result) {
	if s.Evidence == nil {
		s.Evidence = map[string]Result{}
	}
	s.Evidence[flow+"/"+id] = r
}

// Save writes atomically with 0600 (command output may be sensitive).
func (s *State) Save(path string) error {
	if s.Version == 0 {
		s.Version = Version
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := fmt.Sprintf("%s.%d.tmp", path, os.Getpid())
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// MergeSave overlays this run's results onto the latest on-disk state under the
// state lock, so two concurrent checks do not clobber each other's evidence.
func (s *State) MergeSave(path string) error {
	return withLock(path+".lock", func() error {
		cur, err := LoadState(path)
		if err != nil {
			// A corrupt cache must not block work; replace it.
			cur = &State{Version: Version, Evidence: map[string]Result{}}
		}
		for k, v := range s.Evidence {
			cur.Evidence[k] = v
		}
		return cur.Save(path)
	})
}

// withLock takes an exclusive lock file, clears a stale lock, and removes it.
func withLock(path string, fn func() error) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			_ = f.Close()
			defer func() { _ = os.Remove(path) }()
			return fn()
		}
		if !errors.Is(err, os.ErrExist) {
			return err
		}
		if st, statErr := os.Stat(path); statErr == nil && time.Since(st.ModTime()) > 30*time.Second {
			_ = os.Remove(path)
			continue
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("state lock %s is busy", path)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
