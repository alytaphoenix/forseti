package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigRoundTripAndValidation(t *testing.T) {
	t.Setenv("FORSETI_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	c := loadConfig()
	if c.Crew.Engine != "rust" || !c.Services.Memory || c.TTT.Port != 4242 {
		t.Fatalf("defaults = %+v", c)
	}
	if err := setKey(&c, "ttt.port", "5001"); err != nil {
		t.Fatal(err)
	}
	if err := setKey(&c, "services.memory", "false"); err != nil {
		t.Fatal(err)
	}
	if err := setKey(&c, "repo", "~/src/forseti"); err != nil {
		t.Fatal(err)
	}
	if err := saveConfig(c); err != nil {
		t.Fatal(err)
	}
	got := loadConfig()
	if got.TTT.Port != 5001 || got.Services.Memory {
		t.Fatalf("reloaded = %+v", got)
	}
	if got.Repo != filepath.Join(homeDir(), "src/forseti") {
		t.Fatalf("repo = %q (tilde not expanded)", got.Repo)
	}
	if v, ok := getKey(got, "services.memory"); !ok || v != "false" {
		t.Fatalf("getKey = %q %v", v, ok)
	}
}

func TestConfigRejectsBadValues(t *testing.T) {
	c := defaultConfig()
	for _, tc := range []struct{ key, val string }{
		{"ttt.port", "0"},
		{"ttt.port", "abc"},
		{"services.laya", "maybe"},
		{"crew.engine", "sqlite"},
		{"crew.host", "kubernetes"},
	} {
		if err := setKey(&c, tc.key, tc.val); err == nil {
			t.Errorf("setKey(%q, %q) accepted", tc.key, tc.val)
		}
	}
	if err := setKey(&c, "nope", "x"); err == nil {
		t.Error("unknown key accepted")
	}
}

func TestParseTarget(t *testing.T) {
	cases := []struct {
		in        string
		path      string
		line, col int
	}{
		{"/a/b.go", "/a/b.go", 0, 0},
		{"/a/b.go:12", "/a/b.go", 12, 0},
		{"/a/b.go:12:3", "/a/b.go", 12, 3},
		{"/a/b:c", "/a/b:c", 0, 0},
	}
	for _, tc := range cases {
		p, l, c, err := parseTarget(tc.in)
		if err != nil {
			t.Fatalf("%s: %v", tc.in, err)
		}
		if p != tc.path || l != tc.line || c != tc.col {
			t.Errorf("%s => (%s,%d,%d), want (%s,%d,%d)", tc.in, p, l, c, tc.path, tc.line, tc.col)
		}
	}
}

func TestUpwardRepoUsesTheMarker(t *testing.T) {
	root := t.TempDir()
	for _, rel := range []string{
		"herdr-plugin/herdr-plugin.toml",
		"host/Cargo.toml",
		"ttt-plugin/init.lua",
		"pi-extension/index.ts",
	} {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	sub := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := upwardRepo(sub)
	if err != nil {
		t.Fatal(err)
	}
	if got != root {
		t.Fatalf("upwardRepo = %s, want %s", got, root)
	}
	if marker(t.TempDir()) {
		t.Fatal("an empty dir must not be a repo")
	}
}

func TestAssetsInspectAndSync(t *testing.T) {
	repo := t.TempDir()
	pluginDir := filepath.Join(t.TempDir(), "plugins", "forseti")
	ext := filepath.Join(t.TempDir(), "extensions", "forseti.ts")
	t.Setenv("FORSETI_TTT_PLUGIN_DIR", pluginDir)
	t.Setenv("FORSETI_PI_EXTENSION", ext)
	for _, rel := range []string{"ttt-plugin/init.lua", "ttt-plugin/plugin.ttt.json"} {
		p := filepath.Join(repo, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("plugin-"+rel), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	piSrc := filepath.Join(repo, "pi-extension", "index.ts")
	if err := os.MkdirAll(filepath.Dir(piSrc), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(piSrc, []byte("ext"), 0o644); err != nil {
		t.Fatal(err)
	}

	// dry run must not write
	if _, err := syncAssets(repo, true); err != nil {
		t.Fatal(err)
	}
	if fileExists(filepath.Join(pluginDir, "init.lua")) {
		t.Fatal("dry run wrote the plugin")
	}
	// real sync
	if _, err := syncAssets(repo, false); err != nil {
		t.Fatal(err)
	}
	if !fileExists(filepath.Join(pluginDir, "init.lua")) || !fileExists(ext) {
		t.Fatal("sync did not install both assets")
	}
	if assetDrift(repo) {
		t.Fatal("assets still drifted after sync")
	}
	// a source change is drift again
	if err := os.WriteFile(filepath.Join(repo, "ttt-plugin", "init.lua"), []byte("changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !assetDrift(repo) {
		t.Fatal("changed source was not detected")
	}
}

func TestCrewArgvInjectsTheDefaultCrew(t *testing.T) {
	dir := t.TempDir()
	crewFile := filepath.Join(dir, "default.yaml")
	if err := os.WriteFile(crewFile, []byte("name: x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := defaultConfig()
	cfg.Crew.Default = crewFile
	cfg.Crew.Engine = "rust"
	cfg.Crew.Host = "ttt"

	argv, err := crewArgv(cfg, "run", nil)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(argv, " ")
	if !strings.Contains(joined, "-f "+crewFile) {
		t.Fatalf("default crew not injected: %v", argv)
	}
	if !strings.Contains(joined, "--engine rust") || !strings.Contains(joined, "--host ttt") {
		t.Fatalf("engine/host defaults missing: %v", argv)
	}

	// an explicit file wins
	argv, err = crewArgv(cfg, "run", []string{"-f", "other.yaml"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(argv, " "), crewFile) {
		t.Fatalf("default injected despite -f: %v", argv)
	}

	// validate/watch never take the default
	if argv, _ := crewArgv(cfg, "validate", nil); strings.Contains(strings.Join(argv, " "), "-f") {
		t.Fatalf("validate got a default: %v", argv)
	}

	// --help is not hijacked by a missing default
	cfg.Crew.Default = filepath.Join(dir, "nope.yaml")
	if _, err := crewArgv(cfg, "run", []string{"--help"}); err != nil {
		t.Fatalf("--help must not resolve the default: %v", err)
	}
}

func TestResolveCrewDefaultBareNameUsesTheUserDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".config", "forseti", "crews")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(dir, "default.yaml")
	if err := os.WriteFile(want, []byte("name: x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := resolveCrewDefault("default")
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("resolved %q, want %q", got, want)
	}
	if _, err := resolveCrewDefault("missing"); err == nil {
		t.Fatal("missing default must error")
	}
}

func TestHasFileArg(t *testing.T) {
	for _, a := range [][]string{{"-f", "x"}, {"-f=x"}, {"--file", "x"}, {"--file=x"}, {"--f", "x"}} {
		if !hasFileArg(a) {
			t.Errorf("%v not detected", a)
		}
	}
	for _, a := range [][]string{{}, {"--model", "x"}, {"-x", "y"}} {
		if hasFileArg(a) {
			t.Errorf("%v false positive", a)
		}
	}
}
