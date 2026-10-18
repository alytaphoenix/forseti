// forseti-crew — deterministic agent-graph builder + runner on herdr.
//
//	crew run [--headless] [-f crew.yaml]   run the graph (TUI by default)
//	crew validate [-f crew.yaml]          validate only
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"time"

	"forseti/crew/internal/runner"
	"forseti/crew/internal/schema"
	"forseti/crew/internal/switchyard"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "run":
		cmdRun(os.Args[2:])
	case "validate":
		cmdValidate(os.Args[2:])
	case "prompts":
		cmdPrompts(os.Args[2:])
	case "watch":
		cmdWatch(os.Args[2:])
	case "-h", "--help", "help":
		usage()
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `forseti-crew — deterministic agent-graph builder + runner on herdr

usage:
  forseti-crew run [-f crew.yaml] [--headless] [--keep-tab] [--timeout MIN]
                   [--cwd DIR] [--tab LABEL]
                   [--session NAME] [--worktree BRANCH] [--keep-worktree]
                   [--review] [--model P/M] [--host herdr|ttt]
  forseti-crew validate [-f crew.yaml]
  forseti-crew prompts [-f crew.yaml]
  forseti-crew watch [-f run.jsonl] [--follow=false]`)
}

type runFlags struct {
	file         string
	headless     bool
	keepTab      bool
	timeout      int
	cwd          string
	tab          string
	session      string
	worktree     string
	keepWorktree bool
	review       bool
	model        string
	engine       string
	host         string
}

func parseRun(fs *flag.FlagSet, args []string) *runFlags {
	f := &runFlags{}
	fs.StringVar(&f.file, "f", "crew.yaml", "crew file")
	fs.BoolVar(&f.headless, "headless", false, "no TUI; JSONL events to stdout")
	fs.BoolVar(&f.keepTab, "keep-tab", false, "leave the crew tab open after the run")
	fs.IntVar(&f.timeout, "timeout", 10, "per-node settle timeout (minutes)")
	fs.StringVar(&f.cwd, "cwd", "", "repo cwd for panes (default: current dir)")
	fs.StringVar(&f.tab, "tab", "forseti-crew", "crew tab label")
	fs.StringVar(&f.session, "session", "", "named herdr session (hermetic sandbox)")
	fs.StringVar(&f.worktree, "worktree", "", "git branch for a disposable worktree run")
	fs.BoolVar(&f.keepWorktree, "keep-worktree", false, "keep the worktree after the run")
	fs.BoolVar(&f.review, "review", false, "end on a lazygit review of the worktree (implies --keep-worktree + --keep-tab)")
	fs.StringVar(&f.model, "model", "", "override model for direct-model nodes (FORSETI_CREW_MODEL env also works)")
	fs.StringVar(&f.engine, "engine", "", "engine: go (default) or rust — rust delegates to forseti-host (FORSETI_CREW_ENGINE env also works)")
	fs.StringVar(&f.host, "host", "", "where agents run: herdr (default) or ttt (rust engine; FORSETI_CREW_HOST env also works)")
	_ = fs.Parse(args)
	if f.engine == "" {
		f.engine = os.Getenv("FORSETI_CREW_ENGINE")
	}
	if f.engine == "" {
		f.engine = "go"
	}
	if f.engine != "go" && f.engine != "rust" {
		fmt.Fprintln(os.Stderr, "run: --engine must be go or rust")
		os.Exit(2)
	}
	if f.model == "" {
		f.model = os.Getenv("FORSETI_CREW_MODEL")
	}
	if f.host == "" {
		f.host = os.Getenv("FORSETI_CREW_HOST")
	}
	return f
}

func loadCrew(path string) (*schema.Crew, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return schema.Load(data)
}

// execRustRun translates the parsed run flags into `forseti-host crew run` and
// execs it: the Rust engine is a drop-in for the same crew files (docs S40).
func execRustRun(f *runFlags) {
	args := []string{"crew", "run", "-f", f.file, "--timeout", strconv.Itoa(f.timeout)}
	if f.session != "" {
		args = append(args, "--session", f.session)
	}
	if f.tab != "" {
		args = append(args, "--tab", f.tab)
	}
	if f.cwd != "" {
		args = append(args, "--cwd", f.cwd)
	}
	if f.model != "" {
		args = append(args, "--model", f.model)
	}
	if f.headless {
		args = append(args, "--headless")
	}
	if f.keepTab {
		args = append(args, "--keep-tab")
	}
	if f.worktree != "" {
		args = append(args, "--worktree", f.worktree)
	}
	if f.keepWorktree {
		args = append(args, "--keep-worktree")
	}
	if f.review {
		args = append(args, "--review")
	}
	if f.host != "" {
		args = append(args, "--host", f.host)
	}
	execRust(args)
}

// execRust runs the Rust host with inherited stdio and the same exit code.
func execRust(args []string) {
	host, err := switchyard.ResolveHostBin()
	if err != nil {
		fmt.Fprintln(os.Stderr, "engine rust:", err)
		os.Exit(1)
	}
	cmd := exec.Command(host, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			os.Exit(ee.ExitCode())
		}
		fmt.Fprintln(os.Stderr, "engine rust:", err)
		os.Exit(1)
	}
	os.Exit(0)
}

func cmdValidate(args []string) {
	fs := flag.NewFlagSet("validate", flag.ExitOnError)
	explain := fs.Bool("explain", false, "print the resolved graph (roles, patterns, edges) after validating")
	f := parseRun(fs, args)
	if f.engine == "rust" {
		args := []string{"crew", "validate", "-f", f.file}
		if *explain {
			args = append(args, "--explain")
		}
		execRust(args)
		return
	}
	crew, err := loadCrew(f.file)
	if err != nil {
		// P13-B23: a missing file is a read error, not a crew validation verdict
		if os.IsNotExist(err) {
			fmt.Fprintln(os.Stderr, "cannot read:", err)
		} else {
			fmt.Fprintln(os.Stderr, "INVALID:", err)
		}
		os.Exit(1)
	}
	// Phase 18C: hand the routes to the router's own `--dry-run`, so a bad route
	// shape (unknown key, incomplete option-driven type) fails here with the
	// router's message instead of at run time. Advisory only when no router
	// binary is available.
	if note, err := switchyard.DryRunRoutes(crew.Routes); err != nil {
		fmt.Fprintln(os.Stderr, "INVALID (routes):", err)
		os.Exit(1)
	} else if note != "" {
		fmt.Fprintln(os.Stderr, "note:", note)
	}
	if *explain {
		fmt.Print(schema.Explain(crew))
	}
	fmt.Println(f.file, "valid")
}

// cmdPrompts dumps every resolved node's prompt template as JSON, after
// validation (so patterns/roles have expanded). It is the parity surface for
// the staged Rust port: `validate --explain` pins the graph, `prompts` pins the
// prompt text the runner renders — a pattern or role text drift would otherwise
// be invisible until a live run (docs/spikes.md S35).
func cmdPrompts(args []string) {
	fs := flag.NewFlagSet("prompts", flag.ExitOnError)
	f := parseRun(fs, args)
	crew, err := loadCrew(f.file)
	if err != nil {
		fmt.Fprintln(os.Stderr, "INVALID:", err)
		os.Exit(1)
	}
	type node struct {
		Name   string `json:"name"`
		Prompt string `json:"prompt"`
	}
	out := struct {
		Agents []node `json:"agents"`
	}{Agents: make([]node, 0, len(crew.Agents))}
	for i := range crew.Agents {
		out.Agents = append(out.Agents, node{Name: crew.Agents[i].Name, Prompt: crew.Agents[i].Prompt})
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(out)
}

func cmdRun(args []string) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	f := parseRun(fs, args)
	// P13-B23: stray positionals were silently ignored; timeout must be ≥ 1
	if fs.NArg() > 0 {
		fmt.Fprintln(os.Stderr, "run: unexpected arguments:", fs.Args())
		os.Exit(2)
	}
	if f.timeout < 1 {
		fmt.Fprintln(os.Stderr, "run: --timeout must be >= 1 (minutes)")
		os.Exit(2)
	}
	if f.engine == "rust" {
		execRustRun(f)
		return
	}
	if f.host == "ttt" {
		fmt.Fprintln(os.Stderr, "run: the ttt host needs --engine rust")
		os.Exit(2)
	}
	crew, err := loadCrew(f.file)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Fprintln(os.Stderr, "cannot read:", err)
		} else {
			fmt.Fprintln(os.Stderr, "INVALID:", err)
		}
		os.Exit(1)
	}
	cwd := f.cwd
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	// --session: herdrd.SocketPath() resolves HERDR_SESSION (6A-2)
	if f.session != "" {
		os.Setenv("HERDR_SESSION", f.session)
	}
	opts := runner.Options{
		Cwd:            cwd,
		TabLabel:       f.tab,
		NodeTimeout:    time.Duration(f.timeout) * time.Minute,
		KeepTab:        f.keepTab,
		Session:        f.session,
		WorktreeBranch: f.worktree,
		KeepWorktree:   f.keepWorktree,
		Review:         f.review,
		ModelOverride:  f.model,
	}
	if f.headless {
		opts.OnEvent = func(ev runner.Event) {
			b, _ := json.Marshal(ev)
			fmt.Println(string(b))
		}
		r := runner.New(crew, opts)
		if err := r.Run(context.Background()); err != nil {
			fmt.Fprintln(os.Stderr, "run error:", err)
			os.Exit(1)
		}
		for _, st := range r.Snapshot() {
			if st.Status == "failed" || st.Status == "blocked" {
				os.Exit(1)
			}
		}
		if r.ChecksFailed() {
			os.Exit(1)
		}
		return
	}
	failed, err := runTUI(crew, opts, f.file)
	if err != nil {
		fmt.Fprintln(os.Stderr, "run error:", err)
		os.Exit(1)
	}
	// P13-B3/B14: the verdict comes from the LIVE run — the post-hoc glob
	// re-read a possibly-deleted (worktree) or wrong-second log, and a
	// blocked-only run exited 0 here while headless exited 1.
	if failed {
		os.Exit(1)
	}
}
