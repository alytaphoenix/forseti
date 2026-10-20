package main

// A minimal typed view of the herdr socket-API CLI (NDJSON/JSON on stdout).
// `herdr <group> <verb>` prints one envelope {"id":…,"result":{…}}; server
// errors go to stderr as {"error":{code,message}} with exit 1. Every helper
// here parses the result object and never predicts an ID.

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
)

type herdrTab struct {
	TabID       string `json:"tab_id"`
	Label       string `json:"label"`
	Number      int    `json:"number"`
	WorkspaceID string `json:"workspace_id"`
	PaneCount   int    `json:"pane_count"`
	Focused     bool   `json:"focused"`
	AgentStatus string `json:"agent_status"`
}

type herdrPane struct {
	PaneID        string `json:"pane_id"`
	TabID         string `json:"tab_id"`
	WorkspaceID   string `json:"workspace_id"`
	Cwd           string `json:"cwd"`
	ForegroundCwd string `json:"foreground_cwd"`
	Label         string `json:"label"`
	RestoreError  string `json:"restore_error"`
	AgentStatus   string `json:"agent_status"`
	Agent         string `json:"agent"`
}

type herdrAgent struct {
	Name        string `json:"name"`
	PaneID      string `json:"pane_id"`
	TabID       string `json:"tab_id"`
	Agent       string `json:"agent"`
	AgentStatus string `json:"agent_status"`
	Cwd         string `json:"cwd"`
}

type herdrProc struct {
	Argv0   string   `json:"argv0"`
	Name    string   `json:"name"`
	Cmdline string   `json:"cmdline"`
	Cwd     string   `json:"cwd"`
	Argv    []string `json:"argv"`
}

// herdrResult runs one herdr command and returns its result object.
func herdrResult(c Config, args ...string) (map[string]any, error) {
	out, err := runCapture("", nil, c.Herdr.Bin, args...)
	if err != nil {
		return nil, err
	}
	line := lastJSONLine(out)
	if line == "" {
		return nil, fmt.Errorf("herdr %s: no JSON in output", strings.Join(args, " "))
	}
	var env struct {
		Result map[string]any `json:"result"`
		Error  *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(line), &env); err != nil {
		return nil, fmt.Errorf("herdr %s: %w", strings.Join(args, " "), err)
	}
	if env.Error != nil {
		return nil, fmt.Errorf("herdr %s: %s (%s)", strings.Join(args, " "), env.Error.Message, env.Error.Code)
	}
	if env.Result == nil {
		return map[string]any{}, nil
	}
	return env.Result, nil
}

// herdrList decodes result.<field> into []T.
func herdrList[T any](c Config, field string, args ...string) ([]T, error) {
	res, err := herdrResult(c, args...)
	if err != nil {
		return nil, err
	}
	raw, ok := res[field]
	if !ok {
		return nil, fmt.Errorf("herdr %s: result has no %q", strings.Join(args, " "), field)
	}
	data, err := json.Marshal(raw)
	if err != nil {
		return nil, err
	}
	var out []T
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("herdr %s: %w", strings.Join(args, " "), err)
	}
	return out, nil
}

// herdrExec runs a command whose success is only its exit status (`pane run`
// prints nothing on success, so it has no JSON envelope to parse).
func herdrExec(c Config, args ...string) error {
	if _, err := runCapture("", nil, c.Herdr.Bin, args...); err != nil {
		return err
	}
	return nil
}

func herdrTabs(c Config) ([]herdrTab, error) { return herdrList[herdrTab](c, "tabs", "tab", "list") }
func herdrPanes(c Config) ([]herdrPane, error) {
	return herdrList[herdrPane](c, "panes", "pane", "list")
}
func herdrAgents(c Config) ([]herdrAgent, error) {
	return herdrList[herdrAgent](c, "agents", "agent", "list")
}

func herdrProcs(c Config, pane string) ([]herdrProc, error) {
	res, err := herdrResult(c, "pane", "process-info", "--pane", pane)
	if err != nil {
		return nil, err
	}
	obj, _ := res["process_info"].(map[string]any)
	return decodeProcs(obj["foreground_processes"])
}

func decodeProcs(raw any) ([]herdrProc, error) {
	if raw == nil {
		return nil, nil
	}
	data, err := json.Marshal(raw)
	if err != nil {
		return nil, err
	}
	var procs []herdrProc
	if err := json.Unmarshal(data, &procs); err != nil {
		return nil, err
	}
	return procs, nil
}

// jsonObj pulls one nested object out of a result.
func jsonObj(res map[string]any, key string) map[string]any {
	obj, _ := res[key].(map[string]any)
	return obj
}

func jsonStr(obj map[string]any, key string) string {
	s, _ := obj[key].(string)
	return s
}

var shellNames = map[string]bool{
	"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true,
	"fish": true, "tcsh": true, "csh": true, "nu": true, "xonsh": true,
}

// procBase is argv0 without a path or a login-shell "-" prefix.
func procBase(p herdrProc) string {
	name := p.Argv0
	if name == "" {
		name = p.Name
	}
	name = strings.TrimPrefix(name, "-")
	return strings.ToLower(filepath.Base(name))
}

// paneAtShellPrompt reports whether the pane is a plain shell waiting for input
// (the state a restored, formerly-TUI pane comes back in).
func paneAtShellPrompt(procs []herdrProc) bool {
	if len(procs) == 0 {
		return false
	}
	for _, p := range procs {
		if !shellNames[procBase(p)] {
			return false
		}
	}
	return true
}

// paneRuns reports whether any foreground process is the given binary.
func paneRuns(procs []herdrProc, bin string) bool {
	want := strings.ToLower(filepath.Base(bin))
	for _, p := range procs {
		if procBase(p) == want {
			return true
		}
	}
	return false
}
