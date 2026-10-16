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

func TestFireEdgeReArmsSettledTarget(t *testing.T) {
	// P13 BUG-02: a re-firing edge into a settled node re-arms it — bounded
	// cycles (max_visits) actually loop; past the limit, a skip is collected
	// (BUG-01: never emitted under the lock) and nothing re-arms.
	crew, err := schema.Load([]byte(`
name: cycle-test
agents:
  - {name: c, prompt: entry}
  - {name: a, prompt: x}
  - {name: b, prompt: y}
edges:
  - {from: c, to: a}
  - {from: a, to: b}
  - {from: b, to: a, max_visits: 2}
`))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	r := New(crew, Options{Cwd: t.TempDir()})
	es := &edgeState{visited: map[string]int{}, ok: map[string]bool{}}

	r.mu.Lock()
	r.fireEdge(es, crew.Edges[0]) // c→a
	if !r.nodeReady(es, "a") {
		r.mu.Unlock()
		t.Fatal("a ready after its only incoming edge fires")
	}
	r.Nodes["a"].Status = "done"
	r.fireEdge(es, crew.Edges[1]) // a→b
	if !r.nodeReady(es, "b") {
		r.mu.Unlock()
		t.Fatal("b ready after a→b fires")
	}
	r.Nodes["b"].Status = "done"
	r.fireEdge(es, crew.Edges[2]) // b→a (1st use)
	if r.Nodes["a"].Status != "pending" {
		r.mu.Unlock()
		t.Fatalf("b→a must re-arm a (cycle re-dispatch), got %q", r.Nodes["a"].Status)
	}
	if !r.nodeReady(es, "a") {
		r.mu.Unlock()
		t.Fatal("re-armed a must be ready")
	}
	if skips := es.drain(); len(skips) != 0 {
		r.mu.Unlock()
		t.Fatalf("unexpected skips: %+v", skips)
	}
	// 2nd use is still within max_visits: 2 → re-fires (a already pending:
	// no re-arm), no skip; the 3rd attempt exceeds the budget → collected skip
	r.Nodes["b"].Status = "done"
	r.fireEdge(es, crew.Edges[2])
	if skips := es.drain(); len(skips) != 0 {
		r.mu.Unlock()
		t.Fatalf("2nd use within budget must not skip, got %+v", skips)
	}
	if r.Nodes["a"].Status != "pending" {
		r.mu.Unlock()
		t.Fatalf("a stays pending (no double re-arm), got %q", r.Nodes["a"].Status)
	}
	r.fireEdge(es, crew.Edges[2]) // 3rd use exceeds max_visits: 2
	skips := es.drain()
	r.mu.Unlock()
	if len(skips) != 1 || skips[0].Type != "edge_skip" {
		t.Fatalf("want one max_visits skip event, got %+v", skips)
	}
	if r.Nodes["a"].Status != "pending" {
		t.Fatalf("no re-arm past the limit, got %q", r.Nodes["a"].Status)
	}
}

func TestUncappedEdgeRefiresLoopBody(t *testing.T) {
	// P19: only the loop-back edge carries max_visits; the loop BODY's forward
	// edge (subject→reviewer) is uncapped and must re-fire when the subject is
	// re-armed, otherwise the reviewer never re-runs after a revision.
	crew, err := schema.Load([]byte(`
name: loop-refire
agents:
  - {name: entry, prompt: go}
  - {name: subject, prompt: write}
  - {name: reviewer, prompt: review}
edges:
  - {from: entry, to: subject}
  - {from: subject, to: reviewer}
  - {from: reviewer, to: subject, when: "verdict:lbtm", max_visits: 2, loop: true}
`))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	r := New(crew, Options{Cwd: t.TempDir()})
	es := &edgeState{visited: map[string]int{}, ok: map[string]bool{}}
	r.mu.Lock()
	r.fireEdge(es, crew.Edges[0]) // entry→subject
	r.Nodes["subject"].Status = "done"
	r.fireEdge(es, crew.Edges[1]) // subject→reviewer (1st pass)
	r.Nodes["reviewer"].Status = "done"
	r.fireEdge(es, crew.Edges[2]) // reviewer→subject: LBTM re-arms subject
	if r.Nodes["subject"].Status != "pending" {
		r.mu.Unlock()
		t.Fatalf("loop edge must re-arm subject, got %q", r.Nodes["subject"].Status)
	}
	// 2nd pass: the subject re-settles; the uncapped forward edge re-fires and
	// re-dispatches the reviewer.
	r.Nodes["subject"].Status = "done"
	r.fireEdge(es, crew.Edges[1])
	if skips := es.drain(); len(skips) != 0 {
		r.mu.Unlock()
		t.Fatalf("uncapped forward edge must not be capped, got %+v", skips)
	}
	if r.Nodes["reviewer"].Status != "pending" {
		r.mu.Unlock()
		t.Fatalf("forward edge must re-dispatch reviewer, got %q", r.Nodes["reviewer"].Status)
	}
	r.mu.Unlock()
}

func TestCappedEdgeStillStops(t *testing.T) {
	// guard: max_visits is still a hard cap after the P19 default change.
	crew, err := schema.Load([]byte(`
name: capped
agents:
  - {name: a, prompt: x}
  - {name: b, prompt: y}
edges:
  - {from: a, to: b, max_visits: 2}
  - {from: b, to: a, max_visits: 2, loop: true}
`))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	r := New(crew, Options{Cwd: t.TempDir()})
	es := &edgeState{visited: map[string]int{}, ok: map[string]bool{}}
	r.mu.Lock()
	r.fireEdge(es, crew.Edges[0]) // 1st
	r.fireEdge(es, crew.Edges[0]) // 2nd, within cap
	if skips := es.drain(); len(skips) != 0 {
		r.mu.Unlock()
		t.Fatalf("within cap must not skip, got %+v", skips)
	}
	r.fireEdge(es, crew.Edges[0]) // 3rd exceeds the cap
	skips := es.drain()
	r.mu.Unlock()
	if len(skips) != 1 || skips[0].Type != "edge_skip" {
		t.Fatalf("want one max_visits skip, got %+v", skips)
	}
}

func TestNodeReadyFanInAND(t *testing.T) {
	// P13 BUG-05: fan-in AND — the target waits for ALL incoming edges; one
	// missed edge permanently skips it (nothing dispatches half-connected).
	crew, err := schema.Load([]byte(`
name: and-test
agents:
  - {name: x, prompt: x}
  - {name: y, prompt: y}
  - {name: t, prompt: z}
edges:
  - {from: x, to: t}
  - {from: y, to: t}
`))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	r := New(crew, Options{Cwd: t.TempDir()})
	es := &edgeState{visited: map[string]int{}, ok: map[string]bool{}}
	r.mu.Lock()
	r.fireEdge(es, crew.Edges[0]) // x→t only
	if r.nodeReady(es, "t") {
		r.mu.Unlock()
		t.Fatal("t must wait for ALL incoming edges (fan-in AND)")
	}
	r.fireEdge(es, crew.Edges[1]) // y→t too
	if !r.nodeReady(es, "t") {
		r.mu.Unlock()
		t.Fatal("t ready once every incoming edge fired")
	}
	r.mu.Unlock()

	// a missed edge skips the pending target and collects the skip event
	crew2, err := schema.Load([]byte(`
name: miss-test
agents:
  - {name: e, prompt: e}
  - {name: t, prompt: t}
edges:
  - {from: e, to: t}
`))
	if err != nil {
		t.Fatalf("load 2: %v", err)
	}
	r2 := New(crew2, Options{Cwd: t.TempDir()})
	es2 := &edgeState{visited: map[string]int{}, ok: map[string]bool{}}
	r2.mu.Lock()
	r2.missEdge(es2, crew2.Edges[0], "when not matched")
	if r2.Nodes["t"].Status != "skipped" {
		r2.mu.Unlock()
		t.Fatalf("miss must skip the pending target, got %q", r2.Nodes["t"].Status)
	}
	skips := es2.drain()
	r2.mu.Unlock()
	if len(skips) != 1 || skips[0].Type != "edge_skip" || skips[0].Node != "t" {
		t.Fatalf("want one collected edge_skip on t, got %+v", skips)
	}
}

func TestEdgeSatisfiedVerdict(t *testing.T) {
	// P19B: a verdict edge fires only on its own label; the label is the LAST
	// verdict line in the output (the pane echoes the prompt first).
	out := "End with `VERDICT: LGTM` or `VERDICT: LBTM`.\n\nreview body\n\nVERDICT: LBTM"
	ltm := schema.Edge{From: "r", To: "d", When: "verdict:lbtm"}
	lgtm := schema.Edge{From: "r", To: "n", When: "verdict:lgtm"}
	if !edgeSatisfied(ltm, out) {
		t.Fatal("verdict:lbtm must fire on a final LBTM verdict")
	}
	if edgeSatisfied(lgtm, out) {
		t.Fatal("verdict:lgtm must NOT fire on an LBTM verdict")
	}
	if edgeSatisfied(ltm, "no verdict here") {
		t.Fatal("a verdict edge must not fire without a verdict")
	}
	if !edgeSatisfied(lgtm, "all good\nLGTM") {
		t.Fatal("bare LGTM (MetaGPT convention) must fire verdict:lgtm")
	}
}

func TestNodeReadyCycleBodyWaitsForItsGate(t *testing.T) {
	// P19: in x→a→b→a the body edge a→b is a readiness gate — b must wait for
	// a (the old reachability rule marked a→b a back edge and let b run at t0).
	crew, err := schema.Load([]byte(`
name: cyc-ready
agents:
  - {name: x, prompt: x}
  - {name: a, prompt: a}
  - {name: b, prompt: b}
edges:
  - {from: x, to: a}
  - {from: a, to: b}
  - {from: b, to: a, max_visits: 2}
`))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	r := New(crew, Options{})
	es := &edgeState{visited: map[string]int{}, ok: map[string]bool{}}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.nodeReady(es, "b") {
		t.Fatal("b must not be ready before a→b fires")
	}
	r.Nodes["a"].Status = "done"
	r.fireEdge(es, crew.Edges[1]) // a→b
	if !r.nodeReady(es, "b") {
		t.Fatal("b must be ready once a→b fires")
	}
	// the loop-back does not re-gate a: a is already satisfied via x→a
	r.fireEdge(es, crew.Edges[0]) // x→a
	if !r.nodeReady(es, "a") {
		t.Fatal("a must be ready from its gate edge x→a")
	}
}
