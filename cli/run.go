package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

func lookPath(name string) (string, error) {
	return exec.LookPath(name)
}

func commandExists(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

// runCapture runs a command and returns combined output (trimmed) with a
// descriptive error on failure.
func runCapture(dir string, env []string, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	text := strings.TrimSpace(out.String())
	if err != nil {
		if text == "" {
			text = err.Error()
		}
		return text, fmt.Errorf("%s %s: %s", name, strings.Join(args, " "), text)
	}
	return text, nil
}

// runInteractive hands stdio to the child (TUI or long-running CLI).
func runInteractive(dir string, env []string, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func isTTY(f *os.File) bool {
	st, err := f.Stat()
	if err != nil {
		return false
	}
	return st.Mode()&os.ModeCharDevice != 0
}
