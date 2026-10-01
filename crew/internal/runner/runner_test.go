package runner

import (
	"testing"

	"forseti/crew/internal/schema"
)

func TestParsePiStatus(t *testing.T) {
	text := "some answer text\n" +
		"~/repos/forseti (main)\n" +
		"↑5.9k ↓308 R23k CH94.8% $0.002 2.5%/200k (auto)          (opencode-go) glm-5.3-flash"
	cost, ctx := parsePiStatus(text)
	if cost < 0.0019 || cost > 0.0021 {
		t.Fatalf("cost = %v, want 0.002", cost)
	}
	if ctx < 2.4 || ctx > 2.6 {
		t.Fatalf("ctx = %v, want 2.5", ctx)
	}
}

func TestParsePiStatusHalogenFree(t *testing.T) {
	// halogen nodes: $0.000 (models.json cost 0) — still parseable
	text := "answer\n$0.000 0.0%/200k (auto)   halogen/halogen-qwen3.8-flash-next"
	cost, ctx := parsePiStatus(text)
	if cost != 0 || ctx != 0 {
		t.Fatalf("cost = %v ctx = %v, want 0/0", cost, ctx)
	}
}

func TestParsePiStatusNoFooter(t *testing.T) {
	if cost, ctx := parsePiStatus("just text, no status line"); cost != 0 || ctx != 0 {
		t.Fatalf("want zeros, got %v/%v", cost, ctx)
	}
}

func TestEdgeSatisfied(t *testing.T) {
	if !edgeSatisfied(schema.Edge{When: "idle"}, "anything") {
		t.Fatal("idle edge should pass on done output")
	}
	if edgeSatisfied(schema.Edge{When: "re:NEVER_XYZ"}, "output without it") {
		t.Fatal("regex edge should not match")
	}
	if !edgeSatisfied(schema.Edge{When: "re:READY"}, "PLAN_READY here") {
		t.Fatal("regex edge should match")
	}
}
