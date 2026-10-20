package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// solPiTestHome points HOME (and the sol-pi/pi settings paths) at a scratch dir.
func solPiTestHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("FORSETI_CONFIG", filepath.Join(home, ".config", "forseti", "config.json"))
	t.Setenv("FORSETI_PI_SOL_PI_CONFIG", "")
	t.Setenv("FORSETI_PI_SETTINGS", "")
	return home
}

func TestSolPiShippedFileMatchesBuiltin(t *testing.T) {
	// The test runs in cli/, so the distribution file is one level up.
	shipped, err := readSolPiFile(filepath.Join("..", "pi-extension", "sol-pi.json"))
	if err != nil {
		t.Fatalf("shipped config: %v", err)
	}
	want, err := renderSolPi(builtinSolPi())
	if err != nil {
		t.Fatal(err)
	}
	got, err := renderSolPi(shipped)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("pi-extension/sol-pi.json and builtinSolPi() disagree:\n%s\nvs\n%s", got, want)
	}
}

func TestSolPiRenderIsTheDocumentedSchema(t *testing.T) {
	f, err := solPiEffective(defaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	data, err := renderSolPi(f)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"version":                   float64(1),
		"actionFusion":              true,
		"observationPack":           true,
		"evidencePreservingReducer": false,
		"onlineContextCompact":      false,
		"cacheWriteReadRatio":       12.5,
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %#v, want %#v", k, got[k], v)
		}
	}
	if len(got) != len(want) {
		t.Errorf("unexpected keys in %s", data)
	}
	// The reducer route is only written as a pair.
	f.EvidencePreservingReducerProvider = strPtr("openrouter")
	data, _ = renderSolPi(f)
	if strings.Contains(string(data), "evidencePreservingReducerProvider") {
		t.Errorf("half a reducer route was written:\n%s", data)
	}
	f.EvidencePreservingReducerModel = strPtr("deepseek/deepseek-chat")
	data, _ = renderSolPi(f)
	var withRoute map[string]any
	_ = json.Unmarshal(data, &withRoute)
	if withRoute["evidencePreservingReducerProvider"] != "openrouter" || withRoute["evidencePreservingReducerModel"] != "deepseek/deepseek-chat" {
		t.Errorf("reducer route not written:\n%s", data)
	}
}

func strPtr(s string) *string { return &s }

// fakeRepo builds a minimal directory that passes the checkout marker, so
// findRepo resolves it without touching the real checkout.
func fakeRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	for _, rel := range []string{
		"herdr-plugin/herdr-plugin.toml",
		"host/Cargo.toml",
		"ttt-plugin/init.lua",
		"pi-extension/index.ts",
	} {
		p := filepath.Join(repo, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return repo
}

func TestSolPiRejectsUnknownShippedKeys(t *testing.T) {
	repo := fakeRepo(t)
	path := filepath.Join(repo, "pi-extension", "sol-pi.json")
	if err := os.WriteFile(path, []byte(`{"version":1,"actionFusion":true,"nope":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FORSETI_REPO", repo)
	if _, err := solPiDistribution(defaultConfig()); err == nil {
		t.Fatal("unknown key accepted")
	}
	if err := os.WriteFile(path, []byte(`{"version":2,"actionFusion":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := solPiDistribution(defaultConfig()); err == nil {
		t.Fatal("unsupported version accepted")
	}
	// A missing file falls back to the built-in default instead of failing.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	f, err := solPiDistribution(defaultConfig())
	if err != nil || f.Version != 1 {
		t.Fatalf("missing shipped file: %+v %v", f, err)
	}
}

func TestSolPiConfigOverridesAndSync(t *testing.T) {
	solPiTestHome(t)
	t.Setenv("FORSETI_REPO", t.TempDir()) // no shipped file: built-in defaults

	cfg := loadConfig()
	if v, _ := getKey(cfg, "sol_pi.enabled"); v != "true" {
		t.Fatalf("sol_pi.enabled default = %q", v)
	}
	if v, _ := getKey(cfg, "sol_pi.action_fusion"); v != "true" {
		t.Fatalf("sol_pi.action_fusion default = %q", v)
	}
	if v, _ := getKey(cfg, "sol_pi.evidence_preserving_reducer"); v != "false" {
		t.Fatalf("sol_pi.evidence_preserving_reducer default = %q", v)
	}

	if err := setKey(&cfg, "sol_pi.observation_pack", "false"); err != nil {
		t.Fatal(err)
	}
	if err := setKey(&cfg, "sol_pi.online_context_compact", "true"); err != nil {
		t.Fatal(err)
	}
	if err := setKey(&cfg, "sol_pi.cache_write_read_ratio", "3"); err != nil {
		t.Fatal(err)
	}
	if err := saveConfig(cfg); err != nil {
		t.Fatal(err)
	}

	cfg = loadConfig()
	st, err := syncSolPi(cfg, true)
	if err != nil {
		t.Fatal(err)
	}
	if st.Exists || st.InSync || !st.Changed {
		t.Fatalf("dry run wrote the file: %+v", st)
	}
	st, err = syncSolPi(cfg, false)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Exists || !st.InSync || !st.Changed {
		t.Fatalf("sync left the file stale: %+v", st)
	}
	if again, err := syncSolPi(cfg, false); err != nil || again.Changed {
		t.Fatalf("second sync was not a no-op: %+v %v", again, err)
	}
	data, err := os.ReadFile(st.Path)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got["observationPack"] != false || got["onlineContextCompact"] != true || got["cacheWriteReadRatio"] != float64(3) {
		t.Fatalf("installed config = %s", data)
	}

	// A hand edit is drift again and the next sync wins.
	if err := os.WriteFile(st.Path, []byte(`{"version":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if st, _ := solPiStatus(cfg); st.InSync {
		t.Fatal("hand edit not detected as drift")
	}
	if _, err := syncSolPi(cfg, false); err != nil {
		t.Fatal(err)
	}
	if st, _ := solPiStatus(cfg); !st.InSync {
		t.Fatal("sync did not restore the rendered config")
	}
}

func TestSolPiDisabledLeavesTheFileAlone(t *testing.T) {
	solPiTestHome(t)
	t.Setenv("FORSETI_REPO", t.TempDir())
	path := solPiTargetPath()
	if err := writeFileAtomic(path, []byte(`{"version":1,"actionFusion":false}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := loadConfig()
	if err := setKey(&cfg, "sol_pi.enabled", "false"); err != nil {
		t.Fatal(err)
	}
	st, err := syncSolPi(cfg, false)
	if err != nil {
		t.Fatal(err)
	}
	if st.Enabled {
		t.Fatal("disabled config reported as enabled")
	}
	have, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(have) != `{"version":1,"actionFusion":false}` {
		t.Fatalf("disabled sync rewrote the file: %s", have)
	}
}

func TestSolPiRejectsBadValues(t *testing.T) {
	c := defaultConfig()
	for _, k := range []string{
		"sol_pi.enabled", "sol_pi.install", "sol_pi.action_fusion",
		"sol_pi.observation_pack", "sol_pi.evidence_preserving_reducer",
		"sol_pi.online_context_compact",
	} {
		if err := setKey(&c, k, "maybe"); err == nil {
			t.Errorf("%s accepted a non-boolean", k)
		}
	}
	for _, v := range []string{"-1", "NaN", "abc"} {
		if err := setKey(&c, "sol_pi.cache_write_read_ratio", v); err == nil {
			t.Errorf("ratio accepted %q", v)
		}
	}
	if err := setKey(&c, "sol_pi.package", "git:github.com/a b"); err == nil {
		t.Error("package with a space accepted")
	}
	if err := setKey(&c, "sol_pi.cache_write_read_ratio", "0"); err != nil {
		t.Errorf("ratio 0 must be allowed (explicit no-cost write): %v", err)
	}
}

func TestSolPiInstalledDetection(t *testing.T) {
	home := solPiTestHome(t)
	settings := filepath.Join(home, ".pi", "agent", "settings.json")
	if err := os.MkdirAll(filepath.Dir(settings), 0o755); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		body string
		want bool
	}{
		{`{}`, false},
		{`{"packages":[]}`, false},
		{`{"packages":["npm:@scope/other"]}`, false},
		{`{"packages":["npm:sol-pi"]}`, true},
		{`{"packages":["npm:sol-pi@0.1.0"]}`, true},
		{`{"packages":["git:github.com/NVlabs/SoL-Pi"]}`, true},
		{`{"packages":["git:github.com/NVlabs/SoL-Pi@v1"]}`, true},
		{`{"packages":[{"source":"npm:@herbertgao/sol-pi","skills":[]}]}`, true},
	}
	for _, tc := range cases {
		if err := os.WriteFile(settings, []byte(tc.body), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, ok := solPiInstalled(); ok != tc.want {
			t.Errorf("%s => installed=%v, want %v", tc.body, ok, tc.want)
		}
	}
}

func TestEnsureSolPiInstallSkippedWhenAlreadyDeclared(t *testing.T) {
	home := solPiTestHome(t)
	t.Setenv("FORSETI_REPO", t.TempDir())
	settings := filepath.Join(home, ".pi", "agent", "settings.json")
	if err := os.MkdirAll(filepath.Dir(settings), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(settings, []byte(`{"packages":["npm:sol-pi"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := loadConfig()
	// A pi binary that would fail loudly if it were run.
	cfg.Pi.Bin = filepath.Join(t.TempDir(), "no-such-pi")
	if _, installed, err := ensureSolPiInstall(cfg, false); err != nil || installed {
		t.Fatalf("declared package still installed: installed=%v err=%v", installed, err)
	}
	// Disabled disables installation too.
	_ = setKey(&cfg, "sol_pi.install", "false")
	if _, installed, err := ensureSolPiInstall(cfg, false); err != nil || installed {
		t.Fatalf("sol_pi.install=false still installed: %v %v", installed, err)
	}
}

func TestVersionAtLeast(t *testing.T) {
	cases := []struct {
		have, want string
		ok         bool
	}{
		{"1.0.3", "0.84.2", true},
		{"0.84.2", "0.84.2", true},
		{"0.84.1", "0.84.2", false},
		{"v22.22.3", "22.19.0", true},
		{"pi 1.3.0", "0.84.2", true},
		{"nope", "0.84.2", false},
	}
	for _, tc := range cases {
		if got := versionAtLeast(tc.have, tc.want); got != tc.ok {
			t.Errorf("versionAtLeast(%q,%q) = %v, want %v", tc.have, tc.want, got, tc.ok)
		}
	}
}

func TestSolPiReducerRouteIsAPair(t *testing.T) {
	solPiTestHome(t)
	t.Setenv("FORSETI_REPO", t.TempDir())
	cfg := loadConfig()
	if err := setKey(&cfg, "sol_pi.reducer_provider", "openrouter"); err != nil {
		t.Fatal(err)
	}
	if p, m := solPiRouteStrings(cfg); p != "openrouter" || m != "" {
		t.Fatalf("route = %q/%q", p, m)
	}
	if err := setKey(&cfg, "sol_pi.reducer_model", " deepseek/deepseek-chat "); err != nil {
		t.Fatal(err)
	}
	if err := saveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	cfg = loadConfig()
	if m, _ := getKey(cfg, "sol_pi.reducer_model"); m != "deepseek/deepseek-chat" {
		t.Fatalf("reducer model = %q", m)
	}
	f, err := solPiEffective(cfg)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := renderSolPi(f)
	if !strings.Contains(string(data), `"evidencePreservingReducerProvider": "openrouter"`) {
		t.Fatalf("route not rendered:\n%s", data)
	}
	// Clearing both keys removes the route again.
	if err := setKey(&cfg, "sol_pi.reducer_provider", ""); err != nil {
		t.Fatal(err)
	}
	if err := setKey(&cfg, "sol_pi.reducer_model", ""); err != nil {
		t.Fatal(err)
	}
	if err := saveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	f, _ = solPiEffective(loadConfig())
	data, _ = renderSolPi(f)
	if strings.Contains(string(data), "evidencePreservingReducerProvider") {
		t.Fatalf("cleared route still rendered:\n%s", data)
	}
}
