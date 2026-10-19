package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type serviceState struct {
	Name   string `json:"name"`
	Up     bool   `json:"up"`
	Detail string `json:"detail,omitempty"`
}

func serviceScript(repo, name string) (string, error) {
	p := filepath.Join(repo, "scripts", name+"-serve.sh")
	if !fileExists(p) {
		return "", fmt.Errorf("missing %s", p)
	}
	return p, nil
}

func serviceStatus(repo, name string) serviceState {
	st := serviceState{Name: name}
	script, err := serviceScript(repo, name)
	if err != nil {
		st.Detail = err.Error()
		return st
	}
	out, err := runCapture(repo, nil, "sh", script, "status")
	if err != nil {
		st.Detail = trim(out)
		return st
	}
	st.Up = strings.Contains(out, `"status":"ok"`)
	if st.Up {
		st.Detail = "healthy"
	} else {
		st.Detail = trim(out)
	}
	return st
}

func startService(repo, name string) error {
	script, err := serviceScript(repo, name)
	if err != nil {
		return err
	}
	out, err := runCapture(repo, nil, "sh", script, "start")
	if err != nil {
		return err
	}
	_ = out
	return nil
}

func stopService(repo, name string) error {
	script, err := serviceScript(repo, name)
	if err != nil {
		return err
	}
	_, err = runCapture(repo, nil, "sh", script, "stop")
	return err
}

func fileExists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && !st.IsDir()
}
