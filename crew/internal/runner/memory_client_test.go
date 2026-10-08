package runner

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// P14: the crew field is the whole namespace contract - send it only when set.
func TestMemoryRecallCrewField(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte("[]"))
	}))
	defer srv.Close()
	c := &MemoryClient{BaseURL: srv.URL, HTTP: srv.Client()}

	if _, err := c.Recall("q", "node1", 3, ""); err != nil {
		t.Fatal(err)
	}
	if _, present := got["crew"]; present {
		t.Fatalf("crew must be omitted when empty: %v", got)
	}
	if _, err := c.Recall("q", "node1", 3, "alpha"); err != nil {
		t.Fatal(err)
	}
	if got["crew"] != "alpha" {
		t.Fatalf("crew = %v, want alpha", got["crew"])
	}
}

// P14 auto-write: the client pre-fixes agent and caps the posted text.
func TestMemoryWritePayload(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte("{\"id\": 42}"))
	}))
	defer srv.Close()
	c := &MemoryClient{BaseURL: srv.URL, HTTP: srv.Client()}

	big := strings.Repeat("x", 4500)
	id, err := c.Write(big, "alpha", "run-1", "node:coder", []string{"crew", "demo"})
	if err != nil || id != 42 {
		t.Fatalf("write: id=%d err=%v", id, err)
	}
	if got["agent"] != "crew-alpha" || got["crew"] != "alpha" {
		t.Fatalf("namespace fields wrong: %v / %v", got["agent"], got["crew"])
	}
	if txt, _ := got["text"].(string); len(txt) != 4000 {
		t.Fatalf("text not capped at 4000: %d", len(txt))
	}
	if got["source"] != "node:coder" || got["run"] != "run-1" {
		t.Fatalf("provenance lost: %v / %v", got["source"], got["run"])
	}
}
