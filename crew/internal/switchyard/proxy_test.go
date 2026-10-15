package switchyard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"forseti/crew/internal/schema"
)

func testProviders() map[string]piProvider {
	return map[string]piProvider{
		"primary": {Name: "Primary", BaseURL: "http://127.0.0.1:1/v1", API: "openai-completions", APIKey: "sk-literal"},
		"free":    {Name: "Free", BaseURL: "http://127.0.0.1:2/v1", API: "openai-completions", APIKey: "!printf sk-from-cmd"},
	}
}

func TestRouterArgs(t *testing.T) {
	host := Router{Bin: "/x/forseti-host", Kind: RouterHost}
	if got := strings.Join(host.Args("/c.toml", "/r.ndjson", 4321), " "); got !=
		"--config /c.toml --addr 127.0.0.1:4321 --routing-log /r.ndjson" {
		t.Fatalf("host args = %q", got)
	}
	server := Router{Bin: "/x/switchyard-server", Kind: RouterServer}
	if got := strings.Join(server.Args("/c.toml", "/r.ndjson", 4321), " "); got !=
		"--config /c.toml --host 127.0.0.1 --port 4321 --routing-log-file /r.ndjson" {
		t.Fatalf("server args = %q", got)
	}
}

func TestResolveRouterKind(t *testing.T) {
	dir := t.TempDir()
	hostBin := filepath.Join(dir, "forseti-host")
	serverBin := filepath.Join(dir, "switchyard-server")
	for _, p := range []string{hostBin, serverBin} {
		if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	t.Setenv("FORSETI_CREW_ROUTER", "")
	t.Setenv("FORSETI_CREW_ROUTER_BIN", hostBin)
	if r, err := ResolveRouter(); err != nil || r.Kind != RouterHost {
		t.Fatalf("host bin → kind %v, err %v", r.Kind, err)
	}

	t.Setenv("FORSETI_CREW_ROUTER_BIN", serverBin)
	if r, err := ResolveRouter(); err != nil || r.Kind != RouterServer {
		t.Fatalf("upstream bin → kind %v, err %v", r.Kind, err)
	}

	// an explicit FORSETI_CREW_ROUTER wins over the basename heuristic
	t.Setenv("FORSETI_CREW_ROUTER", string(RouterServer))
	t.Setenv("FORSETI_CREW_ROUTER_BIN", hostBin)
	if r, err := ResolveRouter(); err != nil || r.Kind != RouterServer {
		t.Fatalf("forced kind → %v, err %v", r.Kind, err)
	}

	t.Setenv("FORSETI_CREW_ROUTER", "")
	t.Setenv("FORSETI_CREW_ROUTER_BIN", filepath.Join(dir, "missing"))
	if _, err := ResolveRouter(); err == nil {
		t.Fatal("want an error for a missing FORSETI_CREW_ROUTER_BIN")
	}
}

func TestBuildConfig(t *testing.T) {
	routes := []schema.Route{{
		ID: "pool", Type: "stage_router", Efficient: "free/weak", Capable: "primary/strong",
		Picker: "capable_first", Confidence: 0.7,
	}}
	toml, env, err := BuildConfig(routes, testProviders(), apiKeyValue)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"schema_version = 1",
		`[llm_clients."free"]`,
		`[llm_clients."primary"]`,
		"format = \"openai_chat\"",
		`api_key_env = "SWITCHYARD_KEY_FREE"`,
		"[targets.pool_eff]", `id = "weak"`, `llm_client = "free"`,
		"[targets.pool_cap]", `id = "strong"`, `llm_client = "primary"`,
		"[routes.pool]", `id = "pool"`, `type = "stage_router"`,
		`capable_target = "pool_cap"`, `efficient_target = "pool_eff"`,
		`picker = "capable_first"`, "confidence_threshold = 0.7",
	} {
		if !strings.Contains(toml, want) {
			t.Fatalf("config missing %q:\n%s", want, toml)
		}
	}
	if strings.Contains(toml, "sk-") {
		t.Fatalf("API key leaked into the config file:\n%s", toml)
	}
	if env["SWITCHYARD_KEY_FREE"] != "sk-from-cmd" || env["SWITCHYARD_KEY_PRIMARY"] != "sk-literal" {
		t.Fatalf("env = %v", env)
	}
}

func TestBuildConfigMissingProvider(t *testing.T) {
	routes := []schema.Route{{ID: "pool", Efficient: "ghost/weak", Capable: "ghost/strong"}}
	if _, _, err := BuildConfig(routes, testProviders(), apiKeyValue); err == nil || !strings.Contains(err.Error(), "ghost") {
		t.Fatalf("err = %v, want a missing-provider error", err)
	}
}

func TestBuildConfigOptionsRenderedAsTOML(t *testing.T) {
	routes := []schema.Route{{
		ID: "pool", Type: "stage_router", Efficient: "free/weak", Capable: "primary/strong",
		Options: map[string]any{
			"capable_hold_turns": 2,
			"tool_semantics":     map[string]any{"new": []any{"send_message"}},
		},
	}}
	toml, _, err := BuildConfig(routes, testProviders(), apiKeyValue)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"capable_hold_turns = 2",
		`tool_semantics = {"new" = ["send_message"]}`,
	} {
		if !strings.Contains(toml, want) {
			t.Fatalf("config missing %q:\n%s", want, toml)
		}
	}
}

func TestTomlValue(t *testing.T) {
	cases := []struct {
		in   any
		want string
	}{
		{`a"b`, `"a\"b"`},
		{true, "true"},
		{2, "2"},
		{1.0, "1.0"},
		{0.5, "0.5"},
		{[]any{"a", "b"}, `["a", "b"]`},
		{map[string]any{"b": 1, "a": "x"}, `{"a" = "x", "b" = 1}`},
	}
	for _, tc := range cases {
		got, err := tomlValue(tc.in)
		if err != nil {
			t.Fatalf("tomlValue(%v): %v", tc.in, err)
		}
		if got != tc.want {
			t.Fatalf("tomlValue(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
	if _, err := tomlValue(nil); err == nil {
		t.Fatal("nil should not be representable in TOML")
	}
}

// TestRouterAcceptsGeneratedConfig is the integration gate for 18C: every route
// type forseti can emit (plus option-driven shapes) must pass the router's own
// `--dry-run` validation. Skips when no router binary is available.
func TestRouterAcceptsGeneratedConfig(t *testing.T) {
	router, err := ResolveRouter()
	if err != nil {
		t.Skipf("no router binary available: %v", err)
	}
	routes := []schema.Route{
		{ID: "pool", Type: "stage_router", Efficient: "free/weak", Capable: "primary/strong",
			Options: map[string]any{"capable_hold_turns": 2, "tool_semantics": map[string]any{"new": []any{"send_message"}}}},
		{ID: "plan", Type: "plan_execute", Efficient: "free/weak2", Capable: "primary/strong2"},
		{ID: "auto", Type: "auto", Efficient: "free/weak2b", Capable: "primary/strong2b"},
		{ID: "one", Type: "passthrough", Capable: "primary/strong3"},
		{ID: "many", Type: "random", Efficient: "free/weak4", Capable: "primary/strong4"},
		{ID: "judged", Type: "llm_classifier", Efficient: "free/weak5", Capable: "primary/strong5", Judge: "free/weak5"},
		{ID: "off", Type: "noop"},
		{ID: "advice", Type: "advisor",
			Options: map[string]any{"executor_target": "one_cap", "advisor_target": "many_cap", "advisor_max_tokens": 256}},
	}
	toml, env, err := BuildConfig(routes, testProviders(), apiKeyValue)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	cfg := filepath.Join(dir, "switchyard.toml")
	if err := os.WriteFile(cfg, []byte(toml), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := router.Validate(cfg, env); err != nil {
		t.Fatalf("%s rejected the generated config: %v\n%s", router.Kind, err, toml)
	}
}
