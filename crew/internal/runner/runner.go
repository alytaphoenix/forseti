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
	"text/template"
	"time"

	"forseti/crew/internal/herdrd"
	"forseti/crew/internal/schema"
	"forseti/crew/internal/switchyard"
)

// Event is a run-log record (also streamed to the TUI).
type Event struct {
	TS      time.Time `json:"ts"`
	Type    string    `json:"type"` // run_start|node_start|node_done|node_retry|node_blocked|node_failed|node_status|edge_skip|check_pass|check_fail|pattern_matched|route_decision|run_end
	Node    string    `json:"node,omitempty"`
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
	logCh chan Event

	proxy           *switchyard.Proxy
	restoreProvider func()
	worktreeWS      string // worktree workspace id ("" if not created)
	effectiveCwd    string // worktree checkout or Opts.Cwd
	checksDone      map[string]bool
	checksFailed    int
	finished        bool // set before the final run_end emit; freezes the status bridge
	laya            *LayaClient
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
	r := &Run{Crew: crew, Opts: opts, Nodes: map[string]*NodeState{}, logCh: make(chan Event, 512), checksDone: map[string]bool{}}
	r.effectiveCwd = opts.Cwd
	for _, a := range crew.Agents {
		r.Nodes[a.Name] = &NodeState{Name: a.Name, Status: "pending"}
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
	if r.finished || r.Opts.Cwd == "" {
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
	})
	path := filepath.Join(r.Opts.Cwd, ".forseti", "crew-status.json")
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	_ = os.WriteFile(path, out, 0o644)
}

func (r *Run) checksFailedCount() int {
	n := 0
	for _, ch := range r.Crew.Checks {
		if r.checksDone[ch.Name+"_failed"] {
			n++
		}
	}
	return n
}

// Snapshot returns current node states (sorted by crew order).
func (r *Run) Snapshot() []*NodeState {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*NodeState, 0, len(r.Nodes))
	for _, a := range r.Crew.Agents {
		out = append(out, r.Nodes[a.Name])
	}
	return out
}

// ChecksFailed reports whether any check assertion failed (exit-code input).
func (r *Run) ChecksFailed() bool {
	for _, ch := range r.Crew.Checks {
		if r.checksDone[ch.Name+"_failed"] {
			return true
		}
	}
	return false
}

// Run executes the whole graph. Blocks until completion or ctx cancel.
func (r *Run) Run(ctx context.Context) error {
	c, err := r.connect()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(r.effectiveCwd, ".forseti", "bus"), 0o755); err != nil {
		return err
	}
	runDir := filepath.Join(r.effectiveCwd, ".forseti", "runs")
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		return err
	}
	name := fmt.Sprintf("%s-%s.jsonl", time.Now().Format("200601-150405"), r.Crew.Name)
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
		r.proxy = p
		ids := make([]string, len(r.Crew.Routes))
		for i, rt := range r.Crew.Routes {
			ids[i] = rt.ID
		}
		if r.restoreProvider, err = p.MaterializeProvider(ids); err != nil {
			p.Stop()
			r.proxy = nil
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

	// 6. output regex watchers (6C-1): advisory pattern_matched events
	for _, w := range r.Crew.Watch {
		go r.watchLoop(ctx, c, w)
	}

	// 7. wave scheduler: run nodes whose incoming edges are all satisfied
	visitedEdge := map[string]int{} // "from>to" → times used
	satisfied := map[string]bool{}  // node → all incoming satisfied for this trigger
	for _, n := range r.Nodes {
		if len(incoming(r.Crew, n.Name)) == 0 {
			satisfied[n.Name] = true
		}
	}
	for {
		var ready []*NodeState
		r.mu.Lock()
		for _, a := range r.Crew.Agents {
			st := r.Nodes[a.Name]
			if st.Status == "pending" && satisfied[a.Name] {
				ready = append(ready, st)
			}
		}
		r.mu.Unlock()
		if len(ready) == 0 {
			break
		}
		var wg2 sync.WaitGroup
		for _, st := range ready {
			wg2.Add(1)
			go func(st *NodeState) {
				defer wg2.Done()
				r.runNode(ctx, c, st)
			}(st)
		}
		wg2.Wait()
		// after the wave: checks for freshly-settled nodes, laya decision
		// gates (one grouped endpoint call per settled node), then plain gates
		r.runDueChecks(c)
		for _, st := range ready {
			if st.Status == "done" {
				r.evaluateLayaEdges(st, visitedEdge, satisfied)
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
				key := e.From + ">" + e.To
				limit := e.MaxVisits
				if limit == 0 {
					limit = 1
				}
				if visitedEdge[key] >= limit {
					r.emit(Event{Type: "edge_skip", Node: e.To, Info: fmt.Sprintf("%s max_visits reached", key)})
					continue
				}
				if edgeSatisfied(e, st.Output) {
					visitedEdge[key]++
					satisfied[e.To] = true
				} else {
					r.emit(Event{Type: "edge_skip", Node: e.To, Info: fmt.Sprintf("when %q not matched on %s output", e.When, e.From)})
					r.Nodes[e.To].Status = "skipped"
				}
			}
		}
		r.mu.Unlock()
	}

	// any node still pending (unreachable) → skipped
	r.mu.Lock()
	for _, st := range r.Nodes {
		if st.Status == "pending" {
			st.Status = "skipped"
		}
	}
	r.mu.Unlock()
	info := r.summary()
	if r.proxy != nil {
		// run-level route tally (per-node attribution is impossible in
		// switchyard 0.2.0 — pi's session header doesn't attach, S15)
		if st, err := r.proxy.Stats(); err == nil {
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
	r.finished = true // freeze the status bridge; teardown's remove sticks
	r.emit(Event{Type: "run_end", Info: info})
	return nil
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
	defer func() { _ = dc }()
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
func (r *Run) runDueChecks(c *herdrd.Client) {
	for i, ch := range r.Crew.Checks {
		key := fmt.Sprintf("check:%d", i)
		if r.checksDone[key] {
			continue
		}
		r.mu.Lock()
		st := r.Nodes[ch.After]
		settled := st != nil && (st.Status == "done" || st.Status == "failed" || st.Status == "blocked")
		r.mu.Unlock()
		if !settled {
			continue
		}
		r.checksDone[key] = true
		if st.Status != "done" {
			r.emit(Event{Type: "check_fail", Node: ch.Name, Info: fmt.Sprintf("node %s settled %s before check ran", ch.After, st.Status)})
			r.checksDone[ch.Name+"_failed"] = true
			continue
		}
		pass, tail := runCheck(ch.Run, r.effectiveCwd)
		ev := Event{Type: "check_pass", Node: ch.Name}
		if !pass {
			ev.Type = "check_fail"
			r.checksDone[ch.Name+"_failed"] = true
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
	if r.proxy == nil {
		return
	}
	path := r.proxy.RoutingLog
	deadline := time.Now().Add(2 * time.Hour)
	var offset int64
	for time.Now().Before(deadline) {
		if r.proxy == nil {
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
	if r.proxy != nil {
		r.proxy.Stop()
		r.proxy = nil
	}
	if r.worktreeWS != "" && !r.Opts.KeepWorktree {
		wtPath := r.effectiveCwd
		if err := c.WorktreeRemove(r.worktreeWS, true); err != nil {
			r.emit(Event{Type: "node_failed", Info: "worktree remove: " + err.Error()})
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
		r.emit(Event{Type: "run_end", Info: "worktree removed"})
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
func (r *Run) evaluateLayaEdges(st *NodeState, visitedEdge map[string]int, satisfied map[string]bool) {
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
		for _, e := range layaEdges {
			key := e.From + ">" + e.To
			r.emit(Event{Type: "edge_skip", Node: e.To, Info: fmt.Sprintf("%s laya gate failed-safe: %v", key, err)})
			r.mu.Lock()
			r.Nodes[e.To].Status = "skipped"
			r.mu.Unlock()
		}
		return
	}
	for _, e := range layaEdges {
		key := e.From + ">" + e.To
		limit := e.MaxVisits
		if limit == 0 {
			limit = 1
		}
		activate := e.To == chosen && visitedEdge[key] < limit && conf >= e.MinConfidence
		if activate {
			visitedEdge[key]++
			r.mu.Lock()
			satisfied[e.To] = true
			r.mu.Unlock()
		} else {
			why := "not chosen"
			if e.To == chosen && conf < e.MinConfidence {
				why = fmt.Sprintf("abstained: conf %.3f < min_confidence %.2f", conf, e.MinConfidence)
			} else if e.To == chosen && visitedEdge[key] >= limit {
				why = "max_visits reached"
			}
			r.emit(Event{Type: "edge_skip", Node: e.To, Info: fmt.Sprintf("%s laya gate: %s", key, why)})
			r.mu.Lock()
			r.Nodes[e.To].Status = "skipped"
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
	r.emit(Event{Type: "node_start", Node: a.Name, Model: resolved, Info: "pane=" + st.PaneID})

	// render prompt template with upstream outputs
	tpl, err := template.New(a.Name).Parse(a.Prompt)
	if err != nil {
		r.fail(a.Name, fmt.Sprintf("prompt template: %v", err))
		return
	}
	data := map[string]string{}
	for _, e := range incoming(r.Crew, a.Name) {
		if up := r.Nodes[e.From]; up.Output != "" {
			data[e.From] = up.Output
		}
	}
	var sb strings.Builder
	if err := tpl.Execute(&sb, data); err != nil {
		r.fail(a.Name, fmt.Sprintf("prompt render: %v", err))
		return
	}

	// baseline for the empty-settle guard (halogen quirk, 6A-3)
	baseline, _ := c.AgentRead(a.Name, "recent_unwrapped", 400)

	// settle atomically with the prompt (S-spike: separate wait-idle races the
	// pre-prompt idle state and returns the startup screen instead of the answer)
	status, err := c.AgentPromptWait(a.Name, sb.String(),
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
	// halogen empty-content guard (6A-3): the pane grew almost nothing →
	// re-prompt once (S15 quirk: empty content on small max_tokens).
	if paneDelta(baseline, text) < 150 && st.Retries == 0 {
		st.Retries++
		r.emit(Event{Type: "node_retry", Node: a.Name, Info: fmt.Sprintf("settle delta %d chars — re-prompting once", paneDelta(baseline, text))})
		status, err := c.AgentPromptWait(a.Name, sb.String(),
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

func (r *Run) summary() string {
	counts := map[string]int{}
	for _, st := range r.Nodes {
		counts[st.Status]++
	}
	parts := []string{}
	for _, s := range []string{"done", "blocked", "failed", "skipped", "running", "pending"} {
		if counts[s] > 0 {
			parts = append(parts, fmt.Sprintf("%s=%d", s, counts[s]))
		}
	}
	if f := r.checksFailedCount(); f > 0 {
		parts = append(parts, fmt.Sprintf("checks_failed=%d", f))
	}
	var cost float64
	for _, st := range r.Nodes {
		cost += st.CostUSD
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
