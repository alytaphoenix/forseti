// Command herdrstub is a fake `herdr` CLI for the forseti CLI tests. It keeps a
// JSON world file ($STUB_WORLD) and answers the subset of commands the start
// reconciler uses, so tests can simulate create / split / run / agent-start /
// close / restore without a herdr server.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type tab struct {
	TabID       string `json:"tab_id"`
	Label       string `json:"label"`
	Number      int    `json:"number"`
	WorkspaceID string `json:"workspace_id"`
	PaneCount   int    `json:"pane_count"`
	Focused     bool   `json:"focused"`
	AgentStatus string `json:"agent_status"`
}

type pane struct {
	PaneID        string `json:"pane_id"`
	TabID         string `json:"tab_id"`
	WorkspaceID   string `json:"workspace_id"`
	Cwd           string `json:"cwd"`
	ForegroundCwd string `json:"foreground_cwd"`
	Label         string `json:"label"`
	RestoreError  string `json:"restore_error,omitempty"`
	AgentStatus   string `json:"agent_status"`
	Agent         string `json:"agent,omitempty"`
}

type agent struct {
	Name        string `json:"name"`
	PaneID      string `json:"pane_id"`
	TabID       string `json:"tab_id"`
	Agent       string `json:"agent"`
	AgentStatus string `json:"agent_status"`
	Cwd         string `json:"cwd"`
}

type proc struct {
	Argv0   string   `json:"argv0"`
	Name    string   `json:"name"`
	Cmdline string   `json:"cmdline"`
	Cwd     string   `json:"cwd"`
	Argv    []string `json:"argv,omitempty"`
}

type world struct {
	Tabs     []tab             `json:"tabs"`
	Panes    []pane            `json:"panes"`
	Agents   []agent           `json:"agents"`
	Procs    map[string][]proc `json:"procs"`
	Runs     []string          `json:"runs"`
	Closed   []string          `json:"closed"`
	NextTab  int               `json:"next_tab"`
	NextPane int               `json:"next_pane"`
	Down     bool              `json:"down"`
}

func main() {
	path := os.Getenv("STUB_WORLD")
	w := world{Procs: map[string][]proc{}, NextTab: 1, NextPane: 1}
	if data, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(data, &w); err != nil {
			fail("stub world: %v", err)
		}
	}
	if w.Procs == nil {
		w.Procs = map[string][]proc{}
	}
	args := os.Args[1:]
	if len(args) == 0 {
		fail("no args")
	}
	switch args[0] {
	case "status":
		if w.Down {
			fmt.Println("status: not running")
			os.Exit(1)
		}
		fmt.Println("status: running")
		return
	case "plugin":
		fmt.Println("forseti  local  " + os.Getenv("STUB_PLUGIN_ROOT"))
		return
	case "tab":
		tabCmd(&w, args[1:])
	case "pane":
		paneCmd(&w, args[1:])
	case "agent":
		agentCmd(&w, args[1:])
	default:
		fail("unknown command %q", args[0])
	}
	save(path, &w)
}

func tabCmd(w *world, args []string) {
	if len(args) == 0 {
		fail("tab: no verb")
	}
	switch args[0] {
	case "list":
		emit(map[string]any{"tabs": w.Tabs})
	case "focus":
		emit(map[string]any{"focused": args[1]})
	case "close":
		id := args[1]
		var kept []tab
		for _, t := range w.Tabs {
			if t.TabID != id {
				kept = append(kept, t)
			}
		}
		w.Tabs = kept
		var panes []pane
		for _, p := range w.Panes {
			if p.TabID != id {
				panes = append(panes, p)
			}
		}
		w.Panes = panes
		var agents []agent
		for _, a := range w.Agents {
			if a.TabID != id {
				agents = append(agents, a)
			}
		}
		w.Agents = agents
		w.Closed = append(w.Closed, id)
		emit(map[string]any{"tab_id": id})
	case "create":
		opts := flags(args[1:])
		cwd := opts["cwd"]
		label := opts["label"]
		id := fmt.Sprintf("w1:t%d", w.NextTab)
		w.NextTab++
		paneID := fmt.Sprintf("w1:p%d", w.NextPane)
		w.NextPane++
		w.Tabs = append(w.Tabs, tab{TabID: id, Label: label, Number: len(w.Tabs) + 1, WorkspaceID: "w1", PaneCount: 1})
		w.Panes = append(w.Panes, pane{PaneID: paneID, TabID: id, WorkspaceID: "w1", Cwd: cwd, ForegroundCwd: cwd, AgentStatus: "unknown"})
		w.Procs[paneID] = []proc{{Argv0: "-zsh", Name: "zsh", Cmdline: "-zsh", Cwd: cwd}}
		emit(map[string]any{"tab": map[string]any{"tab_id": id, "label": label}, "root_pane": map[string]any{"pane_id": paneID}})
	default:
		fail("tab: unknown verb %q", args[0])
	}
}

func paneCmd(w *world, args []string) {
	if len(args) == 0 {
		fail("pane: no verb")
	}
	switch args[0] {
	case "list":
		emit(map[string]any{"panes": w.Panes})
	case "process-info":
		id := flags(args[1:])["pane"]
		procs := w.Procs[id]
		if procs == nil {
			fail("pane %s: not found", id)
		}
		emit(map[string]any{"process_info": map[string]any{
			"pane_id":              id,
			"shell_pid":            1,
			"foreground_processes": procs,
		}})
	case "split":
		opts := flags(args[1:])
		from := opts["pane"]
		var src *pane
		for i := range w.Panes {
			if w.Panes[i].PaneID == from {
				src = &w.Panes[i]
			}
		}
		if src == nil {
			fail("pane split: no pane %s", from)
		}
		id := fmt.Sprintf("w1:p%d", w.NextPane)
		w.NextPane++
		w.Panes = append(w.Panes, pane{PaneID: id, TabID: src.TabID, WorkspaceID: src.WorkspaceID, Cwd: src.Cwd, ForegroundCwd: src.Cwd, AgentStatus: "unknown"})
		w.Procs[id] = []proc{{Argv0: "-zsh", Name: "zsh", Cmdline: "-zsh", Cwd: src.Cwd}}
		for i := range w.Tabs {
			if w.Tabs[i].TabID == src.TabID {
				w.Tabs[i].PaneCount++
			}
		}
		emit(map[string]any{"pane": map[string]any{"pane_id": id}})
	case "run":
		if len(args) < 3 {
			fail("pane run: usage PANE CMD")
		}
		id, cmd := args[1], strings.Join(args[2:], " ")
		if _, ok := w.Procs[id]; !ok {
			fail("pane run: no pane %s", id)
		}
		w.Runs = append(w.Runs, id+"\t"+cmd)
		fields := strings.Fields(cmd)
		argv0 := ""
		if len(fields) > 0 {
			argv0 = filepath.Base(strings.Trim(fields[0], "'\""))
		}
		w.Procs[id] = []proc{{Argv0: argv0, Name: argv0, Cmdline: cmd}}
		// real herdr prints nothing for `pane run` (success = exit status)
	default:
		fail("pane: unknown verb %q", args[0])
	}
}

func agentCmd(w *world, args []string) {
	if len(args) == 0 {
		fail("agent: no verb")
	}
	switch args[0] {
	case "list":
		emit(map[string]any{"agents": w.Agents})
	case "start":
		name := args[1]
		for _, a := range w.Agents {
			if a.Name == name {
				fail("agent %q already live", name)
			}
		}
		opts := flags(args[2:])
		paneID := opts["pane"]
		var tabID, cwd string
		for _, p := range w.Panes {
			if p.PaneID == paneID {
				tabID, cwd = p.TabID, p.Cwd
			}
		}
		if tabID == "" {
			fail("agent start: no pane %s", paneID)
		}
		a := agent{Name: name, PaneID: paneID, TabID: tabID, Agent: "pi", AgentStatus: "idle", Cwd: cwd}
		w.Agents = append(w.Agents, a)
		emit(map[string]any{"agent": a})
	default:
		fail("agent: unknown verb %q", args[0])
	}
}

// flags parses --key value / --key=value pairs (values may follow positionals).
func flags(args []string) map[string]string {
	out := map[string]string{}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") {
			continue
		}
		key := strings.TrimLeft(a, "-")
		if k, v, ok := strings.Cut(key, "="); ok {
			out[k] = v
			continue
		}
		if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
			out[key] = args[i+1]
			i++
		} else {
			out[key] = "true"
		}
	}
	return out
}

func emit(v any) {
	data, err := json.Marshal(map[string]any{"id": "stub", "result": v})
	if err != nil {
		fail("marshal: %v", err)
	}
	fmt.Println(string(data))
}

func save(path string, w *world) {
	data, err := json.MarshalIndent(w, "", "  ")
	if err != nil {
		fail("marshal world: %v", err)
	}
	tmp := path + "." + strconv.Itoa(os.Getpid()) + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		fail("write world: %v", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		fail("rename world: %v", err)
	}
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
