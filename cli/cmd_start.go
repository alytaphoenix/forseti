package main

// forseti start — the one command that brings the IDE up and keeps it singular.
//
// The environment is: assets in sync, services up, a herdr server, and exactly
// ONE owned IDE tab (ttt in the editor pane, pi in the agent pane). herdr
// restores tab layout across server restarts but cannot restore a TUI process,
// so an owned tab comes back as bare shells; start revives it in place instead
// of creating a second one. What it settles on is recorded in
// ~/.config/forseti/state.json, so the tab is found by identity — never by
// grepping an agent name.

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// upSummary is start's --json document (the name predates `start`: `up` printed
// the same shape).
type upSummary struct {
	Repo     string         `json:"repo"`
	Dir      string         `json:"dir"`
	Assets   []assetResult  `json:"assets,omitempty"`
	SolPi    *solPiState    `json:"sol_pi,omitempty"`
	Services []serviceState `json:"services,omitempty"`
	Herdr    string         `json:"herdr"`
	IDE      *ideSummary    `json:"ide,omitempty"`
	Opened   bool           `json:"opened"`
	Tab      string         `json:"tab,omitempty"`
	Agent    string         `json:"agent,omitempty"`
}

// ideState is one owned IDE tab. The state file maps the tab label to its
// state, so the notes workspace (label `forseti-notes`) can have its own tab.
type ideState struct {
	Label      string `json:"label"`
	Tab        string `json:"tab"`
	EditorPane string `json:"editor_pane"`
	AgentPane  string `json:"agent_pane"`
	Dir        string `json:"dir"`
	Updated    string `json:"updated,omitempty"`
}

type ideStateFile struct {
	Tabs map[string]ideState `json:"tabs"`
}

type ideSummary struct {
	Tab        string   `json:"tab"`
	EditorPane string   `json:"editor_pane"`
	AgentPane  string   `json:"agent_pane"`
	Dir        string   `json:"dir"`
	Created    bool     `json:"created"`
	Editor     string   `json:"editor"` // running | started | busy | would-start | unknown
	Agent      string   `json:"agent"`  // running | started | busy | taken | would-start | unknown
	Closed     []string `json:"closed,omitempty"`
}

type startOpts struct {
	dir      string
	asJSON   bool
	noSync   bool
	noSvc    bool
	noOpen   bool
	attach   bool
	noAttach bool
	replace  bool
	clean    bool
	prune    bool
	yes      bool
	dry      bool
}

// startBoolFlags are the value-less flags, so `forseti start DIR --no-sync`
// parses like `forseti start --no-sync DIR`.
var startBoolFlags = map[string]bool{
	"no-sync": true, "no-services": true, "no-open": true,
	"attach": true, "no-attach": true, "replace": true,
	"clean": true, "prune-dead": true, "yes": true,
	"dry-run": true, "json": true,
}

func splitStartArgs(args []string) (flags, positionals []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			return flags, append(positionals, args[i+1:]...)
		}
		if !strings.HasPrefix(a, "-") || a == "-" {
			positionals = append(positionals, a)
			continue
		}
		flags = append(flags, a)
		if strings.Contains(a, "=") || startBoolFlags[strings.TrimLeft(a, "-")] {
			continue
		}
		if i+1 < len(args) { // this flag takes a value
			i++
			flags = append(flags, args[i])
		}
	}
	return flags, positionals
}

func cmdStart(args []string) {
	var o startOpts
	fs := flag.NewFlagSet("start", flag.ExitOnError)
	fs.StringVar(&o.dir, "dir", "", "target directory (default: the current directory)")
	var label string
	fs.StringVar(&label, "label", "", "tab label (default: herdr.editor_label)")
	fs.BoolVar(&o.noSync, "no-sync", false, "skip the ttt-plugin / pi-extension sync")
	fs.BoolVar(&o.noSvc, "no-services", false, "do not start memory/laya")
	fs.BoolVar(&o.noOpen, "no-open", false, "do not open (or revive) the IDE tab")
	fs.BoolVar(&o.attach, "attach", false, "if herdr is not running, exec herdr (attached) first")
	fs.BoolVar(&o.noAttach, "no-attach", false, "if herdr is not running, fail instead of starting it headless")
	fs.BoolVar(&o.replace, "replace", false, "create a fresh IDE tab instead of reviving the owned one")
	fs.BoolVar(&o.clean, "clean", false, "also prune dead forseti tabs in other workspaces")
	fs.BoolVar(&o.prune, "prune-dead", false, "also close tabs whose saved directory is gone")
	fs.BoolVar(&o.yes, "yes", false, "do not ask before closing stale tabs")
	fs.BoolVar(&o.dry, "dry-run", false, "print what would happen")
	fs.BoolVar(&o.asJSON, "json", false, "machine-readable summary")
	flags, positionals := splitStartArgs(args)
	_ = fs.Parse(flags)

	cfg := loadConfig()
	if label != "" {
		cfg.Herdr.EditorLabel = label
	}
	repo, err := findRepo(cfg)
	if err != nil {
		die("%v", err)
	}
	st, _ := readIDEState(cfg.Herdr.EditorLabel)

	dir := o.dir
	if dir == "" && len(positionals) > 0 {
		dir = positionals[0]
	}
	if dir == "" {
		dir = dirOrCwd()
		// A shell in $HOME is not a project: prefer the last IDE directory.
		if filepath.Clean(dir) == filepath.Clean(homeDir()) && st.Dir != "" && dirExists(st.Dir) {
			dir = st.Dir
		}
	}
	abs, err := filepath.Abs(expandHome(dir))
	if err != nil {
		die("target dir: %v", err)
	}
	if fi, err := os.Stat(abs); err != nil || !fi.IsDir() {
		die("%s is not a directory", abs)
	}
	abs = filepath.Clean(abs)

	sum := upSummary{Repo: repo, Dir: abs}
	line := func(kind, label, detail string) {
		if !o.asJSON {
			fmt.Printf("%-11s %-14s %s\n", kind, label, detail)
		}
	}

	// --- environment: assets + services -----------------------------------
	if !o.noSync {
		results, err := syncAssets(repo, o.dry)
		if err != nil {
			die("sync: %v", err)
		}
		sum.Assets = results
		for _, r := range results {
			switch {
			case r.Changed && o.dry:
				line("would sync", r.Asset.Label, r.Asset.Dst)
			case r.Changed:
				line("synced", r.Asset.Label, r.Asset.Dst)
			default:
				line("in sync", r.Asset.Label, r.Asset.Dst)
			}
		}
		pst, err := syncSolPi(cfg, o.dry)
		if err != nil {
			warn("sol-pi: %v", err)
		} else {
			sum.SolPi = &pst
			if !o.asJSON {
				reportSolPi(pst, o.dry)
			}
			if pst.Enabled && pst.Install && !pst.Installed {
				switch {
				case o.dry:
					line("would install", "sol-pi package", pst.Package)
				default:
					if pkg, installed, err := ensureSolPiInstall(cfg, false); err != nil {
						warn("sol-pi package did not install: %v", err)
						line("skipped", "sol-pi package", "pi install failed — see the warning")
					} else if installed {
						line("installed", "sol-pi package", pkg)
					}
				}
			}
		}
	}

	if !o.noSvc {
		for _, svc := range []struct {
			name string
			on   bool
		}{{"memory", cfg.Services.Memory}, {"laya", cfg.Services.Laya}} {
			if !svc.on {
				continue
			}
			sst := serviceStatus(repo, svc.name)
			if sst.Up {
				sum.Services = append(sum.Services, sst)
				line("up", svc.name, sst.Detail)
				continue
			}
			if o.dry {
				line("would start", svc.name, sst.Detail)
				continue
			}
			if err := startService(repo, svc.name); err != nil {
				warn("%s did not start: %v", svc.name, err)
				continue
			}
			sst = serviceStatus(repo, svc.name)
			sum.Services = append(sum.Services, sst)
			line("started", svc.name, sst.Detail)
		}
	}

	// --- herdr ------------------------------------------------------------
	if err := ensureHerdr(cfg, o); err != nil {
		sum.Herdr = err.Error()
		if o.asJSON {
			emitJSON(sum)
		}
		die("%v", err)
	}
	_, detail := herdrServer(cfg)
	sum.Herdr = detail
	line("up", "herdr server", detail)

	if !herdrPluginLinked(cfg) {
		if o.dry {
			line("would link", "herdr plugin", filepath.Join(repo, "herdr-plugin"))
		} else if out, err := runCapture(repo, nil, cfg.Herdr.Bin, "plugin", "link", filepath.Join(repo, "herdr-plugin")); err != nil {
			warn("herdr plugin link: %v (%s)", err, trim(out))
		} else {
			line("linked", "herdr plugin", filepath.Join(repo, "herdr-plugin"))
		}
	}

	// --- the IDE tab ------------------------------------------------------
	s := &starter{cfg: cfg, dir: abs, o: o, line: line}
	if o.noOpen {
		if o.clean {
			if err := s.load(); err == nil {
				s.prune("")
			}
		}
	} else if res, err := s.run(); err != nil {
		warn("IDE tab: %v", err)
	} else {
		sum.IDE = &res
		sum.Opened = res.Tab != ""
		sum.Tab = res.Tab
		if res.Agent == "running" || res.Agent == "started" {
			sum.Agent = cfg.Herdr.Agent
		}
		if !o.asJSON {
			switch {
			case res.Tab == "":
				line("would open", "IDE tab", res.Dir+" (editor + agent)")
			case o.dry:
				line("would", "IDE tab", fmt.Sprintf("%s · editor %s · agent %s", res.Tab, res.Editor, res.Agent))
			case res.Created:
				line("opened", "IDE tab", fmt.Sprintf("%s · editor %s · agent %s", res.Tab, res.Editor, res.Agent))
			default:
				line("revived", "IDE tab", fmt.Sprintf("%s · editor %s · agent %s", res.Tab, res.Editor, res.Agent))
			}
		}
	}

	if o.asJSON {
		emitJSON(sum)
	}
}

// --- herdr bootstrap ------------------------------------------------------

// ensureHerdr returns nil when a server answers. A missing server is started
// headless (default) or attached (--attach); --no-attach keeps the old failure.
func ensureHerdr(cfg Config, o startOpts) error {
	if running, _ := herdrServer(cfg); running {
		return nil
	}
	if o.dry {
		// Report what a real run would do; the reconcile step will warn that it
		// cannot inspect anything without a server.
		return nil
	}
	if o.noAttach {
		_, detail := herdrServer(cfg)
		return fmt.Errorf("herdr server is not running (%s)\n  start it with `herdr`, or drop --no-attach", detail)
	}
	if o.attach && isTTY(os.Stdin) {
		if err := runInteractive("", nil, cfg.Herdr.Bin); err != nil {
			return fmt.Errorf("herdr: %w", err)
		}
	} else if err := spawnHerdrServer(cfg); err != nil {
		return fmt.Errorf("could not start the herdr server: %w", err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if running, _ := herdrServer(cfg); running {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	_, detail := herdrServer(cfg)
	return fmt.Errorf("herdr server did not come up (%s) — see %s", detail, herdrBootLog())
}

func herdrBootLog() string {
	return filepath.Join(filepath.Dir(configPath()), "herdr-boot.log")
}

// spawnHerdrServer starts `herdr server` detached (its own session, log to a
// file) so a non-attached start controls it without owning a TTY.
func spawnHerdrServer(cfg Config) error {
	logPath := herdrBootLog()
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	cmd := exec.Command(cfg.Herdr.Bin, "server")
	cmd.Stdout = f
	cmd.Stderr = f
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

// --- the starter ----------------------------------------------------------

type starter struct {
	cfg  Config
	dir  string
	o    startOpts
	line func(kind, label, detail string)

	tabs   []herdrTab
	panes  []herdrPane
	agents []herdrAgent
}

func (s *starter) warn(format string, args ...any) {
	if !s.o.asJSON {
		warn(format, args...)
	}
}

func (s *starter) load() error {
	var err error
	if s.tabs, err = herdrTabs(s.cfg); err != nil {
		return err
	}
	if s.panes, err = herdrPanes(s.cfg); err != nil {
		return err
	}
	if s.agents, err = herdrAgents(s.cfg); err != nil {
		return err
	}
	return nil
}

func (s *starter) tabByID(id string) *herdrTab {
	for i := range s.tabs {
		if s.tabs[i].TabID == id {
			return &s.tabs[i]
		}
	}
	return nil
}

func (s *starter) panesOf(tab string) []herdrPane {
	var out []herdrPane
	for _, p := range s.panes {
		if p.TabID == tab {
			out = append(out, p)
		}
	}
	return out
}

func (s *starter) agentIn(tab string) *herdrAgent {
	for i := range s.agents {
		if s.agents[i].TabID == tab {
			return &s.agents[i]
		}
	}
	return nil
}

func (s *starter) namedAgent() *herdrAgent {
	for i := range s.agents {
		if s.agents[i].Name == s.cfg.Herdr.Agent {
			return &s.agents[i]
		}
	}
	return nil
}

// pickTab finds the tab forseti owns: the recorded id first, then the tab that
// holds the live named agent, then our label (preferring the target directory).
func (s *starter) pickTab(st ideState) *herdrTab {
	if s.o.replace {
		return nil
	}
	if st.Tab != "" {
		if t := s.tabByID(st.Tab); t != nil {
			return t
		}
	}
	if a := s.namedAgent(); a != nil {
		if t := s.tabByID(a.TabID); t != nil && t.Label == s.cfg.Herdr.EditorLabel {
			return t
		}
	}
	var best *herdrTab
	for i := range s.tabs {
		t := &s.tabs[i]
		if t.Label != s.cfg.Herdr.EditorLabel {
			continue
		}
		if s.tabCwd(t) == s.dir {
			return t
		}
		if best == nil {
			best = t
		}
	}
	return best
}

func (s *starter) tabCwd(t *herdrTab) string {
	for _, p := range s.panesOf(t.TabID) {
		if p.Cwd != "" {
			return filepath.Clean(p.Cwd)
		}
	}
	return ""
}

func (s *starter) run() (ideSummary, error) {
	if err := s.load(); err != nil {
		return ideSummary{}, err
	}
	st, _ := readIDEState(s.cfg.Herdr.EditorLabel)
	tab := s.pickTab(st)
	if tab == nil {
		res, err := s.create()
		if err != nil {
			return res, err
		}
		s.prune(res.Tab)
		return res, nil
	}
	res, err := s.revive(tab, st)
	if err != nil {
		return res, err
	}
	s.prune(res.Tab)
	return res, nil
}

func (s *starter) create() (ideSummary, error) {
	res := ideSummary{Dir: s.dir, Created: true}
	if s.o.dry {
		res.Editor, res.Agent = "would-start", "would-start"
		return res, nil
	}
	args := []string{"tab", "create", "--cwd", s.dir, "--label", s.cfg.Herdr.EditorLabel, "--no-focus"}
	if v := vaultRoot(); v != "" {
		args = append(args, "--env", "FORSETI_VAULT="+v)
	}
	out, err := herdrResult(s.cfg, args...)
	if err != nil {
		return res, err
	}
	tabID := jsonStr(jsonObj(out, "tab"), "tab_id")
	editor := jsonStr(jsonObj(out, "root_pane"), "pane_id")
	if tabID == "" || editor == "" {
		return res, fmt.Errorf("tab create: unexpected response %v", out)
	}
	split, err := herdrResult(s.cfg, "pane", "split", "--pane", editor, "--direction", "right", "--no-focus")
	if err != nil {
		return res, err
	}
	agentPane := jsonStr(jsonObj(split, "pane"), "pane_id")
	if agentPane == "" {
		return res, fmt.Errorf("pane split: unexpected response %v", split)
	}
	res.Tab, res.EditorPane, res.AgentPane = tabID, editor, agentPane
	res.Editor = s.startEditor(editor)
	res.Agent = s.startAgent(agentPane)
	_, _ = herdrResult(s.cfg, "tab", "focus", tabID)
	s.save(ideState{Tab: tabID, EditorPane: editor, AgentPane: agentPane})
	return res, nil
}

func (s *starter) revive(tab *herdrTab, st ideState) (ideSummary, error) {
	res := ideSummary{Tab: tab.TabID, Dir: s.dir}
	panes := s.panesOf(tab.TabID)
	has := func(id string) bool {
		for _, p := range panes {
			if p.PaneID == id {
				return true
			}
		}
		return false
	}

	// Which pane is the agent, which the editor: what we recorded, then
	// observable evidence (a live agent / a ttt process), then position.
	agentPane := ""
	if has(st.AgentPane) {
		agentPane = st.AgentPane
	}
	if a := s.namedAgent(); a != nil && has(a.PaneID) {
		agentPane = a.PaneID
	}
	editor := ""
	if st.EditorPane != "" && st.EditorPane != agentPane && has(st.EditorPane) {
		editor = st.EditorPane
	}
	if editor == "" {
		for _, p := range panes {
			if p.PaneID == agentPane {
				continue
			}
			if procs, err := herdrProcs(s.cfg, p.PaneID); err == nil &&
				(paneRuns(procs, s.cfg.TTT.Bin) || paneRuns(procs, "ttt")) {
				editor = p.PaneID
				break
			}
		}
	}
	if editor == "" {
		for _, p := range panes {
			if p.PaneID != agentPane {
				editor = p.PaneID
				break
			}
		}
	}
	if agentPane == "" {
		for _, p := range panes {
			if p.PaneID != editor {
				agentPane = p.PaneID
				break
			}
		}
	}
	if s.o.dry {
		res.EditorPane, res.AgentPane = editor, agentPane
		switch {
		case editor == "":
			res.Created = true
			res.Editor = "would-start"
		default:
			res.Editor = s.startEditor(editor) // read-only in dry-run
		}
		if agentPane == "" {
			res.Agent = "would-split"
		} else {
			res.Agent = s.startAgent(agentPane)
		}
		return res, nil
	}
	// A restore that collapsed the tab to one pane gets its split back.
	if editor == "" && len(panes) > 0 {
		editor = panes[0].PaneID
	}
	if editor == "" {
		return res, fmt.Errorf("tab %s has no usable pane", tab.TabID)
	}
	if agentPane == "" {
		split, err := herdrResult(s.cfg, "pane", "split", "--pane", editor, "--direction", "right", "--no-focus")
		if err != nil {
			return res, err
		}
		if agentPane = jsonStr(jsonObj(split, "pane"), "pane_id"); agentPane == "" {
			return res, fmt.Errorf("pane split: unexpected response %v", split)
		}
	}
	res.EditorPane, res.AgentPane = editor, agentPane
	res.Editor = s.startEditor(editor)
	res.Agent = s.startAgent(agentPane)
	if res.Editor == "started" || res.Agent == "started" {
		_, _ = herdrResult(s.cfg, "tab", "focus", tab.TabID)
	}
	s.save(ideState{Tab: tab.TabID, EditorPane: editor, AgentPane: agentPane})
	return res, nil
}

// startEditor makes sure ttt runs in the editor pane. A restored pane sits at a
// shell prompt; anything else in the pane belongs to the user and is left alone.
func (s *starter) startEditor(pane string) string {
	procs, err := herdrProcs(s.cfg, pane)
	if err != nil {
		return "unknown"
	}
	if !paneAtShellPrompt(procs) {
		if paneRuns(procs, s.cfg.TTT.Bin) || paneRuns(procs, "ttt") {
			return "running"
		}
		s.warn("IDE editor pane %s is busy (%s) — leaving it alone", pane, firstProc(procs))
		return "busy"
	}
	if s.o.dry {
		return "would-start"
	}
	if err := herdrExec(s.cfg, "pane", "run", pane, s.editorCmd()); err != nil {
		s.warn("ttt did not start in %s: %v", pane, err)
		return "unknown"
	}
	return "started"
}

// editorCmd is the command typed into the editor pane's shell.
func (s *starter) editorCmd() string {
	cmd := shellQuote(s.cfg.TTT.Bin)
	if tttListening(s.cfg.TTT.Port) {
		s.warn("ttt port %d is busy — starting ttt without --listen (jump/follow stay off)", s.cfg.TTT.Port)
	} else {
		cmd += " --listen"
	}
	cmd += " " + shellQuote(s.dir)
	if v := vaultRoot(); v != "" {
		cmd += " " + shellQuote(v)
	}
	return cmd
}

// startAgent makes sure our pi agent owns the agent pane.
func (s *starter) startAgent(pane string) string {
	if a := s.namedAgent(); a != nil {
		if a.PaneID == pane {
			return "running"
		}
		s.warn("agent %q is live in another pane (%s) — not starting a second one", s.cfg.Herdr.Agent, a.PaneID)
		return "taken"
	}
	procs, err := herdrProcs(s.cfg, pane)
	if err != nil {
		return "unknown"
	}
	if !paneAtShellPrompt(procs) {
		s.warn("IDE agent pane %s is busy (%s) — leaving it alone", pane, firstProc(procs))
		return "busy"
	}
	if s.o.dry {
		return "would-start"
	}
	if _, err := herdrResult(s.cfg, "agent", "start", s.cfg.Herdr.Agent, "--kind", "pi", "--pane", pane); err != nil {
		s.warn("pi did not start in %s: %v", pane, err)
		return "unknown"
	}
	return "started"
}

func (s *starter) save(st ideState) {
	st.Dir = s.dir
	st.Label = s.cfg.Herdr.EditorLabel
	st.Updated = time.Now().UTC().Format(time.RFC3339)
	if err := writeIDEState(st); err != nil {
		s.warn("could not record the IDE tab: %v", err)
	}
}

// --- pruning --------------------------------------------------------------

type pruneCandidate struct {
	TabID  string
	Number int
	Label  string
	Reason string
}

// prune closes tabs forseti can prove are dead: our own label with nothing but
// shells in it, and (--prune-dead) tabs whose saved directory is gone. A tab
// with a live agent or a non-shell foreground process is never touched.
func (s *starter) prune(keep string) {
	var ws string
	if keep != "" {
		if t := s.tabByID(keep); t != nil {
			ws = t.WorkspaceID
		}
		if ws == "" {
			return // the kept tab vanished between calls: prune nothing blindly
		}
	} else if !s.o.clean {
		return
	}
	var candidates []pruneCandidate
	for _, t := range s.tabs {
		if t.TabID == keep {
			continue
		}
		if !s.o.clean && t.WorkspaceID != ws {
			continue
		}
		panes := s.panesOf(t.TabID)
		if len(panes) == 0 {
			continue
		}
		live := s.agentIn(t.TabID) != nil
		switch {
		case t.Label == s.cfg.Herdr.EditorLabel && !live && s.tabDead(panes):
			candidates = append(candidates, pruneCandidate{t.TabID, t.Number, tabLabel(t), "dead " + s.cfg.Herdr.EditorLabel + " tab"})
		case s.o.prune && !live && s.tabUnrestorable(panes):
			candidates = append(candidates, pruneCandidate{t.TabID, t.Number, tabLabel(t), "saved directory is gone"})
		}
	}
	if len(candidates) == 0 {
		return
	}
	// A dry run only reports; a non-interactive caller (plugin, script, JSON)
	// never waits — the candidates are provably dead tabs.
	if !s.o.dry && !s.o.yes && !s.o.asJSON && !s.confirm(candidates) {
		return
	}
	for _, c := range candidates {
		if s.o.dry {
			s.line("would close", "tab "+strconv.Itoa(c.Number), fmt.Sprintf("%s (%s)", c.Label, c.Reason))
			continue
		}
		if _, err := herdrResult(s.cfg, "tab", "close", c.TabID); err != nil {
			s.warn("could not close tab %s: %v", c.TabID, err)
			continue
		}
		s.line("closed", "tab "+strconv.Itoa(c.Number), fmt.Sprintf("%s (%s)", c.Label, c.Reason))
	}
}

func (s *starter) confirm(candidates []pruneCandidate) bool {
	if !isTTY(os.Stdin) {
		return true // provably dead tabs only; scripts and the plugin never hang
	}
	fmt.Printf("forseti: close %d stale tab(s)?\n", len(candidates))
	for _, c := range candidates {
		fmt.Printf("  tab %d  %s  (%s)\n", c.Number, c.Label, c.Reason)
	}
	fmt.Print("[Y/n] ")
	answer, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "", "y", "yes":
		return true
	}
	return false
}

// tabDead reports whether every pane of the tab is an idle shell.
func (s *starter) tabDead(panes []herdrPane) bool {
	for _, p := range panes {
		procs, err := herdrProcs(s.cfg, p.PaneID)
		if err != nil {
			continue // an unreadable pane is not evidence of life
		}
		if len(procs) > 0 && !paneAtShellPrompt(procs) {
			return false
		}
	}
	return true
}

// tabUnrestorable reports a tab herdr could not rebuild (missing cwd).
func (s *starter) tabUnrestorable(panes []herdrPane) bool {
	for _, p := range panes {
		if p.RestoreError != "" {
			return true
		}
		if p.Cwd != "" && !dirExists(p.Cwd) {
			return true
		}
	}
	return false
}

// --- state ----------------------------------------------------------------

func ideStatePath() string {
	if v := os.Getenv("FORSETI_STATE"); v != "" {
		return expandHome(v)
	}
	return filepath.Join(filepath.Dir(configPath()), "state.json")
}

func readIDEState(label string) (ideState, error) {
	var st ideState
	data, err := os.ReadFile(ideStatePath())
	if err != nil {
		return st, err
	}
	var f ideStateFile
	if err := json.Unmarshal(data, &f); err != nil {
		return st, err
	}
	if st, ok := f.Tabs[label]; ok {
		return st, nil
	}
	return ideState{}, os.ErrNotExist
}

// writeIDEState merges one label into the state file (atomic, 0600).
func writeIDEState(st ideState) error {
	path := ideStatePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f := ideStateFile{Tabs: map[string]ideState{}}
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &f)
		if f.Tabs == nil {
			f.Tabs = map[string]ideState{}
		}
	}
	label := st.Label
	if label == "" {
		label = "forseti"
		st.Label = label
	}
	f.Tabs[label] = st
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// --- health (status / doctor) ---------------------------------------------

type ideHealth struct {
	Tab    string
	Number int
	Editor string // running | dead | absent
	Agent  string // idle | working | blocked | done | absent
	Dir    string
	OK     bool
}

// inspectIDE reports the owned tab's health; false when herdr is unreachable.
func inspectIDE(cfg Config) (ideHealth, bool) {
	h := ideHealth{}
	if running, _ := herdrServer(cfg); !running {
		return h, false
	}
	st, _ := readIDEState(cfg.Herdr.EditorLabel)
	h.Tab, h.Dir = st.Tab, st.Dir
	if st.Tab == "" {
		return h, true
	}
	tabs, err := herdrTabs(cfg)
	if err != nil {
		return h, true
	}
	var tab *herdrTab
	for i := range tabs {
		if tabs[i].TabID == st.Tab {
			tab = &tabs[i]
		}
	}
	if tab == nil {
		h.Tab = "" // the recorded tab is gone
		return h, true
	}
	h.Number = tab.Number
	if st.EditorPane != "" {
		if procs, err := herdrProcs(cfg, st.EditorPane); err == nil &&
			(paneRuns(procs, cfg.TTT.Bin) || paneRuns(procs, "ttt")) {
			h.Editor = "running"
		} else {
			h.Editor = "dead"
		}
	}
	if agents, err := herdrAgents(cfg); err == nil {
		for _, a := range agents {
			if a.Name == cfg.Herdr.Agent || a.PaneID == st.AgentPane {
				h.Agent = a.AgentStatus
			}
		}
	}
	h.OK = h.Editor == "running" &&
		(h.Agent == "idle" || h.Agent == "working" || h.Agent == "done")
	return h, true
}

// --- small helpers --------------------------------------------------------

func ideDetail(h ideHealth) string {
	editor := h.Editor
	switch editor {
	case "running":
		editor = "ttt"
	case "dead":
		editor = "ttt down"
	default:
		editor = "no editor pane"
	}
	agent := h.Agent
	if agent == "" {
		agent = "no agent"
	}
	return fmt.Sprintf("tab %d · %s · agent %s · %s", h.Number, editor, agent, h.Dir)
}

func tabLabel(t herdrTab) string {
	if t.Label != "" {
		return t.Label
	}
	return strconv.Itoa(t.Number)
}

func firstProc(procs []herdrProc) string {
	if len(procs) == 0 {
		return "nothing running"
	}
	p := procs[0]
	if p.Cmdline != "" {
		return p.Cmdline
	}
	if p.Argv0 != "" {
		return p.Argv0
	}
	return p.Name
}

func dirOrCwd() string {
	d, _ := os.Getwd()
	return d
}

// dirExists is a DIRECTORY probe (fileExists means "a regular file", which is
// what the asset checks want).
func dirExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}

// vaultRoot is the evergreen notes vault ttt opens as a second workspace root.
func vaultRoot() string {
	v := os.Getenv("FORSETI_VAULT")
	if v == "" {
		v = filepath.Join(homeDir(), "forseti")
	}
	v = expandHome(v)
	if fi, err := os.Stat(v); err == nil && fi.IsDir() {
		return v
	}
	return ""
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
