// forseti-tools — the Phase 9 MCP shim: serves every tool spec in
// ~/.config/forseti/tools.d/ to any MCP-speaking agent over stdio, plus a
// CLI surface for validation/probing/eval (the tool builder's gates).
//
//	forseti-tools serve              # MCP stdio server (pi spawns this)
//	forseti-tools validate           # load + validate + run probes
//	forseti-tools call NAME ARGS     # direct executor invocation
//	forseti-tools eval               # probe sets from crew/eval/tools.d/
//	forseti-tools doctor             # registration state (pi mcp.json)
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"forseti/crew/internal/toolspec"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "serve":
		cmdServe()
	case "validate":
		cmdValidate()
	case "call":
		cmdCall(os.Args[2:])
	case "eval":
		cmdEval()
	case "doctor":
		cmdDoctor()
	case "-h", "--help", "help":
		usage()
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `forseti-tools — declarative tool specs served over MCP

usage:
  forseti-tools serve                    # MCP stdio server (pi spawns this)
  forseti-tools validate                 # validate all specs + run probes
  forseti-tools call NAME [JSON-ARGS]    # direct executor call
  forseti-tools eval                     # probe sets in crew/eval/tools.d/
  forseti-tools doctor                   # pi registration state`)
}

func loadAll() ([]*toolspec.Spec, error) {
	return toolspec.LoadDir(toolspec.Dir())
}

// ---- serve (MCP stdio, NDJSON JSON-RPC) ------------------------------------
// S20 wire facts (pi's bundled client): newline-delimited JSON; initialize
// result needs protocolVersion + capabilities + serverInfo{name,version};
// tools/list items need name + inputSchema(object); tools/call result needs
// content array; protocol versions 2024-11-05 … 2025-11-25 accepted.

func cmdServe() {
	specs, err := loadAll()
	if err != nil {
		fmt.Fprintln(os.Stderr, "forseti-tools:", err)
		os.Exit(1)
	}
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 1<<20), 1<<20)
	out := bufio.NewWriter(os.Stdout)
	defer out.Flush()

	write := func(v any) {
		b, _ := json.Marshal(v)
		out.Write(b)
		out.WriteByte('\n')
		out.Flush()
	}
	reply := func(id any, result any) {
		write(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
	}
	rpcErr := func(id any, code int, msg string) {
		write(map[string]any{"jsonrpc": "2.0", "id": id,
			"error": map[string]any{"code": code, "message": msg}})
	}

	for in.Scan() {
		line := strings.TrimSpace(in.Text())
		if line == "" {
			continue
		}
		var req struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      json.RawMessage `json:"id"`
			Method  string          `json:"method"`
			Params  json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			rpcErr(nil, -32700, "parse error")
			continue
		}
		id := any(nil)
		if len(req.ID) > 0 {
			_ = json.Unmarshal(req.ID, &id)
		}
		switch req.Method {
		case "initialize":
			var p struct {
				ProtocolVersion string `json:"protocolVersion"`
			}
			_ = json.Unmarshal(req.Params, &p)
			ver := p.ProtocolVersion
			if ver == "" {
				ver = "2024-11-05"
			}
			reply(id, map[string]any{
				"protocolVersion": ver,
				"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
				"serverInfo":      map[string]any{"name": "forseti-tools", "version": "0.1.0"},
			})
		case "notifications/initialized", "notifications/cancelled":
			// notification — no reply
		case "ping":
			reply(id, map[string]any{})
		case "tools/list":
			tools := make([]map[string]any, 0, len(specs))
			for _, s := range specs {
				tools = append(tools, map[string]any{
					"name":        s.Name,
					"description": s.Description,
					"inputSchema": s.Parameters,
				})
			}
			reply(id, map[string]any{"tools": tools})
		case "tools/call":
			var p struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			}
			_ = json.Unmarshal(req.Params, &p)
			spec := find(specs, p.Name)
			if spec == nil {
				rpcErr(id, -32602, "unknown tool: "+p.Name)
				continue
			}
			var args map[string]any
			if len(p.Arguments) > 0 {
				if err := json.Unmarshal(p.Arguments, &args); err != nil {
					rpcErr(id, -32602, "bad arguments: "+err.Error())
					continue
				}
			}
			res, err := spec.Call(context.Background(), args)
			if err != nil {
				reply(id, map[string]any{
					"content": []map[string]any{{"type": "text", "text": "tool error: " + err.Error()}},
					"isError": true,
				})
				continue
			}
			text := ""
			if res.Stdout != "" || res.Stderr != "" {
				text = strings.TrimSpace(res.Stdout)
				if res.Stderr != "" {
					if text != "" {
						text += "\n"
					}
					text += "[stderr] " + strings.TrimSpace(res.Stderr)
				}
			} else if res.Body != "" {
				text = res.Body
			}
			if text == "" {
				text = fmt.Sprintf("ok (exit %d)", res.ExitCode)
			}
			isErr := spec.Executor.Type == "http" && (res.Status < 200 || res.Status > 299)
			if isErr && res.ExitCode == 0 {
				res.ExitCode = 1
			}
			reply(id, map[string]any{
				"content": []map[string]any{{"type": "text", "text": text}},
				"isError": isErr,
			})
		default:
			if len(req.ID) > 0 {
				rpcErr(id, -32601, "method not found: "+req.Method)
			}
		}
	}
}

func find(specs []*toolspec.Spec, name string) *toolspec.Spec {
	for _, s := range specs {
		if s.Name == name {
			return s
		}
	}
	return nil
}

// ---- validate ----------------------------------------------------------------

func cmdValidate() {
	specs, err := loadAll()
	if err != nil {
		fmt.Fprintln(os.Stderr, "INVALID:", err)
		os.Exit(1)
	}
	if len(specs) == 0 {
		fmt.Println("no tool specs in", toolspec.Dir(), "(create one in the crew TUI or by hand)")
		return
	}
	failed := false
	for _, s := range specs {
		if s.Probe == nil {
			fmt.Printf("  DRAFT  %-24s no probe — a tool without a probe is a draft\n", s.Name)
			continue
		}
		out, err := s.RunProbe(context.Background())
		if err != nil {
			failed = true
			fmt.Printf("  FAIL   %-24s %v\n", s.Name, err)
			continue
		}
		fmt.Printf("  PASS   %-24s %s\n", s.Name, out)
	}
	if failed {
		os.Exit(1)
	}
}

// ---- call ----------------------------------------------------------------------

func cmdCall(args []string) {
	if len(args) < 1 {
		usage()
		os.Exit(2)
	}
	specs, err := loadAll()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	spec := find(specs, args[0])
	if spec == nil {
		fmt.Fprintf(os.Stderr, "unknown tool %q (specs in %s)\n", args[0], toolspec.Dir())
		os.Exit(2)
	}
	var argsMap map[string]any
	if len(args) > 1 {
		if err := json.Unmarshal([]byte(strings.Join(args[1:], " ")), &argsMap); err != nil {
			fmt.Fprintln(os.Stderr, "bad args json:", err)
			os.Exit(2)
		}
	}
	res, err := spec.Call(context.Background(), argsMap)
	if err != nil {
		fmt.Fprintln(os.Stderr, "call error:", err)
		os.Exit(1)
	}
	if spec.Executor.Type == "http" {
		if res.Status < 200 || res.Status > 299 {
			fmt.Printf("HTTP %d\n%s", res.Status, res.Body)
			os.Exit(1)
		}
		fmt.Print(res.Body)
		return
	}
	if res.Stderr != "" {
		fmt.Fprint(os.Stderr, res.Stderr)
	}
	if res.ExitCode != 0 {
		os.Exit(res.ExitCode)
	}
	fmt.Print(res.Stdout)
}

// ---- eval ------------------------------------------------------------------

func cmdEval() {
	// probe sets: crew/eval/tools.d/<name>.jsonl — each line
	// {"args": {...}, "expect_contains": "..."} run against the named tool
	cwd, _ := os.Getwd()
	dir := filepath.Join(cwd, "crew", "eval", "tools.d")
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) == 0 {
		fmt.Println("no tool probe sets in", dir, "— nothing to eval")
		return
	}
	specs, err := loadAll()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	failed := false
	for _, e := range entries {
		name := strings.TrimSuffix(e.Name(), filepath.Ext(e.Name()))
		spec := find(specs, name)
		if spec == nil {
			fmt.Printf("  SKIP   %-24s no spec\n", name)
			continue
		}
		n, okCount := 0, 0
		fileFailed := false
		f, err := os.Open(filepath.Join(dir, e.Name()))
		if err != nil {
			failed = true
			continue
		}
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			var p struct {
				Args           map[string]any `json:"args"`
				ExpectContains string         `json:"expect_contains"`
				ExpectExit     int            `json:"expect_exit"`
			}
			if json.Unmarshal(sc.Bytes(), &p) != nil {
				continue
			}
			n++
			res, err := spec.Call(context.Background(), p.Args)
			if err != nil {
				fileFailed = true
				fmt.Printf("  FAIL   %s[%d]: %v\n", name, n, err)
				continue
			}
			combined := res.Stdout + res.Stderr
			if p.ExpectContains != "" && !strings.Contains(combined, p.ExpectContains) {
				fileFailed = true
				fmt.Printf("  FAIL   %s[%d]: missing %q in %q\n", name, n, p.ExpectContains, combined)
				continue
			}
			if res.ExitCode != p.ExpectExit {
				fileFailed = true
				fmt.Printf("  FAIL   %s[%d]: exit %d (want %d)\n", name, n, res.ExitCode, p.ExpectExit)
				continue
			}
			okCount++
		}
		f.Close()
		if fileFailed {
			failed = true
		}
		fmt.Printf("  %-6s %-24s %d/%d probes\n", map[bool]string{true: "PASS", false: "FAIL"}[!fileFailed], name, okCount, n)
	}
	if failed {
		os.Exit(1)
	}
}

// ---- doctor -------------------------------------------------------------------

func cmdDoctor() {
	home, _ := os.UserHomeDir()
	cfg := filepath.Join(home, ".pi", "agent", "mcp.json")
	raw, err := os.ReadFile(cfg)
	if err != nil {
		fmt.Printf("pi mcp.json: ABSENT (%s)\n", cfg)
		fmt.Println("register with: pi mcp add forseti-tools -- <abs>/crew/bin/forseti-tools serve")
		return
	}
	var m struct {
		MCPServers map[string]struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
	}
	_ = json.Unmarshal(raw, &m)
	srv, ok := m.MCPServers["forseti-tools"]
	if !ok {
		fmt.Println("pi mcp.json: present, but no forseti-tools entry")
		return
	}
	fmt.Printf("pi mcp.json: registered (command=%s args=%v)\n", srv.Command, strings.Join(srv.Args, " "))
	fmt.Println("verify with: pi mcp list")
}
