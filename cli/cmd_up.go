package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
)

type upSummary struct {
	Repo     string         `json:"repo"`
	Dir      string         `json:"dir"`
	Assets   []assetResult  `json:"assets,omitempty"`
	Services []serviceState `json:"services,omitempty"`
	Herdr    string         `json:"herdr"`
	Opened   bool           `json:"opened"`
	Tab      string         `json:"tab,omitempty"`
	Agent    string         `json:"agent,omitempty"`
}

// openResult is herdr-plugin/scripts/open.sh's stdout JSON.
type openResult struct {
	Forseti    string `json:"forseti"` // created | reused
	Tab        string `json:"tab"`
	EditorPane string `json:"editor_pane"`
	AgentPane  string `json:"agent_pane"`
	Agent      string `json:"agent"`
}

func cmdUp(args []string) {
	fs := flag.NewFlagSet("up", flag.ExitOnError)
	dirFlag := fs.String("dir", "", "target directory (default: current)")
	noSync := fs.Bool("no-sync", false, "skip the ttt-plugin / pi-extension sync")
	noServices := fs.Bool("no-services", false, "do not start memory/laya")
	noOpen := fs.Bool("no-open", false, "do not open the Forseti tab")
	attach := fs.Bool("attach", false, "if herdr is not running, exec herdr first")
	dry := fs.Bool("dry-run", false, "print what would happen")
	asJSON := fs.Bool("json", false, "machine-readable summary")
	_ = fs.Parse(args)

	cfg := loadConfig()
	repo, err := findRepo(cfg)
	if err != nil {
		die("%v", err)
	}
	dir := *dirFlag
	if dir == "" {
		dir, _ = os.Getwd()
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		die("target dir: %v", err)
	}
	if st, err := os.Stat(abs); err != nil || !st.IsDir() {
		die("%s is not a directory", abs)
	}

	sum := upSummary{Repo: repo, Dir: abs}

	if !*noSync {
		results, err := syncAssets(repo, *dry)
		if err != nil {
			die("sync: %v", err)
		}
		sum.Assets = results
		if !*asJSON {
			for _, r := range results {
				switch {
				case r.Changed && *dry:
					fmt.Printf("would sync  %-14s %s\n", r.Asset.Label, r.Asset.Dst)
				case r.Changed:
					fmt.Printf("synced      %-14s %s\n", r.Asset.Label, r.Asset.Dst)
				default:
					fmt.Printf("in sync     %-14s %s\n", r.Asset.Label, r.Asset.Dst)
				}
			}
		}
	}

	if !*noServices {
		for _, svc := range []struct {
			name string
			on   bool
		}{{"memory", cfg.Services.Memory}, {"laya", cfg.Services.Laya}} {
			if !svc.on {
				continue
			}
			st := serviceStatus(repo, svc.name)
			if st.Up {
				sum.Services = append(sum.Services, st)
				if !*asJSON {
					fmt.Printf("up          %-14s %s\n", svc.name, st.Detail)
				}
				continue
			}
			if *dry {
				if !*asJSON {
					fmt.Printf("would start %-14s (%s)\n", svc.name, st.Detail)
				}
				continue
			}
			if err := startService(repo, svc.name); err != nil {
				warn("%s did not start: %v", svc.name, err)
				continue
			}
			st = serviceStatus(repo, svc.name)
			sum.Services = append(sum.Services, st)
			if !*asJSON {
				fmt.Printf("started     %-14s %s\n", svc.name, st.Detail)
			}
		}
	}

	running, detail := herdrServer(cfg)
	sum.Herdr = detail
	if !running {
		if *attach && isTTY(os.Stdin) && !*dry {
			if !*asJSON {
				fmt.Println("herdr       not running — starting it (quit herdr to return)")
			}
			if err := runInteractive("", nil, cfg.Herdr.Bin); err != nil {
				die("herdr: %v", err)
			}
			running, detail = herdrServer(cfg)
			sum.Herdr = detail
		}
		if !running {
			if *asJSON {
				emitJSON(sum)
			}
			die("herdr server is not running (%s)\n  start it with `herdr` (or `forseti up --attach`), then rerun", detail)
		}
	}
	if !*asJSON {
		fmt.Printf("up          %-14s %s\n", "herdr server", detail)
	}

	if !herdrPluginLinked(cfg) {
		if *dry {
			if !*asJSON {
				fmt.Printf("would link  %-14s %s\n", "herdr plugin", filepath.Join(repo, "herdr-plugin"))
			}
		} else if out, err := runCapture(repo, nil, cfg.Herdr.Bin, "plugin", "link", filepath.Join(repo, "herdr-plugin")); err != nil {
			warn("herdr plugin link: %v (%s)", err, trim(out))
		} else if !*asJSON {
			fmt.Printf("linked      %-14s %s\n", "herdr plugin", filepath.Join(repo, "herdr-plugin"))
		}
	}

	if !*noOpen {
		if *dry {
			if !*asJSON {
				fmt.Printf("would open  %-14s %s\n", "Forseti tab", abs)
			}
		} else {
			script := filepath.Join(repo, "herdr-plugin", "scripts", "open.sh")
			env := []string{
				"HERDR_BIN_PATH=" + cfg.Herdr.Bin,
				"FORSETI_AGENT_NAME=" + cfg.Herdr.Agent,
				"FORSETI_EDITOR_LABEL=" + cfg.Herdr.EditorLabel,
				fmt.Sprintf("FORSETI_TTT_PORT=%d", cfg.TTT.Port),
			}
			out, err := runCapture(abs, env, "sh", script)
			if err != nil {
				warn("could not open the Forseti tab: %v", err)
			} else {
				var res openResult
				if line := lastJSONLine(out); line != "" {
					_ = json.Unmarshal([]byte(line), &res)
				}
				if res.Forseti == "created" || res.Forseti == "reused" {
					sum.Opened = true
					sum.Tab, sum.Agent = res.Tab, res.Agent
					if !*asJSON {
						if res.Forseti == "reused" {
							fmt.Printf("reused      %-14s agent %s\n", "Forseti tab", res.Agent)
						} else {
							fmt.Printf("opened      %-14s %s (agent %s)\n", "Forseti tab", res.Tab, res.Agent)
						}
					}
				} else if !*asJSON {
					fmt.Printf("open        %-14s %s\n", "Forseti tab", firstLine(out))
				}
			}
		}
	}

	if *asJSON {
		emitJSON(sum)
	}
}

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

// lastJSONLine returns the last line that looks like a JSON object (open.sh may
// print warnings before its summary).
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
