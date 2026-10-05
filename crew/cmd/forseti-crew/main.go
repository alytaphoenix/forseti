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
	"time"

	"forseti/crew/internal/runner"
	"forseti/crew/internal/schema"
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
                   [--review] [--model P/M]
  forseti-crew validate [-f crew.yaml]
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
	_ = fs.Parse(args)
	if f.model == "" {
		f.model = os.Getenv("FORSETI_CREW_MODEL")
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

func cmdValidate(args []string) {
	fs := flag.NewFlagSet("validate", flag.ExitOnError)
	f := parseRun(fs, args)
	if _, err := loadCrew(f.file); err != nil {
		// P13-B23: a missing file is a read error, not a crew validation verdict
		if os.IsNotExist(err) {
			fmt.Fprintln(os.Stderr, "cannot read:", err)
		} else {
			fmt.Fprintln(os.Stderr, "INVALID:", err)
		}
		os.Exit(1)
	}
	fmt.Println(f.file, "valid")
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
