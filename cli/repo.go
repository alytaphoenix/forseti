package main

import (
	"fmt"
	"os"
	"path/filepath"
)

// marker reports whether dir is a forseti checkout (all three components
// present, so a random parent dir cannot match).
func marker(dir string) bool {
	for _, rel := range []string{
		"herdr-plugin/herdr-plugin.toml",
		"host/Cargo.toml",
		"ttt-plugin/init.lua",
		"pi-extension/index.ts",
	} {
		if _, err := os.Stat(filepath.Join(dir, rel)); err != nil {
			return false
		}
	}
	return true
}

func findRepo(c Config) (string, error) {
	if v := os.Getenv("FORSETI_REPO"); v != "" {
		return canonicalRepo(expandHome(v))
	}
	if c.Repo != "" {
		return canonicalRepo(expandHome(c.Repo))
	}
	if defaultRepo != "" {
		if r, err := canonicalRepo(expandHome(defaultRepo)); err == nil {
			return r, nil
		}
	}
	cwd, _ := os.Getwd()
	if r, err := upwardRepo(cwd); err == nil {
		return r, nil
	}
	if exe, err := os.Executable(); err == nil {
		if r, err := upwardRepo(filepath.Dir(exe)); err == nil {
			return r, nil
		}
	}
	return "", fmt.Errorf("no forseti checkout found — run from the repo, or set FORSETI_REPO / `forseti config set repo DIR`")
}

func canonicalRepo(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	if !marker(abs) {
		return "", fmt.Errorf("%s is not a forseti checkout (missing herdr-plugin/, host/, ttt-plugin/ or pi-extension/)", abs)
	}
	return abs, nil
}

func upwardRepo(start string) (string, error) {
	dir, err := filepath.Abs(start)
	if err != nil {
		return "", err
	}
	for {
		if marker(dir) {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("not inside a forseti checkout")
		}
		dir = parent
	}
}

// crewBin locates forseti-crew: the repo build first, then $PATH.
func crewBin(repo string) (string, error) {
	local := filepath.Join(repo, "crew", "bin", "forseti-crew")
	if st, err := os.Stat(local); err == nil && !st.IsDir() {
		return local, nil
	}
	if p, err := lookPath("forseti-crew"); err == nil {
		return p, nil
	}
	return "", fmt.Errorf("forseti-crew not built (cd crew && go build -o bin/forseti-crew ./cmd/forseti-crew)")
}

// flowBin locates forseti-flow: the repo build first, then $PATH.
func flowBin(repo string) (string, error) {
	local := filepath.Join(repo, "crew", "bin", "forseti-flow")
	if st, err := os.Stat(local); err == nil && !st.IsDir() {
		return local, nil
	}
	if p, err := lookPath("forseti-flow"); err == nil {
		return p, nil
	}
	return "", fmt.Errorf("forseti-flow not built (cd crew && go build -o bin/forseti-flow ./cmd/forseti-flow)")
}

// hostBin locates forseti-host: release, then debug, then $PATH.
func hostBin(repo string) (string, error) {
	for _, rel := range []string{"release", "debug"} {
		p := filepath.Join(repo, "host", "target", rel, "forseti-host")
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p, nil
		}
	}
	if p, err := lookPath("forseti-host"); err == nil {
		return p, nil
	}
	return "", fmt.Errorf("forseti-host not built (cd host && cargo build --release)")
}
