package main

import (
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// --- fake herdr plumbing ---------------------------------------------------

type stubWorld struct {
	Tabs     []herdrTab             `json:"tabs"`
	Panes    []herdrPane            `json:"panes"`
	Agents   []herdrAgent           `json:"agents"`
	Procs    map[string][]herdrProc `json:"procs"`
	Runs     []string               `json:"runs"`
	Closed   []string               `json:"closed"`
	NextTab  int                    `json:"next_tab"`
	NextPane int                    `json:"next_pane"`
	Down     bool                   `json:"down"`
}

var (
	stubOnce sync.Once
	stubPath string
	stubErr  error
	stubDir  string
)

func TestMain(m *testing.M) {
	stubDir, _ = os.MkdirTemp("", "forseti-herdr-stub-")
	code := m.Run()
	_ = os.RemoveAll(stubDir)
	os.Exit(code)
}

func herdrStub(t *testing.T) string {
	t.Helper()
	stubOnce.Do(func() {
		exe := filepath.Join(stubDir, "herdr")
		out, err := exec.Command("go", "build", "-o", exe, "./testdata/herdrstub").CombinedOutput()
		if err != nil {
			stubErr = &stubBuildError{msg: string(out), err: err}
			return
		}
		stubPath = exe
	})
	if stubErr != nil {
		t.Fatalf("build herdr stub: %v", stubErr)
	}
	return stubPath
}

type stubBuildError struct {
	msg string
	err error
}

func (e *stubBuildError) Error() string { return e.err.Error() + "\n" + e.msg }

type fixture struct {
	t      *testing.T
	cfg    Config
	dir    string
	world  string
	state  string
	plugin string
}

func newFixture(t *testing.T, w stubWorld) *fixture {
	t.Helper()
	home := t.TempDir()
	dir := filepath.Join(home, "project")
	vault := filepath.Join(home, "vault")
	for _, d := range []string{dir, vault} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	f := &fixture{
		t:      t,
		dir:    dir,
		world:  filepath.Join(home, "world.json"),
		state:  filepath.Join(home, "state.json"),
		plugin: filepath.Join(home, "plugin"),
	}
	if w.Procs == nil {
		w.Procs = map[string][]herdrProc{}
	}
	if w.NextTab == 0 {
		w.NextTab = 1
	}
	if w.NextPane == 0 {
		w.NextPane = 1
	}
	f.writeWorld(w)
	t.Setenv("STUB_WORLD", f.world)
	t.Setenv("STUB_PLUGIN_ROOT", f.plugin)
	t.Setenv("FORSETI_STATE", f.state)
	t.Setenv("FORSETI_VAULT", vault)
	f.cfg = defaultConfig()
	f.cfg.Herdr.Bin = herdrStub(t)
	f.cfg.Herdr.EditorLabel = "forseti"
	f.cfg.Herdr.Agent = "coder"
	f.cfg.TTT.Bin = "ttt"
	f.cfg.TTT.Port = freePort(t)
	return f
}

// freePort returns a port nothing is listening on, so the editor command keeps
// its --listen flag (the real 4242 often has a ttt on it).
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return port
}

func (f *fixture) writeWorld(w stubWorld) {
	data, err := json.MarshalIndent(w, "", "  ")
	if err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(f.world, data, 0o600); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) readWorld() stubWorld {
	data, err := os.ReadFile(f.world)
	if err != nil {
		f.t.Fatal(err)
	}
	var w stubWorld
	if err := json.Unmarshal(data, &w); err != nil {
		f.t.Fatal(err)
	}
	return w
}

func (f *fixture) starter(o startOpts) *starter {
	return &starter{cfg: f.cfg, dir: f.dir, o: o, line: func(string, string, string) {}}
}

func shellProc() herdrProc { return herdrProc{Argv0: "-zsh", Name: "zsh", Cmdline: "-zsh"} }

// --- tests -----------------------------------------------------------------

func TestSplitStartArgs(t *testing.T) {
	cases := []struct {
		in          []string
		wantFlags   []string
		wantPositio []string
	}{
		{[]string{"--no-sync", "/a"}, []string{"--no-sync"}, []string{"/a"}},
		{[]string{"/a", "--json"}, []string{"--json"}, []string{"/a"}},
		{[]string{"--dir", "/b", "--clean"}, []string{"--dir", "/b", "--clean"}, nil},
		{[]string{"--dir=/c", "--prune-dead", "x"}, []string{"--dir=/c", "--prune-dead"}, []string{"x"}},
	}
	for _, tc := range cases {
		flags, positionals := splitStartArgs(tc.in)
		if strings.Join(flags, " ") != strings.Join(tc.wantFlags, " ") {
			t.Errorf("%v: flags = %v, want %v", tc.in, flags, tc.wantFlags)
		}
		if strings.Join(positionals, " ") != strings.Join(tc.wantPositio, " ") {
			t.Errorf("%v: positionals = %v", tc.in, positionals)
		}
	}
}

func TestPaneProbeHelpers(t *testing.T) {
	dir := t.TempDir()
	if !dirExists(dir) {
		t.Fatal("dirExists missed a directory")
	}
	if dirExists(filepath.Join(dir, "nope")) {
		t.Fatal("dirExists accepted a missing path")
	}
	if fileExists(dir) {
		t.Fatal("fileExists is a file probe and must not accept a directory")
	}
	shell := []herdrProc{shellProc()}
	if !paneAtShellPrompt(shell) {
		t.Fatal("a lone zsh is a shell prompt")
	}
	if paneAtShellPrompt(nil) {
		t.Fatal("no processes is not a shell prompt")
	}
	if paneAtShellPrompt([]herdrProc{{Argv0: "vim"}}) {
		t.Fatal("vim is not a shell prompt")
	}
	if !paneRuns([]herdrProc{{Argv0: "/Users/x/go/bin/ttt"}}, "ttt") {
		t.Fatal("ttt by path not detected")
	}
	if paneRuns(shell, "ttt") {
		t.Fatal("false positive")
	}
}

func TestStateRoundTrip(t *testing.T) {
	home := t.TempDir()
	t.Setenv("FORSETI_STATE", filepath.Join(home, "state.json"))
	want := ideState{Tab: "w1:t8", EditorPane: "w1:p1", AgentPane: "w1:p2", Dir: "/tmp/x", Label: "forseti"}
	if err := writeIDEState(want); err != nil {
		t.Fatal(err)
	}
	got, err := readIDEState("forseti")
	if err != nil {
		t.Fatal(err)
	}
	if got.Tab != want.Tab || got.EditorPane != want.EditorPane || got.AgentPane != want.AgentPane {
		t.Fatalf("round trip = %+v", got)
	}
	st, err := os.Stat(filepath.Join(home, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("state mode = %04o, want 0600", st.Mode().Perm())
	}
}

func TestStartCreatesThenRevivesInPlace(t *testing.T) {
	f := newFixture(t, stubWorld{
		Tabs:  []herdrTab{{TabID: "w1:t1", Label: "user", Number: 1, WorkspaceID: "w1", PaneCount: 1}},
		Panes: []herdrPane{{PaneID: "w1:p1", TabID: "w1:t1", WorkspaceID: "w1", Cwd: "/"}},
		Procs: map[string][]herdrProc{"w1:p1": {shellProc()}},
	})
	opts := startOpts{yes: true}

	res, err := f.starter(opts).run()
	if err != nil {
		t.Fatal(err)
	}
	if !res.Created || res.Tab == "" || res.EditorPane == "" || res.AgentPane == "" {
		t.Fatalf("create result = %+v", res)
	}
	if res.Editor != "started" || res.Agent != "started" {
		t.Fatalf("create did not start both: %+v", res)
	}
	w := f.readWorld()
	if len(w.Tabs) != 2 {
		t.Fatalf("tabs = %d, want 2 (user + IDE)", len(w.Tabs))
	}
	if len(w.Runs) != 1 || !strings.Contains(w.Runs[0], "--listen") || !strings.Contains(w.Runs[0], f.dir) {
		t.Fatalf("editor command = %v", w.Runs)
	}
	if len(w.Agents) != 1 || w.Agents[0].Name != "coder" || w.Agents[0].PaneID != res.AgentPane {
		t.Fatalf("agents = %+v", w.Agents)
	}
	st, err := readIDEState("forseti")
	if err != nil {
		t.Fatal(err)
	}
	if st.Tab != res.Tab || st.EditorPane != res.EditorPane || st.AgentPane != res.AgentPane || st.Dir != f.dir {
		t.Fatalf("state = %+v", st)
	}

	// A herdr restart: same tabs/panes, TUI processes gone, agent not resumed.
	paneProcs := map[string][]herdrProc{}
	for _, p := range w.Panes {
		paneProcs[p.PaneID] = []herdrProc{shellProc()}
	}
	w.Agents = nil
	w.Runs = nil
	w.Procs = paneProcs
	f.writeWorld(w)

	res2, err := f.starter(opts).run()
	if err != nil {
		t.Fatal(err)
	}
	if res2.Created || res2.Tab != res.Tab {
		t.Fatalf("revive result = %+v (want same tab %s)", res2, res.Tab)
	}
	if res2.Editor != "started" || res2.Agent != "started" {
		t.Fatalf("revive did not restart both: %+v", res2)
	}
	if res2.EditorPane != res.EditorPane || res2.AgentPane != res.AgentPane {
		t.Fatalf("revive moved panes: %+v vs %+v", res2, res)
	}
	w2 := f.readWorld()
	if len(w2.Tabs) != 2 {
		t.Fatalf("revive created a tab: %d tabs", len(w2.Tabs))
	}
	if len(w2.Agents) != 1 || w2.Agents[0].Name != "coder" {
		t.Fatalf("revive agents = %+v", w2.Agents)
	}
}

func TestReviveLeavesBusyPanesAlone(t *testing.T) {
	f := newFixture(t, stubWorld{
		Tabs: []herdrTab{{TabID: "w1:t1", Label: "forseti", Number: 1, WorkspaceID: "w1", PaneCount: 2}},
		Panes: []herdrPane{
			{PaneID: "w1:p1", TabID: "w1:t1", WorkspaceID: "w1", Cwd: "/"},
			{PaneID: "w1:p2", TabID: "w1:t1", WorkspaceID: "w1", Cwd: "/"},
		},
		Procs: map[string][]herdrProc{
			"w1:p1": {{Argv0: "vim", Name: "vim", Cmdline: "vim main.go"}},
			"w1:p2": {{Argv0: "htop", Name: "htop", Cmdline: "htop"}},
		},
	})
	if err := writeIDEState(ideState{Tab: "w1:t1", EditorPane: "w1:p1", AgentPane: "w1:p2", Dir: "/"}); err != nil {
		t.Fatal(err)
	}
	res, err := f.starter(startOpts{yes: true}).run()
	if err != nil {
		t.Fatal(err)
	}
	if res.Editor != "busy" || res.Agent != "busy" {
		t.Fatalf("busy panes were touched: %+v", res)
	}
	w := f.readWorld()
	if len(w.Runs) != 0 || len(w.Agents) != 0 {
		t.Fatalf("started something: runs=%v agents=%v", w.Runs, w.Agents)
	}
}

func TestStartClosesDeadForsetiDuplicate(t *testing.T) {
	f := newFixture(t, stubWorld{
		Tabs: []herdrTab{
			{TabID: "w1:t1", Label: "forseti", Number: 1, WorkspaceID: "w1", PaneCount: 2},
			{TabID: "w1:t8", Label: "forseti", Number: 8, WorkspaceID: "w1", PaneCount: 3},
			{TabID: "w1:t9", Label: "notes", Number: 9, WorkspaceID: "w1", PaneCount: 1},
		},
		Panes: []herdrPane{
			{PaneID: "w1:p1", TabID: "w1:t1", WorkspaceID: "w1", Cwd: "/"},
			{PaneID: "w1:p2", TabID: "w1:t1", WorkspaceID: "w1", Cwd: "/"},
			{PaneID: "w1:p8a", TabID: "w1:t8", WorkspaceID: "w1", Cwd: "/"},
			{PaneID: "w1:p8b", TabID: "w1:t8", WorkspaceID: "w1", Cwd: "/"},
			{PaneID: "w1:p8c", TabID: "w1:t8", WorkspaceID: "w1", Cwd: "/"},
			{PaneID: "w1:p9", TabID: "w1:t9", WorkspaceID: "w1", Cwd: "/"},
		},
		Procs: map[string][]herdrProc{
			"w1:p1": {shellProc()}, "w1:p2": {shellProc()},
			"w1:p8a": {shellProc()}, "w1:p8b": {shellProc()}, "w1:p8c": {shellProc()},
			"w1:p9": {shellProc()},
		},
	})
	if err := writeIDEState(ideState{Tab: "w1:t1", EditorPane: "w1:p1", AgentPane: "w1:p2", Dir: "/"}); err != nil {
		t.Fatal(err)
	}
	res, err := f.starter(startOpts{yes: true}).run()
	if err != nil {
		t.Fatal(err)
	}
	if res.Tab != "w1:t1" {
		t.Fatalf("revived %s, want the tracked tab", res.Tab)
	}
	w := f.readWorld()
	if len(w.Closed) != 1 || w.Closed[0] != "w1:t8" {
		t.Fatalf("closed = %v, want exactly the dead forseti duplicate", w.Closed)
	}
	for _, tab := range w.Tabs {
		if tab.TabID == "w1:t9" {
			return
		}
	}
	t.Fatal("a user tab was closed")
}

func TestPruneDeadForeignIsOptIn(t *testing.T) {
	live := t.TempDir() // an existing directory must never count as "gone"
	build := func() stubWorld {
		return stubWorld{
			Tabs: []herdrTab{
				{TabID: "w1:t1", Label: "forseti", Number: 1, WorkspaceID: "w1", PaneCount: 2},
				{TabID: "w1:t25", Label: "form-drive", Number: 25, WorkspaceID: "w1", PaneCount: 1},
				{TabID: "w1:t27", Label: "s16-lazygit", Number: 27, WorkspaceID: "w1", PaneCount: 1},
			},
			Panes: []herdrPane{
				{PaneID: "w1:p1", TabID: "w1:t1", WorkspaceID: "w1", Cwd: live},
				{PaneID: "w1:p2", TabID: "w1:t1", WorkspaceID: "w1", Cwd: live},
				{PaneID: "w1:p25", TabID: "w1:t25", WorkspaceID: "w1", Cwd: "/gone/dir", RestoreError: "Saved directory is unavailable."},
				{PaneID: "w1:p27", TabID: "w1:t27", WorkspaceID: "w1", Cwd: live},
			},
			Procs: map[string][]herdrProc{
				"w1:p1": {shellProc()}, "w1:p2": {shellProc()},
				"w1:p25": {shellProc()}, "w1:p27": {shellProc()},
			},
		}
	}

	f := newFixture(t, build())
	if err := writeIDEState(ideState{Tab: "w1:t1", EditorPane: "w1:p1", AgentPane: "w1:p2", Dir: "/"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.starter(startOpts{yes: true}).run(); err != nil {
		t.Fatal(err)
	}
	if w := f.readWorld(); len(w.Closed) != 0 {
		t.Fatalf("closed a foreign tab without --prune-dead: %v", w.Closed)
	}

	f2 := newFixture(t, build())
	if err := writeIDEState(ideState{Tab: "w1:t1", EditorPane: "w1:p1", AgentPane: "w1:p2", Dir: "/"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f2.starter(startOpts{yes: true, prune: true}).run(); err != nil {
		t.Fatal(err)
	}
	w := f2.readWorld()
	if len(w.Closed) != 1 || w.Closed[0] != "w1:t25" {
		t.Fatalf("closed = %v, want the vanished-cwd tab", w.Closed)
	}
}

func TestAgentNameTakenElsewhere(t *testing.T) {
	f := newFixture(t, stubWorld{
		Tabs: []herdrTab{
			{TabID: "w1:t3", Label: "other", Number: 3, WorkspaceID: "w1", PaneCount: 1},
			{TabID: "w1:t1", Label: "forseti", Number: 1, WorkspaceID: "w1", PaneCount: 2},
		},
		Panes: []herdrPane{
			{PaneID: "w1:p3", TabID: "w1:t3", WorkspaceID: "w1", Cwd: "/"},
			{PaneID: "w1:p1", TabID: "w1:t1", WorkspaceID: "w1", Cwd: "/"},
			{PaneID: "w1:p2", TabID: "w1:t1", WorkspaceID: "w1", Cwd: "/"},
		},
		Agents: []herdrAgent{{Name: "coder", PaneID: "w1:p3", TabID: "w1:t3", Agent: "pi", AgentStatus: "idle"}},
		Procs: map[string][]herdrProc{
			"w1:p3": {{Argv0: "pi", Name: "node"}},
			"w1:p1": {shellProc()}, "w1:p2": {shellProc()},
		},
	})
	if err := writeIDEState(ideState{Tab: "w1:t1", EditorPane: "w1:p1", AgentPane: "w1:p2", Dir: "/"}); err != nil {
		t.Fatal(err)
	}
	res, err := f.starter(startOpts{yes: true}).run()
	if err != nil {
		t.Fatal(err)
	}
	if res.Agent != "taken" {
		t.Fatalf("agent = %q, want taken", res.Agent)
	}
	if res.Editor != "started" {
		t.Fatalf("editor = %q, want started", res.Editor)
	}
}

func TestDryRunMutatesNothing(t *testing.T) {
	f := newFixture(t, stubWorld{
		Tabs:  []herdrTab{{TabID: "w1:t9", Label: "forseti", Number: 9, WorkspaceID: "w1", PaneCount: 2}},
		Panes: []herdrPane{{PaneID: "w1:p9a", TabID: "w1:t9", WorkspaceID: "w1", Cwd: "/"}, {PaneID: "w1:p9b", TabID: "w1:t9", WorkspaceID: "w1", Cwd: "/"}},
		Procs: map[string][]herdrProc{"w1:p9a": {shellProc()}, "w1:p9b": {shellProc()}},
	})
	before := f.readWorld()
	if _, err := f.starter(startOpts{yes: true, dry: true}).run(); err != nil {
		t.Fatal(err)
	}
	after := f.readWorld()
	if len(after.Runs) != 0 || len(after.Agents) != 0 || len(after.Closed) != 0 || len(after.Tabs) != len(before.Tabs) {
		t.Fatalf("dry run mutated the world: %+v", after)
	}
	if _, err := os.Stat(f.state); !os.IsNotExist(err) {
		t.Fatal("dry run wrote the state file")
	}
}

func TestInspectIDE(t *testing.T) {
	f := newFixture(t, stubWorld{})
	opts := startOpts{yes: true}
	res, err := f.starter(opts).run()
	if err != nil {
		t.Fatal(err)
	}
	h, ok := inspectIDE(f.cfg)
	if !ok {
		t.Fatal("inspectIDE: herdr unreachable")
	}
	if !h.OK || h.Tab != res.Tab || h.Editor != "running" || h.Agent != "idle" || h.Number == 0 {
		t.Fatalf("health = %+v (res %+v)", h, res)
	}
	if detail := ideDetail(h); !strings.Contains(detail, "ttt · agent idle") {
		t.Fatalf("detail = %q", detail)
	}
}
