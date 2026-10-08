// Package runner executes a crew graph on herdr: one dedicated tab, one pane
// per node, deterministic edge-gated scheduling (no LLM routing of edges).
//
// Hand-off contract (design.md §Phase 5):
//   - each node's captured output goes to .forseti/bus/<node>.md
//   - downstream prompts template it in via {{ .<node> }}
//   - every step is appended to .forseti/runs/<run>.jsonl
//
// Phase 6 additions (docs/spikes.md S11–S15):
//   - checks: shell assertions run after their after-node settles (6A-1)
//   - --session: run against a named herdr session socket (6A-2), bootstrap
//     via a temp pane in the live session when the sandbox is down (S11)
//   - empty-output retry for halogen-routed nodes (6A-3)
//   - --worktree: run inside a disposable herdr worktree workspace (6A-4, S13)
//   - live status subscription + blocked notifications (6B-2/6B-3)
//   - agent.view.set projection (6B-4), ttt status bridge (6B-5)
//   - watch: pane.wait_for_output regex watchers (6C-1, S12)
//   - cost capture from pi's status line (6C-3)
//   - switchyard proxy lifecycle + routing-log tail (6D-1/6D-4/6D-6)
package runner

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"text/template"
	"time"
	"unicode/utf8"

	"forseti/crew/internal/herdrd"
	"forseti/crew/internal/schema"
	"forseti/crew/internal/switchyard"
)

// Event is a run-log record (also streamed to the TUI).
type Event struct {
	TS      time.Time `json:"ts"`
	Type    string    `json:"type"` // run_start|phase_start|phase_done|node_start|node_done|node_retry|node_blocked|node_failed|node_status|edge_skip|check_pass|check_fail|check_skip|pattern_matched|route_decision|laya_decision|memory_recall|memory_write|teardown|run_end
	Node    string    `json:"node,omitempty"`
	Phase   string    `json:"phase,omitempty"` // phase name on phase events (P11)
	Info    string    `json:"info,omitempty"`
	Model   string    `json:"model,omitempty"`
	CostUSD float64   `json:"cost_usd,omitempty"`
	CtxPct  float64   `json:"ctx_pct,omitempty"`
	Line    string    `json:"line,omitempty"` // pattern_matched matched line
}

type NodeState struct {
	Name       string
	Status     string // pending|running|done|blocked|failed|skipped
	PaneID     string
	Output     string
	Visits     int
	Started    time.Time
	Ended      time.Time
	LiveStatus string // herdr agent_status (stream subscription)
	CostUSD    float64
	CtxPct     float64
	Retries    int
	PhaseIdx   int // P11: phase position (-1 when the crew declares no phases)
}

// phaseState is the P11 barrier bookkeeping for one declared phase.
type phaseState struct {
	name  string
	nodes []string
	// terminal states release the barrier (done/failed/skipped; blocked holds
	// it — surfaced, never auto-answered, matching the human-in-the-loop rule)
	terminal int
	started  bool // phase_start emitted
	done     bool // phase_done emitted
}

type Options struct {
	Cwd            string        // pane cwd (repo root)
	WorkspaceID    string        // herdr workspace to create the crew tab in ("" = focused)
	TabLabel       string        // default "forseti-crew"
	NodeTimeout    time.Duration // per-node prompt settle timeout (default 10m)
	KeepTab        bool          // leave the crew tab open after the run
	Session        string        // named herdr session (sandbox); bootstrapped if down
	WorktreeBranch string        // run inside a disposable worktree ("" = main checkout)
	KeepWorktree   bool          // keep the worktree after the run
	Review         bool          // P7-3: end the run on a lazygit review of the worktree
	ModelOverride  string        // substitute model for direct-model nodes (outages; routes keep their pools)
	OnEvent        func(Event)
}

type Run struct {
	Crew  *schema.Crew
	Opts  Options
	mu    sync.Mutex
	Nodes map[string]*NodeState
	logf  *os.File

	phases          []phaseState      // P11 barrier bookkeeping (nil when no phases)
	backEdges       map[string]bool   // P13: cycle re-entry keys (schema.BackEdges)
	proxy           *switchyard.Proxy // P13 BUG-17: guarded by mu (read from the routing-log goroutine)
	restoreProvider func()
	worktreeWS      string          // worktree workspace id ("" if not created)
	effectiveCwd    string          // worktree checkout or Opts.Cwd
	checksDone      map[string]bool // P13 BUG-04: guarded by mu
	checksFailed    int
	finished        atomic.Bool // P13 BUG-17: freeze the status bridge (read from stream goroutines)
	laya            *LayaClient
	memory          *MemoryClient
	runID           string // P14: stable per-run id stamped on memory writes
}

func New(crew *schema.Crew, opts Options) *Run {
	if opts.TabLabel == "" {
		opts.TabLabel = "forseti-crew"
	}
	if opts.NodeTimeout == 0 {
		opts.NodeTimeout = 10 * time.Minute
	}
	// --review implies keeping both the tab and the worktree (P7-3): the human
	// lands on the agents' diff in lazygit when the run ends.
	if opts.Review {
		opts.KeepTab = true
		opts.KeepWorktree = true
	}
	r := &Run{Crew: crew, Opts: opts, Nodes: map[string]*NodeState{}, checksDone: map[string]bool{}}
	r.runID = time.Now().UTC().Format("20060102-150405") // P14: memory-write provenance
	r.effectiveCwd = opts.Cwd
	r.backEdges = crew.BackEdges() // P13: cycle re-entry edges
	// P11: per-node phase index + barrier bookkeeping. No phases → -1 → the
	// scheduler behaves exactly as before (single implicit phase).
	for _, a := range crew.Agents {
		r.Nodes[a.Name] = &NodeState{Name: a.Name, Status: "pending", PhaseIdx: crew.PhaseOf(a.Name)}
	}
	for i := range crew.Phases {
		r.phases = append(r.phases, phaseState{name: crew.Phases[i].Name, nodes: append([]string{}, crew.Phases[i].Agents...)})
	}
	return r
}

func (r *Run) emit(ev Event) {
	ev.TS = time.Now()
	if r.logf != nil {
		b, _ := json.Marshal(ev)
		_, _ = r.logf.Write(append(b, '\n'))
	}
	if r.Opts.OnEvent != nil {
		r.Opts.OnEvent(ev)
	}
	r.writeStatusBridge()
}

// writeStatusBridge maintains .forseti/crew-status.json for the ttt status bar
// (6B-5; notifications are disabled on this setup, so the badge is the alert).
// Skipped once the run has finished — late stream pushes must not resurrect
// the file after teardown removes it (hit live: stale badge at 11:00).
// Path = Opts.Cwd (the main checkout) ALWAYS: with --worktree the effective
// cwd is a disposable checkout that gets deleted — a badge living there would
// go stale in the stable one (hit live 11:09).
func (r *Run) writeStatusBridge() {
	if r.finished.Load() || r.Opts.Cwd == "" {
		return
	}
	r.mu.Lock()
	counts := map[string]int{}
	for _, st := range r.Nodes {
		counts[st.Status]++
	}
	r.mu.Unlock()
	out, _ := json.Marshal(map[string]any{
		"crew": r.Crew.Name, "ts": time.Now().Format(time.RFC3339),
		"total": len(r.Crew.Agents), "done": counts["done"],
		"running": counts["running"], "blocked": counts["blocked"],
		"failed": counts["failed"], "checks_failed": r.checksFailedCount(),
		"phase": r.currentPhase(), // P11 ("" when the crew has no phases)
	})
	path := filepath.Join(r.Opts.Cwd, ".forseti", "crew-status.json")
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	_ = os.WriteFile(path, out, 0o644)
}

func (r *Run) checksFailedCount() int {
	r.mu.Lock() // P13 BUG-04: called from stream goroutines via emit
	defer r.mu.Unlock()
	n := 0
	for _, ch := range r.Crew.Checks {
		if r.checksDone[ch.Name+"_failed"] {
			n++
		}
	}
	return n
}

// Snapshot returns current node states (sorted by crew order).
// Snapshot returns value COPIES of the node states (sorted by crew order).
// P13 BUG-15: it used to return shared pointers — the TUI's unsynchronized
// reads then raced every runner write; copies make reads race-free.
func (r *Run) Snapshot() []NodeState {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]NodeState, 0, len(r.Nodes))
	for _, a := range r.Crew.Agents {
		if st := r.Nodes[a.Name]; st != nil {
			out = append(out, *st)
		}
	}
	return out
}

// NodeSnapshot returns a value copy of one node's state (P13 BUG-15/B8).
func (r *Run) NodeSnapshot(name string) (NodeState, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if st := r.Nodes[name]; st != nil {
		return *st, true
	}
	return NodeState{}, false
}

// ChecksFailed reports whether any check assertion failed (exit-code input).
func (r *Run) ChecksFailed() bool {
	r.mu.Lock() // P13 BUG-04: checksDone is also read from stream goroutines
	defer r.mu.Unlock()
	for _, ch := range r.Crew.Checks {
		if r.checksDone[ch.Name+"_failed"] {
			return true
		}
	}
	return false
}

// Run executes the whole graph. Blocks until completion. Cancellation is
// BEST-EFFORT (P13 BUG-13): ctx is checked between waves and stops the
// watcher goroutines; in-flight node waits and herdr calls are not
// ctx-aware — kill the process to abandon a run (teardown then leaks; that
// is the documented tradeoff).
func (r *Run) Run(ctx context.Context) error {
	c, err := r.connect()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(r.effectiveCwd, ".forseti", "bus"), 0o755); err != nil {
		return err
	}
	runDir := filepath.Join(r.effectiveCwd, ".forseti", "runs")
	// P14 memory hand-off: build the client once (auto-write goroutines then
	// only read the pointer) and drop .forseti/memory-crew so pi panes — which
	// get no per-pane env through the herdr socket API (S26) — can resolve
	// their crew namespace for memory_write / memory_recall.
	if mem := r.Crew.Memory; mem != nil {
		r.memory = NewMemoryClient()
		ns := r.MemoryNS()
		path := filepath.Join(r.effectiveCwd, ".forseti", "memory-crew")
		if err := os.WriteFile(path, []byte(ns+"\n"), 0o644); err != nil {
			r.emit(Event{Type: "run_start", Info: "memory-crew hand-off write failed: " + err.Error()})
		}
	}
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		return err
	}
	name := fmt.Sprintf("%s-%s.jsonl", time.Now().Format("200601-150405.000000"), r.Crew.Name)
	r.logf, err = os.Create(filepath.Join(runDir, name))
	if err != nil {
		return err
	}
	defer r.logf.Close()
	r.emit(Event{Type: "run_start", Info: fmt.Sprintf("crew=%s agents=%d edges=%d session=%q worktree=%q",
		r.Crew.Name, len(r.Crew.Agents), len(r.Crew.Edges), r.Opts.Session, r.Opts.WorktreeBranch)})

	// teardown in reverse order of setup
	defer r.teardown(c)

	// 0. switchyard proxy (6D-1): one per run, before any agent starts
	if len(r.Crew.Routes) > 0 {
		p, err := switchyard.Start(r.Crew.Routes, runDir, strings.TrimSuffix(name, ".jsonl")+".routing.jsonl")
		if err != nil {
			return fmt.Errorf("switchyard proxy: %w", err)
		}
		r.setProxy(p)
		ids := make([]string, len(r.Crew.Routes))
		for i, rt := range r.Crew.Routes {
			ids[i] = rt.ID
		}
		if r.restoreProvider, err = p.MaterializeProvider(ids); err != nil {
			p.Stop()
			r.setProxy(nil)
			return fmt.Errorf("pi provider materialization: %w", err)
		}
		r.emit(Event{Type: "route_decision", Info: fmt.Sprintf("proxy on :%d (routing log %s)", p.Port, p.RoutingLog)})
		go r.tailRoutingLog()
	}

	// 1. worktree (6A-4): disposable checkout; panes + checks run there
	if r.Opts.WorktreeBranch != "" {
		ws, path, err := c.WorktreeCreate(r.Opts.Cwd, r.Opts.WorktreeBranch, "", "forseti-crew-wt", 90*time.Second)
		if err != nil {
			// a crashed earlier run can leave an unregistered husk under
			// herdr's worktrees root; sweep it and retry once
			if swept := huskToSweep(err); swept != "" {
				r.emit(Event{Type: "run_start", Info: "sweeping leftover worktree husk: " + swept})
				_ = os.RemoveAll(swept)
				_ = exec.Command("git", "-C", r.Opts.Cwd, "worktree", "prune").Run()
				ws, path, err = c.WorktreeCreate(r.Opts.Cwd, r.Opts.WorktreeBranch, "", "forseti-crew-wt", 90*time.Second)
			}
			if err != nil {
				return fmt.Errorf("worktree create: %w", err)
			}
		}
		r.worktreeWS, r.effectiveCwd = ws, path
		r.emit(Event{Type: "run_start", Info: "worktree ready: " + path})
		// bus/runs dirs live in the worktree
		if err := os.MkdirAll(filepath.Join(r.effectiveCwd, ".forseti", "bus"), 0o755); err != nil {
			return err
		}
	}

	ws := r.Opts.WorkspaceID
	if ws == "" {
		if ws, err = c.FocusedWorkspace(); err != nil {
			return err
		}
	}

	// 2. declarative tab: right-leaning spine of N panes (S8 verified)
	paneIDs, tabID, err := createCrewTab(c, ws, r.Opts.TabLabel, r.effectiveCwd, len(r.Crew.Agents))
	if err != nil {
		return fmt.Errorf("create crew tab: %w", err)
	}
	defer func() {
		if !r.Opts.KeepTab && tabID != "" {
			_, _ = c.Call("tab.close", map[string]any{"tab_id": tabID}, 10*time.Second)
		}
	}()
	for i, a := range r.Crew.Agents {
		r.Nodes[a.Name].PaneID = paneIDs[i]
	}

	// 3. project crew agents into herdr's Agents sidebar (6B-4, S10: UI-only)
	viewSource := "crew:" + r.Crew.Name
	if err := c.AgentViewSet(viewSource, r.Crew.Name,
		map[string]any{"op": "in", "field": "pane_id", "values": paneIDs},
		[]map[string]any{{"field": "attention", "order": "desc"}}); err != nil {
		r.emit(Event{Type: "node_failed", Info: "view projection: " + err.Error()})
	}

	// 4. start all agents up-front (readiness is per-agent; S9: wait idle before first prompt)
	var wg sync.WaitGroup
	for _, a := range r.Crew.Agents {
		wg.Add(1)
		go func(a schema.Agent) {
			defer wg.Done()
			args := []string{}
			switch {
			case a.Route != "":
				args = append(args, "--model", "switchyard/"+a.Route)
			case a.Model != "":
				args = append(args, "--model", r.effectiveModel(a))
			}
			// disposable worktree = fresh dir every run → pi's project-trust
			// prompt would stall every prompt (hit live). Auto-trust for the
			// run; user args come later so an explicit -na can override.
			if r.Opts.WorktreeBranch != "" {
				args = append(args, "-a")
			}
			args = append(args, a.Args...)
			if err := c.AgentStart(a.Name, a.Kind, r.Nodes[a.Name].PaneID, args, 90*time.Second); err != nil {
				// a freshly-spawned sandbox server can answer ping before its
				// pane PTYs are attachable (hit live: agent_pane_busy on
				// every pane) — settle and retry once
				var ae *herdrd.APIError
				if errors.As(err, &ae) && ae.Code == "agent_pane_busy" {
					time.Sleep(1200 * time.Millisecond)
					err = c.AgentStart(a.Name, a.Kind, r.Nodes[a.Name].PaneID, args, 90*time.Second)
				}
				if err != nil {
					r.fail(a.Name, fmt.Sprintf("agent.start: %v", err))
					return
				}
			}
			// "done" = idle-but-unseen (unfocused tab); both mean ready for input
			if err := c.AgentWait(a.Name, []string{"idle", "done", "blocked"}, 90*time.Second); err != nil {
				r.fail(a.Name, fmt.Sprintf("agent readiness: %v", err))
				return
			}
		}(a)
	}
	wg.Wait()
	for _, a := range r.Crew.Agents {
		if st := r.Nodes[a.Name].Status; st == "failed" || st == "blocked" {
			r.emit(Event{Type: "run_end", Info: "aborted: agent " + a.Name + " " + st})
			return fmt.Errorf("agent %s %s during startup", a.Name, st)
		}
	}

	// 5. live status stream (6B-2): pane.agent_status_changed per crew pane
	subs := make([]map[string]any, 0, len(paneIDs))
	for _, pid := range paneIDs {
		subs = append(subs, map[string]any{"type": "pane.agent_status_changed", "pane_id": pid})
	}
	if sub, err := c.Subscribe(subs, 15*time.Second); err == nil {
		go r.pumpStatus(c, sub)
		defer sub.Close()
	} else {
		r.emit(Event{Type: "node_failed", Info: "status stream unavailable: " + err.Error()})
	}

	// 6. output regex watchers (6C-1): advisory pattern_matched events.
	// P13 BUG-16: run-scoped context — watchers actually stop at Run return
	// (both callers pass Background(); the TUI spawns a fresh Run per keypress).
	runCtx, cancelWatchers := context.WithCancel(ctx)
	defer cancelWatchers()
	for _, w := range r.Crew.Watch {
		go r.watchLoop(runCtx, c, w)
	}

	// 7. wave scheduler (P13 BUG-02/03/05 rework): fan-in AND — a node
	// dispatches when ALL its incoming edges have fired (no incoming edges =
	// entry); a matched edge re-firing into a settled node re-arms it, so
	// bounded cycles (max_visits) actually loop; a missed edge permanently
	// skips its target (it can never be AND-satisfied via that edge again —
	// unless a cycle re-fires the edge and re-arms it).
	// P11 barrier: additionally, every node in earlier phases must be terminal
	// (done/failed/skipped) before a phase's nodes become dispatchable.
	es := &edgeState{visited: map[string]int{}, ok: map[string]bool{}}
	for {
		if ctx.Err() != nil { // P13 BUG-13: best-effort cancel between waves
			break
		}
		var ready []*NodeState
		r.mu.Lock()
		for _, a := range r.Crew.Agents {
			if r.nodeReady(es, a.Name) {
				ready = append(ready, r.Nodes[a.Name])
			}
		}
		r.mu.Unlock()
		if len(ready) == 0 {
			break
		}
		r.emitPhaseStarts(ready)
		var wg2 sync.WaitGroup
		for _, st := range ready {
			wg2.Add(1)
			go func(st *NodeState) {
				defer wg2.Done()
				r.runNode(runCtx, c, st)
			}(st)
		}
		wg2.Wait()
		// after the wave: checks for freshly-settled nodes, laya decision
		// gates (one grouped endpoint call per settled node), then plain gates
		r.runDueChecks(c)
		for _, st := range ready {
			if st.Status == "done" {
				r.evaluateLayaEdges(st, es)
			}
		}
		r.mu.Lock()
		for _, st := range ready {
			if st.Status != "done" {
				continue
			}
			for _, e := range r.Crew.OutEdges(st.Name) {
				if e.IsLaya() {
					continue // handled by evaluateLayaEdges (grouped call)
				}
				if edgeSatisfied(e, st.Output) {
					r.fireEdge(es, e)
				} else {
					r.missEdge(es, e, fmt.Sprintf("when %q not matched on %s output", e.When, e.From))
				}
			}
		}
		r.mu.Unlock()
		for _, ev := range es.drain() { // P13 BUG-01: emit only OUTSIDE the lock
			r.emit(ev)
		}
		r.updatePhaseProgress()
	}

	// any node still pending (unreachable) → skipped; P13 BUG-18: phase
	// progress runs once more so sweep-skipped members emit phase_done too.
	r.mu.Lock()
	for _, st := range r.Nodes {
		if st.Status == "pending" {
			st.Status = "skipped"
		}
	}
	blocked := false
	for _, st := range r.Nodes {
		if st.Status == "blocked" {
			blocked = true
		}
	}
	r.mu.Unlock()
	r.updatePhaseProgress()
	// P13 BUG-09: a run ending blocked is the human-in-the-loop outcome —
	// keep the tab + worktree (and skip branch -D) so the summoned human
	// still has the panes, the checkout, and the WIP branch to look at.
	if blocked {
		r.Opts.KeepTab = true
		r.Opts.KeepWorktree = true
	}
	info := r.summary()
	if p := r.getProxy(); p != nil {
		// run-level route tally (per-node attribution is impossible in
		// switchyard 0.2.0 — pi's session header doesn't attach, S15)
		if st, err := p.Stats(); err == nil {
			if tiers, ok := st["tiers"].(map[string]any); ok && len(tiers) > 0 {
				info += " | routed tiers: " + fmtTallies(tiers)
			}
			if models, ok := st["models"].(map[string]any); ok && len(models) > 0 {
				info += " | models: " + fmtTallies(models)
			}
		}
	}
	// P7-3: end the run on a lazygit review pane of the worktree
	if r.Opts.Review && r.worktreeWS != "" && r.effectiveCwd != r.Opts.Cwd {
		if err := openReviewPane(c, tabID, r.effectiveCwd); err != nil {
			info += " | review pane failed: " + err.Error()
		} else {
			info += fmt.Sprintf(" | review ready: lazygit on %s — attach the sandbox session (`herdr session attach %s`), commit, then merge from the main checkout",
				r.effectiveCwd, r.Opts.Session)
		}
	}
	r.finished.Store(true) // freeze the status bridge; teardown's remove sticks
	r.emit(Event{Type: "run_end", Info: info})
	return nil
}

// ---- P11 phase barrier helpers ----

// setProxy/getProxy guard the proxy pointer (P13 BUG-17: it was written by
// teardown while the routing-log goroutine read it bare).
func (r *Run) setProxy(p *switchyard.Proxy) {
	r.mu.Lock()
	r.proxy = p
	r.mu.Unlock()
}

func (r *Run) getProxy() *switchyard.Proxy {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.proxy
}

// edgeState is the P13 scheduler's shared edge bookkeeping (one instance per
// run; used by the plain-edge pass AND evaluateLayaEdges).
type edgeState struct {
	visited map[string]int  // "from>to" → times fired
	ok      map[string]bool // "from>to" → last evaluation matched
	skips   []Event         // BUG-01: collected under r.mu, emitted after unlock
}

func (es *edgeState) key(e schema.Edge) string { return e.From + ">" + e.To }

func (es *edgeState) limit(e schema.Edge) int {
	if e.MaxVisits == 0 {
		return 1
	}
	return e.MaxVisits
}

// drain returns the collected skip events and clears the buffer.
func (es *edgeState) drain() []Event {
	evs := es.skips
	es.skips = nil
	return evs
}

// fireEdge records a matched edge (P13 BUG-02): a re-firing edge into a
// settled node re-arms it, so bounded cycles (max_visits) actually loop.
// Called with r.mu held.
func (r *Run) fireEdge(es *edgeState, e schema.Edge) {
	key := es.key(e)
	if es.visited[key] >= es.limit(e) {
		es.skips = append(es.skips, Event{Type: "edge_skip", Node: e.To,
			Info: key + " max_visits reached"})
		return
	}
	es.visited[key]++
	es.ok[key] = true
	if t := r.Nodes[e.To]; t.Status == "done" || t.Status == "skipped" {
		t.Status = "pending" // re-dispatch; visits bounded by Σ max_visits
		t.Output = ""
		t.Retries = 0
	}
}

// missEdge records a failed gate (P13 BUG-01/05): the target can never be
// AND-satisfied via this edge again — skip it now (a later cycle re-fire of
// the same edge can re-arm it). Called with r.mu held.
func (r *Run) missEdge(es *edgeState, e schema.Edge, why string) {
	es.skips = append(es.skips, Event{Type: "edge_skip", Node: e.To,
		Info: es.key(e) + " " + why})
	if t := r.Nodes[e.To]; t.Status == "pending" {
		t.Status = "skipped"
	}
}

// nodeReady (P13 BUG-05): fan-in AND — a node dispatches when ALL its
// incoming edges have fired; no incoming edges = entry. Cycle BACK edges
// (schema.BackEdges) gate re-dispatch only — requiring them initially would
// deadlock every cycle by construction. Phase barrier unchanged (P11).
// Called with r.mu held.
func (r *Run) nodeReady(es *edgeState, name string) bool {
	st := r.Nodes[name]
	if st == nil || st.Status != "pending" || !r.phaseUnlocked(st.PhaseIdx) {
		return false
	}
	for _, e := range incoming(r.Crew, name) {
		if r.backEdges[e.From+">"+e.To] {
			continue // cycle re-entry edge: gates re-dispatch, not entry
		}
		if !es.ok[es.key(e)] {
			return false
		}
	}
	return true
}

// phaseUnlocked (called with r.mu held): a node is dispatchable when every
// earlier phase is fully terminal. PhaseIdx < 0 = no phases → always true.
func (r *Run) phaseUnlocked(idx int) bool {
	if idx <= 0 {
		return true
	}
	for j := 0; j < idx && j < len(r.phases); j++ {
		if !r.phases[j].done {
			return false
		}
	}
	return true
}

// emitPhaseStarts fires phase_start for each phase whose first node dispatches
// with this wave (P11).
func (r *Run) emitPhaseStarts(ready []*NodeState) {
	if len(r.phases) == 0 {
		return
	}
	var fired []phaseState
	r.mu.Lock()
	for _, st := range ready {
		if st.PhaseIdx >= 0 && st.PhaseIdx < len(r.phases) && !r.phases[st.PhaseIdx].started {
			r.phases[st.PhaseIdx].started = true
			fired = append(fired, r.phases[st.PhaseIdx])
		}
	}
	r.mu.Unlock()
	for _, p := range fired {
		r.emit(Event{Type: "phase_start", Phase: p.name,
			Info: fmt.Sprintf("agents: %s", strings.Join(p.nodes, ", "))})
	}
}

// updatePhaseProgress re-counts terminal nodes per phase after each wave;
// a phase whose nodes are ALL terminal fires phase_done, releasing the next
// phase's barrier (P11). blocked holds a phase open — surfaced, never
// auto-answered (the run then ends with the later phases skipped).
func (r *Run) updatePhaseProgress() {
	if len(r.phases) == 0 {
		return
	}
	terminal := map[string]bool{"done": true, "failed": true, "skipped": true}
	var fired []phaseState
	r.mu.Lock()
	for j := range r.phases {
		p := &r.phases[j]
		if p.done {
			continue
		}
		p.terminal = 0
		for _, n := range p.nodes {
			if terminal[r.Nodes[n].Status] {
				p.terminal++
			}
		}
		if p.terminal == len(p.nodes) {
			p.done = true
			fired = append(fired, *p)
		}
	}
	r.mu.Unlock()
	for _, p := range fired {
		r.emit(Event{Type: "phase_done", Phase: p.name,
			Info: fmt.Sprintf("%d/%d settled", p.terminal, len(p.nodes))})
	}
}

// currentPhase is the phase the run is in: the first not-yet-done phase
// ("" when no phases are declared or every phase finished).
func (r *Run) currentPhase() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	for j := range r.phases {
		if !r.phases[j].done {
			return r.phases[j].name
		}
	}
	return ""
}

// openReviewPane splits a lazygit pane onto the crew tab (P7-3).
func openReviewPane(c *herdrd.Client, tabID, worktree string) error {
	if tabID == "" {
		return fmt.Errorf("no crew tab")
	}
	res, err := c.Call("pane.list", struct{}{}, 10*time.Second)
	if err != nil {
		return err
	}
	var list struct {
		Panes []struct {
			PaneID string `json:"pane_id"`
			TabID  string `json:"tab_id"`
		} `json:"panes"`
	}
	if err := json.Unmarshal(res, &list); err != nil {
		return err
	}
	first := ""
	for _, p := range list.Panes {
		if p.TabID == tabID {
			first = p.PaneID
			break
		}
	}
	if first == "" {
		return fmt.Errorf("no panes in crew tab")
	}
	split, err := c.Call("pane.split", map[string]any{
		"target_pane_id": first, "direction": "right", "focus": false,
	}, 15*time.Second)
	if err != nil {
		return err
	}
	var sp struct {
		Pane struct {
			PaneID string `json:"pane_id"`
		} `json:"pane"`
	}
	_ = json.Unmarshal(split, &sp)
	if sp.Pane.PaneID == "" {
		return fmt.Errorf("split returned no pane id")
	}
	// no socket pane.run surface (S8-era fact: CLI-only) — type into the fresh
	// shell and press Enter, mirroring what `herdr pane run` does.
	if _, err := c.Call("pane.send_text", map[string]any{
		"pane_id": sp.Pane.PaneID, "text": "lazygit -p " + worktree,
	}, 10*time.Second); err != nil {
		return err
	}
	_, err = c.Call("pane.send_keys", map[string]any{
		"pane_id": sp.Pane.PaneID, "keys": []string{"enter"},
	}, 10*time.Second)
	if err != nil {
		return err
	}
	_, _ = c.Call("tab.focus", map[string]any{"tab_id": tabID}, 10*time.Second)
	return nil
}

// fmtTallies renders stats snapshots ("efficient" → {calls: N, errors: E})
// as "efficient=N" (+ "=N(errors=E)" when errors are present).
func fmtTallies(m map[string]any) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		v, ok := m[k].(map[string]any)
		if !ok {
			parts = append(parts, fmt.Sprintf("%s=%v", k, m[k]))
			continue
		}
		calls, _ := v["calls"].(float64) // v0.2.0 field; total_requests is the 0.3.0 name
		if calls == 0 {
			calls, _ = v["total_requests"].(float64)
		}
		s := fmt.Sprintf("%s=%d", k, int(calls))
		if errs, ok := v["errors"].(float64); ok && errs > 0 {
			s += fmt.Sprintf(" (errors=%d)", int(errs))
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, " ")
}

// connect resolves the herdr socket, bootstrapping a named sandbox session
// when needed (S11: spawn via a temp pane in the live session; server persists).
func (r *Run) connect() (*herdrd.Client, error) {
	if r.Opts.Session == "" {
		return herdrd.New()
	}
	sock, err := herdrd.SocketPathFor(r.Opts.Session)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(sock); err == nil {
		return herdrd.NewAt(sock)
	}
	// bootstrap: named server only starts under a TTY; use a temp pane of the
	// live session and clear the nesting-marker env vars
	defSock, err := herdrd.SocketPathFor("")
	if err != nil {
		return nil, err
	}
	dc, err := herdrd.NewAt(defSock)
	if err != nil {
		return nil, fmt.Errorf("sandbox %q down and default session not running: %w", r.Opts.Session, err)
	}
	ws, err := dc.FocusedWorkspace()
	if err != nil {
		return nil, err
	}
	cmd := "env -u HERDR_ENV -u HERDR_PANE_ID -u HERDR_SOCKET_PATH -u HERDR_SESSION -u HERDR_WORKSPACE_ID -u HERDR_TAB_ID herdr --session " + r.Opts.Session
	res, err := dc.Call("layout.apply", map[string]any{
		"workspace_id": ws, "tab_label": "forseti-sandbox-boot", "focus": false,
		"root": map[string]any{"type": "pane", "label": "boot", "cwd": r.Opts.Cwd,
			"command": []string{"sh", "-c", cmd}},
	}, 20*time.Second)
	if err != nil {
		return nil, fmt.Errorf("sandbox bootstrap: %w", err)
	}
	var out struct {
		Layout struct {
			TabID string `json:"tab_id"`
		} `json:"layout"`
	}
	_ = json.Unmarshal(res, &out)
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(sock); err == nil {
			if out.Layout.TabID != "" {
				_, _ = dc.Call("tab.close", map[string]any{"tab_id": out.Layout.TabID}, 10*time.Second)
			}
			r.emit(Event{Type: "run_start", Info: "sandbox session " + r.Opts.Session + " bootstrapped"})
			rc, err := herdrd.NewAt(sock)
			if err != nil {
				return nil, err
			}
			// the socket can appear before the server can create panes
			// (hit live: agent_pane_busy on every pane) — poll ping until
			// the server answers authoritatively
			healthDeadline := time.Now().Add(20 * time.Second)
			for time.Now().Before(healthDeadline) {
				if err := rc.Ping(); err == nil {
					return rc, nil
				}
				time.Sleep(400 * time.Millisecond)
			}
			return nil, fmt.Errorf("sandbox %q socket up but server not answering ping within 20s", r.Opts.Session)
		}
		time.Sleep(250 * time.Millisecond)
	}
	return nil, fmt.Errorf("sandbox %q did not come up within 30s", r.Opts.Session)
}

// pumpStatus consumes the status stream: updates live badges (6B-2) and fires
// blocked notifications (6B-3, best-effort — disabled toasts fall back to the
// ttt status bridge which every event writes anyway).
func (r *Run) pumpStatus(c *herdrd.Client, sub *herdrd.Subscription) {
	for ev := range sub.Events {
		var data struct {
			PaneID      string `json:"pane_id"`
			AgentStatus string `json:"agent_status"`
		}
		if err := json.Unmarshal(ev.Data, &data); err != nil || data.PaneID == "" {
			continue
		}
		r.mu.Lock()
		for _, st := range r.Nodes {
			if st.PaneID == data.PaneID {
				st.LiveStatus = data.AgentStatus
				if data.AgentStatus == "blocked" && st.Status == "running" {
					st.Status = "blocked"
					st.Ended = time.Now()
				}
			}
		}
		name := ""
		for _, st := range r.Nodes {
			if st.PaneID == data.PaneID {
				name = st.Name
			}
		}
		r.mu.Unlock()
		if name != "" {
			r.emit(Event{Type: "node_status", Node: name, Info: data.AgentStatus})
			if data.AgentStatus == "blocked" {
				go func(node string) {
					_ = c.NotificationShow("forseti-crew: "+node+" needs you",
						"agent blocked — approval/question UI in its pane", "request")
				}(name)
			}
		}
	}
}

// watchLoop arms pane.wait_for_output repeatedly (S12: matches past scrollback,
// so already-emitted lines are skipped) — advisory only.
func (r *Run) watchLoop(ctx context.Context, c *herdrd.Client, w schema.Watcher) {
	seen := map[string]bool{}
	for ctx.Err() == nil {
		r.mu.Lock()
		st := r.Nodes[w.Node]
		pane, running := st.PaneID, st.Status == "running"
		r.mu.Unlock()
		if !running || pane == "" {
			time.Sleep(500 * time.Millisecond)
			continue
		}
		line, err := c.PaneWaitForOutput(pane, "regex", w.Match, 30*time.Second)
		if err != nil {
			time.Sleep(500 * time.Millisecond)
			continue
		}
		if !seen[line] {
			seen[line] = true
			r.emit(Event{Type: "pattern_matched", Node: w.Node, Line: line, Info: "watch " + w.Match})
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// runDueChecks runs checks whose after-node just settled (6A-1). Failures do
// not stop the graph; they flip the process exit code via ChecksFailed().
// P13 BUG-25: a blocked/skipped after-node is an advisory check_skip (the
// designed human-in-the-loop / gate-skip paths), not a failure — only a
// FAILED after-node reddens the exit code. BUG-04/26: checksDone and the
// status capture stay under r.mu (reads happen on stream goroutines via
// emit → checksFailedCount).
func (r *Run) runDueChecks(c *herdrd.Client) {
	for i, ch := range r.Crew.Checks {
		key := fmt.Sprintf("check:%d", i)
		r.mu.Lock()
		doneKey := r.checksDone[key]
		var status string
		if st := r.Nodes[ch.After]; st != nil {
			status = st.Status
		}
		r.mu.Unlock()
		if doneKey {
			continue
		}
		settled := status == "done" || status == "failed" || status == "blocked" || status == "skipped"
		if !settled {
			continue
		}
		r.mu.Lock()
		r.checksDone[key] = true
		r.mu.Unlock()
		if status != "done" {
			evType := "check_skip"
			if status == "failed" { // a failed after-node IS a failure
				evType = "check_fail"
				r.mu.Lock()
				r.checksDone[ch.Name+"_failed"] = true
				r.mu.Unlock()
			}
			r.emit(Event{Type: evType, Node: ch.Name,
				Info: fmt.Sprintf("node %s settled %s before check ran", ch.After, status)})
			continue
		}
		pass, tail := runCheck(ch.Run, r.effectiveCwd)
		ev := Event{Type: "check_pass", Node: ch.Name}
		if !pass {
			ev.Type = "check_fail"
			r.mu.Lock()
			r.checksDone[ch.Name+"_failed"] = true
			r.mu.Unlock()
		}
		ev.Info = strings.TrimSpace(tail)
		r.emit(ev)
	}
}

func runCheck(shell, dir string) (bool, string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", shell)
	cmd.Dir = dir
	// P13 BUG-08: CommandContext kills only the direct child — grandchildren
	// survive and inherit the pipes, hanging CombinedOutput past the deadline.
	// WaitDelay bounds the pipe wait; the process group makes the kill sweep.
	cmd.WaitDelay = 30 * time.Second
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	out, err := cmd.CombinedOutput()
	tail := string(out)
	if len(tail) > 240 {
		tail = "…" + tail[len(tail)-240:]
	}
	if err != nil {
		return false, fmt.Sprintf("%v | %s", err, tail)
	}
	return true, tail
}

// tailRoutingLog streams switchyard's routing JSONL into the run log (6D-6).
func (r *Run) tailRoutingLog() {
	if r.getProxy() == nil {
		return
	}
	path := r.getProxy().RoutingLog
	deadline := time.Now().Add(2 * time.Hour)
	var offset int64
	for time.Now().Before(deadline) {
		if r.getProxy() == nil {
			return
		}
		f, err := os.Open(path)
		if err != nil {
			time.Sleep(500 * time.Millisecond)
			continue
		}
		fi, _ := f.Stat()
		if fi.Size() > offset {
			_, _ = f.Seek(offset, 0)
			sc := bufio.NewScanner(f)
			for sc.Scan() {
				var rec struct {
					Model  string `json:"model"`
					Tokens int    `json:"total_tokens"`
					TS     string `json:"ts"`
				}
				if json.Unmarshal(sc.Bytes(), &rec) == nil && rec.Model != "" {
					r.emit(Event{Type: "route_decision", Model: rec.Model,
						Info: fmt.Sprintf("routed call: %d tokens @ %s", rec.Tokens, rec.TS)})
				}
			}
			offset = fi.Size()
		}
		_ = f.Close()
		time.Sleep(750 * time.Millisecond)
	}
}

// teardown reverses setup: view projection, provider entry, proxy, worktree.
func (r *Run) teardown(c *herdrd.Client) {
	if c != nil {
		_ = c.AgentViewClear("crew:" + r.Crew.Name)
	}
	if r.restoreProvider != nil {
		r.restoreProvider()
		r.restoreProvider = nil
	}
	if p := r.getProxy(); p != nil {
		p.Stop()
		r.setProxy(nil)
	}
	if r.worktreeWS != "" && !r.Opts.KeepWorktree {
		wtPath := r.effectiveCwd
		if err := c.WorktreeRemove(r.worktreeWS, true); err != nil {
			// P13 BUG-10: teardown's own failures are `teardown` events —
			// a node_failed here used to flip the TUI/watch exit codes
			r.emit(Event{Type: "teardown", Info: "worktree remove: " + err.Error()})
		}
		// worktree.remove can leave a husk (e.g. our untracked .forseti dir);
		// sweep it — but only under herdr's own worktrees root
		if wtPath != r.Opts.Cwd && strings.Contains(wtPath, ".herdr/worktrees") {
			if _, err := os.Stat(wtPath); err == nil {
				_ = os.RemoveAll(wtPath)
			}
			_ = exec.Command("git", "-C", r.Opts.Cwd, "worktree", "prune").Run()
		}
		if branch := r.Opts.WorktreeBranch; branch != "" {
			// branch -D can fail while the worktree is still registered; prune
			// then retry once
			if err := exec.Command("git", "-C", r.Opts.Cwd, "branch", "-D", branch).Run(); err != nil {
				_ = exec.Command("git", "-C", r.Opts.Cwd, "worktree", "prune").Run()
				_ = exec.Command("git", "-C", r.Opts.Cwd, "branch", "-D", branch).Run()
			}
		}
		// P13 BUG-10: teardown notes are `teardown` events, NOT run_end — the
		// second run_end used to overwrite watch's summary + defeat its exit code
		r.emit(Event{Type: "teardown", Info: "worktree removed"})
	}
	// status bridge → idle (stable checkout path, matching writeStatusBridge)
	if r.Opts.Cwd != "" {
		path := filepath.Join(r.Opts.Cwd, ".forseti", "crew-status.json")
		_ = os.Remove(path)
	}
}

// effectiveModel applies the outage model override to direct-model nodes.
func (r *Run) effectiveModel(a schema.Agent) string {
	if r.Opts.ModelOverride != "" && a.Model != "" {
		return r.Opts.ModelOverride
	}
	return a.Model
}

// evaluateLayaEdges resolves a settled node's laya-gated outgoing edges with
// ONE bounded decision (S19): a choice over the group's targets (+ implicit
// "other"). Argmax above the winning edge's min_confidence activates exactly
// that edge; everything else skips with the distribution recorded. A dead or
// failing endpoint fail-safes to skip — the graph never hangs on decisions.
//
// State precedence: the group's first declared state_file (the artifact the
// agent wrote — the terminal scrollback truncates the encoder and is polluted
// by the prompt echo, hit live); else the node's captured output.
func (r *Run) evaluateLayaEdges(st *NodeState, es *edgeState) {
	var layaEdges []schema.Edge
	for _, e := range r.Crew.OutEdges(st.Name) {
		if e.IsLaya() {
			layaEdges = append(layaEdges, e)
		}
	}
	if len(layaEdges) == 0 {
		return
	}
	state := st.Output
	stateFile := ""
	for _, e := range layaEdges {
		if e.StateFile != "" {
			stateFile = e.StateFile
			break
		}
	}
	if stateFile != "" {
		if b, err := os.ReadFile(filepath.Join(r.effectiveCwd, stateFile)); err == nil {
			state = string(b)
			r.emit(Event{Type: "laya_decision", Node: st.Name, Info: "state from file: " + stateFile})
		} else {
			r.emit(Event{Type: "laya_decision", Node: st.Name, Info: "state_file unreadable (" + err.Error() + ") — falling back to output"})
		}
	}
	if len(state) > 4000 {
		state = state[:4000] // state = evidence, not a full dump (upstream cap 50k)
	}
	choices := make([]layaChoice, 0, len(layaEdges))
	for _, e := range layaEdges {
		choices = append(choices, layaChoice{Target: e.To, Instructions: e.LayaInstructions()})
	}
	chosen, conf, rec, err := r.layaClient().Decide(state, choices)
	if rec != nil {
		r.emit(Event{Type: "laya_decision", Node: st.Name, Model: fmt.Sprint(rec["checkpoint"]),
			Info: fmt.Sprintf("chosen=%s conf=%.3f latency=%dms", rec["chosen"], conf, rec["latency_ms"])})
	}
	if err != nil {
		// failed-safe: endpoint down/ambiguous → every laya edge misses
		// (P13: skip events collected, target skipped under one lock)
		r.mu.Lock()
		for _, e := range layaEdges {
			r.missEdge(es, e, "laya gate failed-safe: "+err.Error())
		}
		r.mu.Unlock()
		return
	}
	for _, e := range layaEdges {
		if e.To == chosen && conf >= e.MinConfidence {
			r.mu.Lock()
			r.fireEdge(es, e) // enforces max_visits + re-arm internally
			r.mu.Unlock()
		} else {
			why := "not chosen"
			if e.To == chosen {
				why = fmt.Sprintf("abstained: conf %.3f < min_confidence %.2f", conf, e.MinConfidence)
			}
			r.mu.Lock()
			r.missEdge(es, e, "laya gate: "+why)
			r.mu.Unlock()
		}
	}
}

// layaClient lazily builds the decision client (config from FORSETI_LAYA_URL).
func (r *Run) layaClient() *LayaClient {
	if r.laya == nil {
		r.laya = NewLayaClient()
	}
	return r.laya
}

func (r *Run) runNode(ctx context.Context, c *herdrd.Client, st *NodeState) {
	a := r.Crew.Agent(st.Name)
	resolved := r.effectiveModel(*a)
	if a.Route != "" {
		resolved = "switchyard/" + a.Route
	}
	r.mu.Lock()
	st.Status = "running"
	st.Started = time.Now()
	st.Visits++
	r.mu.Unlock()
	startEv := Event{Type: "node_start", Node: a.Name, Model: resolved, Info: "pane=" + st.PaneID}
	if st.PhaseIdx >= 0 && st.PhaseIdx < len(r.Crew.Phases) {
		startEv.Phase = r.Crew.Phases[st.PhaseIdx].Name
	}
	r.emit(startEv)

	// render prompt template with upstream outputs + a memory helper:
	// {{ memory "query" }} recalls shared agent memory at render time
	// (advisory context; empty string on any failure — never fails a run)
	// P11: phase instructions prefix the prompt through the SAME render —
	// instructions can reference upstream outputs and {{ memory }} exactly
	// like the agent's own prompt can.
	render := a.Prompt
	if st.PhaseIdx >= 0 && st.PhaseIdx < len(r.Crew.Phases) {
		if ins := strings.TrimSpace(r.Crew.Phases[st.PhaseIdx].Instructions); ins != "" {
			render = ins + "\n\n" + a.Prompt
		}
	}
	tpl, err := template.New(a.Name).Funcs(template.FuncMap{
		"memory": func(q string) string { return r.recallMemory(a.Name, q) },
	}).Parse(render)
	if err != nil {
		r.fail(a.Name, fmt.Sprintf("prompt template: %v", err))
		return
	}
	// P13 BUG-06: upstream outputs are bound UNCONDITIONALLY — a skipped or
	// empty upstream used to render the literal "<no value>" into the prompt.
	data := map[string]string{}
	r.mu.Lock()
	for _, e := range incoming(r.Crew, a.Name) {
		if up := r.Nodes[e.From]; up != nil {
			data[e.From] = up.Output
		}
	}
	retries := st.Retries
	r.mu.Unlock()
	var sb strings.Builder
	if err := tpl.Execute(&sb, data); err != nil {
		r.fail(a.Name, fmt.Sprintf("prompt render: %v", err))
		return
	}
	prompt := sb.String()
	// P14 auto-recall: prepend the crew memory block AFTER the template render
	// (the block itself is never template-expanded). Query = memory_query when
	// declared, else node name + prompt head. Advisory: down/empty → no block.
	if mem := r.Crew.Memory; mem != nil && mem.AutoRecallOn() && !a.MemoryOff() {
		if block := r.autoRecallBlock(a, prompt); block != "" {
			prompt = block + "\n\n---\n\n" + prompt
		}
	}

	// baseline for the empty-settle guard (halogen quirk, 6A-3)
	baseline, _ := c.AgentRead(a.Name, "recent_unwrapped", 400)

	// settle atomically with the prompt (S-spike: separate wait-idle races the
	// pre-prompt idle state and returns the startup screen instead of the answer)
	status, err := c.AgentPromptWait(a.Name, prompt,
		[]string{"idle", "done", "blocked"}, r.Opts.NodeTimeout)
	if err != nil {
		r.fail(a.Name, fmt.Sprintf("agent.prompt+wait: %v", err))
		return
	}
	if status == "blocked" {
		r.mu.Lock()
		st.Status = "blocked"
		st.Ended = time.Now()
		r.mu.Unlock()
		r.emit(Event{Type: "node_blocked", Node: a.Name, Info: "approval/question UI detected — needs you in the pane"})
		return
	}
	text, err := c.AgentRead(a.Name, "recent_unwrapped", 400)
	if err != nil {
		r.fail(a.Name, fmt.Sprintf("agent.read: %v", err))
		return
	}
	// LAN-box empty-content guard (6A-3): the pane grew almost nothing →
	// re-prompt once (S15 quirk: empty content on small max_tokens).
	// P13 BUG-23: box-only — the guard's false positives re-prompted every
	// short-prompt/short-answer node on healthy models (double cost).
	// 2026-10-05: the llama-swap box rebranded halogen→valhalla, same quirk.
	if (strings.Contains(resolved, "halogen") || strings.Contains(resolved, "valhalla")) &&
		paneDelta(baseline, text) < 150 && retries == 0 {
		r.mu.Lock()
		st.Retries++
		r.mu.Unlock()
		r.emit(Event{Type: "node_retry", Node: a.Name, Info: fmt.Sprintf("settle delta %d chars — re-prompting once", paneDelta(baseline, text))})
		status, err := c.AgentPromptWait(a.Name, prompt,
			[]string{"idle", "done", "blocked"}, r.Opts.NodeTimeout)
		if err != nil {
			r.fail(a.Name, fmt.Sprintf("agent.prompt+wait (retry): %v", err))
			return
		}
		if status == "blocked" {
			r.mu.Lock()
			st.Status = "blocked"
			st.Ended = time.Now()
			r.mu.Unlock()
			r.emit(Event{Type: "node_blocked", Node: a.Name, Info: "approval/question UI detected — needs you in the pane"})
			return
		}
		if text, err = c.AgentRead(a.Name, "recent_unwrapped", 400); err != nil {
			r.fail(a.Name, fmt.Sprintf("agent.read (retry): %v", err))
			return
		}
	}
	cost, ctxPct := parsePiStatus(text)
	bus := filepath.Join(r.effectiveCwd, ".forseti", "bus", a.Name+".md")
	if err := os.WriteFile(bus, []byte(text), 0o644); err != nil {
		r.emit(Event{Type: "node_failed", Node: a.Name, Info: "bus write: " + err.Error()})
		r.mu.Lock()
		st.Status = "failed"
		r.mu.Unlock()
		return
	}
	r.mu.Lock()
	st.Output = text
	st.Status = "done"
	st.Ended = time.Now()
	st.CostUSD, st.CtxPct = cost, ctxPct
	r.mu.Unlock()
	r.emit(Event{Type: "node_done", Node: a.Name, Model: resolved, CostUSD: cost, CtxPct: ctxPct,
		Info: fmt.Sprintf("%d chars → %s", len(text), bus)})
	// P14 auto-write: post the finished output to the crew memory namespace
	// (fire-and-forget goroutine — a down service costs an event, never the run).
	if mem := r.Crew.Memory; mem != nil && mem.AutoWriteOn() && !a.MemoryOff() {
		r.autoWrite(a.Name, text)
	}
}

// paneDelta is the heuristic empty-settle measure (6A-3): how much the pane
// grew between the baseline read and the post-settle read. A real answer adds
// the prompt echo plus its own text (hundreds of chars); an empty-content
// settle grows the pane by the echo alone.
func paneDelta(baseline, after string) int {
	return len(strings.TrimSpace(after)) - len(strings.TrimSpace(baseline))
}

var (
	piCostRe = regexp.MustCompile(`\$([0-9]+(?:\.[0-9]+)?)`)
	piCtxRe  = regexp.MustCompile(`([0-9]+(?:\.[0-9]+)?)%/`)
)

// parsePiStatus extracts $ cost + context % from pi's footer (last matches).
func parsePiStatus(text string) (float64, float64) {
	var cost, ctx float64
	if m := piCostRe.FindAllStringSubmatch(text, -1); len(m) > 0 {
		fmt.Sscanf(m[len(m)-1][1], "%f", &cost)
	}
	if m := piCtxRe.FindAllStringSubmatch(text, -1); len(m) > 0 {
		fmt.Sscanf(m[len(m)-1][1], "%f", &ctx)
	}
	return cost, ctx
}

func (r *Run) fail(node, msg string) {
	r.mu.Lock()
	if st, ok := r.Nodes[node]; ok {
		st.Status = "failed"
		st.Ended = time.Now()
	}
	r.mu.Unlock()
	r.emit(Event{Type: "node_failed", Node: node, Info: msg})
}

// huskToSweep extracts a leftover worktree path from a create failure
// ("'/…/.herdr/worktrees/<repo>/<branch>' already exists"). Only paths under
// herdr's own worktrees root are sweepable.
func huskToSweep(err error) string {
	msg := err.Error()
	i := strings.Index(msg, ".herdr/worktrees")
	if i < 0 {
		return ""
	}
	start := strings.LastIndex(msg[:i], "'")
	end := strings.Index(msg[i:], "'")
	if start < 0 || end < 0 {
		return ""
	}
	return msg[start+1 : i+end]
}

// recallMemory queries the shared memory service for a node's prompt render.
// Never fails: endpoint down / empty → "". The result is capped so a prompt
// stays bounded.
func (r *Run) recallMemory(node, query string) string {
	if r.memory == nil {
		r.memory = NewMemoryClient()
	}
	rec, err := r.memory.Recall(query, node, 3, r.MemoryNS())
	if err != nil {
		r.emit(Event{Type: "memory_recall", Node: node, Info: "unavailable: " + err.Error()})
		return ""
	}
	r.emit(Event{Type: "memory_recall", Node: node, Info: fmt.Sprintf("%d entries for %q", len(rec), query)})
	if len(rec) == 0 {
		return ""
	}
	var b strings.Builder
	for _, row := range rec {
		// P13 BUG-07: unchecked assertions used to panic the whole process on
		// a schema-drifted response (violating the helper's never-fail contract)
		ts, _ := row["ts"].(string)
		txt, _ := row["text"].(string)
		if len(ts) > 10 {
			ts = ts[:10]
		}
		if txt == "" {
			continue
		}
		fmt.Fprintf(&b, "- [%s] %s\n", ts, txt)
	}
	out := b.String()
	if len(out) > 2000 {
		out = out[:2000]
	}
	return out
}

// MemoryNS returns the crew's memory namespace (P14) — "" when the crew has
// no memory: block, which keeps every call below invisible to the service.
func (r *Run) MemoryNS() string {
	if r.Crew.Memory == nil {
		return ""
	}
	return r.Crew.Memory.NS(r.Crew.Name)
}

// memClient lazily builds the memory client. Pre-created in Run() when the
// crew declares memory, so auto-write goroutines only ever read the pointer.
func (r *Run) memClient() *MemoryClient {
	if r.memory == nil {
		r.memory = NewMemoryClient()
	}
	return r.memory
}

// autoRecallBlock (P14) is the dispatch-time auto-recall: it queries with the
// node's memory_query (or its name + a prompt head) and renders a compact
// "## Shared memory" block to prepend to the prompt. Advisory: any failure
// or empty result → "" — a down service never changes what gets dispatched
// beyond the missing block.
func (r *Run) autoRecallBlock(a *schema.Agent, prompt string) string {
	mem := r.Crew.Memory
	k := mem.RecallK
	if k <= 0 {
		k = 3
	}
	q := a.MemoryQuery
	if q == "" {
		head := truncateUTF8(prompt, 200)
		q = a.Name + " " + head
	}
	rec, err := r.memClient().Recall(q, a.Name, k, r.MemoryNS())
	if err != nil {
		r.emit(Event{Type: "memory_recall", Node: a.Name, Info: "auto: unavailable: " + err.Error()})
		return ""
	}
	if len(rec) == 0 {
		r.emit(Event{Type: "memory_recall", Node: a.Name, Info: fmt.Sprintf("auto: 0 entries for %q", truncateUTF8(q, 60))})
		return ""
	}
	r.emit(Event{Type: "memory_recall", Node: a.Name, Info: fmt.Sprintf("auto: %d entries for %q", len(rec), truncateUTF8(q, 60))})
	var b strings.Builder
	b.WriteString("## Shared memory (from the crew; trust but verify)\n")
	for _, row := range rec {
		txt, _ := row["text"].(string)
		ts, _ := row["ts"].(string)
		if len(ts) > 10 {
			ts = ts[:10]
		}
		if txt == "" {
			continue
		}
		fmt.Fprintf(&b, "- [%s] %s\n", ts, txt)
	}
	out := strings.TrimSpace(b.String())
	if len(out) > 2000 {
		out = truncateUTF8(out, 2000)
	}
	return out
}

// autoWrite (P14) posts a finished node's output to the crew memory
// namespace. Fire-and-forget: errors only emit an event, the run never sees
// them.
func (r *Run) autoWrite(node, text string) {
	mem := r.Crew.Memory
	text = truncateUTF8(text, mem.WriteMaxChars)
	if strings.TrimSpace(text) == "" {
		return
	}
	go func() {
		id, err := r.memClient().Write(text, r.MemoryNS(), r.runID, "node:"+node, []string{"crew", r.Crew.Name})
		if err != nil {
			r.emit(Event{Type: "memory_write", Node: node, Info: "unavailable: " + err.Error()})
			return
		}
		r.emit(Event{Type: "memory_write", Node: node, Info: fmt.Sprintf("#%d -> crew-%s", id, r.MemoryNS())})
	}()
}

// truncateUTF8 cuts s to at most n bytes, never mid-rune.
func truncateUTF8(s string, n int) string {
	if n <= 0 || len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

func (r *Run) summary() string {
	// P13: mu-guarded — Summary() is also called from the TUI's run goroutine
	// right after Run() returns, where a final status-pump write may still be
	// landing under mu. The checks tally is inlined because Go's mutex is not
	// reentrant (checksFailedCount locks too).
	r.mu.Lock()
	counts := map[string]int{}
	var cost float64
	for _, st := range r.Nodes {
		counts[st.Status]++
		cost += st.CostUSD
	}
	f := 0
	for _, ch := range r.Crew.Checks {
		if r.checksDone[ch.Name+"_failed"] {
			f++
		}
	}
	r.mu.Unlock()
	parts := []string{}
	for _, s := range []string{"done", "blocked", "failed", "skipped", "running", "pending"} {
		if counts[s] > 0 {
			parts = append(parts, fmt.Sprintf("%s=%d", s, counts[s]))
		}
	}
	if f > 0 {
		parts = append(parts, fmt.Sprintf("checks_failed=%d", f))
	}
	parts = append(parts, fmt.Sprintf("cost=$%.4f", cost))
	return strings.Join(parts, " ")
}

// Summary is the exported one-line result tally.
func (r *Run) Summary() string { return r.summary() }

func incoming(c *schema.Crew, name string) []schema.Edge {
	var out []schema.Edge
	for _, e := range c.Edges {
		if e.To == name {
			out = append(out, e)
		}
	}
	return out
}

func edgeSatisfied(e schema.Edge, output string) bool {
	if strings.HasPrefix(e.When, "re:") {
		re, err := compileCached(e.When[3:])
		if err != nil {
			return false
		}
		return re.MatchString(output)
	}
	// status edge: node completed means it reached idle (blocked/failed handled earlier)
	return e.When == "idle"
}

// createCrewTab builds a right-leaning spine BSP tree of n panes and applies it.
// Returns pane ids in tree order (left→right) + the new tab id. (S8 verified shape.)
func createCrewTab(c *herdrd.Client, ws, label, cwd string, n int) ([]string, string, error) {
	var root map[string]any
	for i := 0; i < n; i++ {
		pane := map[string]any{"type": "pane", "label": fmt.Sprintf("crew-%d", i+1), "cwd": cwd}
		if root == nil {
			root = pane
			continue
		}
		root = map[string]any{
			"type": "split", "direction": "right",
			"ratio": float64(i) / float64(i+1),
			"first": root, "second": pane,
		}
	}
	res, err := c.Call("layout.apply", map[string]any{
		"workspace_id": ws, "tab_label": label, "focus": false, "root": root,
	}, 20*time.Second)
	if err != nil {
		return nil, "", err
	}
	var out struct {
		Layout struct {
			TabID string         `json:"tab_id"`
			Root  map[string]any `json:"root"`
		} `json:"layout"`
	}
	if err := json.Unmarshal(res, &out); err != nil {
		return nil, "", err
	}
	var paneIDs []string
	var walk func(map[string]any)
	walk = func(node map[string]any) {
		if node == nil {
			return
		}
		if node["type"] == "pane" {
			if id, ok := node["pane_id"].(string); ok {
				paneIDs = append(paneIDs, id)
			}
			return
		}
		if f, ok := node["first"].(map[string]any); ok {
			walk(f)
		}
		if s, ok := node["second"].(map[string]any); ok {
			walk(s)
		}
	}
	walk(out.Layout.Root)
	if len(paneIDs) != n {
		return nil, "", fmt.Errorf("expected %d panes from layout, got %d", n, len(paneIDs))
	}
	return paneIDs, out.Layout.TabID, nil
}
