// Package switchyard manages the per-run routing process: the crew's agents
// speak to a switchyard route pool, and the crew supervises the router for the
// duration of a run.
//
// Phase 18: the default router is our own Rust host (`host/`, binary
// `forseti-host`) which embeds switchyard as a library — no separate server
// process, no cargo-installed binary. `switchyard-server` remains a fallback
// (FORSETI_CREW_ROUTER=switchyard-server).
//
// Facts (S14/S15/S28, docs/spikes.md):
//   - switchyard 0.3.0 route types: stage_router | plan_execute |
//     llm_classifier | passthrough | random | noop | composite | advisor |
//     prefill_router (switchyard-runner/src/algorithm.rs AlgorithmSpec).
//   - TOML: schema_version=1, [llm_clients.X] format/base_url/api_key_env?,
//     [targets.X] id/llm_client, [routes.X] id/type/<type keys>.
//   - readiness: GET /health → {"status":"ok"}; GET /v1/stats counters.
//   - routing log: NDJSON per request (`--routing-log` host /
//     `--routing-log-file` server).
//   - config validation: `--config X --dry-run` on both binaries.
//   - pi provider entry: api openai-completions, model ids = route ids.
package switchyard

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"forseti/crew/internal/schema"
)

// RouterKind identifies the routing process the crew supervises.
type RouterKind string

const (
	// RouterHost is our own Rust host (`host/`, binary `forseti-host`): it
	// embeds switchyard as a library, so nothing is spawned from cargo's bin dir.
	RouterHost RouterKind = "forseti-host"
	// RouterServer is the upstream `switchyard-server` (cargo-installed).
	RouterServer RouterKind = "switchyard-server"
)

// Router is the resolved routing binary plus how to talk to it.
type Router struct {
	Bin  string
	Kind RouterKind
}

// ResolveRouter finds the routing process:
//
//	FORSETI_CREW_ROUTER_BIN (explicit path; kind from the basename unless
//	                         FORSETI_CREW_ROUTER forces it)
//	→ forseti-host built in this repo (host/target/release, then debug)
//	→ forseti-host on $PATH
//	→ FORSETI_SWITCHYARD_BIN / ~/.cargo/bin/switchyard-server / $PATH (legacy)
//
// Set FORSETI_CREW_ROUTER=switchyard-server to force the upstream binary.
func ResolveRouter() (Router, error) {
	forced := os.Getenv("FORSETI_CREW_ROUTER")
	if p := os.Getenv("FORSETI_CREW_ROUTER_BIN"); p != "" {
		if _, err := os.Stat(p); err != nil {
			return Router{}, fmt.Errorf("FORSETI_CREW_ROUTER_BIN=%s: %w", p, err)
		}
		return Router{Bin: p, Kind: routerKindOf(forced, filepath.Base(p))}, nil
	}
	if forced != string(RouterServer) {
		for _, p := range candidateHostBins() {
			if _, err := os.Stat(p); err == nil {
				return Router{Bin: p, Kind: RouterHost}, nil
			}
		}
		if p, err := exec.LookPath(string(RouterHost)); err == nil {
			return Router{Bin: p, Kind: RouterHost}, nil
		}
	}
	bin, err := resolveSwitchyardServer()
	if err != nil {
		return Router{}, fmt.Errorf("no router found — build ours (cargo build --release in host/) or cargo install switchyard-server, or set FORSETI_CREW_ROUTER_BIN: %w", err)
	}
	return Router{Bin: bin, Kind: RouterServer}, nil
}

func routerKindOf(forced, base string) RouterKind {
	switch RouterKind(forced) {
	case RouterHost, RouterServer:
		return RouterKind(forced)
	}
	if strings.Contains(base, string(RouterServer)) {
		return RouterServer
	}
	return RouterHost
}

// candidateHostBins locates our forseti-host build by walking up from the
// working directory to the repo root (host/Cargo.toml).
func candidateHostBins() []string {
	var out []string
	if p := os.Getenv("FORSETI_HOST_BIN"); p != "" {
		out = append(out, p)
	}
	if wd, err := os.Getwd(); err == nil {
		if root := findRepoRoot(wd); root != "" {
			out = append(out,
				filepath.Join(root, "host", "target", "release", string(RouterHost)),
				filepath.Join(root, "host", "target", "debug", string(RouterHost)))
		}
	}
	return out
}

// ResolveHostBin locates our Rust host binary (the engine's second
// implementation): FORSETI_HOST_BIN → host/target/{release,debug} built in this
// repo → $PATH. `crew "run|validate" --engine rust` delegates to it.
func ResolveHostBin() (string, error) {
	for _, p := range candidateHostBins() {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	if p, err := exec.LookPath(string(RouterHost)); err == nil {
		return p, nil
	}
	return "", fmt.Errorf("forseti-host not found — build host/ (cargo build --release) or set FORSETI_HOST_BIN")
}

func findRepoRoot(dir string) string {
	for {
		if _, err := os.Stat(filepath.Join(dir, "host", "Cargo.toml")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

func resolveSwitchyardServer() (string, error) {
	if p := os.Getenv("FORSETI_SWITCHYARD_BIN"); p != "" {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		p := filepath.Join(home, ".cargo", "bin", string(RouterServer))
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return exec.LookPath(string(RouterServer))
}

// Args is the router's command line (pure — unit-tested).
func (r Router) Args(configPath, logPath string, port int) []string {
	if r.Kind == RouterHost {
		return []string{"--config", configPath,
			"--addr", fmt.Sprintf("127.0.0.1:%d", port),
			"--routing-log", logPath}
	}
	return []string{"--config", configPath,
		"--host", "127.0.0.1", "--port", fmt.Sprint(port),
		"--routing-log-file", logPath}
}

// Validate runs the router's own dry-run config check, so a bad routes file
// fails with the router's message instead of "exited early". Both binaries
// accept `--config X --dry-run` (S28).
func (r Router) Validate(configPath string, env map[string]string) error {
	args := []string{"--config", configPath, "--dry-run"}
	cmd := exec.Command(r.Bin, args...)
	cmd.Env = append(os.Environ(), mapToEnv(env)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s --dry-run: %s: %w", r.Kind, strings.TrimSpace(string(out)), err)
	}
	return nil
}

// piProvider is the subset of a pi models.json provider entry we need to
// build switchyard llm_clients (secrets stay out of the TOML — they are
// passed as env vars at spawn time).
type piProvider struct {
	Name    string `json:"name"`
	BaseURL string `json:"baseUrl"`
	APIKey  string `json:"apiKey"`
	API     string `json:"api"`
}

func piModelsPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".pi", "agent", "models.json"), nil
}

// apiKeyValue resolves a pi apiKey value: "!cmd …" runs through sh -c
// (pi's models.json convention), anything else is literal.
func apiKeyValue(v string) (string, error) {
	if !strings.HasPrefix(v, "!") {
		return v, nil
	}
	cmd := exec.Command("sh", "-c", v[1:])
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("resolve !apiKey: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// Proxy is one supervised router process for a run.
type Proxy struct {
	Bin        string
	Kind       RouterKind
	Port       int
	ConfigPath string
	RoutingLog string
	ServerLog  string
	Env        map[string]string // extra env (resolved API keys) for the process

	cmd *exec.Cmd
}

// routeProviders returns the sorted, unique pi providers referenced by routes.
func routeProviders(routes []schema.Route) []string {
	seen := map[string]bool{}
	var out []string
	for _, r := range routes {
		for _, m := range []string{r.Efficient, r.Capable, r.Judge} {
			if m == "" {
				continue
			}
			p := providerOf(m)
			if !seen[p] {
				seen[p] = true
				out = append(out, p)
			}
		}
	}
	sort.Strings(out)
	return out
}

// modelTarget splits a "provider/model" reference into its two halves.
func modelTarget(ref string) (provider, model string) {
	if i := strings.Index(ref, "/"); i >= 0 {
		return ref[:i], ref[i+1:]
	}
	return "", ref
}

// BuildConfig renders the routes TOML for a crew's routes. API keys never enter
// the file: each referenced provider's key is resolved through `key` and
// returned in the env map as SWITCHYARD_KEY_<PROVIDER>.
//
// The route table itself comes from schema.Route.RouteKeys, so forseti emits
// only the keys it can derive and passes `options` through unchanged — a
// switchyard release with new route types/keys needs no forseti change.
func BuildConfig(routes []schema.Route, provs map[string]piProvider, key func(string) (string, error)) (string, map[string]string, error) {
	env := map[string]string{}
	var clients strings.Builder
	for _, p := range routeProviders(routes) {
		prov, ok := provs[p]
		if !ok {
			return "", nil, fmt.Errorf("provider %q not in ~/.pi/agent/models.json", p)
		}
		// P13-B22: quoted table keys — a provider id containing '.' (e.g.
		// "open.ai") silently became a NESTED TOML table and corrupted the config
		fmt.Fprintf(&clients, "\n[llm_clients.%q]\nformat = \"openai_chat\"\nbase_url = %q\n", p, prov.BaseURL)
		if prov.APIKey != "" {
			val, err := key(prov.APIKey)
			if err != nil {
				return "", nil, fmt.Errorf("provider %s: %w", p, err)
			}
			k := switchyardKeyEnv(p)
			env[k] = val
			fmt.Fprintf(&clients, "api_key_env = %q\n", k)
		}
	}

	var targets, routesT strings.Builder
	for _, r := range routes {
		effName, capName := r.TargetNames()
		if r.Efficient != "" {
			pv, m := modelTarget(r.Efficient)
			fmt.Fprintf(&targets, "\n[targets.%s]\nid = %q\nllm_client = %q\n", effName, m, pv)
		}
		if r.Capable != "" {
			pv, model := modelTarget(r.Capable)
			fmt.Fprintf(&targets, "\n[targets.%s]\nid = %q\nllm_client = %q\n", capName, model, pv)
		}
		if r.Judge != "" {
			pv, model := modelTarget(r.Judge)
			fmt.Fprintf(&targets, "\n[targets.%s]\nid = %q\nllm_client = %q\n", r.JudgeTarget(), model, pv)
		}
		keys, err := r.RouteKeys()
		if err != nil {
			return "", nil, err
		}
		fmt.Fprintf(&routesT, "\n[routes.%s]\nid = %q\ntype = %q\n", r.ID, r.ID, r.EffectiveType())
		for _, k := range sortedKeys(keys) {
			v, err := tomlValue(keys[k])
			if err != nil {
				return "", nil, fmt.Errorf("route %q: %s: %w", r.ID, k, err)
			}
			fmt.Fprintf(&routesT, "%s = %s\n", k, v)
		}
	}
	return "schema_version = 1\n" + clients.String() + targets.String() + routesT.String(), env, nil
}

// switchyardKeyEnv is the env var name that carries a provider's API key to the
// router process (never into the TOML).
func switchyardKeyEnv(provider string) string {
	return "SWITCHYARD_KEY_" + strings.ToUpper(strings.ReplaceAll(strings.ReplaceAll(provider, "-", "_"), ".", "_"))
}

func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// tomlValue renders a Go value as a TOML value: scalars, arrays, and inline
// tables for maps (switchyard's nested config — tool_semantics, classifier,
// subagents — deserializes from inline tables identically to sub-tables).
func tomlValue(v any) (string, error) {
	switch t := v.(type) {
	case string:
		return strconv.Quote(t), nil
	case bool:
		return strconv.FormatBool(t), nil
	case int:
		return strconv.Itoa(t), nil
	case int64:
		return strconv.FormatInt(t, 10), nil
	case float64:
		return tomlFloat(t), nil
	case nil:
		return "", fmt.Errorf("null is not representable in TOML")
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Slice, reflect.Array:
		parts := make([]string, 0, rv.Len())
		for i := 0; i < rv.Len(); i++ {
			s, err := tomlValue(rv.Index(i).Interface())
			if err != nil {
				return "", err
			}
			parts = append(parts, s)
		}
		return "[" + strings.Join(parts, ", ") + "]", nil
	case reflect.Map:
		keys := make([]string, 0, rv.Len())
		for _, k := range rv.MapKeys() {
			if k.Kind() != reflect.String {
				return "", fmt.Errorf("TOML table keys must be strings, got %s", k.Kind())
			}
			keys = append(keys, k.String())
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			s, err := tomlValue(rv.MapIndex(reflect.ValueOf(k)).Interface())
			if err != nil {
				return "", err
			}
			parts = append(parts, strconv.Quote(k)+" = "+s)
		}
		return "{" + strings.Join(parts, ", ") + "}", nil
	}
	return "", fmt.Errorf("%T is not representable in TOML", v)
}

// tomlFloat keeps a float a float: TOML reads a bare `1` as an integer, which
// serde rejects for an f64 field (e.g. confidence_threshold = 1).
func tomlFloat(f float64) string {
	s := strconv.FormatFloat(f, 'g', -1, 64)
	if !strings.ContainsAny(s, ".eE") {
		s += ".0"
	}
	return s
}

// piProviders reads forseti's provider registry out of pi's models.json (S15:
// that is where the crew's provider/model strings are resolved).
func piProviders() (map[string]piProvider, error) {
	modelsPath, err := piModelsPath()
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(modelsPath)
	if err != nil {
		return nil, fmt.Errorf("read pi models.json: %w", err)
	}
	var reg struct {
		Providers map[string]piProvider `json:"providers"`
	}
	if err := json.Unmarshal(raw, &reg); err != nil {
		return nil, fmt.Errorf("parse pi models.json: %w", err)
	}
	return reg.Providers, nil
}

// DryRunRoutes validates a crew's routes with the router's own `--dry-run`,
// without binding a port or resolving real API keys — `crew validate` uses it so
// a bad route shape fails at validation time with the router's message.
//
// Providers that are not in pi's models.json are substituted with a placeholder
// endpoint (the dry-run only parses the config), which keeps validation
// independent of this machine's provider registry. Returns a human note when no
// dry-run could run (no router binary / no readable models.json).
func DryRunRoutes(routes []schema.Route) (string, error) {
	if len(routes) == 0 {
		return "", nil
	}
	router, err := ResolveRouter()
	if err != nil {
		return "routes not dry-run: " + err.Error(), nil
	}
	provs := map[string]piProvider{}
	if real, err := piProviders(); err == nil {
		provs = real
	}
	for _, p := range routeProviders(routes) {
		if _, ok := provs[p]; !ok {
			provs[p] = piProvider{Name: p, BaseURL: "http://127.0.0.1:1/v1"}
		}
	}
	// The router refuses an empty `api_key_env`, but a dry-run never dials a
	// provider: resolve the real key when the local registry has one, else use
	// a stub so validation stays independent of this machine's key files.
	dryRunKey := func(ref string) (string, error) {
		if v, err := apiKeyValue(ref); err == nil && strings.TrimSpace(v) != "" {
			return v, nil
		}
		return "forseti-dry-run", nil
	}
	toml, env, err := BuildConfig(routes, provs, dryRunKey)
	if err != nil {
		return "", err
	}
	dir, err := os.MkdirTemp("", "forseti-routes-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	cfg := filepath.Join(dir, "switchyard.toml")
	if err := os.WriteFile(cfg, []byte(toml), 0o600); err != nil {
		return "", err
	}
	if err := router.Validate(cfg, env); err != nil {
		return "", err
	}
	return "", nil
}

// Start generates the TOML from the crew's routes, picks a free port, spawns
// the router, dry-run-validates the config, and waits for /health. Secrets
// never appear in the TOML: each referenced pi provider's key is resolved once
// and passed as SWITCHYARD_KEY_<PROVIDER> in the process env.
//
// routingLog is the per-run JSONL path (unique per run — the tailer reads from
// offset 0, so a shared filename would replay earlier runs' records).
func Start(routes []schema.Route, runDir, routingLog string) (*Proxy, error) {
	router, err := ResolveRouter()
	if err != nil {
		return nil, err
	}

	// resolve referenced providers from pi's models.json (S15: that's where
	// forseti's provider/model strings live)
	providers, err := piProviders()
	if err != nil {
		return nil, err
	}

	toml, env, err := BuildConfig(routes, providers, apiKeyValue)
	if err != nil {
		return nil, err
	}

	port, err := freePort()
	if err != nil {
		return nil, err
	}
	p := &Proxy{
		Bin:        router.Bin,
		Kind:       router.Kind,
		Port:       port,
		ConfigPath: filepath.Join(runDir, "switchyard.toml"),
		RoutingLog: filepath.Join(runDir, routingLog),
		ServerLog:  filepath.Join(runDir, "router.log"),
		Env:        env,
	}
	if err := os.WriteFile(p.ConfigPath, []byte(toml), 0o600); err != nil {
		return nil, err
	}

	// pre-flight: let the router itself reject a bad routes file, with its own
	// message, before we bind a port and blame "exited early" (S28 --dry-run).
	if err := router.Validate(p.ConfigPath, env); err != nil {
		return nil, err
	}

	cmd := exec.Command(router.Bin, router.Args(p.ConfigPath, p.RoutingLog, port)...)
	cmd.Env = append(os.Environ(), mapToEnv(env)...)
	logf, err := os.Create(p.ServerLog)
	if err != nil {
		return nil, err
	}
	defer logf.Close()
	cmd.Stdout = logf
	cmd.Stderr = logf
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	p.cmd = cmd

	// readiness: /health within 15 s
	deadline := time.Now().Add(15 * time.Second)
	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	for time.Now().Before(deadline) {
		if p.alive() {
			resp, err := hc.Get(base + "/health")
			if err == nil {
				resp.Body.Close()
				if resp.StatusCode == 200 {
					return p, nil
				}
			}
		} else {
			return nil, fmt.Errorf("%s exited early; log: %s", p.Kind, p.ServerLog)
		}
		time.Sleep(150 * time.Millisecond)
	}
	p.Stop()
	return nil, fmt.Errorf("%s not healthy within 15s (port %d); log: %s", p.Kind, port, p.ServerLog)
}

func (p *Proxy) alive() bool {
	return p.cmd != nil && p.cmd.ProcessState == nil
}

// ProviderEntry is the pi models.json provider for this proxy. One model
// entry per route id (upstream pi integration requirement).
func (p *Proxy) ProviderEntry(routeIDs []string) map[string]any {
	models := make([]map[string]any, 0, len(routeIDs))
	for _, id := range routeIDs {
		models = append(models, map[string]any{
			"id":            id,
			"name":          "Switchyard route " + id,
			"reasoning":     true,
			"input":         []string{"text"},
			"cost":          map[string]any{"input": 0, "output": 0, "cacheRead": 0, "cacheWrite": 0},
			"contextWindow": 131072,
			"maxTokens":     8192,
		})
	}
	return map[string]any{
		"name":    "Switchyard Router",
		"baseUrl": fmt.Sprintf("http://127.0.0.1:%d/v1", p.Port),
		"api":     "openai-completions",
		"apiKey":  "switchyard",
		"compat": map[string]any{
			"supportsDeveloperRole":      false,
			"sendSessionAffinityHeaders": true,
			"sessionAffinityFormat":      "openrouter",
		},
		"models": models,
	}
}

// hc bounds every switchyard HTTP call (P13-B10: the default client with no
// deadline could hang a run forever on a half-open server — the 15 s health
// loop only checked its deadline BETWEEN blocking calls).
var hc = &http.Client{Timeout: 2 * time.Second}

// MaterializeProvider writes the switchyard provider into ~/.pi/agent/models.json
// and returns a restore function.
// P13-B9: restore REMOVES the "switchyard" key (never byte-restores) — a
// crash mid-run used to leave a dead provider pointing at a dead port, and a
// byte-restore clobbered any models.json edit made during the run. A stale
// entry from a previously crashed run is detected (port probe) and swept.
func (p *Proxy) MaterializeProvider(routeIDs []string) (func(), error) {
	path, err := piModelsPath()
	if err != nil {
		return nil, err
	}
	orig, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var reg map[string]any
	if err := json.Unmarshal(orig, &reg); err != nil {
		return nil, fmt.Errorf("parse pi models.json: %w", err)
	}
	prov, ok := reg["providers"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("pi models.json has no providers map")
	}
	if prev, exists := prov["switchyard"]; exists {
		if dead := prevStale(prev); dead {
			// sweep: a leftover from a crashed run (dead port) — overwrite
			delete(prov, "switchyard")
		} else {
			return nil, fmt.Errorf("pi models.json already has a LIVE switchyard provider — another crew run may be active; refusing to stomp it")
		}
	}
	prov["switchyard"] = p.ProviderEntry(routeIDs)
	out, err := json.MarshalIndent(reg, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, out, 0o644); err != nil {
		return nil, err
	}
	restore := func() {
		// key-removal restore: unmarshal fresh (the user may have edited the
		// file during the run), delete our key, write back.
		if fresh, err := os.ReadFile(path); err == nil {
			var reg2 map[string]any
			if json.Unmarshal(fresh, &reg2) == nil {
				if prov2, ok := reg2["providers"].(map[string]any); ok {
					if ours, _ := prov2["switchyard"].(map[string]any); ours != nil {
						if baseUrl, _ := ours["baseUrl"].(string); baseUrl == fmt.Sprintf("http://127.0.0.1:%d/v1", p.Port) {
							delete(prov2, "switchyard")
							if b, err := json.MarshalIndent(reg2, "", "  "); err == nil {
								_ = os.WriteFile(path, b, 0o644)
								return
							}
						}
					}
				}
			}
		}
		_ = os.WriteFile(path, orig, 0o644) // fallback: original bytes
	}
	return restore, nil
}

// prevStale probes a pre-existing switchyard entry: healthy → false (live
// owner), unreachable → true (husk from a crashed run, safe to sweep).
func prevStale(prev any) bool {
	m, ok := prev.(map[string]any)
	if !ok {
		return true
	}
	baseUrl, _ := m["baseUrl"].(string)
	if baseUrl == "" {
		return true
	}
	resp, err := hc.Get(strings.TrimSuffix(baseUrl, "/") + "/models")
	if err != nil {
		return true
	}
	resp.Body.Close()
	return resp.StatusCode >= 500
}

// BaseURL is the loopback root the router serves on (the crew points pi at
// `<BaseURL>/v1`).
func (p *Proxy) BaseURL() string { return fmt.Sprintf("http://127.0.0.1:%d", p.Port) }

// Stats fetches GET /v1/stats.
func (p *Proxy) Stats() (map[string]any, error) {
	resp, err := hc.Get(p.BaseURL() + "/v1/stats")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

// Stop kills the server process.
func (p *Proxy) Stop() {
	if p.cmd != nil && p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
		_, _ = p.cmd.Process.Wait()
		p.cmd = nil
	}
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

func mapToEnv(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k, v := range m {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}

func providerOf(model string) string {
	// "provider/model" or bare "model"
	if i := strings.Index(model, "/"); i >= 0 {
		return model[:i]
	}
	return ""
}
