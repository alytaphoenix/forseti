package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func tttListening(port int) bool {
	c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 250*time.Millisecond)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

// tttExec POSTs one exec script to ttt's --listen server.
func tttExec(port int, script string) (string, error) {
	url := fmt.Sprintf("http://127.0.0.1:%d/exec", port)
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Post(url, "text/plain", strings.NewReader(script))
	if err != nil {
		return "", fmt.Errorf("ttt /exec unreachable (is ttt running with --listen?): %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return string(body), fmt.Errorf("ttt /exec: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return string(body), nil
}

// tttJump writes the plugin hand-off and dispatches Forseti: Jump.
func tttJump(port int, pluginDir, path string, line, col int) error {
	if !tttListening(port) {
		return fmt.Errorf("ttt is not listening on 127.0.0.1:%d — run `forseti up`", port)
	}
	req := map[string]any{"path": path}
	if line > 0 {
		req["line"] = line
	}
	if col > 0 {
		req["col"] = col
	}
	data, err := json.Marshal(req)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(pluginDir, 0o755); err != nil {
		return err
	}
	tmp := filepath.Join(pluginDir, ".jump.json.tmp")
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, filepath.Join(pluginDir, "jump.json")); err != nil {
		return err
	}
	_, err = tttExec(port, `exec "Forseti: Jump"`)
	return err
}

// tttHasTerminalAPI probes the binary for the forseti fork's Lua module.
// (strings is only a hint — a false negative is a warning, never a failure.)
func tttHasTerminalAPI(bin string) (found bool, checked bool) {
	path, err := lookPath(bin)
	if err != nil {
		return false, false
	}
	if !commandExists("strings") {
		return false, false
	}
	out, err := runCapture("", nil, "strings", path)
	if err != nil {
		return false, false
	}
	return strings.Contains(out, "ttt.terminal"), true
}
