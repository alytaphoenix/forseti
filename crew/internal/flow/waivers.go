// Waivers are the explicit escape hatch: a blocking gate can be downgraded for
// a bounded time, with a reason, and the decision is appended to a ledger.
package flow

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Waiver is one ledger row.
type Waiver struct {
	At        time.Time  `json:"at"`
	Flow      string     `json:"flow"`
	Stage     string     `json:"stage"`
	Evidence  string     `json:"evidence"`
	Reason    string     `json:"reason"`
	Session   string     `json:"session,omitempty"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

// ReadWaivers reads the NDJSON ledger; a missing ledger is empty.
func ReadWaivers(path string) ([]Waiver, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	var out []Waiver
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	line := 0
	for sc.Scan() {
		line++
		raw := sc.Bytes()
		if len(raw) == 0 {
			continue
		}
		var w Waiver
		if err := json.Unmarshal(raw, &w); err != nil {
			return nil, fmt.Errorf("%s:%d: %w", path, line, err)
		}
		out = append(out, w)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// AppendWaiver appends one row with 0600 permissions.
func AppendWaiver(path string, w Waiver) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(w)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(data, '\n'))
	return err
}

// ActiveWaiver returns the latest unexpired waiver for the tuple, if any.
func ActiveWaiver(list []Waiver, flowID, stage, evidence string, now time.Time) *Waiver {
	var hit *Waiver
	for i := range list {
		w := &list[i]
		if w.Flow != flowID || w.Stage != stage || w.Evidence != evidence {
			continue
		}
		if w.ExpiresAt != nil && !w.ExpiresAt.After(now) {
			continue
		}
		hit = w
	}
	return hit
}

// WaiverAllowed reports whether evidence may be waived for this flow.
func WaiverAllowed(fl *Flow, evidence string) error {
	if !fl.WaiversEnabled() {
		return fmt.Errorf("waivers are disabled for flow %q", fl.ID)
	}
	if fl.Waivers != nil {
		for _, id := range fl.Waivers.Never {
			if id == evidence {
				return fmt.Errorf("evidence %q is in waivers.never and cannot be waived", evidence)
			}
		}
	}
	return nil
}
