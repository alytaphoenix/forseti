// watch: tail-render a run's JSONL log into a live status table (6B-1).
// No herdr dependency — works on headless runs, post-hoc, and while following.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"forseti/crew/internal/runner"
)

type watchTick struct{}

type watchModel struct {
	path          string
	follow        bool
	crewName      string
	nodes         map[string]*runner.Event // node → last significant event
	status        map[string]string
	started       map[string]time.Time
	ended         map[string]time.Time
	cost          map[string]float64
	phase         map[string]string // P11: node → phase name (from node_start)
	phaseOrder    []string          // P11: first-seen phase order
	checks        []string          // rendered check results
	summary       string
	quitFlag      bool
	lastTS        time.Time
	width, height int
	err           string
}

func cmdWatch(args []string) {
	fs := flag.NewFlagSet("watch", flag.ExitOnError)
	follow := fs.Bool("follow", true, "keep tailing the run file")
	path := fs.String("f", "", "run JSONL (default: newest in .forseti/runs)")
	_ = fs.Parse(args)
	p := *path
	if p == "" {
		cwd, _ := os.Getwd()
		matches, err := filepath.Glob(filepath.Join(cwd, ".forseti", "runs", "*.jsonl"))
		if err != nil || len(matches) == 0 {
			fmt.Fprintln(os.Stderr, "no run logs in .forseti/runs")
			os.Exit(2)
		}
		sort.Strings(matches)
		p = matches[len(matches)-1]
	}
	m := &watchModel{path: p, follow: *follow, nodes: map[string]*runner.Event{},
		status: map[string]string{}, started: map[string]time.Time{},
		ended: map[string]time.Time{}, cost: map[string]float64{},
		phase: map[string]string{}}
	m.replay()
	prog := tea.NewProgram(m, tea.WithAltScreen())
	if _, err := prog.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "watch:", err)
		os.Exit(1)
	}
	if strings.Contains(m.summary, "failed") || strings.Contains(m.summary, "checks_failed") {
		os.Exit(1)
	}
}

// replay reads the whole file and folds events into state.
func (m *watchModel) replay() {
	f, err := os.Open(m.path)
	if err != nil {
		m.err = err.Error()
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	for sc.Scan() {
		var ev runner.Event
		if json.Unmarshal(sc.Bytes(), &ev) != nil {
			continue
		}
		m.fold(ev)
	}
}

func (m *watchModel) fold(ev runner.Event) {
	m.lastTS = ev.TS
	switch ev.Type {
	case "run_start":
		if i := strings.Index(ev.Info, "crew="); i >= 0 {
			rest := ev.Info[i+5:]
			if sp := strings.Index(rest, " "); sp >= 0 {
				m.crewName = rest[:sp]
			}
		}
	case "node_start":
		m.status[ev.Node] = "running"
		m.started[ev.Node] = ev.TS
		if ev.Phase != "" { // P11: group rows under phase headers
			if _, seen := m.phase[ev.Node]; !seen {
				m.phase[ev.Node] = ev.Phase
			}
			if !contains(m.phaseOrder, ev.Phase) {
				m.phaseOrder = append(m.phaseOrder, ev.Phase)
			}
		}
		delete(m.ended, ev.Node)
	case "node_done":
		m.status[ev.Node] = "done"
		m.ended[ev.Node] = ev.TS
		if ev.CostUSD > 0 {
			m.cost[ev.Node] = ev.CostUSD
		}
	case "node_retry":
		// keep running status
	case "node_blocked":
		m.status[ev.Node] = "blocked"
		m.ended[ev.Node] = ev.TS
	case "node_failed":
		m.status[ev.Node] = "failed"
		m.ended[ev.Node] = ev.TS
	case "check_pass", "check_fail":
		mark := "✓"
		if ev.Type == "check_fail" {
			mark = "✗"
		}
		m.checks = append(m.checks, fmt.Sprintf("%s %s %s", mark, ev.Node, ev.Info))
	case "run_end":
		m.summary = ev.Info
		if !m.follow {
			m.quitFlag = true
		}
	}
}

func (m *watchModel) Init() tea.Cmd {
	return m.tick()
}

func (m *watchModel) tick() tea.Cmd {
	return tea.Tick(400*time.Millisecond, func(time.Time) tea.Msg { return watchTick{} })
}

func (m *watchModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil
	case watchTick:
		if m.follow {
			m.replay()
		}
		if m.quitFlag {
			return m, tea.Quit
		}
		return m, m.tick()
	case tea.KeyMsg:
		if s := msg.String(); s == "q" || s == "ctrl+c" {
			return m, tea.Quit
		}
	}
	return m, nil
}

func (m *watchModel) View() string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("RUN: %s (crew=%s)%s\n", filepath.Base(m.path), m.crewName, followTag(m.follow)))
	names := make([]string, 0, len(m.status))
	for n := range m.status {
		names = append(names, n)
	}
	sort.Strings(names)
	row := func(n string) {
		dur := ""
		if st, ok := m.started[n]; ok {
			end, ended := m.ended[n]
			if !ended {
				end = time.Now()
			}
			dur = end.Sub(st).Round(time.Second).String()
		}
		cost := ""
		if c := m.cost[n]; c > 0 {
			cost = fmt.Sprintf(" $%.4f", c)
		}
		b.WriteString(fmt.Sprintf(" %s %-14s %8s%s\n", statusGlyph[m.status[n]], n, dur, cost))
	}
	if len(m.phaseOrder) > 0 {
		// P11: phased run — rows grouped under phase headers
		for _, p := range m.phaseOrder {
			b.WriteString(fmt.Sprintf("phase %s\n", p))
			for _, n := range names {
				if m.phase[n] == p {
					row(n)
				}
			}
		}
	} else {
		for _, n := range names {
			row(n)
		}
	}
	if len(m.checks) > 0 {
		b.WriteString("\nchecks:\n")
		for _, c := range m.checks {
			b.WriteString("  " + c + "\n")
		}
	}
	if m.summary != "" {
		b.WriteString("\n" + m.summary + "\n")
	}
	if m.err != "" {
		b.WriteString("\nerror: " + m.err + "\n")
	}
	b.WriteString("\nq quit")
	return b.String()
}

func followTag(f bool) string {
	if f {
		return " [following]"
	}
	return ""
}

// contains is a tiny slice helper (P11 watch phase grouping).
func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
