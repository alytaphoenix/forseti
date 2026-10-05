// TUI shell: left = graph list / builder, right = live monitor of the selected
// node, bottom = event log. Monitor is read-only (no nested-terminal v1);
// per S7 there is no output push surface, so the tail is a debounced agent.read
// (700 ms) while the selected node is running.
package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/mattn/go-runewidth"
	"gopkg.in/yaml.v3"

	"forseti/crew/internal/herdrd"
	"forseti/crew/internal/runner"
	"forseti/crew/internal/schema"
	"forseti/crew/internal/toolspec"
)

type logMsg struct{ s string }
type monitorTick struct{}

type monitorReadMsg struct { // P13-B11: AgentRead off the event loop
	node string
	text string
	ok   bool
}

type toolsProbeMsg struct{ msg string } // P13-B11: probe subprocess off the loop

type runDoneMsg struct{ summary string } // P13-B8: run completion via message

type formField struct {
	label string
	input textinput.Model
}

type form struct {
	kind   string // "agent" | "edge"
	fields []formField
	step   int
}

type model struct {
	crew   *schema.Crew
	opts   runner.Options
	run    *runner.Run
	client *herdrd.Client
	prog   *tea.Program
	path   string // P13-B2: where -f loaded the crew from; save goes HERE

	selected      int
	log           []string
	mode          string // "view" | "build"
	form          *form
	buildMsg      string
	monitor       string
	monitorNode   string
	metaTab       bool // 6C-2: right pane shows node meta instead of output
	width, height int
	running       bool
}

func runTUI(crew *schema.Crew, opts runner.Options, path string) (bool, error) {
	if path == "" {
		path = "crew.yaml"
	}
	m := &model{crew: crew, opts: opts, mode: "view", path: path}
	p := tea.NewProgram(m, tea.WithAltScreen())
	m.prog = p
	_, err := p.Run()
	// P13-B3/B14: the exit verdict comes from the LIVE run state (blocked
	// counts, matching the headless contract) — never a post-hoc log glob
	// (worktree logs are deleted; same-second logs mislead).
	failed := false
	if m.run != nil {
		for _, st := range m.run.Snapshot() {
			if st.Status == "failed" || st.Status == "blocked" {
				failed = true
			}
		}
		if m.run.ChecksFailed() {
			failed = true
		}
	}
	return failed, err
}

func (m *model) Init() tea.Cmd {
	c, err := herdrd.New()
	if err != nil {
		m.log = append(m.log, "herdr: "+err.Error())
	} else {
		m.client = c
	}
	return m.monitorTick()
}

func (m *model) monitorTick() tea.Cmd {
	return tea.Tick(700*time.Millisecond, func(time.Time) tea.Msg { return monitorTick{} })
}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil
	case logMsg:
		m.log = append(m.log, msg.s)
		if len(m.log) > 200 {
			m.log = m.log[len(m.log)-200:]
		}
		return m, nil
	case monitorTick:
		return m.updateMonitor()
	case monitorReadMsg: // P13-B11: AgentRead ran off the event loop
		if msg.ok {
			m.monitor, m.monitorNode = msg.text, msg.node
		}
		return m, nil
	case toolsProbeMsg: // P13-B11: tools validate ran off the event loop
		m.buildMsg = msg.msg
		return m, nil
	case runDoneMsg: // P13-B8: run completion mutates state only via messages
		m.running = false
		m.log = append(m.log, "run finished: "+msg.summary)
		return m, nil
	case tea.KeyMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

// selectedName is the agent the selection points at (display order — B16).
func (m *model) selectedName() string {
	order := m.displayOrder()
	if len(order) == 0 {
		return ""
	}
	return m.crew.Agents[order[m.selected%len(order)]].Name
}

func (m *model) updateMonitor() (tea.Model, tea.Cmd) {
	var cmd tea.Cmd
	if m.client != nil && m.run != nil {
		name := m.selectedName()
		if st, ok := m.run.NodeSnapshot(name); ok { // P13-B8: value copy, no shared-pointer reads
			switch {
			case st.Status == "running":
				// P13-B11: the socket read runs OFF the event loop (a stalled
				// herdr used to freeze the whole TUI for up to 15 s per tick)
				client, node := m.client, name
				cmd = func() tea.Msg {
					txt, err := client.AgentRead(node, "recent_unwrapped", 200)
					if err != nil {
						return monitorReadMsg{node: node, ok: false}
					}
					return monitorReadMsg{node: node, text: txt, ok: true}
				}
			case m.monitorNode != name && st.Output != "":
				m.monitor, m.monitorNode = st.Output, name
			}
		}
	}
	return m, tea.Batch(m.monitorTick(), cmd)
}

// metaView renders the 6C-2 meta tab for the selected node.
func (m *model) metaView() string {
	name := m.selectedName() // P13-B16: display order, not declaration order
	if name == "" {
		name = "—"
	}
	var b strings.Builder
	b.WriteString("META: " + name + "\n\n")
	a := m.crew.Agent(name)
	var st runner.NodeState
	hasSt := false
	if m.run != nil {
		st, hasSt = m.run.NodeSnapshot(name) // P13-B8: value copy
	}
	if !hasSt || a == nil {
		b.WriteString("(no run yet — press r to start)")
		return b.String()
	}
	model := a.Model
	if a.Route != "" {
		model = a.Route + " (routed pool)"
	}
	phase := "—"
	if pi := m.crew.PhaseOf(a.Name); pi >= 0 {
		phase = fmt.Sprintf("%d %s", pi+1, m.crew.Phases[pi].Name)
	}
	fmt.Fprintf(&b, "pane:      %s\n", st.PaneID)
	fmt.Fprintf(&b, "model:     %s\n", model)
	fmt.Fprintf(&b, "phase:     %s\n", phase)
	fmt.Fprintf(&b, "status:    %s", st.Status)
	if st.LiveStatus != "" && st.LiveStatus != st.Status {
		fmt.Fprintf(&b, " (herdr: %s)", st.LiveStatus)
	}
	b.WriteString("\n")
	if !st.Started.IsZero() {
		end, has := st.Ended, !st.Ended.IsZero()
		if !has {
			end = time.Now()
		}
		fmt.Fprintf(&b, "started:   %s\n", st.Started.Format("15:04:05"))
		fmt.Fprintf(&b, "duration:  %s%s\n", end.Sub(st.Started).Round(time.Second), map[bool]string{true: "", false: " (running)"}[has])
	}
	fmt.Fprintf(&b, "visits:    %d\n", st.Visits)
	fmt.Fprintf(&b, "retries:   %d\n", st.Retries)
	fmt.Fprintf(&b, "output:    %d chars\n", len(st.Output))
	if st.CostUSD > 0 || st.CtxPct > 0 {
		fmt.Fprintf(&b, "cost:      $%.4f (%.1f%% ctx)\n", st.CostUSD, st.CtxPct)
	}
	return b.String()
}

func (m *model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.form != nil {
		return m.formKey(msg)
	}
	switch msg.String() {
	case "ctrl+c":
		return m, tea.Quit
	case "q":
		if m.mode == "build" {
			m.mode = "view"
			return m, nil
		}
		return m, tea.Quit
	case "j", "down":
		if n := len(m.displayOrder()); n > 0 { // P13-B16: navigate display order
			m.selected = (m.selected + 1) % n
			m.monitorNode = ""
		}
	case "k", "up":
		if n := len(m.displayOrder()); n > 0 {
			m.selected = (m.selected - 1 + n) % n
			m.monitorNode = ""
		}
	case "r":
		return m.startRun()
	case "tab":
		// 6C-2: toggle the right pane between output tail and node meta
		m.metaTab = !m.metaTab
		return m, nil
	case "f":
		// focus the REAL herdr pane of the selected node (human-in-the-loop:
		// takeover happens in the actual pane, never inside the monitor)
		if m.client != nil {
			if name := m.selectedName(); name != "" {
				if err := m.client.AgentFocus(name); err != nil {
					return m.logf("focus %s: %v", name, err), nil
				}
				return m.logf("focused real pane: %s", name), nil
			}
		}
	case "b":
		m.mode = "build"
		m.buildMsg = "a=add agent · e=add edge · R=add route · p=add phase · T=add tool · P=probe tools · A=adopt live · s=save · q=back"
	case "v":
		m.mode = "view"
	case "a":
		if m.mode == "build" {
			m.startAgentForm()
		}
	case "e":
		if m.mode == "build" {
			m.startEdgeForm()
		}
	case "R":
		if m.mode == "build" {
			m.startRouteForm()
		}
	case "T":
		if m.mode == "build" {
			m.startToolForm()
		}
	case "p":
		if m.mode == "build" {
			m.startPhaseForm()
		}
	case "P":
		if m.mode == "build" {
			return m, m.probeToolsCmd() // P13-B11: subprocess off the event loop
		}
	case "A":
		if m.mode == "build" {
			m.adoptLive()
		}
	case "s":
		if m.mode == "build" {
			m.saveCrew()
		}
	}
	return m, nil
}

func (m *model) startRun() (tea.Model, tea.Cmd) {
	if m.running {
		return m, nil
	}
	data, err := yaml.Marshal(m.crew)
	if err != nil {
		return m.logf("marshal: %v", err), nil
	}
	fresh, err := schema.Load(data)
	if err != nil {
		return m.logf("invalid crew, not started: %v", err), nil
	}
	m.run = runner.New(fresh, m.opts)
	m.monitor, m.monitorNode = "", ""
	m.running = true
	prog := m.prog
	m.opts.OnEvent = func(ev runner.Event) {
		prog.Send(logMsg{s: fmt.Sprintf("[%s] %s %s", ev.Type, ev.Node, ev.Info)})
	}
	m.run.Opts.OnEvent = m.opts.OnEvent
	run := m.run
	go func() {
		if err := run.Run(context.Background()); err != nil {
			prog.Send(logMsg{s: "run error: " + err.Error()})
		}
		// P13-B8: completion mutates model state only via a message — the
		// goroutine used to write m.running unsynchronized
		prog.Send(runDoneMsg{summary: run.Summary()})
	}()
	return m.logf("run started (%d agents)", len(fresh.Agents)), m.monitorTick()
}

func (m *model) logf(format string, a ...any) *model {
	m.log = append(m.log, fmt.Sprintf(format, a...))
	return m
}

// ---- builder ----

func (m *model) formKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	f := m.form
	ti := &f.fields[f.step].input
	switch msg.String() {
	case "esc":
		m.form = nil
		return m, nil
	case "ctrl+c": // P13-B17: ctrl+c was swallowed while a form was open
		return m, tea.Quit
	case "shift+tab": // P13-B17: back one field (tab only moved forward)
		if f.step > 0 {
			f.step--
			f.fields[f.step].input.Focus()
			return m, nil
		}
		return m, nil
	case "enter", "tab":
		if f.step+1 < len(f.fields) {
			f.step++
			f.fields[f.step].input.Focus()
			return m, nil
		}
		vals := make([]string, len(f.fields))
		for i := range f.fields {
			vals[i] = f.fields[i].input.Value()
		}
		switch f.kind {
		case "agent":
			a := schema.Agent{Name: vals[0], Kind: "pi", Prompt: vals[2]}
			if vals[1] != "" {
				a.Model = vals[1]
			}
			if len(vals) > 3 && vals[3] != "" {
				a.Route = vals[3]
			}
			m.crew.Agents = append(m.crew.Agents, a)
			m.buildMsg = "agent " + vals[0] + " added"
		case "edge":
			m.crew.Edges = append(m.crew.Edges, schema.Edge{From: vals[0], To: vals[1], When: vals[2]})
			m.buildMsg = fmt.Sprintf("edge %s→%s added (when=%s)", vals[0], vals[1], vals[2])
		case "route":
			conf := 0.5
			if strings.TrimSpace(vals[4]) != "" {
				if n, err := fmt.Sscanf(vals[4], "%g", &conf); n != 1 || err != nil { // P13-B18: no silent garbage
					m.buildMsg = "route confidence must be a number 0–1 (got " + vals[4] + ")"
					m.form = nil
					return m, nil
				}
			}
			rt := schema.Route{ID: vals[0], Efficient: vals[1], Capable: vals[2],
				Picker: vals[3], Confidence: conf}
			if rt.Picker == "" {
				rt.Picker = "efficient_first"
			}
			m.crew.Routes = append(m.crew.Routes, rt)
			m.buildMsg = fmt.Sprintf("route %s added (attach agents with route: %s)", vals[0], vals[0])
		case "phase":
			// P13-B18: validate on submit — name rule, members exist,
			// not already phased, no duplicate member (saveCrew also
			// round-trips, but the user needs the pointer at submit time)
			name := strings.TrimSpace(vals[0])
			if !schema.ValidName(name) {
				m.buildMsg = "phase name must match [a-z][a-z0-9_-]{0,31} (got " + vals[0] + ")"
				m.form = nil
				return m, nil
			}
			if strings.TrimSpace(vals[1]) == "" {
				m.buildMsg = "phase " + name + ": instructions required"
				m.form = nil
				return m, nil
			}
			agents := []string{}
			seenMem := map[string]bool{}
			for _, s := range strings.Split(vals[2], ",") {
				if s = strings.TrimSpace(s); s != "" {
					if seenMem[s] {
						m.buildMsg = "phase " + name + ": member " + s + " listed twice"
						m.form = nil
						return m, nil
					}
					seenMem[s] = true
					agents = append(agents, s)
				}
			}
			if len(agents) == 0 {
				m.buildMsg = "phase " + name + ": at least one member agent required"
				m.form = nil
				return m, nil
			}
			for _, s := range agents {
				if m.crew.Agent(s) == nil {
					m.buildMsg = "phase " + name + ": unknown agent " + s
					m.form = nil
					return m, nil
				}
				for _, p := range m.crew.Phases {
					for _, existing := range p.Agents {
						if existing == s {
							m.buildMsg = "agent " + s + " is already in phase " + p.Name
							m.form = nil
							return m, nil
						}
					}
				}
			}
			m.crew.Phases = append(m.crew.Phases, schema.Phase{
				Name: name, Instructions: vals[1], Agents: agents,
			})
			m.buildMsg = fmt.Sprintf("phase %s added (%s) — order = list order", name, strings.Join(agents, ", "))
		case "tool":
			msg := m.saveTool(vals)
			m.buildMsg = msg
		}
		m.form = nil
		return m, nil
	}
	var cmd tea.Cmd
	f.fields[f.step].input, cmd = ti.Update(msg)
	return m, cmd
}

func (m *model) startAgentForm() {
	ti := textinput.New()
	ti.Placeholder = "planner"
	ti.Focus()
	ti2 := textinput.New()
	ti2.Placeholder = "halogen/halogen-qwen3.8-flash-next (or leave blank for route)"
	ti3 := textinput.New()
	ti3.Placeholder = "Plan the task. Output a numbered plan."
	ti4 := textinput.New()
	ti4.Placeholder = "route id (blank = use model)"
	m.form = &form{kind: "agent", fields: []formField{
		{label: "name", input: ti},
		{label: "model", input: ti2},
		{label: "prompt", input: ti3},
		{label: "route", input: ti4},
	}}
}

// startPhaseForm (P11): an ordered work stage with instructions. Agents are a
// comma-separated member list; saveCrew's schema.Load round-trip validates
// membership, name rules, and backward edges.
func (m *model) startPhaseForm() {
	names := []string{}
	for _, a := range m.crew.Agents {
		names = append(names, a.Name)
	}
	ti := textinput.New()
	ti.Placeholder = "research"
	ti.Focus()
	ti2 := textinput.New()
	ti2.Placeholder = "Read-only exploration. No edits. Record findings for the build phase."
	ti3 := textinput.New()
	ti3.Placeholder = "members: " + strings.Join(names, ", ")
	m.form = &form{kind: "phase", fields: []formField{
		{label: "name", input: ti},
		{label: "instructions", input: ti2},
		{label: "agents", input: ti3},
	}}
}

func (m *model) startEdgeForm() {
	names := []string{}
	for _, a := range m.crew.Agents {
		names = append(names, a.Name)
	}
	ti := textinput.New()
	ti.Placeholder = "from (" + strings.Join(names, "|") + ")"
	ti.Focus()
	ti2 := textinput.New()
	ti2.Placeholder = "to (" + strings.Join(names, "|") + ")"
	ti3 := textinput.New()
	ti3.Placeholder = "when: idle | re:PATTERN"
	ti3.SetValue("idle")
	m.form = &form{kind: "edge", fields: []formField{
		{label: "from", input: ti}, {label: "to", input: ti2}, {label: "when", input: ti3},
	}}
}

func (m *model) startRouteForm() {
	ti := textinput.New()
	ti.Placeholder = "auto-pool"
	ti.Focus()
	ti2 := textinput.New()
	ti2.Placeholder = "efficient: halogen/halogen-qwen3.8-flash-next"
	ti3 := textinput.New()
	ti3.Placeholder = "capable: opencode-go/glm-5.3-flash"
	ti4 := textinput.New()
	ti4.Placeholder = "picker: efficient_first | capable_first"
	ti5 := textinput.New()
	ti5.Placeholder = "confidence (0-1, default 0.5)"
	m.form = &form{kind: "route", fields: []formField{
		{label: "id", input: ti},
		{label: "efficient", input: ti2},
		{label: "capable", input: ti3},
		{label: "picker", input: ti4},
		{label: "confidence", input: ti5},
	}}
}

// startToolForm (P9-4): the declarative tool spec authoring form.
// params syntax: "name:type:description" separated by ';' — one line for the
// whole properties table.
// probe syntax: "expect_contains" (exit 0 assumed; blank = probe-free DRAFT).
func (m *model) startToolForm() {
	ti := textinput.New()
	ti.Placeholder = "current_time_utc"
	ti.Focus()
	ti2 := textinput.New()
	ti2.Placeholder = "what the model reads in the tool list"
	ti3 := textinput.New()
	ti3.Placeholder = "shell | http"
	ti4 := textinput.New()
	ti4.Placeholder = "shell: run template · http: url"
	ti5 := textinput.New()
	ti5.Placeholder = "params: repo:string:the repo path;depth:number:walk depth"
	ti6 := textinput.New()
	ti6.Placeholder = "probe expect_contains (blank = draft)"
	m.form = &form{kind: "tool", fields: []formField{
		{label: "name", input: ti},
		{label: "description", input: ti2},
		{label: "executor type", input: ti3},
		{label: "run / url", input: ti4},
		{label: "params", input: ti5},
		{label: "probe", input: ti6},
	}}
}

// toolParamSpec parses "name:type:description;..." into properties.
func toolParamSpec(s string) map[string]any {
	props := map[string]any{}
	for _, part := range strings.Split(s, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		bits := strings.SplitN(part, ":", 3)
		name := strings.TrimSpace(bits[0])
		if name == "" {
			continue
		}
		typ := "string"
		desc := ""
		if len(bits) > 1 && strings.TrimSpace(bits[1]) != "" {
			typ = strings.TrimSpace(bits[1])
		}
		if len(bits) > 2 {
			desc = strings.TrimSpace(bits[2])
		}
		props[name] = map[string]any{"type": typ, "description": desc}
	}
	return props
}

// probeToolsCmd (P13-B11) runs the tools validate subprocess OFF the event
// loop — it used to block Update for the sum of every probe's timeout.
func (m *model) probeToolsCmd() tea.Cmd {
	bin, dir := toolsBin(), toolsDir()
	return func() tea.Msg {
		if _, err := os.Stat(dir); err != nil {
			return toolsProbeMsg{msg: "no tools dir: " + dir}
		}
		out, err := exec.Command(bin, "validate").CombinedOutput()
		msg := strings.TrimSpace(string(out))
		if err != nil {
			return toolsProbeMsg{msg: "probe FAIL: " + msg}
		}
		return toolsProbeMsg{msg: msg}
	}
}

func toolsDir() string {
	if d := os.Getenv("FORSETI_TOOLS_DIR"); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "forseti", "tools.d")
}

func toolsBin() string {
	if b := os.Getenv("FORSETI_TOOLS_BIN"); b != "" {
		return b
	}
	// prefer the sibling binary next to the running forseti-crew
	if exe, err := os.Executable(); err == nil {
		sib := filepath.Join(filepath.Dir(exe), "forseti-tools")
		if _, err := os.Stat(sib); err == nil {
			return sib
		}
	}
	return "forseti-tools"
}

// saveTool writes a tool spec from the form (P9-4): validate-on-save, probe
// when declared; the file lands in FORSETI_TOOLS_DIR (default tools.d).
func (m *model) saveTool(vals []string) string {
	name, desc, execType, runURL, params, probeContains := vals[0], vals[1], vals[2], vals[3], vals[4], vals[5]
	spec := map[string]any{
		"name":        name,
		"description": desc,
		"parameters": map[string]any{
			"type":       "object",
			"properties": toolParamSpec(params),
		},
		"executor": func() map[string]any {
			if execType == "http" {
				return map[string]any{"type": "http", "url": runURL}
			}
			return map[string]any{"type": "shell", "run": runURL}
		}(),
	}
	if strings.TrimSpace(probeContains) != "" {
		spec["probe"] = map[string]any{"args": map[string]any{}, "expect_exit": 0, "expect_contains": probeContains}
	}
	out, err := yaml.Marshal(spec)
	if err != nil {
		return "tool marshal: " + err.Error()
	}
	// validate by round-tripping through the toolspec loader
	if _, err := toolspec.Load(out); err != nil {
		return "invalid, not saved: " + err.Error()
	}
	path := filepath.Join(toolsDir(), name+".yaml")
	if err := os.WriteFile(path, out, 0o644); err != nil {
		return "save: " + err.Error()
	}
	return "saved " + path + " (pi picks it up on its next session; probe with P)"
}

func (m *model) adoptLive() {
	if m.client == nil {
		m.buildMsg = "no herdr connection"
		return
	}
	agents, err := m.client.AgentList()
	if err != nil {
		m.buildMsg = "adopt: " + err.Error()
		return
	}
	added := 0
	for _, a := range agents {
		if m.crew.Agent(a.Name) != nil {
			continue
		}
		m.crew.Agents = append(m.crew.Agents, schema.Agent{
			Name: a.Name, Kind: "pi",
			Prompt: "(adopted from live agent — set a prompt)",
		})
		added++
	}
	m.buildMsg = fmt.Sprintf("adopted %d live agent(s)", added)
}

func (m *model) saveCrew() {
	data, err := yaml.Marshal(m.crew)
	if err != nil {
		m.buildMsg = "save: " + err.Error()
		return
	}
	if _, err := schema.Load(data); err != nil {
		m.buildMsg = "invalid, not saved: " + err.Error()
		return
	}
	// P13-B2: save goes where -f loaded the crew from — the hardcoded
	// "crew.yaml" used to overwrite the wrong file (root default) while the
	// user believed they were editing the -f crew.
	path := m.path
	if path == "" {
		path = "crew.yaml"
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		m.buildMsg = "save: " + err.Error()
		return
	}
	m.buildMsg = "saved " + path
}

// ---- view ----

var statusGlyph = map[string]string{
	"pending": "○", "running": "●", "done": "✓",
	"blocked": "!", "failed": "✗", "skipped": "–",
}

// displayOrder (P13-B16): agent indices in DISPLAY order — phase-grouped
// when phases exist, else declaration order. j/k and the ▶ marker both use
// it (the marker used to jump non-linearly when declaration ≠ phase order).
func (m *model) displayOrder() []int {
	if len(m.crew.Phases) == 0 {
		out := make([]int, len(m.crew.Agents))
		for i := range m.crew.Agents {
			out[i] = i
		}
		return out
	}
	var out []int
	seen := map[int]bool{}
	for pi := range m.crew.Phases {
		for _, name := range m.crew.Phases[pi].Agents {
			for i, a := range m.crew.Agents {
				if a.Name == name && !seen[i] { // renders each agent ONCE
					seen[i] = true
					out = append(out, i)
				}
			}
		}
	}
	for i := range m.crew.Agents { // stragglers (validated away, render defensively)
		if !seen[i] {
			out = append(out, i)
		}
	}
	return out
}

// agentRow renders one node row in the left pane (status glyph, name, model).
func (m *model) agentRow(b *strings.Builder, i int, selectedName string) {
	a := m.crew.Agents[i]
	st := "pending"
	live := ""
	cost := ""
	if m.run != nil {
		if ns, ok := m.run.NodeSnapshot(a.Name); ok { // P13-B8: value copy
			st = ns.Status
			live = ns.LiveStatus
			if ns.CostUSD > 0 {
				cost = fmt.Sprintf(" $%.4f", ns.CostUSD)
			}
		}
	}
	resolved := a.Model
	if a.Route != "" {
		resolved = "⇄" + a.Route
	}
	liveTag := ""
	if live != "" && live != st {
		liveTag = " (" + live + ")"
	}
	line := fmt.Sprintf("%s %s %s%s%s", statusGlyph[st], a.Name, resolved, cost, liveTag)
	if a.Name == selectedName {
		b.WriteString("▶ " + line + "\n")
	} else {
		b.WriteString("  " + line + "\n")
	}
}

func (m *model) View() string {
	if m.width == 0 {
		return "loading…"
	}
	leftW := 34
	rightW := m.width - leftW - 3

	var left strings.Builder
	left.WriteString("CREW: " + m.crew.Name + " [" + m.mode + "]\n")
	if m.mode == "view" {
		// P13-B16: rows render in display order; the ▶ marker follows the
		// same order j/k walks (phase-grouped when phases exist).
		selected := m.selectedName()
		order := m.displayOrder()
		if len(m.crew.Phases) > 0 {
			// P11: phased crews group nodes under ordered phase headers —
			// headers emit at phase boundaries along the display order
			last := -99
			for _, i := range order {
				if pi := m.crew.PhaseOf(m.crew.Agents[i].Name); pi != last {
					if pi >= 0 {
						left.WriteString(fmt.Sprintf("— phase %d %s\n", pi+1, m.crew.Phases[pi].Name))
					}
					last = pi
				}
				m.agentRow(&left, i, selected)
			}
		} else {
			for _, i := range order {
				m.agentRow(&left, i, selected)
			}
		}
		for _, e := range m.crew.Edges {
			left.WriteString(fmt.Sprintf("    %s→%s\n", e.From, e.To))
		}
		left.WriteString("\nj/k select · r run · f focus pane · tab meta · b build · q quit")
	} else if m.form != nil {
		f := m.form
		left.WriteString(fmt.Sprintf("\nadd %s — field %d/%d (enter next · esc cancel)\n\n", f.kind, f.step+1, len(f.fields)))
		left.WriteString("  " + f.fields[f.step].label + ":\n  " + f.fields[f.step].input.View() + "\n")
	} else {
		left.WriteString("\nagents:\n")
		for _, a := range m.crew.Agents {
			resolved := a.Model
			if a.Route != "" {
				resolved = "⇄" + a.Route
			}
			left.WriteString("  · " + a.Name + " (" + resolved + ")\n")
		}
		if len(m.crew.Phases) > 0 { // P11
			left.WriteString("phases:\n")
			for i, p := range m.crew.Phases {
				left.WriteString(fmt.Sprintf("  · %d %s (%s)\n", i+1, p.Name, strings.Join(p.Agents, ", ")))
			}
		}
		left.WriteString("edges:\n")
		for _, e := range m.crew.Edges {
			left.WriteString(fmt.Sprintf("  · %s→%s when=%s\n", e.From, e.To, e.When))
		}
		for _, rt := range m.crew.Routes {
			left.WriteString(fmt.Sprintf("  route · %s: efficient=%s capable=%s\n", rt.ID, rt.Efficient, rt.Capable))
		}
		left.WriteString("\na=add agent · e=add edge · R=add route · p=add phase · A=adopt live · s=save · q=back")
	}
	if m.buildMsg != "" {
		left.WriteString("\n" + m.buildMsg)
	}

	// right column: monitor or meta
	var mon string
	if m.metaTab {
		mon = m.metaView()
	} else if m.monitorNode != "" {
		mon = "MONITOR: " + m.monitorNode + "\n" + m.monitor
	} else {
		mon = "MONITOR: (select a running node to tail)"
	}

	// P13-B4: clamp the pane height — a 1-row terminal made m.height-2
	// negative and the tail slice panicked the whole TUI.
	viewH := m.height - 2
	if viewH < 1 {
		viewH = 1
	}
	// P13-B19: BOTH panes clip to the last viewH lines (only the right one
	// used to, so the selected row + help could scroll off-screen).
	clip := func(ls []string) []string {
		if len(ls) > viewH {
			return ls[len(ls)-viewH:]
		}
		return ls
	}
	leftLines := clip(padLines(left.String(), viewH))
	rightLines := clip(padLines(mon, viewH))

	var out strings.Builder
	for i := range leftLines {
		lc := truncate(pad(leftLines[i], leftW), leftW)
		rc := ""
		if i < len(rightLines) {
			rc = truncate(rightLines[i], rightW)
		}
		out.WriteString(lc + " │ " + rc + "\n")
	}
	ev := ""
	if len(m.log) > 0 {
		ev = m.log[len(m.log)-1]
	}
	out.WriteString(truncate("» "+ev, m.width))
	return out.String()
}

// pad pads to a DISPLAY width (P13-B5: byte length zigzagged the separator
// column — every row carries a multi-byte glyph like ● or ✓).
func pad(s string, w int) string {
	if d := runewidth.StringWidth(s); d >= w {
		return s
	} else {
		return s + strings.Repeat(" ", w-d)
	}
}

func padLines(s string, n int) []string {
	ls := strings.Split(s, "\n")
	for len(ls) < n {
		ls = append(ls, "")
	}
	return ls
}

// truncate cuts at a display-width boundary (P13-B5: s[:w] could split a
// multi-byte rune mid-sequence — mojibake at the pane edge).
func truncate(s string, w int) string {
	if w <= 0 {
		return ""
	}
	return runewidth.Truncate(s, w, "")
}
