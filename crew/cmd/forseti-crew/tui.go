// TUI shell: left = graph list / builder, right = live monitor of the selected
// node, bottom = event log. Monitor is read-only (no nested-terminal v1);
// per S7 there is no output push surface, so the tail is a debounced agent.read
// (700 ms) while the selected node is running.
package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"gopkg.in/yaml.v3"

	"forseti/crew/internal/herdrd"
	"forseti/crew/internal/runner"
	"forseti/crew/internal/schema"
)

type logMsg struct{ s string }
type monitorTick struct{}

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

	selected    int
	log         []string
	mode        string // "view" | "build"
	form        *form
	buildMsg  string
	monitor   string
	monitorNode string
	width, height int
	running   bool
}

func runTUI(crew *schema.Crew, opts runner.Options) error {
	m := &model{crew: crew, opts: opts, mode: "view"}
	p := tea.NewProgram(m, tea.WithAltScreen())
	m.prog = p
	_, err := p.Run()
	return err
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
	case tea.KeyMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

func (m *model) updateMonitor() (tea.Model, tea.Cmd) {
	if m.client != nil && len(m.crew.Agents) > 0 && m.run != nil {
		name := m.crew.Agents[m.selected%len(m.crew.Agents)].Name
		if st := m.run.Nodes[name]; st != nil {
			switch {
			case st.Status == "running":
				if txt, err := m.client.AgentRead(name, "recent_unwrapped", 200); err == nil {
					m.monitor, m.monitorNode = txt, name
				}
			case m.monitorNode != name && st.Output != "":
				m.monitor, m.monitorNode = st.Output, name
			}
		}
	}
	return m, m.monitorTick()
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
		if len(m.crew.Agents) > 0 {
			m.selected = (m.selected + 1) % len(m.crew.Agents)
			m.monitorNode = ""
		}
	case "k", "up":
		if len(m.crew.Agents) > 0 {
			m.selected = (m.selected - 1 + len(m.crew.Agents)) % len(m.crew.Agents)
			m.monitorNode = ""
		}
	case "r":
		return m.startRun()
	case "f":
		// focus the REAL herdr pane of the selected node (human-in-the-loop:
		// takeover happens in the actual pane, never inside the monitor)
		if m.client != nil && len(m.crew.Agents) > 0 {
			name := m.crew.Agents[m.selected%len(m.crew.Agents)].Name
			if err := m.client.AgentFocus(name); err != nil {
				return m.logf("focus %s: %v", name, err), nil
			}
			return m.logf("focused real pane: %s", name), nil
		}
	case "b":
		m.mode = "build"
		m.buildMsg = "a=add agent · e=add edge · A=adopt live · s=save · q=back"
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
	go func() {
		err := m.run.Run(context.Background())
		if err != nil {
			prog.Send(logMsg{s: "run error: " + err.Error()})
		}
		prog.Send(logMsg{s: "run finished: " + m.run.Summary()})
		m.running = false
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
			m.crew.Agents = append(m.crew.Agents, schema.Agent{
				Name: vals[0], Model: vals[1], Kind: "pi", Prompt: vals[2],
			})
			m.buildMsg = "agent " + vals[0] + " added"
		case "edge":
			m.crew.Edges = append(m.crew.Edges, schema.Edge{From: vals[0], To: vals[1], When: vals[2]})
			m.buildMsg = fmt.Sprintf("edge %s→%s added (when=%s)", vals[0], vals[1], vals[2])
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
	ti2.Placeholder = "opencode-go/glm-5.3-flash"
	ti3 := textinput.New()
	ti3.Placeholder = "Plan the task. Output a numbered plan."
	m.form = &form{kind: "agent", fields: []formField{
		{label: "name", input: ti},
		{label: "model", input: ti2},
		{label: "prompt", input: ti3},
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
	if err := os.WriteFile("crew.yaml", data, 0o644); err != nil {
		m.buildMsg = "save: " + err.Error()
		return
	}
	m.buildMsg = "saved crew.yaml"
}

// ---- view ----

var statusGlyph = map[string]string{
	"pending": "○", "running": "●", "done": "✓",
	"blocked": "!", "failed": "✗", "skipped": "–",
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
		for i, a := range m.crew.Agents {
			st := "pending"
			if m.run != nil {
				st = m.run.Nodes[a.Name].Status
			}
			line := fmt.Sprintf("%s %s %s", statusGlyph[st], a.Name, a.Model)
			if i == m.selected%max(1, len(m.crew.Agents)) {
				left.WriteString("▶ " + line + "\n")
			} else {
				left.WriteString("  " + line + "\n")
			}
		}
		for _, e := range m.crew.Edges {
			left.WriteString(fmt.Sprintf("    %s→%s\n", e.From, e.To))
		}
		left.WriteString("\nj/k select · r run · f focus pane · b build · q quit")
	} else if m.form != nil {
		f := m.form
		left.WriteString(fmt.Sprintf("\nadd %s — field %d/%d (enter next · esc cancel)\n\n", f.kind, f.step+1, len(f.fields)))
		left.WriteString("  " + f.fields[f.step].label + ":\n  " + f.fields[f.step].input.View() + "\n")
	} else {
		left.WriteString("\nagents:\n")
		for _, a := range m.crew.Agents {
			left.WriteString("  · " + a.Name + " (" + a.Model + ")\n")
		}
		left.WriteString("edges:\n")
		for _, e := range m.crew.Edges {
			left.WriteString(fmt.Sprintf("  · %s→%s when=%s\n", e.From, e.To, e.When))
		}
		left.WriteString("\na=add agent · e=add edge · A=adopt live · s=save · q=back")
	}
	if m.buildMsg != "" {
		left.WriteString("\n" + m.buildMsg)
	}

	// right column: monitor
	mon := m.monitor
	if m.monitorNode != "" {
		mon = "MONITOR: " + m.monitorNode + "\n" + mon
	} else {
		mon = "MONITOR: (select a running node to tail)"
	}

	leftLines := padLines(left.String(), m.height-2)
	rightLines := padLines(mon, m.height-2)
	if len(rightLines) > m.height-2 {
		rightLines = rightLines[len(rightLines)-(m.height-2):]
	}

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

func pad(s string, w int) string {
	if len(s) >= w {
		return s
	}
	return s + strings.Repeat(" ", w-len(s))
}

func padLines(s string, n int) []string {
	ls := strings.Split(s, "\n")
	for len(ls) < n {
		ls = append(ls, "")
	}
	return ls
}

func truncate(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if len(s) <= w {
		return s
	}
	return s[:w]
}
