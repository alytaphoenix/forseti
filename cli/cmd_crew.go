package main

import (
	"errors"
	"os"
	"os/exec"
	"strings"
)

// cmdCrew runs the Go CLI (which can delegate to the Rust engine) with the
// configured engine/host/model defaults.
func cmdCrew(sub string, args []string) {
	cfg := loadConfig()
	repo, err := findRepo(cfg)
	if err != nil {
		die("%v", err)
	}
	bin, err := crewBin(repo)
	if err != nil {
		die("%v", err)
	}
	argv := []string{sub}
	if sub == "run" {
		if cfg.Crew.Engine != "" {
			argv = append(argv, "--engine", cfg.Crew.Engine)
		}
		if cfg.Crew.Host != "" {
			argv = append(argv, "--host", cfg.Crew.Host)
		}
		if cfg.Crew.Model != "" && !hasArg(args, "--model") {
			argv = append(argv, "--model", cfg.Crew.Model)
		}
	}
	argv = append(argv, args...)
	err = runInteractive("", nil, bin, argv...)
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			os.Exit(exit.ExitCode())
		}
		die("%v", err)
	}
}

func hasArg(args []string, want string) bool {
	for _, a := range args {
		if a == want || strings.HasPrefix(a, want+"=") {
			return true
		}
	}
	return false
}
