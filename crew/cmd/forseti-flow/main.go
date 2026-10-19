// forseti-flow — validate and explain a declarative per-project workflow.
//
//	forseti-flow validate [-f .forseti/flow.yaml] [--explain] [--json]
//	forseti-flow explain  [-f .forseti/flow.yaml]
//
// Phase F0 surface only. The evidence engine, git hooks, in-session runtime and
// crew compiler land in later phases (see the flow plan).
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"forseti/crew/internal/flow"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "validate":
		cmdValidate(os.Args[2:])
	case "explain":
		cmdExplain(os.Args[2:])
	case "check":
		cmdCheck(os.Args[2:])
	case "eval":
		cmdEval(os.Args[2:])
	case "ci":
		cmdCI(os.Args[2:])
	case "run":
		cmdRun(os.Args[2:])
	case "simulate":
		cmdSimulate(os.Args[2:])
	case "edit":
		cmdEdit(os.Args[2:])
	case "waive":
		cmdWaive(os.Args[2:])
	case "install-hooks":
		cmdInstallHooks(os.Args[2:])
	case "init":
		cmdInit(os.Args[2:])
	case "version", "-v", "--version":
		fmt.Printf("forseti-flow (flow language v%d)\n", flow.Version)
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "forseti-flow: unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `forseti-flow — declarative per-project workflows

usage:
  forseti-flow validate [-f .forseti/flow.yaml] [--explain] [--json]
  forseti-flow explain  [-f .forseti/flow.yaml]
  forseti-flow check    [--flow ID] [--stage STAGE] [--dir DIR] [--json]
  forseti-flow eval     --evidence ID [--flow ID] [--stage STAGE] [--dir DIR] [--json]
  forseti-flow ci       [-f FILE]              # GitHub Actions snippet
  forseti-flow run      --flow ID [crew flags...]
  forseti-flow simulate [--flow ID] [--diff REF] [--dir DIR] [--json]
  forseti-flow edit     [-f FILE]              # $EDITOR (builder-lite)
  forseti-flow waive    --flow ID --stage STAGE --evidence ID --reason TEXT [--ttl D]
  forseti-flow install-hooks [--print] [--force]
  forseti-flow init     [--preset repo|review-loop|docs-required|release] [--print]

The default file is .forseti/flow.yaml, searched upward from the current
directory. Exit codes: 0 ok, 1 blocked/invalid, 2 usage/config.`)
}

func cmdValidate(args []string) {
	fs := flag.NewFlagSet("validate", flag.ExitOnError)
	file := fs.String("f", "", "flow file (default: .forseti/flow.yaml, searched upward)")
	explain := fs.Bool("explain", false, "print the resolved flow after validating")
	asJSON := fs.Bool("json", false, "machine-readable result")
	_ = fs.Parse(args)

	path, err := resolveFile(*file)
	if err != nil {
		if *asJSON {
			writeJSON(jsonResult{Valid: false, File: path, Error: err.Error()})
		} else {
			fmt.Fprintln(os.Stderr, "cannot read:", err)
		}
		os.Exit(1)
	}
	f, err := load(path)
	if err != nil {
		if *asJSON {
			writeJSON(jsonResult{Valid: false, File: path, Error: err.Error()})
		} else {
			fmt.Fprintln(os.Stderr, "INVALID:", err)
		}
		os.Exit(1)
	}
	if *asJSON {
		writeJSON(jsonResult{Valid: true, File: path, Flows: f.Flows})
		return
	}
	if *explain {
		fmt.Print(flow.ExplainFile(f))
	}
	if len(f.Flows) == 1 {
		fmt.Printf("%s valid (flow %s, %s)\n", path, f.Flows[0].ID, f.Flows[0].EffectiveMode())
		return
	}
	fmt.Printf("%s valid (%d flows)\n", path, len(f.Flows))
}

func cmdExplain(args []string) {
	fs := flag.NewFlagSet("explain", flag.ExitOnError)
	file := fs.String("f", "", "flow file (default: .forseti/flow.yaml, searched upward)")
	_ = fs.Parse(args)

	path, err := resolveFile(*file)
	if err != nil {
		fmt.Fprintln(os.Stderr, "cannot read:", err)
		os.Exit(1)
	}
	f, err := load(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "INVALID:", err)
		os.Exit(1)
	}
	fmt.Print(flow.ExplainFile(f))
}

func load(path string) (*flow.File, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return flow.Load(data)
}

// resolveFile picks the explicit path or searches upward for .forseti/flow.yaml.
func resolveFile(explicit string) (string, error) {
	if explicit != "" {
		abs, err := filepath.Abs(explicit)
		if err != nil {
			return explicit, err
		}
		return abs, nil
	}
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		cand := filepath.Join(dir, ".forseti", "flow.yaml")
		if st, err := os.Stat(cand); err == nil && !st.IsDir() {
			return cand, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no .forseti/flow.yaml found from %s upward", mustGetwd())
		}
		dir = parent
	}
}

func mustGetwd() string {
	wd, err := os.Getwd()
	if err != nil {
		return "."
	}
	return wd
}

type jsonResult struct {
	Valid bool         `json:"valid"`
	File  string       `json:"file,omitempty"`
	Flows []*flow.Flow `json:"flows,omitempty"`
	Error string       `json:"error,omitempty"`
}

func writeJSON(v any) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}
