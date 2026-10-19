package main

import (
	"errors"
	"os"
	"os/exec"
)

// cmdFlow runs forseti-flow with the repo's local build when present. The flow
// binary owns the language; this wrapper only locates it and preserves exit
// codes (0 valid, 1 invalid, 2 usage).
func cmdFlow(sub string, args []string) {
	cfg := loadConfig()
	repo, err := findRepo(cfg)
	if err != nil {
		die("%v", err)
	}
	bin, err := flowBin(repo)
	if err != nil {
		die("%v", err)
	}
	argv := append([]string{sub}, args...)
	err = runInteractive("", []string{"FORSETI_REPO=" + repo}, bin, argv...)
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			os.Exit(exit.ExitCode())
		}
		die("%v", err)
	}
}
