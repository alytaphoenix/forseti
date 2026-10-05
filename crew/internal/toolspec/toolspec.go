// Package toolspec defines the forseti tool-spec format (Phase 9): a YAML
// file per tool in ~/.config/forseti/tools.d/, served to MCP-speaking agents
// by the forseti-tools shim. A tool without a probe is a draft.
//
// Spec shape:
//
//	name: current_time_utc
//	description: "Current UTC time"
//	parameters:            # JSON Schema for tools/list inputSchema
//	  type: object
//	  properties: {}
//	executor:
//	  type: shell          # shell | http
//	  run: "date -u +%FT%TZ"   # shell: text/template, {{ .field }} = arg
//	timeout_ms: 5000
//	probe:                 # smoke check (validated unless skip)
//	  args: {}
//	  expect_exit: 0
//	  expect_contains: "T"
package toolspec

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"text/template"
	"time"

	"gopkg.in/yaml.v3"
)

var nameRule = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)

type Spec struct {
	Name        string         `yaml:"name"`
	Description string         `yaml:"description"`
	Parameters  map[string]any `yaml:"parameters"` // JSON Schema (object type)
	Executor    Executor       `yaml:"executor"`
	TimeoutMS   int            `yaml:"timeout_ms,omitempty"`
	Probe       *Probe         `yaml:"probe,omitempty"`
}

type Executor struct {
	Type    string            `yaml:"type"` // shell | http
	Run     string            `yaml:"run,omitempty"`
	URL     string            `yaml:"url,omitempty"`
	Method  string            `yaml:"method,omitempty"`
	Headers map[string]string `yaml:"headers,omitempty"`
	Body    string            `yaml:"body,omitempty"`
}

type Probe struct {
	Skip           bool           `yaml:"skip,omitempty"`
	Args           map[string]any `yaml:"args"`
	ExpectExit     int            `yaml:"expect_exit"`
	ExpectContains string         `yaml:"expect_contains,omitempty"`
}

// Dir resolves the spec directory (FORSETI_TOOLS_DIR override).
func Dir() string {
	if d := os.Getenv("FORSETI_TOOLS_DIR"); d != "" {
		return d
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "tools.d"
	}
	return filepath.Join(home, ".config", "forseti", "tools.d")
}

// LoadDir loads every *.yaml/*.yml in dir (sorted by name).
// P13-B24: duplicate tool names across files are rejected (tools/list used
// to advertise the tool twice; find silently picked the first).
func LoadDir(dir string) ([]*Spec, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // no tools yet — valid, empty
		}
		return nil, err
	}
	var out []*Spec
	seen := map[string]string{}
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || (!strings.HasSuffix(n, ".yaml") && !strings.HasSuffix(n, ".yml")) {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, n))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", n, err)
		}
		s, err := Load(raw)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", n, err)
		}
		if prev, dup := seen[s.Name]; dup {
			return nil, fmt.Errorf("duplicate tool name %q in %s and %s", s.Name, prev, n)
		}
		seen[s.Name] = n
		out = append(out, s)
	}
	return out, nil
}

// Load parses + validates one spec.
func Load(data []byte) (*Spec, error) {
	var s Spec
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(false)
	if err := dec.Decode(&s); err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	if err := s.Validate(); err != nil {
		return nil, err
	}
	return &s, nil
}

func (s *Spec) Validate() error {
	if !nameRule.MatchString(s.Name) {
		return fmt.Errorf("name %q must match [a-z][a-z0-9_]{0,31}", s.Name)
	}
	if strings.TrimSpace(s.Description) == "" {
		return fmt.Errorf("empty description (the model reads it in tools/list)")
	}
	if s.Parameters == nil {
		s.Parameters = map[string]any{"type": "object", "properties": map[string]any{}}
	}
	schema := s.Parameters
	if t, _ := schema["type"].(string); t != "object" {
		return fmt.Errorf("parameters.type must be \"object\" (MCP tools take an arguments object)")
	}
	// P13-B24: properties must be a MAP whenever present — a non-map only
	// surfaced (as a silent nil) when a probe declared args.
	if p, ok := schema["properties"]; ok {
		if _, ok := p.(map[string]any); !ok {
			return fmt.Errorf("parameters.properties must be a map")
		}
	}
	switch s.Executor.Type {
	case "shell":
		if strings.TrimSpace(s.Executor.Run) == "" {
			return fmt.Errorf("executor.run required for shell")
		}
		// P13-B13: a shell template referencing a declared param OUTSIDE
		// {{ q .param }} is an injection hole — the model supplies the value;
		// refuse it at Load so quoting is the only path for parameters.
		if err := lintUnquotedParams(s.Executor.Run, s.declaredParams()); err != nil {
			return fmt.Errorf("executor.run: %w", err)
		}
	case "http":
		if strings.TrimSpace(s.Executor.URL) == "" {
			return fmt.Errorf("executor.url required for http")
		}
		if s.Probe != nil && !s.Probe.Skip && s.Probe.ExpectExit != 0 {
			// P13-B24: the http executor checks status only — expect_exit is
			// silently meaningless and misleads the author
			return fmt.Errorf("probe.expect_exit is meaningless for http executors (expect a 2xx status + expect_contains)")
		}
	default:
		return fmt.Errorf("executor.type %q unsupported (v1: shell|http)", s.Executor.Type)
	}
	if s.TimeoutMS == 0 {
		s.TimeoutMS = 5000
	}
	if s.Probe != nil && !s.Probe.Skip {
		// probe args must satisfy the schema's declared properties (shallow)
		props, _ := schema["properties"].(map[string]any)
		for k := range s.Probe.Args {
			if _, ok := props[k]; !ok {
				return fmt.Errorf("probe.args key %q not declared in parameters.properties", k)
			}
		}
	}
	return nil
}

// shQuote single-quotes a value for safe shell interpolation.
func shQuote(v string) string {
	return "'" + strings.ReplaceAll(v, "'", `'\''`) + "'"
}

// declaredParams returns the declared parameter names (P13-B13).
func (s *Spec) declaredParams() []string {
	props, _ := s.Parameters["properties"].(map[string]any)
	out := make([]string, 0, len(props))
	for k := range props {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// lintUnquotedParams refuses shell templates that reference a declared
// parameter outside {{ q .param }} (P13-B13): the model supplies the value,
// so an unquoted {{ .repo }} with repo="; rm -rf ~" is arbitrary execution.
// A bare {{ .param }} (no function call) is exactly that hole.
func lintUnquotedParams(tmpl string, params []string) error {
	for _, p := range params {
		// matches {{ .p }} / {{.p}} but NOT {{ q .p }}
		re := regexp.MustCompile(`\{\{\s*\.(` + regexp.QuoteMeta(p) + `)\s*\}\}`)
		if re.MatchString(tmpl) {
			return fmt.Errorf("parameter %q is interpolated UNQUOTED ({{ .%s }}) — use {{ q .%s }}; the model supplies this value", p, p, p)
		}
	}
	return nil
}

// ValidateArgs checks model-supplied call arguments against the declared
// properties (P13-B13): declared types are enforced and undeclared keys are
// rejected — args used to flow into the executor unchecked.
func (s *Spec) ValidateArgs(args map[string]any) error {
	props, ok := s.Parameters["properties"].(map[string]any)
	if !ok {
		props = map[string]any{}
	}
	for k, v := range args {
		p, ok := props[k]
		if !ok {
			return fmt.Errorf("argument %q is not declared in parameters", k)
		}
		spec, _ := p.(map[string]any)
		want, _ := spec["type"].(string)
		if want == "" {
			continue
		}
		got := ""
		switch v.(type) {
		case string:
			got = "string"
		case bool:
			got = "boolean"
		case float64, int:
			got = "number"
		case []any:
			got = "array"
		case map[string]any:
			got = "object"
		}
		if want != got {
			return fmt.Errorf("argument %q must be %s, got %s", k, want, got)
		}
	}
	return nil
}

// expand renders a text/template with the args map; funcs: q (shell-quote).
func expand(tmpl string, args map[string]any) (string, error) {
	t, err := template.New("x").Funcs(template.FuncMap{"q": shQuote}).Parse(tmpl)
	if err != nil {
		return "", fmt.Errorf("executor template: %w", err)
	}
	if args == nil {
		args = map[string]any{}
	}
	var b bytes.Buffer
	if err := t.Execute(&b, args); err != nil {
		return "", fmt.Errorf("executor render: %w", err)
	}
	return b.String(), nil
}

// CallResult is one executor invocation.
type CallResult struct {
	ExitCode int
	Stdout   string
	Stderr   string
	Status   int    // http status (http executor)
	Body     string // http body
}

// Call runs the executor with the given arguments.
func (s *Spec) Call(ctx context.Context, args map[string]any) (*CallResult, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Duration(s.TimeoutMS)*time.Millisecond)
	defer cancel()
	switch s.Executor.Type {
	case "shell":
		run, err := expand(s.Executor.Run, args)
		if err != nil {
			return nil, err
		}
		cmd := exec.CommandContext(ctx, "/bin/sh", "-c", run)
		var out, errb bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &errb
		err = cmd.Run()
		res := &CallResult{Stdout: out.String(), Stderr: errb.String()}
		if exitErr, ok := err.(*exec.ExitError); ok {
			res.ExitCode = exitErr.ExitCode()
			return res, nil
		}
		if err != nil {
			if ctx.Err() != nil {
				return nil, fmt.Errorf("timeout after %dms", s.TimeoutMS)
			}
			return nil, err
		}
		return res, nil
	case "http":
		url, err := expand(s.Executor.URL, args)
		if err != nil {
			return nil, err
		}
		method := s.Executor.Method
		if method == "" {
			method = "GET"
		}
		var rdr *bytes.Reader
		if s.Executor.Body != "" {
			body, err := expand(s.Executor.Body, args)
			if err != nil {
				return nil, err
			}
			rdr = bytes.NewReader([]byte(body))
		} else {
			rdr = bytes.NewReader(nil)
		}
		req, err := http.NewRequestWithContext(ctx, method, url, rdr)
		if err != nil {
			return nil, err
		}
		for k, v := range s.Executor.Headers {
			hv, err := expand(v, args)
			if err != nil {
				return nil, err
			}
			req.Header.Set(k, hv)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, fmt.Errorf("timeout after %dms", s.TimeoutMS)
			}
			return nil, err
		}
		defer resp.Body.Close()
		var b bytes.Buffer
		_, _ = b.ReadFrom(resp.Body)
		return &CallResult{Status: resp.StatusCode, Body: b.String()}, nil
	default:
		return nil, fmt.Errorf("executor.type %q unsupported", s.Executor.Type)
	}
}

// RunProbe executes the spec's probe (when present) and checks expectations.
func (s *Spec) RunProbe(ctx context.Context) (string, error) {
	if s.Probe == nil {
		return "", fmt.Errorf("no probe — the tool is a DRAFT (add probe: {args, expect_exit, expect_contains})")
	}
	if s.Probe.Skip {
		return "skipped", nil
	}
	res, err := s.Call(ctx, s.Probe.Args)
	if err != nil {
		return "", err
	}
	combined := res.Stdout + res.Stderr
	if s.Executor.Type == "http" {
		// P13-B7: the http executor fills Body only — the old combined
		// (stdout+stderr) was always empty, so any expect_contains failed
		combined = res.Body
		if res.Status < 200 || res.Status > 299 {
			return "", fmt.Errorf("probe status %d: %s", res.Status, trunc(combined))
		}
	} else if res.ExitCode != s.Probe.ExpectExit {
		return "", fmt.Errorf("probe exit %d (want %d): %s", res.ExitCode, s.Probe.ExpectExit, trunc(combined))
	}
	if s.Probe.ExpectContains != "" && !strings.Contains(combined, s.Probe.ExpectContains) {
		return "", fmt.Errorf("probe output missing %q: %s", s.Probe.ExpectContains, trunc(combined))
	}
	return trunc(combined), nil
}

func trunc(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 300 {
		return s[:300] + "…"
	}
	return s
}
