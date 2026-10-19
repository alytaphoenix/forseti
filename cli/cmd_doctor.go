package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type check struct {
	Name   string `json:"name"`
	State  string `json:"state"` // ok | warn | fail
	Detail string `json:"detail,omitempty"`
	Fix    string `json:"fix,omitempty"`
}

func cmdDoctor(args []string) {
	asJSON := false
	for _, a := range args {
		if a == "--json" {
			asJSON = true
		}
	}
	cfg := loadConfig()
	var checks []check
	add := func(name, state, detail, fix string) {
		checks = append(checks, check{name, state, detail, fix})
	}

	repo, repoErr := findRepo(cfg)
	if repoErr != nil {
		add("repo", "fail", repoErr.Error(), "run from the checkout, or `forseti config set repo DIR`")
	} else {
		add("repo", "ok", repo, "")
	}

	if _, err := os.Stat(configPath()); err == nil {
		add("config", "ok", configPath(), "")
	} else {
		add("config", "warn", "not created yet (defaults in use)", "forseti config init")
	}

	// herdr
	if _, err := lookPath(cfg.Herdr.Bin); err != nil {
		add("herdr", "fail", "binary not found: "+cfg.Herdr.Bin, "install herdr (https://herdr.dev)")
	} else if running, detail := herdrServer(cfg); !running {
		add("herdr", "fail", "server not running: "+detail, "run `herdr` (or `forseti up --attach`)")
	} else if !herdrPluginLinked(cfg) {
		add("herdr plugin", "fail", "forseti plugin is not linked", "forseti up  (links it automatically)")
	} else {
		add("herdr", "ok", "server running; plugin linked", "")
	}

	// ttt
	if _, err := lookPath(cfg.TTT.Bin); err != nil {
		add("ttt", "fail", "binary not found: "+cfg.TTT.Bin, "build the forseti fork (docs/ttt-terminal-api.md)")
	} else {
		if found, checked := tttHasTerminalAPI(cfg.TTT.Bin); checked && !found {
			add("ttt terminal API", "warn", "binary has no ttt.terminal (upstream build)", "build the forseti fork and put it on PATH")
		} else {
			add("ttt terminal API", "ok", "present (or not probeable)", "")
		}
		if tttListening(cfg.TTT.Port) {
			add("ttt listen", "ok", fmt.Sprintf("127.0.0.1:%d", cfg.TTT.Port), "")
		} else {
			add("ttt listen", "warn", fmt.Sprintf("not listening on %d", cfg.TTT.Port), "forseti up")
		}
	}

	// pi
	if _, err := lookPath(cfg.Pi.Bin); err != nil {
		add("pi", "fail", "binary not found: "+cfg.Pi.Bin, "install pi (@earendil-works/pi-coding-agent)")
	} else if out, err := runCapture("", nil, cfg.Pi.Bin, "--version"); err == nil {
		add("pi", "ok", firstLine(out), "")
	} else {
		add("pi", "warn", "found, version probe failed", "")
	}

	// assets + grants
	if repoErr == nil {
		drift := 0
		for _, a := range inspectAssets(repo) {
			if a.Changed {
				drift++
			}
		}
		if drift == 0 {
			add("assets", "ok", "ttt plugin + pi extension in sync", "")
		} else {
			add("assets", "warn", fmt.Sprintf("%d stale installed file(s)", drift), "forseti config sync")
		}
	}
	if granted, ok := pluginGrants(); ok {
		missing := []string{}
		for _, p := range []string{"terminal.control", "terminal.read", "fs.read", "fs.write", "commands"} {
			if !granted[p] {
				missing = append(missing, p)
			}
		}
		if len(missing) == 0 {
			add("ttt grants", "ok", "forseti plugin permissions granted", "")
		} else {
			add("ttt grants", "warn", "missing: "+strings.Join(missing, ", "), "approve/reload the forseti plugin in ttt")
		}
	}

	// crew binaries
	if repoErr == nil {
		if _, err := crewBin(repo); err != nil {
			add("forseti-crew", "warn", err.Error(), "cd crew && go build -o bin/forseti-crew ./cmd/forseti-crew")
		} else {
			add("forseti-crew", "ok", "built", "")
		}
		if _, err := hostBin(repo); err != nil {
			add("forseti-host", "warn", err.Error(), "cd host && cargo build --release")
		} else {
			add("forseti-host", "ok", "built", "")
		}
	}

	// provider keys
	for _, key := range []string{"opencode-go.key", "openrouter.key"} {
		p := filepath.Join(homeDir(), ".config", "forseti", key)
		if st, err := os.Stat(p); err == nil {
			add(key, "ok", fmt.Sprintf("%s (mode %04o)", p, st.Mode().Perm()), "")
		} else {
			add(key, "warn", "missing "+p, "create it with the provider key (0600)")
		}
	}

	// services
	if repoErr == nil {
		for _, svc := range []struct {
			name string
			on   bool
		}{{"memory", cfg.Services.Memory}, {"laya", cfg.Services.Laya}} {
			if !svc.on {
				add(svc.name, "ok", "disabled in config", "")
				continue
			}
			st := serviceStatus(repo, svc.name)
			if st.Up {
				add(svc.name, "ok", st.Detail, "")
			} else {
				add(svc.name, "warn", st.Detail, "forseti up  (or scripts/"+svc.name+"-serve.sh start)")
			}
		}
	}

	// stale lock
	cwd, _ := os.Getwd()
	if pid, ok := readRunLock(filepath.Join(cwd, ".forseti", "run.lock")); ok {
		add("run lock", "warn", fmt.Sprintf("held by pid %d", pid), "wait for the run, or remove .forseti/run.lock if the pid is dead")
	}

	failed := false
	for _, c := range checks {
		if c.State == "fail" {
			failed = true
		}
	}
	if asJSON {
		emitJSON(map[string]any{"version": Version, "failed": failed, "checks": checks})
	} else {
		for _, c := range checks {
			mark := "ok  "
			switch c.State {
			case "warn":
				mark = "warn"
			case "fail":
				mark = "FAIL"
			}
			fmt.Printf("%s  %-17s %s\n", mark, c.Name, c.Detail)
			if c.Fix != "" && c.State != "ok" {
				fmt.Printf("      %-17s fix: %s\n", "", c.Fix)
			}
		}
		if failed {
			fmt.Println()
			fmt.Println("some checks failed — see the fixes above")
		}
	}
	if failed {
		os.Exit(1)
	}
}

type forsetiGrant struct {
	Name        string         `json:"name"`
	Permissions map[string]any `json:"permissions"`
}

func pluginGrants() (map[string]bool, bool) {
	path := filepath.Join(homeDir(), ".config", "ttt", "plugins.ttt.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	var entries []forsetiGrant
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, false
	}
	for _, e := range entries {
		if e.Name != "forseti" {
			continue
		}
		out := map[string]bool{}
		for k, v := range e.Permissions {
			switch vv := v.(type) {
			case bool:
				out[k] = vv
			case []any:
				out[k] = len(vv) > 0
			}
		}
		return out, true
	}
	return nil, false
}
