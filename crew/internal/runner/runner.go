// Package runner executes a crew graph on herdr: one dedicated tab, one pane
// per node, deterministic edge-gated scheduling (no LLM routing).
//
// Hand-off contract (design.md §Phase 5):
//   - each node's captured output goes to .forseti/bus/<node>.md
//   - downstream prompts template it in via {{ .<node> }}
//   - every step is appended to .forseti/runs/<run>.jsonl
package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"text/template"
	"time"

	"forseti/crew/internal/herdrd"
	"forseti/crew/internal/schema"
)

// Event is a run-log record (also streamed to the TUI).
type Event struct {
	TS   time.Time `json:"ts"`
	Type string    `json:"type"` // run_start|node_start|node_done|node_blocked|node_failed|edge_skip|run_end
	Node string    `json:"node,omitempty"`
	Info string    `json:"info,omitempty"`
}

type NodeState struct {
	Name    string
	Status  string // pending|running|done|blocked|failed|skipped
	PaneID  string
	Output  string
	Visits  int
	Started time.Time
	Ended   time.Time
}

type Options struct {
	Cwd            string        // pane cwd (repo root)
	WorkspaceID    string        // herdr workspace to create the crew tab in ("" = focused)
	TabLabel       string        // default "forseti-crew"
	NodeTimeout    time.Duration // per-node prompt settle timeout (default 10m)
	KeepTab        bool          // leave the crew tab open after the run
	HeadlessPrompt string        // unused placeholder for future
	OnEvent        func(Event)
}

type Run struct {
	Crew  *schema.Crew
	Opts  Options
	mu    sync.Mutex
	Nodes map[string]*NodeState
	logf  *os.File
	logCh chan Event
}

func New(crew *schema.Crew, opts Options) *Run {
	if opts.TabLabel == "" {
		opts.TabLabel = "forseti-crew"
	}
	if opts.NodeTimeout == 0 {
		opts.NodeTimeout = 10 * time.Minute
	}
	r := &Run{Crew: crew, Opts: opts, Nodes: map[string]*NodeState{}, logCh: make(chan Event, 512)}
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

// Run executes the whole graph. Blocks until completion or ctx cancel.
func (r *Run) Run(ctx context.Context) error {
	c, err := herdrd.New()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(r.Opts.Cwd, ".forseti", "bus"), 0o755); err != nil {
		return err
	}
	runDir := filepath.Join(r.Opts.Cwd, ".forseti", "runs")
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		return err
	}
	name := fmt.Sprintf("%s-%s.jsonl", time.Now().Format("200601-150405"), r.Crew.Name)
	r.logf, err = os.Create(filepath.Join(runDir, name))
	if err != nil {
		return err
	}
	defer r.logf.Close()
	r.emit(Event{Type: "run_start", Info: fmt.Sprintf("crew=%s agents=%d edges=%d", r.Crew.Name, len(r.Crew.Agents), len(r.Crew.Edges))})

	ws := r.Opts.WorkspaceID
	if ws == "" {
		if ws, err = c.FocusedWorkspace(); err != nil {
			return err
		}
	}

	// 1. declarative tab: right-leaning spine of N panes (S8 verified)
	paneIDs, tabID, err := createCrewTab(c, ws, r.Opts.TabLabel, r.Opts.Cwd, len(r.Crew.Agents))
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

	// 2. start all agents up-front (readiness is per-agent; S9: wait idle before first prompt)
	var wg sync.WaitGroup
	for _, a := range r.Crew.Agents {
		wg.Add(1)
		go func(a schema.Agent) {
			defer wg.Done()
			args := []string{}
			if a.Model != "" {
				args = append(args, "--model", a.Model)
			}
			args = append(args, a.Args...)
			if err := c.AgentStart(a.Name, a.Kind, r.Nodes[a.Name].PaneID, args, 90*time.Second); err != nil {
				r.fail(a.Name, fmt.Sprintf("agent.start: %v", err))
				return
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

	// 3. wave scheduler: run nodes whose incoming edges are all satisfied
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
		// after the wave, evaluate outgoing edges to unlock downstream nodes
		r.mu.Lock()
		for _, st := range ready {
			if st.Status != "done" {
				continue
			}
			for _, e := range r.Crew.OutEdges(st.Name) {
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
	r.emit(Event{Type: "run_end", Info: r.summary()})
	return nil
}

func (r *Run) runNode(ctx context.Context, c *herdrd.Client, st *NodeState) {
	a := r.Crew.Agent(st.Name)
	r.mu.Lock()
	st.Status = "running"
	st.Started = time.Now()
	st.Visits++
	r.mu.Unlock()
	r.emit(Event{Type: "node_start", Node: a.Name, Info: "pane=" + st.PaneID})

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
	bus := filepath.Join(r.Opts.Cwd, ".forseti", "bus", a.Name+".md")
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
	r.mu.Unlock()
	r.emit(Event{Type: "node_done", Node: a.Name, Info: fmt.Sprintf("%d chars → %s", len(text), bus)})
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

func (r *Run) statusOf(c *herdrd.Client, name string) string {
	agents, err := c.AgentList()
	if err != nil {
		return "unknown"
	}
	for _, a := range agents {
		if a.Name == name {
			return a.AgentStatus
		}
	}
	return "unknown"
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
