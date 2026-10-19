package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type statusLine struct {
	Name   string `json:"name"`
	State  string `json:"state"` // up | down | warn | none
	Detail string `json:"detail,omitempty"`
}

type statusReport struct {
	Version string       `json:"version"`
	Repo    string       `json:"repo"`
	Config  string       `json:"config"`
	Lines   []statusLine `json:"status"`
}

type crewRunStatus struct {
	Crew    string `json:"crew"`
	Phase   string `json:"phase"`
	Total   int    `json:"total"`
	Done    int    `json:"done"`
	Running int    `json:"running"`
	Blocked int    `json:"blocked"`
	Failed  int    `json:"failed"`
	TS      string `json:"ts"`
}

func cmdStatus(args []string) {
	asJSON := false
	for _, a := range args {
		if a == "--json" {
			asJSON = true
		}
	}
	cfg := loadConfig()
	repo, repoErr := findRepo(cfg)
	rep := statusReport{Version: Version, Config: configPath()}
	if repoErr == nil {
		rep.Repo = repo
	} else {
		rep.Repo = repoOrUnknown(cfg)
	}

	// herdr
	if running, detail := herdrServer(cfg); running {
		rep.Lines = append(rep.Lines, statusLine{"herdr", "up", detail})
	} else {
		rep.Lines = append(rep.Lines, statusLine{"herdr", "down", detail})
	}

	// ttt listener
	if tttListening(cfg.TTT.Port) {
		rep.Lines = append(rep.Lines, statusLine{"ttt", "up", fmt.Sprintf("127.0.0.1:%d", cfg.TTT.Port)})
	} else {
		rep.Lines = append(rep.Lines, statusLine{"ttt", "down", fmt.Sprintf("not listening on %d", cfg.TTT.Port)})
	}

	// pi
	if out, err := runCapture("", nil, cfg.Pi.Bin, "--version"); err == nil {
		rep.Lines = append(rep.Lines, statusLine{"pi", "up", firstLine(out)})
	} else {
		rep.Lines = append(rep.Lines, statusLine{"pi", "down", "not found"})
	}

	// assets
	if repoErr == nil {
		drift := 0
		for _, a := range inspectAssets(repo) {
			if a.Changed {
				drift++
			}
		}
		if drift == 0 {
			rep.Lines = append(rep.Lines, statusLine{"assets", "up", "in sync"})
		} else {
			rep.Lines = append(rep.Lines, statusLine{"assets", "warn", fmt.Sprintf("%d stale (forseti config sync)", drift)})
		}
	}

	// services
	if repoErr == nil {
		for _, svc := range []struct {
			name string
			on   bool
		}{{"memory", cfg.Services.Memory}, {"laya", cfg.Services.Laya}} {
			if !svc.on {
				rep.Lines = append(rep.Lines, statusLine{svc.name, "none", "disabled in config"})
				continue
			}
			st := serviceStatus(repo, svc.name)
			state := "down"
			if st.Up {
				state = "up"
			}
			rep.Lines = append(rep.Lines, statusLine{svc.name, state, st.Detail})
		}
	}

	// current crew run + lock (relative to the current directory)
	cwd, _ := os.Getwd()
	if run, err := readCrewStatus(filepath.Join(cwd, ".forseti", "crew-status.json")); err == nil {
		detail := fmt.Sprintf("%s %d/%d done (running %d, blocked %d, failed %d)",
			run.Crew, run.Done, run.Total, run.Running, run.Blocked, run.Failed)
		if run.Phase != "" {
			detail = fmt.Sprintf("%s · phase %s · %d/%d done (running %d, blocked %d, failed %d)",
				run.Crew, run.Phase, run.Done, run.Total, run.Running, run.Blocked, run.Failed)
		}
		rep.Lines = append(rep.Lines, statusLine{"crew", "up", detail})
	} else {
		rep.Lines = append(rep.Lines, statusLine{"crew", "none", "no live run in this directory"})
	}
	if pid, ok := readRunLock(filepath.Join(cwd, ".forseti", "run.lock")); ok {
		rep.Lines = append(rep.Lines, statusLine{"lock", "warn", fmt.Sprintf("held by pid %d", pid)})
	} else {
		rep.Lines = append(rep.Lines, statusLine{"lock", "none", "free"})
	}

	if asJSON {
		emitJSON(rep)
		return
	}
	fmt.Printf("forseti %s\n", Version)
	fmt.Printf("%-8s %s\n", "repo", rep.Repo)
	fmt.Printf("%-8s %s\n", "config", rep.Config)
	for _, l := range rep.Lines {
		fmt.Printf("%-8s %-5s %s\n", l.Name, l.State, l.Detail)
	}
	if !allUp(rep.Lines) {
		fmt.Println()
		fmt.Println("hint: forseti up   (bring the workspace up) · forseti doctor   (diagnose)")
	}
}

func allUp(lines []statusLine) bool {
	for _, l := range lines {
		if l.State == "down" || l.State == "warn" {
			return false
		}
	}
	return true
}

func readCrewStatus(path string) (crewRunStatus, error) {
	var out crewRunStatus
	data, err := os.ReadFile(path)
	if err != nil {
		return out, err
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return out, err
	}
	return out, nil
}

func readRunLock(path string) (int, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return 0, true
	}
	return pid, true
}
