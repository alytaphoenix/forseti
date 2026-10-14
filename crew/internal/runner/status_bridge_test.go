package runner

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"forseti/crew/internal/schema"
)

// TestStatusBridgeShape pins the .forseti/crew-status.json contract the ttt
// focus bridge (P15) consumes: counts for the badge, per-node rows in crew
// order for the sidebar, and the crew tab id for `herdr tab focus`.
func TestStatusBridgeShape(t *testing.T) {
	crew, err := schema.Load([]byte(`
name: bridge-test
agents:
  - {name: scout, prompt: x}
  - {name: build, prompt: y}
phases:
  - {name: recon, instructions: look, agents: [scout]}
  - {name: make, instructions: build, agents: [build]}
`))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	r := New(crew, Options{Cwd: t.TempDir()})
	r.mu.Lock()
	r.crewTab = "tab_test123"
	r.Nodes["scout"].Status = "running"
	r.mu.Unlock()
	r.writeStatusBridge()

	raw, err := os.ReadFile(filepath.Join(r.Opts.Cwd, ".forseti", "crew-status.json"))
	if err != nil {
		t.Fatalf("read status file: %v", err)
	}
	var st struct {
		Crew     string          `json:"crew"`
		Total    int             `json:"total"`
		Running  int             `json:"running"`
		Phase    string          `json:"phase"`
		Tab      string          `json:"tab"`
		NodesRaw json.RawMessage `json:"nodes"`
	}
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatalf("decode: %v\n%s", err, raw)
	}
	if st.Crew != "bridge-test" || st.Total != 2 || st.Running != 1 {
		t.Fatalf("core fields wrong: %+v", st)
	}
	if st.Phase != "recon" {
		t.Fatalf("phase = %q, want recon", st.Phase)
	}
	if st.Tab != "tab_test123" {
		t.Fatalf("tab = %q, want tab_test123", st.Tab)
	}
	var nodes []struct {
		Name   string `json:"name"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(st.NodesRaw, &nodes); err != nil {
		t.Fatalf("nodes decode: %v", err)
	}
	if len(nodes) != 2 || nodes[0].Name != "scout" || nodes[0].Status != "running" ||
		nodes[1].Name != "build" || nodes[1].Status != "pending" {
		t.Fatalf("nodes wrong (crew order + run status): %+v", nodes)
	}

	// finished runs must not resurrect the file (teardown removed it — the
	// stale-badge guard, 6B-5): same freeze applies to the new fields
	statusPath := filepath.Join(r.Opts.Cwd, ".forseti", "crew-status.json")
	_ = os.Remove(statusPath)
	r.finished.Store(true)
	r.writeStatusBridge()
	if _, err := os.Stat(statusPath); !os.IsNotExist(err) {
		t.Fatal("finished run must not write the status bridge")
	}
}
