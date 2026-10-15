package switchyard

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"forseti/crew/internal/schema"
)

// TestLiveRouterServesRoute is the supervision gate for Phase 18B: the crew's
// own Start() path must resolve a router, render the TOML, dry-run it, bind,
// answer /health, serve a routed completion, and append a routing record.
//
// It dials the free LAN model, which is flaky by design of the box, so it is
// opt-in: FORSETI_LIVE_ROUTER=1. Unit tests must stay offline.
func TestLiveRouterServesRoute(t *testing.T) {
	if os.Getenv("FORSETI_LIVE_ROUTER") != "1" {
		t.Skip("set FORSETI_LIVE_ROUTER=1 to run the live routed request")
	}
	// valhalla is the free LAN model; both tiers point at it so the gate needs
	// no paid key (opencode-go returns 402 since 2026-10-05).
	const model = "valhalla/valhalla-flash-next"
	dir := t.TempDir()
	p, err := Start([]schema.Route{{
		ID: "live-pool", Type: "stage_router", Efficient: model, Capable: model,
	}}, dir, "routing.jsonl")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer p.Stop()
	if p.Kind != RouterHost {
		t.Logf("note: running on %s (%s)", p.Kind, p.Bin)
	}

	body, _ := json.Marshal(map[string]any{
		"model":    "live-pool",
		"messages": []map[string]string{{"role": "user", "content": "Reply with exactly: ROUTED_OK"}},
	})
	req, _ := http.NewRequest("POST", p.BaseURL()+"/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 90 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("routed request: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("routed request status %d: %s", resp.StatusCode, raw)
	}
	if !strings.Contains(string(raw), "ROUTED_OK") {
		t.Fatalf("routed response missing the answer: %s", raw)
	}

	// the routing record is written asynchronously; poll briefly
	logPath := filepath.Join(dir, "routing.jsonl")
	deadline := time.Now().Add(5 * time.Second)
	for {
		data, err := os.ReadFile(logPath)
		if err == nil && len(data) > 0 {
			var rec struct {
				RouteID   string `json:"route_id"`
				Algorithm string `json:"algorithm"`
				Model     string `json:"model"`
			}
			if err := json.Unmarshal(bytes.TrimSpace(data), &rec); err == nil {
				if rec.RouteID != "live-pool" || rec.Model == "" {
					t.Fatalf("routing record = %+v", rec)
				}
				t.Logf("routing record: route=%s algorithm=%s model=%s", rec.RouteID, rec.Algorithm, rec.Model)
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no routing record at %s", logPath)
		}
		time.Sleep(200 * time.Millisecond)
	}
}
