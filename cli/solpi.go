package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// SoL-Pi (github.com/NVlabs/SoL-Pi) is a third-party pi extension package that
// forseti ships as part of the distribution. forseti owns two things:
//
//   - the install (a pi package recorded in pi's settings.json), and
//   - its configuration file (sol-pi.json), rendered from the forseti config so
//     the mechanisms toggle with `forseti config set sol_pi.*` instead of
//     hand-editing JSON. pi-extension/sol-pi.json in the checkout is the shipped
//     default; the config keys override it; the result is installed at
//     ~/.pi/agent/sol-pi.json.
//
// SoL-Pi stops loading on unknown keys, malformed JSON or a non-boolean flag, so
// solPiFile is an allowlist of exactly its schema.
const (
	// solPiDefaultPackage is upstream's documented install source (a git source
	// tracks NVlabs/SoL-Pi main; pin a ref with `@v1` if you want it fixed).
	solPiDefaultPackage = "git:github.com/NVlabs/SoL-Pi"
	solPiDefaultRatio   = 12.5
	solPiMinPiVersion   = "0.84.2"
	solPiTestedPi       = "0.84.2, 1.0.4, 1.3.0"
	solPiMinNodeVersion = "22.19.0"
)

// solPiFile is the sol-pi.json schema. Every mechanism is opt-in, so a nil
// pointer means "not specified" (the shipped default decides).
type solPiFile struct {
	Version                           int      `json:"version"`
	ActionFusion                      *bool    `json:"actionFusion,omitempty"`
	ObservationPack                   *bool    `json:"observationPack,omitempty"`
	EvidencePreservingReducer         *bool    `json:"evidencePreservingReducer,omitempty"`
	EvidencePreservingReducerProvider *string  `json:"evidencePreservingReducerProvider,omitempty"`
	EvidencePreservingReducerModel    *string  `json:"evidencePreservingReducerModel,omitempty"`
	OnlineContextCompact              *bool    `json:"onlineContextCompact,omitempty"`
	CacheWriteReadRatio               *float64 `json:"cacheWriteReadRatio,omitempty"`
}

// solPiState is what up/config sync/doctor report.
type solPiState struct {
	Enabled         bool   `json:"enabled"`
	Install         bool   `json:"install"`
	Package         string `json:"package"`
	Path            string `json:"path"`
	Exists          bool   `json:"exists"`
	InSync          bool   `json:"in_sync"`
	Changed         bool   `json:"changed"`
	Installed       bool   `json:"installed"`
	InstalledSource string `json:"installed_source,omitempty"`
}

func solPiShippedPath(repo string) string {
	return filepath.Join(repo, "pi-extension", "sol-pi.json")
}

func solPiTargetPath() string {
	if v := os.Getenv("FORSETI_PI_SOL_PI_CONFIG"); v != "" {
		return expandHome(v)
	}
	return filepath.Join(homeDir(), ".pi", "agent", "sol-pi.json")
}

func piSettingsPath() string {
	if v := os.Getenv("FORSETI_PI_SETTINGS"); v != "" {
		return expandHome(v)
	}
	return filepath.Join(homeDir(), ".pi", "agent", "settings.json")
}

func boolPtr(b bool) *bool { return &b }

// builtinSolPi is the fallback when the checkout's pi-extension/sol-pi.json is
// missing. It mirrors that file (TestSolPiShippedFileMatchesBuiltin pins it):
// the two local mechanisms that make no extra model calls and never stop a run.
func builtinSolPi() solPiFile {
	return solPiFile{
		Version:                   1,
		ActionFusion:              boolPtr(true),
		ObservationPack:           boolPtr(true),
		EvidencePreservingReducer: boolPtr(false),
		OnlineContextCompact:      boolPtr(false),
		CacheWriteReadRatio:       floatPtr(solPiDefaultRatio),
	}
}

func floatPtr(f float64) *float64 { return &f }

// readSolPiFile decodes a sol-pi.json strictly (unknown keys are an error, as
// they are for the extension itself).
func readSolPiFile(path string) (solPiFile, error) {
	var f solPiFile
	data, err := os.ReadFile(path)
	if err != nil {
		return f, err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return f, fmt.Errorf("%s: %v", path, err)
	}
	if f.Version != 1 {
		return f, fmt.Errorf("%s: version must be 1, got %d", path, f.Version)
	}
	if f.CacheWriteReadRatio != nil && *f.CacheWriteReadRatio < 0 {
		return f, fmt.Errorf("%s: cacheWriteReadRatio must be >= 0", path)
	}
	return f, nil
}

// solPiDistribution reads the shipped default from the checkout (or the
// built-in copy when the file is absent).
func solPiDistribution(c Config) (solPiFile, error) {
	repo, err := findRepo(c)
	if err != nil {
		return builtinSolPi(), nil
	}
	f, err := readSolPiFile(solPiShippedPath(repo))
	if os.IsNotExist(err) {
		return builtinSolPi(), nil
	}
	if err != nil {
		return solPiFile{}, err
	}
	return f, nil
}

// solPiEffective layers the `sol_pi.*` config overrides onto the shipped default.
func solPiEffective(c Config) (solPiFile, error) {
	f, err := solPiDistribution(c)
	if err != nil {
		return f, err
	}
	o := c.SolPi
	if o.ActionFusion != nil {
		f.ActionFusion = o.ActionFusion
	}
	if o.ObservationPack != nil {
		f.ObservationPack = o.ObservationPack
	}
	if o.EvidencePreservingReducer != nil {
		f.EvidencePreservingReducer = o.EvidencePreservingReducer
	}
	if o.OnlineContextCompact != nil {
		f.OnlineContextCompact = o.OnlineContextCompact
	}
	if o.CacheWriteReadRatio != nil {
		f.CacheWriteReadRatio = o.CacheWriteReadRatio
	}
	if o.ReducerProvider != nil {
		f.EvidencePreservingReducerProvider = o.ReducerProvider
	}
	if o.ReducerModel != nil {
		f.EvidencePreservingReducerModel = o.ReducerModel
	}
	return f, nil
}

// solPiReasonerRoute reports the configured reducer provider/model. The pair is
// only meaningful together, so half a route is not written to the file.
func solPiReasonerRoute(f solPiFile) (provider, model string, ok bool) {
	if f.EvidencePreservingReducerProvider == nil || f.EvidencePreservingReducerModel == nil {
		return "", "", false
	}
	p, m := *f.EvidencePreservingReducerProvider, *f.EvidencePreservingReducerModel
	if p == "" || m == "" {
		return "", "", false
	}
	return p, m, true
}

// renderSolPi emits the installed file: every flag explicit (sol-pi defaults a
// missing one to false), the ratio resolved, and the reducer route only when
// both halves are set.
func renderSolPi(f solPiFile) ([]byte, error) {
	flag := func(p *bool) bool { return p != nil && *p }
	ratio := solPiDefaultRatio
	if f.CacheWriteReadRatio != nil {
		ratio = *f.CacheWriteReadRatio
	}
	out := struct {
		Version                           int     `json:"version"`
		ActionFusion                      bool    `json:"actionFusion"`
		ObservationPack                   bool    `json:"observationPack"`
		EvidencePreservingReducer         bool    `json:"evidencePreservingReducer"`
		EvidencePreservingReducerProvider string  `json:"evidencePreservingReducerProvider,omitempty"`
		EvidencePreservingReducerModel    string  `json:"evidencePreservingReducerModel,omitempty"`
		OnlineContextCompact              bool    `json:"onlineContextCompact"`
		CacheWriteReadRatio               float64 `json:"cacheWriteReadRatio"`
	}{
		Version:                   1,
		ActionFusion:              flag(f.ActionFusion),
		ObservationPack:           flag(f.ObservationPack),
		EvidencePreservingReducer: flag(f.EvidencePreservingReducer),
		OnlineContextCompact:      flag(f.OnlineContextCompact),
		CacheWriteReadRatio:       ratio,
	}
	if p, m, ok := solPiReasonerRoute(f); ok {
		out.EvidencePreservingReducerProvider, out.EvidencePreservingReducerModel = p, m
	}
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

func solPiEnabled(c Config) bool {
	return c.SolPi.Enabled == nil || *c.SolPi.Enabled
}

func solPiInstallAllowed(c Config) bool {
	return c.SolPi.Install == nil || *c.SolPi.Install
}

func solPiPackage(c Config) string {
	if p := strings.TrimSpace(c.SolPi.Package); p != "" {
		return p
	}
	return solPiDefaultPackage
}

// solPiStatus compares the installed file with the rendered config and asks pi
// whether a sol-pi package is declared.
func solPiStatus(c Config) (solPiState, error) {
	st := solPiState{
		Enabled: solPiEnabled(c),
		Install: solPiInstallAllowed(c),
		Package: solPiPackage(c),
		Path:    solPiTargetPath(),
	}
	if src, ok := solPiInstalled(); ok {
		st.Installed, st.InstalledSource = true, src
	}
	f, err := solPiEffective(c)
	if err != nil {
		return st, err
	}
	want, err := renderSolPi(f)
	if err != nil {
		return st, err
	}
	if have, err := os.ReadFile(st.Path); err == nil {
		st.Exists = true
		st.InSync = bytes.Equal(want, have)
	}
	return st, nil
}

// syncSolPi writes the rendered config (atomic, 0644). A disabled distribution
// leaves whatever file is installed alone: every mechanism off is the built-in
// default, so the file is inert rather than destructive.
func syncSolPi(c Config, dry bool) (solPiState, error) {
	st, err := solPiStatus(c)
	if err != nil {
		return st, err
	}
	if !st.Enabled || st.InSync {
		return st, nil
	}
	st.Changed = true
	if dry {
		return st, nil
	}
	f, err := solPiEffective(c)
	if err != nil {
		return st, err
	}
	want, err := renderSolPi(f)
	if err != nil {
		return st, err
	}
	if err := writeFileAtomic(st.Path, want, 0o644); err != nil {
		return st, err
	}
	st.Exists, st.InSync = true, true
	return st, nil
}

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(path), err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("install %s: %w", path, err)
	}
	return nil
}

// piPackages lists the package sources declared in pi's user-level settings.
// Object-form entries (`{source: …}`) are accepted alongside plain strings.
func piPackages() []string {
	data, err := os.ReadFile(piSettingsPath())
	if err != nil {
		return nil
	}
	var parsed struct {
		Packages []json.RawMessage `json:"packages"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		return nil
	}
	var out []string
	for _, raw := range parsed.Packages {
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			out = append(out, s)
			continue
		}
		var obj struct {
			Source string `json:"source"`
		}
		if err := json.Unmarshal(raw, &obj); err == nil && obj.Source != "" {
			out = append(out, obj.Source)
		}
	}
	return out
}

// solPiSourceIsSolPi matches by package identity, so a user who installed
// npm:sol-pi (or a pinned git ref) is not treated as missing.
func solPiSourceIsSolPi(src string) bool {
	s := strings.ToLower(strings.TrimSpace(src))
	for _, p := range []string{"npm:", "git:", "https://", "http://", "ssh://"} {
		s = strings.TrimPrefix(s, p)
	}
	s = strings.TrimSuffix(s, "/")
	if i := strings.LastIndex(s, "@"); i > 0 {
		s = s[:i]
	}
	s = strings.TrimSuffix(s, ".git")
	if i := strings.LastIndexAny(s, "/:"); i >= 0 {
		s = s[i+1:]
	}
	return s == "sol-pi" || s == "solpi"
}

func solPiInstalled() (string, bool) {
	for _, src := range piPackages() {
		if solPiSourceIsSolPi(src) {
			return src, true
		}
	}
	return "", false
}

// ensureSolPiInstall installs the package when the distribution is enabled,
// installing is allowed, and pi has no sol-pi package declared yet.
func ensureSolPiInstall(c Config, dry bool) (source string, installed bool, err error) {
	st, err := solPiStatus(c)
	if err != nil {
		return "", false, err
	}
	if !st.Enabled || !st.Install || st.Installed || dry {
		return st.Package, false, nil
	}
	if _, err := lookPath(c.Pi.Bin); err != nil {
		return st.Package, false, fmt.Errorf("pi binary not found: %s", c.Pi.Bin)
	}
	if out, err := runCapture("", nil, c.Pi.Bin, "install", st.Package); err != nil {
		return st.Package, false, fmt.Errorf("%v", trim(out))
	}
	return st.Package, true, nil
}

// reportSolPi prints one aligned line for up / config sync.
func reportSolPi(st solPiState, dry bool) {
	switch {
	case !st.Enabled:
		fmt.Printf("%-11s %-14s %s\n", "disabled", "sol-pi", "off in config")
	case st.Changed && dry:
		fmt.Printf("%-11s %-14s %s\n", "would sync", "sol-pi config", st.Path)
	case st.Changed:
		fmt.Printf("%-11s %-14s %s\n", "synced", "sol-pi config", st.Path)
	default:
		fmt.Printf("%-11s %-14s %s\n", "in sync", "sol-pi config", st.Path)
	}
}

// versionTuple extracts the first dotted numeric run of a version string
// ("pi 1.0.3" and "v22.22.3" both work).
func versionTuple(s string) ([]int, bool) {
	i := 0
	for i < len(s) && !(s[i] >= '0' && s[i] <= '9') {
		i++
	}
	if i == len(s) {
		return nil, false
	}
	j := i
	for j < len(s) && (s[j] == '.' || (s[j] >= '0' && s[j] <= '9')) {
		j++
	}
	tok := strings.TrimSuffix(s[i:j], ".")
	if tok == "" {
		return nil, false
	}
	var out []int
	for _, part := range strings.Split(tok, ".") {
		n, err := strconv.Atoi(part)
		if err != nil {
			return nil, false
		}
		out = append(out, n)
	}
	return out, true
}

func versionAtLeast(have, want string) bool {
	h, ok1 := versionTuple(have)
	w, ok2 := versionTuple(want)
	if !ok1 || !ok2 {
		return false
	}
	for i := 0; i < len(h) || i < len(w); i++ {
		var hv, wv int
		if i < len(h) {
			hv = h[i]
		}
		if i < len(w) {
			wv = w[i]
		}
		if hv != wv {
			return hv > wv
		}
	}
	return true
}

func piVersion(c Config) (string, bool) {
	out, err := runCapture("", nil, c.Pi.Bin, "--version")
	if err != nil {
		return "", false
	}
	v := firstLine(out)
	if _, ok := versionTuple(v); !ok {
		return "", false
	}
	return v, true
}

func nodeVersion() (string, bool) {
	out, err := runCapture("", nil, "node", "--version")
	if err != nil {
		return "", false
	}
	v := firstLine(out)
	if _, ok := versionTuple(v); !ok {
		return "", false
	}
	return v, true
}

// solPiChecks is doctor's sol-pi section: the managed file, the pi package, the
// runtime floors, a project-local override, and a half-set reducer route.
func solPiChecks(c Config) []check {
	if !solPiEnabled(c) {
		return []check{{"sol-pi", "ok", "disabled in config", ""}}
	}
	st, err := solPiStatus(c)
	if err != nil {
		return []check{{"sol-pi config", "warn", err.Error(), "fix pi-extension/sol-pi.json in the checkout"}}
	}
	var out []check
	switch {
	case !st.Exists:
		out = append(out, check{"sol-pi config", "warn", "missing " + st.Path, "forseti config sync"})
	case !st.InSync:
		out = append(out, check{"sol-pi config", "warn", "stale " + st.Path + " (differs from the forseti config)", "forseti config sync"})
	default:
		out = append(out, check{"sol-pi config", "ok", st.Path, ""})
	}
	if st.Installed {
		out = append(out, check{"sol-pi package", "ok", st.InstalledSource, ""})
	} else {
		out = append(out, check{"sol-pi package", "warn", "not installed in pi", "forseti up  (or: " + c.Pi.Bin + " install " + st.Package + ")"})
	}
	if cwd, err := os.Getwd(); err == nil {
		if p := filepath.Join(cwd, ".pi", "sol-pi.json"); fileExists(p) {
			out = append(out, check{"sol-pi project", "warn", p + " replaces the managed file", "remove it, or mirror it with forseti config set sol_pi.*"})
		}
	}
	if v, ok := piVersion(c); ok {
		if !versionAtLeast(v, solPiMinPiVersion) {
			out = append(out, check{"sol-pi pi", "warn", "pi " + v + " (sol-pi needs >= " + solPiMinPiVersion + ")", "update pi"})
		} else {
			out = append(out, check{"sol-pi pi", "ok", "pi " + v + " (tested: " + solPiTestedPi + ")", ""})
		}
	}
	if v, ok := nodeVersion(); ok {
		if !versionAtLeast(v, solPiMinNodeVersion) {
			out = append(out, check{"sol-pi node", "warn", "node " + v + " (sol-pi needs >= " + solPiMinNodeVersion + ")", "install Node.js " + solPiMinNodeVersion + "+"})
		} else {
			out = append(out, check{"sol-pi node", "ok", "node " + v, ""})
		}
	}
	if provider, model := solPiRouteStrings(c); (provider == "") != (model == "") {
		out = append(out, check{"sol-pi reducer", "warn", "reducer route half-set (provider/model)", "set both sol_pi.reducer_provider and sol_pi.reducer_model"})
	}
	return out
}

// solPiRouteStrings reports the effective reducer provider/model as written
// (either side may be empty when the route is half-set).
func solPiRouteStrings(c Config) (provider, model string) {
	f, err := solPiEffective(c)
	if err != nil {
		return "", ""
	}
	if f.EvidencePreservingReducerProvider != nil {
		provider = *f.EvidencePreservingReducerProvider
	}
	if f.EvidencePreservingReducerModel != nil {
		model = *f.EvidencePreservingReducerModel
	}
	return provider, model
}
