package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
)

var (
	lineColRe = regexp.MustCompile(`^(.*):([0-9]+):([0-9]+)$`)
	lineRe    = regexp.MustCompile(`^(.*):([0-9]+)$`)
)

// parseTarget splits "path[:line[:col]]" without breaking paths that contain
// colons (the numeric suffix is what makes it a position).
func parseTarget(s string) (path string, line, col int, err error) {
	path = s
	if m := lineColRe.FindStringSubmatch(s); m != nil {
		line, _ = strconv.Atoi(m[2])
		col, _ = strconv.Atoi(m[3])
		path = m[1]
	} else if m := lineRe.FindStringSubmatch(s); m != nil {
		line, _ = strconv.Atoi(m[2])
		path = m[1]
	}
	if path == "" {
		return "", 0, 0, fmt.Errorf("empty path")
	}
	path = expandHome(path)
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", 0, 0, err
	}
	return abs, line, col, nil
}

func cmdOpen(args []string) {
	if len(args) != 1 {
		die("usage: forseti open <path[:line[:col]]>")
	}
	cfg := loadConfig()
	path, line, col, err := parseTarget(args[0])
	if err != nil {
		die("%v", err)
	}
	if _, err := os.Stat(path); err != nil {
		die("no such file: %s", path)
	}
	if err := tttJump(cfg.TTT.Port, tttPluginDir(), path, line, col); err != nil {
		die("%v", err)
	}
	pos := ""
	if line > 0 {
		pos = fmt.Sprintf(":%d", line)
		if col > 0 {
			pos += fmt.Sprintf(":%d", col)
		}
	}
	fmt.Printf("opened %s%s in ttt\n", path, pos)
}
