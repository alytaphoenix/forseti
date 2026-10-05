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

func TestPhaseUnlocked(t *testing.T) {
	crew, err := schema.Load([]byte(`
name: barrier-test
agents:
  - {name: a1, prompt: x}
  - {name: a2, prompt: y}
  - {name: b1, prompt: z}
phases:
  - {name: p1, instructions: first, agents: [a1, a2]}
  - {name: p2, instructions: second, agents: [b1]}
`))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	r := New(crew, Options{Cwd: t.TempDir()})
	if r.phaseUnlocked(0) != true || r.phaseUnlocked(-1) != true {
		t.Fatal("phase -1/0 must always be unlocked")
	}
	// phase 1 locked while p1 has a running node
	r.mu.Lock()
	r.Nodes["a1"].Status = "done"
	r.Nodes["a2"].Status = "running"
	r.mu.Unlock()
	if r.phaseUnlocked(1) {
		t.Fatal("phase 1 must stay locked while a phase-0 node runs")
	}
	// a skipped node is terminal — releases the barrier (progress re-counted
	// after each wave, matching the scheduler's update-then-scan ordering)
	r.mu.Lock()
	r.Nodes["a2"].Status = "skipped"
	r.mu.Unlock()
	r.updatePhaseProgress()
	if !r.phaseUnlocked(1) {
		t.Fatal("phase 1 must unlock once every phase-0 node is terminal")
	}

	// blocked holds the barrier (fresh run: done is monotonic, so a
	// blocked-after-done sequence needs its own runner)
	crew2, err := schema.Load([]byte(`
name: barrier-test-2
agents:
  - {name: a1, prompt: x}
  - {name: a2, prompt: y}
  - {name: b1, prompt: z}
phases:
  - {name: p1, instructions: first, agents: [a1, a2]}
  - {name: p2, instructions: second, agents: [b1]}
`))
	if err != nil {
		t.Fatalf("load 2: %v", err)
	}
	r2 := New(crew2, Options{Cwd: t.TempDir()})
	r2.mu.Lock()
	r2.Nodes["a1"].Status = "done"
	r2.Nodes["a2"].Status = "blocked"
	r2.mu.Unlock()
	r2.updatePhaseProgress()
	if r2.phaseUnlocked(1) {
		t.Fatal("blocked phase-0 node must hold the barrier")
	}
	if p := r2.currentPhase(); p != "p1" {
		t.Fatalf("currentPhase = %q, want p1 (blocked phase still open)", p)
	}
}

func TestPhaseProgressEvents(t *testing.T) {
	crew, err := schema.Load([]byte(`
name: progress-test
agents:
  - {name: a1, prompt: x}
  - {name: b1, prompt: y}
phases:
  - {name: p1, instructions: first, agents: [a1]}
  - {name: p2, instructions: second, agents: [b1]}
`))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	r := New(crew, Options{Cwd: t.TempDir()})
	r.Nodes["a1"].PhaseIdx = 0
	r.Nodes["b1"].PhaseIdx = 1

	r.emitPhaseStarts([]*NodeState{r.Nodes["a1"]})
	if !r.phases[0].started {
		t.Fatal("phase_start not marked")
	}
	r.Nodes["a1"].Status = "done"
	r.updatePhaseProgress()
	if !r.phases[0].done || r.phases[1].done {
		t.Fatalf("phase progress wrong: %+v", r.phases)
	}
	if p := r.currentPhase(); p != "p2" {
		t.Fatalf("currentPhase = %q, want p2", p)
	}
	// all terminal → currentPhase empty
	r.Nodes["b1"].Status = "skipped"
	r.updatePhaseProgress()
	if p := r.currentPhase(); p != "" {
		t.Fatalf("currentPhase after finish = %q, want empty", p)
	}
}
