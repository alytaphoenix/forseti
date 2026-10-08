// Package switchyard manages a per-run switchyard-server proxy (NVIDIA NeMo
// Switchyard) so crew nodes can route model calls through a shared route.
//
// Facts (S14/S15, docs/spikes.md):
//   - server 0.2.0: route types noop|random|passthrough|llm_classifier|stage_router
//     (no "auto"; the auto preset == stage_router efficient_first + 0.5).
//   - TOML: schema_version=1, [llm_clients.X] format/base_url/api_key_env?,
//     [targets.X] id/llm_client, [routes.X] id/type/capable_target/
//     efficient_target/picker/confidence_threshold.
//   - readiness: GET /health → {"status":"ok"}; GET /v1/stats counters.
//   - routing log: --routing-log-file appends JSONL per request.
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
	"sort"
	"strings"
	"time"

	"forseti/crew/internal/schema"
)

// ResolveBin finds the switchyard-server binary:
// FORSETI_SWITCHYARD_BIN → ~/.cargo/bin/switchyard-server → PATH.
func ResolveBin() (string, error) {
	if p := os.Getenv("FORSETI_SWITCHYARD_BIN"); p != "" {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	home, err := os.UserHomeDir()
	if err == nil {
		p := filepath.Join(home, ".cargo", "bin", "switchyard-server")
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return exec.LookPath("switchyard-server")
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

// Proxy is one spawned switchyard-server for a run.
type Proxy struct {
	Bin        string
	Port       int
	ConfigPath string
	RoutingLog string
	ServerLog  string
	Env        map[string]string // extra env (resolved API keys) for the process

	cmd *exec.Cmd
}

// Start generates the TOML from the crew's routes, picks a free port, spawns
// the server, and waits for /health. Secrets never appear in the TOML: each
// referenced pi provider's key is resolved once and passed as
// SWITCHYARD_KEY_<PROVIDER> in the server env.
//
// routingLog is the per-run JSONL path (unique per run — the tailer reads from
// offset 0, so a shared filename would replay earlier runs' records).
func Start(routes []schema.Route, runDir, routingLog string) (*Proxy, error) {
	bin, err := ResolveBin()
	if err != nil {
		return nil, fmt.Errorf("switchyard-server not found (cargo install switchyard-server or set FORSETI_SWITCHYARD_BIN): %w", err)
	}

	// resolve referenced providers from pi's models.json (S15: that's where
	// forseti's provider/model strings live)
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

	used := map[string]bool{}
	for _, r := range routes {
		for _, m := range []string{r.Efficient, r.Capable} {
			p := providerOf(m)
			if _, ok := reg.Providers[p]; !ok {
				return nil, fmt.Errorf("route %q: provider %q not in ~/.pi/agent/models.json", r.ID, p)
			}
			used[p] = true
		}
	}
	names := make([]string, 0, len(used))
	for p := range used {
		names = append(names, p)
	}
	sort.Strings(names)

	env := map[string]string{}
	var clients strings.Builder
	for _, p := range names {
		prov := reg.Providers[p]
		// P13-B22: quoted table keys — a provider id containing '.' (e.g.
		// "open.ai") silently became a NESTED TOML table and corrupted the config
		fmt.Fprintf(&clients, "\n[llm_clients.%q]\nformat = \"openai_chat\"\nbase_url = %q\n", p, prov.BaseURL)
		if prov.APIKey != "" {
			val, err := apiKeyValue(prov.APIKey)
			if err != nil {
				return nil, fmt.Errorf("provider %s: %w", p, err)
			}
			envKey := "SWITCHYARD_KEY_" + strings.ToUpper(strings.ReplaceAll(strings.ReplaceAll(p, "-", "_"), ".", "_"))
			env[envKey] = val
			fmt.Fprintf(&clients, "api_key_env = %q\n", envKey)
		}
	}

	var targets, routesT strings.Builder
	for _, r := range routes {
		eff := strings.SplitN(r.Efficient, "/", 2)
		cap := strings.SplitN(r.Capable, "/", 2)
		fmt.Fprintf(&targets, "\n[targets.%s_eff]\nid = %q\nllm_client = %q\n", r.ID, eff[len(eff)-1], eff[0])
		fmt.Fprintf(&targets, "\n[targets.%s_cap]\nid = %q\nllm_client = %q\n", r.ID, cap[len(cap)-1], cap[0])
		fmt.Fprintf(&routesT, "\n[routes.%s]\nid = %q\ntype = %q\ncapable_target = %q\nefficient_target = %q\npicker = %q\nconfidence_threshold = %g\n",
			r.ID, r.ID, r.Type, r.ID+"_cap", r.ID+"_eff", r.Picker, r.Confidence)
	}

	toml := "schema_version = 1\n" + clients.String() + targets.String() + routesT.String()

	port, err := freePort()
	if err != nil {
		return nil, err
	}
	p := &Proxy{
		Bin:        bin,
		Port:       port,
		ConfigPath: filepath.Join(runDir, "switchyard.toml"),
		RoutingLog: filepath.Join(runDir, routingLog),
		ServerLog:  filepath.Join(runDir, "switchyard-server.log"),
		Env:        env,
	}
	if err := os.WriteFile(p.ConfigPath, []byte(toml), 0o600); err != nil {
		return nil, err
	}

	cmd := exec.Command(bin, "--config", p.ConfigPath,
		"--host", "127.0.0.1", "--port", fmt.Sprint(port),
		"--routing-log-file", p.RoutingLog)
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
			return nil, fmt.Errorf("switchyard-server exited early; log: %s", p.ServerLog)
		}
		time.Sleep(150 * time.Millisecond)
	}
	p.Stop()
	return nil, fmt.Errorf("switchyard-server not healthy within 15s (port %d); log: %s", port, p.ServerLog)
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

// Stats fetches GET /v1/stats.
func (p *Proxy) Stats() (map[string]any, error) {
	resp, err := hc.Get(fmt.Sprintf("http://127.0.0.1:%d/v1/stats", p.Port))
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
