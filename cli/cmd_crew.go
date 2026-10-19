package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// cmdCrew runs the Go CLI (which can delegate to the Rust engine) with the
// configured engine/host/model/default-crew defaults.
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
	argv, err := crewArgv(cfg, sub, args)
	if err != nil {
		die("%v", err)
	}
	err = runInteractive("", nil, bin, argv...)
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			os.Exit(exit.ExitCode())
		}
		die("%v", err)
	}
}

// crewArgv builds the forseti-crew argv: configured defaults first, then the
// user's args (so an explicit flag always wins). `run` with no -f/--file uses
// crew.default when set.
func crewArgv(cfg Config, sub string, args []string) ([]string, error) {
	argv := []string{sub}
	if sub != "run" {
		return append(argv, args...), nil
	}
	if cfg.Crew.Engine != "" {
		argv = append(argv, "--engine", cfg.Crew.Engine)
	}
	if cfg.Crew.Host != "" {
		argv = append(argv, "--host", cfg.Crew.Host)
	}
	if cfg.Crew.Model != "" && !hasArg(args, "--model") {
		argv = append(argv, "--model", cfg.Crew.Model)
	}
	if cfg.Crew.Default != "" && !hasFileArg(args) && !hasArg(args, "--help") && !hasArg(args, "-h") {
		path, err := resolveCrewDefault(cfg.Crew.Default)
		if err != nil {
			return nil, err
		}
		argv = append(argv, "-f", path)
	}
	return append(argv, args...), nil
}

// resolveCrewDefault expands ~ and resolves a bare name under
// ~/.config/forseti/crews (name or name.yaml).
func resolveCrewDefault(raw string) (string, error) {
	p := expandHome(raw)
	if !fileExists(p) && !strings.ContainsRune(p, os.PathSeparator) {
		base := filepath.Join(homeDir(), ".config", "forseti", "crews")
		for _, cand := range []string{
			filepath.Join(base, p),
			filepath.Join(base, p+".yaml"),
			filepath.Join(base, p+".yml"),
		} {
			if fileExists(cand) {
				p = cand
				break
			}
		}
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	if !fileExists(abs) {
		return "", fmt.Errorf("crew.default %q not found (tried %s; set `forseti config set crew.default PATH`)", raw, abs)
	}
	return abs, nil
}

func hasArg(args []string, want string) bool {
	for _, a := range args {
		if a == want || strings.HasPrefix(a, want+"=") {
			return true
		}
	}
	return false
}

// hasFileArg reports an explicit crew file (-f/--file, with either separator).
func hasFileArg(args []string) bool {
	for _, a := range args {
		if a == "-f" || a == "--f" || a == "--file" ||
			strings.HasPrefix(a, "-f=") || strings.HasPrefix(a, "--f=") || strings.HasPrefix(a, "--file=") {
			return true
		}
	}
	return false
}
