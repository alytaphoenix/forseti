package main

// `forseti down` and the herdr / output helpers shared with cmd_start.go.
// `forseti up` is an alias of `forseti start` (cmd_start.go).

import (
	"encoding/json"
	"fmt"
)

func cmdDown(args []string) {
	asJSON := false
	for _, a := range args {
		if a == "--json" {
			asJSON = true
		}
	}
	cfg := loadConfig()
	repo, err := findRepo(cfg)
	if err != nil {
		die("%v", err)
	}
	var states []serviceState
	for _, svc := range []struct {
		name string
		on   bool
	}{{"memory", cfg.Services.Memory}, {"laya", cfg.Services.Laya}} {
		if !svc.on {
			continue
		}
		if err := stopService(repo, svc.name); err != nil {
			warn("%s: %v", svc.name, err)
		}
		st := serviceStatus(repo, svc.name)
		states = append(states, st)
		if !asJSON {
			fmt.Printf("stopped     %-14s %s\n", svc.name, st.Detail)
		}
	}
	if asJSON {
		emitJSON(map[string]any{"services": states})
	}
}

func herdrServer(c Config) (bool, string) {
	out, err := runCapture("", nil, c.Herdr.Bin, "status", "server")
	if err != nil {
		return false, firstLine(out)
	}
	return true, firstLine(out)
}

// lastJSONLine returns the last line that looks like a JSON object (some herdr
// subcommands print warnings before their summary).
func lastJSONLine(s string) string {
	lines := splitLines(s)
	for i := len(lines) - 1; i >= 0; i-- {
		l := trim(lines[i])
		if len(l) >= 2 && l[0] == '{' && l[len(l)-1] == '}' {
			return l
		}
	}
	return ""
}

func firstLine(s string) string {
	for _, l := range splitLines(s) {
		if trim(l) != "" {
			return trim(l)
		}
	}
	return ""
}

func splitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	out = append(out, s[start:])
	return out
}

func emitJSON(v any) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		die("json: %v", err)
	}
	fmt.Println(string(data))
}
