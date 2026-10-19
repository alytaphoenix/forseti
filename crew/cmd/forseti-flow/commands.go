package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"forseti/crew/internal/flow"
)

// stringList is a repeatable flag value.
type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error {
	*s = append(*s, v)
	return nil
}

func humanMap(ids []string) map[string]bool {
	if len(ids) == 0 {
		return nil
	}
	m := make(map[string]bool, len(ids))
	for _, id := range ids {
		m[id] = true
	}
	return m
}

func cmdCheck(args []string) {
	fs := flag.NewFlagSet("check", flag.ExitOnError)
	file := fs.String("f", "", "flow file (default: .forseti/flow.yaml, searched upward)")
	flowID := fs.String("flow", "", "flow id (required when the file declares several)")
	stage := fs.String("stage", "pre_commit", "lifecycle stage (pre_commit, pre_push, post_turn, ...)")
	dir := fs.String("dir", "", "checkout root (default: derived from the flow file)")
	asJSON := fs.Bool("json", false, "machine-readable result")
	msgFile := fs.String("message-file", "", "commit message file (commit_msg stage)")
	var assume stringList
	fs.Var(&assume, "assume", "human evidence id to treat as confirmed (repeatable)")
	_ = fs.Parse(args)

	_, fl, path, err := loadSelected(*file, *flowID)
	if err != nil {
		fail(err)
	}
	root := *dir
	if root == "" {
		root = flowRoot(path)
	}
	stageName := flow.NormalizeStage(*stage)

	if stageName == "commit_msg" {
		if *msgFile == "" {
			fail(fmt.Errorf("check --stage commit_msg requires --message-file"))
		}
		data, err := os.ReadFile(*msgFile)
		if err != nil {
			fail(err)
		}
		res, err := flow.CheckCommitMessage(fl, string(data))
		if err != nil {
			fail(err)
		}
		ok := res.Status == flow.StatusPass
		if *asJSON {
			writeJSON(struct {
				Flow   string      `json:"flow"`
				Stage  string      `json:"stage"`
				OK     bool        `json:"ok"`
				Result flow.Result `json:"result"`
			}{fl.ID, stageName, ok, res})
		} else if ok {
			fmt.Printf("OK: %s commit message\n", fl.ID)
		} else {
			fmt.Printf("BLOCKED: %s commit message — %s\n", fl.ID, res.Detail)
		}
		if !ok {
			os.Exit(1)
		}
		return
	}

	statePath := flow.StatePath(root)
	state, err := flow.LoadState(statePath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "warning:", err)
		state = &flow.State{Version: flow.Version, Evidence: map[string]flow.Result{}}
	}
	ev := flow.NewEvaluator(root, fl, state)
	ev.Human = humanMap(assume)
	waivers, err := flow.ReadWaivers(resolveRootPath(root, fl.WaiverLedger()))
	if err != nil {
		fail(fmt.Errorf("waiver ledger: %w", err))
	}
	res, err := flow.CheckStage(context.Background(), ev, stageName, waivers)
	if err != nil {
		fail(err)
	}
	if err := state.MergeSave(statePath); err != nil {
		fmt.Fprintln(os.Stderr, "warning: could not save flow state:", err)
	}

	if *asJSON {
		writeJSON(res)
	} else {
		fmt.Printf("flow %s · %s\n", res.Flow, res.Stage)
		for _, e := range res.Evidence {
			fmt.Printf("  %-6s %-20s (%s) %s\n", strings.ToUpper(e.Status), e.ID, e.Source, e.Detail)
		}
		for _, w := range res.Warnings {
			fmt.Println("  WARN  ", w)
		}
		switch {
		case res.Blocked:
			fmt.Printf("BLOCKED: %s — %s\n", res.Stage, strings.Join(res.Messages, "; "))
		case len(res.Warnings) > 0:
			fmt.Printf("OK (%d warning(s))\n", len(res.Warnings))
		default:
			fmt.Println("OK")
		}
	}
	if res.Blocked {
		os.Exit(1)
	}
}

// cmdEval evaluates one evidence id. The pi runtime uses it for phase gates
// (which are not lifecycle stages).
func cmdEval(args []string) {
	fs := flag.NewFlagSet("eval", flag.ExitOnError)
	file := fs.String("f", "", "flow file")
	flowID := fs.String("flow", "", "flow id")
	evidence := fs.String("evidence", "", "evidence id")
	stage := fs.String("stage", "", "optional stage for waiver lookup")
	dir := fs.String("dir", "", "checkout root")
	asJSON := fs.Bool("json", false, "machine-readable result")
	var assume stringList
	fs.Var(&assume, "assume", "human evidence id to treat as confirmed (repeatable)")
	_ = fs.Parse(args)

	if strings.TrimSpace(*evidence) == "" {
		fail(fmt.Errorf("eval requires --evidence"))
	}
	_, fl, path, err := loadSelected(*file, *flowID)
	if err != nil {
		fail(err)
	}
	root := *dir
	if root == "" {
		root = flowRoot(path)
	}
	statePath := flow.StatePath(root)
	state, err := flow.LoadState(statePath)
	if err != nil {
		state = &flow.State{Version: flow.Version, Evidence: map[string]flow.Result{}}
	}
	ev := flow.NewEvaluator(root, fl, state)
	ev.Human = humanMap(assume)
	res := ev.Eval(context.Background(), *evidence)
	if *stage != "" {
		waivers, _ := flow.ReadWaivers(resolveRootPath(root, fl.WaiverLedger()))
		if w := flow.ActiveWaiver(waivers, fl.ID, flow.NormalizeStage(*stage), *evidence, time.Now()); w != nil {
			res.Status = flow.StatusWaived
			res.Detail = "waived: " + w.Reason
		}
	}
	if err := state.MergeSave(statePath); err != nil {
		fmt.Fprintln(os.Stderr, "warning: could not save flow state:", err)
	}
	if *asJSON {
		writeJSON(res)
	} else {
		fmt.Printf("%s %s: %s\n", strings.ToUpper(res.Status), res.ID, res.Detail)
	}
	if res.Status != flow.StatusPass && res.Status != flow.StatusWaived {
		os.Exit(1)
	}
}

// cmdRun compiles the flow to a crew, delegates the agent graph to
// forseti-crew, then runs terminal shell phases (and their gates) in order.
func cmdRun(args []string) {
	file, flowID, dir, rest := extractRunFlags(args)
	_, fl, path, err := loadSelected(file, flowID)
	if err != nil {
		fail(err)
	}
	root := dir
	if root == "" {
		root = flowRoot(path)
	}
	data, err := flow.Marshal(fl)
	if err != nil {
		fail(err)
	}
	gen := filepath.Join(root, ".forseti", "flow", "generated", fl.ID+".crew.yaml")
	if err := os.MkdirAll(filepath.Dir(gen), 0o700); err != nil {
		fail(err)
	}
	if err := os.WriteFile(gen, data, 0o644); err != nil {
		fail(err)
	}
	fmt.Fprintln(os.Stderr, "forseti flow: generated", gen)

	if len(flow.ShellPhases(fl)) > 0 && hasArg(rest, "--worktree") {
		fail(fmt.Errorf("flow has terminal shell phases; they cannot run safely with --worktree (the worktree is torn down) — run without --worktree or add --keep-worktree and run the phase manually"))
	}
	crewBin, err := resolveCrewBin(root)
	if err != nil {
		fail(err)
	}
	crewArgs := append([]string{"run", "-f", gen}, rest...)
	cmd := exec.Command(crewBin, crewArgs...)
	cmd.Dir = root
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			os.Exit(exit.ExitCode())
		}
		fail(err)
	}

	for _, p := range flow.ShellPhases(fl) {
		fmt.Fprintf(os.Stderr, "forseti flow: shell phase %s: %s\n", p.ID, p.Run)
		sh := exec.Command("sh", "-c", p.Run)
		sh.Dir = root
		sh.Stdin, sh.Stdout, sh.Stderr = os.Stdin, os.Stdout, os.Stderr
		if err := sh.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "forseti flow: shell phase %s failed\n", p.ID)
			os.Exit(1)
		}
		if p.Gate == nil || len(p.Gate.Require) == 0 {
			continue
		}
		statePath := flow.StatePath(root)
		state, err := flow.LoadState(statePath)
		if err != nil {
			state = &flow.State{Version: flow.Version, Evidence: map[string]flow.Result{}}
		}
		ev := flow.NewEvaluator(root, fl, state)
		bad := false
		for _, id := range p.Gate.Require {
			r := ev.Eval(context.Background(), id)
			fmt.Fprintf(os.Stderr, "forseti flow: %-6s %s: %s\n", strings.ToUpper(r.Status), id, r.Detail)
			if r.Status != flow.StatusPass && r.Status != flow.StatusWaived {
				bad = true
			}
		}
		_ = state.MergeSave(statePath)
		if bad {
			fmt.Fprintf(os.Stderr, "forseti flow: shell phase %s gate is not satisfied\n", p.ID)
			os.Exit(1)
		}
	}
}

// extractRunFlags pulls flow-level flags out of the argv and leaves the rest
// for forseti-crew (--headless, --session, --engine, ...).
func extractRunFlags(args []string) (file, flowID, dir string, rest []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--flow" && i+1 < len(args):
			flowID = args[i+1]
			i++
		case strings.HasPrefix(a, "--flow="):
			flowID = strings.TrimPrefix(a, "--flow=")
		case a == "-f" && i+1 < len(args):
			file = args[i+1]
			i++
		case strings.HasPrefix(a, "-f="):
			file = strings.TrimPrefix(a, "-f=")
		case a == "--dir" && i+1 < len(args):
			dir = args[i+1]
			i++
		case strings.HasPrefix(a, "--dir="):
			dir = strings.TrimPrefix(a, "--dir=")
		default:
			rest = append(rest, a)
		}
	}
	return
}

func hasArg(args []string, name string) bool {
	for _, a := range args {
		if a == name || strings.HasPrefix(a, name+"=") {
			return true
		}
	}
	return false
}

func resolveCrewBin(root string) (string, error) {
	if b := os.Getenv("FORSETI_CREW_BIN"); b != "" {
		return b, nil
	}
	var candidates []string
	if r := os.Getenv("FORSETI_REPO"); r != "" {
		candidates = append(candidates, filepath.Join(r, "crew", "bin", "forseti-crew"))
	}
	candidates = append(candidates,
		filepath.Join(root, "crew", "bin", "forseti-crew"),
		filepath.Join(root, "bin", "forseti-crew"),
	)
	for _, p := range candidates {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p, nil
		}
	}
	if p, err := exec.LookPath("forseti-crew"); err == nil {
		return p, nil
	}
	return "", fmt.Errorf("forseti-crew not found (build it with `cd crew && go build -o bin/forseti-crew ./cmd/forseti-crew` or set FORSETI_CREW_BIN)")
}

// cmdSimulate evaluates every evidence and every stage without running an
// agent. `--diff REF` previews a historical/base diff.
func cmdSimulate(args []string) {
	fs := flag.NewFlagSet("simulate", flag.ExitOnError)
	file := fs.String("f", "", "flow file")
	flowID := fs.String("flow", "", "flow id")
	diff := fs.String("diff", "", "base ref for diff evidence (default: each evidence's base)")
	dir := fs.String("dir", "", "checkout root")
	asJSON := fs.Bool("json", false, "machine-readable result")
	var assume stringList
	fs.Var(&assume, "assume", "human evidence id to treat as confirmed (repeatable)")
	_ = fs.Parse(args)

	_, fl, path, err := loadSelected(*file, *flowID)
	if err != nil {
		fail(err)
	}
	root := *dir
	if root == "" {
		root = flowRoot(path)
	}
	statePath := flow.StatePath(root)
	state, err := flow.LoadState(statePath)
	if err != nil {
		state = &flow.State{Version: flow.Version, Evidence: map[string]flow.Result{}}
	}
	ev := flow.NewEvaluator(root, fl, state)
	ev.BaseOverride = *diff
	ev.Human = humanMap(assume)
	results := make([]flow.Result, 0, len(fl.Evidence))
	for _, id := range fl.EvidenceIDs() {
		results = append(results, ev.Eval(context.Background(), id))
	}
	waivers, _ := flow.ReadWaivers(resolveRootPath(root, fl.WaiverLedger()))
	type stageView struct {
		Stage    string   `json:"stage"`
		OK       bool     `json:"ok"`
		Blocked  bool     `json:"blocked"`
		Warnings []string `json:"warnings,omitempty"`
		Messages []string `json:"messages,omitempty"`
		Pattern  string   `json:"pattern,omitempty"`
	}
	var stages []stageView
	for _, st := range flow.StageOrder {
		rule, ok := fl.On[st]
		if !ok {
			continue
		}
		if st == "pre_tool" || st == "commit_msg" {
			v := stageView{Stage: st, OK: true}
			if st == "commit_msg" {
				v.Pattern = rule.Pattern
			}
			stages = append(stages, v)
			continue
		}
		r, err := flow.CheckStage(context.Background(), ev, st, waivers)
		if err != nil {
			fail(err)
		}
		stages = append(stages, stageView{Stage: st, OK: r.OK, Blocked: r.Blocked, Warnings: r.Warnings, Messages: r.Messages})
	}
	_ = state.MergeSave(statePath)

	if *asJSON {
		writeJSON(struct {
			Flow     string        `json:"flow"`
			Evidence []flow.Result `json:"evidence"`
			Stages   []stageView   `json:"stages"`
		}{fl.ID, results, stages})
		return
	}
	fmt.Printf("flow %s (%s)\n", fl.ID, fl.EffectiveMode())
	if *diff != "" {
		fmt.Printf("diff base: %s\n", *diff)
	}
	fmt.Println("evidence:")
	for _, r := range results {
		fmt.Printf("  %-6s %-20s (%s) %s\n", strings.ToUpper(r.Status), r.ID, r.Source, r.Detail)
	}
	fmt.Println("stages:")
	for _, s := range stages {
		switch {
		case s.Pattern != "":
			fmt.Printf("  %-11s pattern=%s\n", s.Stage, s.Pattern)
		case s.Blocked:
			fmt.Printf("  %-11s BLOCKED — %s\n", s.Stage, strings.Join(s.Messages, "; "))
		case len(s.Warnings) > 0:
			fmt.Printf("  %-11s OK (%d warning(s))\n", s.Stage, len(s.Warnings))
		default:
			fmt.Printf("  %-11s OK\n", s.Stage)
		}
	}
}

func cmdEdit(args []string) {
	fs := flag.NewFlagSet("edit", flag.ExitOnError)
	file := fs.String("f", "", "flow file")
	forceEditor := fs.Bool("editor", false, "use $EDITOR instead of the TUI")
	raw := fs.Bool("raw", false, "open the raw-YAML editor instead of the structured builder")
	_ = fs.Parse(args)
	path, err := resolveFile(*file)
	if err != nil {
		fail(err)
	}
	if !*forceEditor && stdoutIsTTY() {
		var err error
		if *raw {
			err = runFlowEditor(path)
		} else {
			err = runFlowBuilder(path)
		}
		if err != nil {
			fail(err)
		}
		return
	}
	editor := os.Getenv("EDITOR")
	if editor == "" {
		editor = "vi"
	}
	cmd := exec.Command(editor, path)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		fail(err)
	}
}

func stdoutIsTTY() bool {
	st, err := os.Stdout.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

func cmdCI(args []string) {
	fs := flag.NewFlagSet("ci", flag.ExitOnError)
	file := fs.String("f", "", "flow file")
	_ = fs.Parse(args)
	path, err := resolveFile(*file)
	if err != nil {
		fail(err)
	}
	f, err := load(path)
	if err != nil {
		fail(err)
	}
	fmt.Println("# Generated by forseti-flow ci — adjust the build/install step to your CI.")
	fmt.Println("name: forseti-flow")
	fmt.Println("on: [pull_request]")
	fmt.Println("jobs:")
	fmt.Println("  flow:")
	fmt.Println("    runs-on: ubuntu-latest")
	fmt.Println("    steps:")
	fmt.Println("      - uses: actions/checkout@v4")
	fmt.Println("      - uses: actions/setup-go@v5")
	fmt.Println("        with: { go-version: '1.26' }")
	fmt.Println("      - run: go build -o \"$RUNNER_TEMP/forseti-flow\" ./crew/cmd/forseti-flow")
	for _, fl := range f.Flows {
		stage := ""
		switch {
		case fl.On["pre_pr"] != nil:
			stage = "pre_pr"
		case fl.On["pre_push"] != nil:
			stage = "pre_push"
		}
		if stage == "" {
			continue
		}
		fmt.Printf("      - run: \"$RUNNER_TEMP/forseti-flow\" check --flow %s --stage %s --dir \"$GITHUB_WORKSPACE\"\n", fl.ID, stage)
	}
}

func cmdWaive(args []string) {
	fs := flag.NewFlagSet("waive", flag.ExitOnError)
	file := fs.String("f", "", "flow file")
	flowID := fs.String("flow", "", "flow id")
	stage := fs.String("stage", "pre_commit", "lifecycle stage")
	evidence := fs.String("evidence", "", "evidence id to waive")
	reason := fs.String("reason", "", "why the gate is being waived")
	ttl := fs.String("ttl", "", "waiver lifetime (default: the flow's waivers.ttl; 0 = no expiry)")
	session := fs.String("session", "", "session label for the ledger")
	dir := fs.String("dir", "", "checkout root")
	_ = fs.Parse(args)

	if strings.TrimSpace(*evidence) == "" {
		fail(fmt.Errorf("waive requires --evidence"))
	}
	_, fl, path, err := loadSelected(*file, *flowID)
	if err != nil {
		fail(err)
	}
	root := *dir
	if root == "" {
		root = flowRoot(path)
	}
	stageName := flow.NormalizeStage(*stage)
	rule, ok := fl.On[stageName]
	if !ok {
		fail(fmt.Errorf("flow %q does not declare on.%s", fl.ID, stageName))
	}
	found := false
	for _, id := range rule.Require {
		if id == *evidence {
			found = true
			break
		}
	}
	if !found {
		fail(fmt.Errorf("evidence %q is not required by on.%s", *evidence, stageName))
	}
	if err := flow.WaiverAllowed(fl, *evidence); err != nil {
		fail(err)
	}
	if fl.WaiverReasonRequired() && strings.TrimSpace(*reason) == "" {
		fail(fmt.Errorf("waive requires --reason"))
	}
	now := time.Now()
	var expires *time.Time
	if *ttl != "0" {
		raw := *ttl
		if raw == "" {
			raw = fl.WaiverTTL()
		}
		d, err := time.ParseDuration(raw)
		if err != nil {
			fail(fmt.Errorf("--ttl %q: %w", raw, err))
		}
		if d < 0 {
			fail(fmt.Errorf("--ttl must be >= 0"))
		}
		t := now.Add(d)
		expires = &t
	}
	w := flow.Waiver{
		At: now, Flow: fl.ID, Stage: stageName, Evidence: *evidence,
		Reason: strings.TrimSpace(*reason), Session: *session, ExpiresAt: expires,
	}
	ledger := resolveRootPath(root, fl.WaiverLedger())
	if err := flow.AppendWaiver(ledger, w); err != nil {
		fail(err)
	}
	until := "no expiry"
	if expires != nil {
		until = "until " + expires.Format(time.RFC3339)
	}
	fmt.Printf("waived %s for %s/%s (%s)\n", *evidence, fl.ID, stageName, until)
}

func cmdInstallHooks(args []string) {
	fs := flag.NewFlagSet("install-hooks", flag.ExitOnError)
	file := fs.String("f", "", "flow file")
	dir := fs.String("dir", "", "checkout root")
	printOnly := fs.Bool("print", false, "print the hook blocks instead of writing")
	force := fs.Bool("force", false, "append the block to an existing hook without markers")
	_ = fs.Parse(args)

	path, err := resolveFile(*file)
	if err != nil {
		fail(err)
	}
	f, err := load(path)
	if err != nil {
		fail(err)
	}
	root := *dir
	if root == "" {
		root = flowRoot(path)
	}
	exe, err := os.Executable()
	if err != nil {
		fail(err)
	}
	exe, _ = filepath.Abs(exe)

	hooks := map[string]string{"pre_commit": "pre-commit", "pre_push": "pre-push", "commit_msg": "commit-msg"}
	var stages []string
	for stage := range hooks {
		stages = append(stages, stage)
	}
	sort.Strings(stages)

	blocks := map[string]string{}
	for _, stage := range stages {
		var lines []string
		for _, fl := range f.Flows {
			if _, ok := fl.On[stage]; !ok {
				continue
			}
			line := fmt.Sprintf("%s check --flow %s --stage %s --dir %s",
				shQuote(exe), shQuote(fl.ID), stage, shQuote(root))
			if stage == "commit_msg" {
				line += ` --message-file "$1"`
			}
			lines = append(lines, line)
		}
		if len(lines) == 0 {
			continue
		}
		var b strings.Builder
		b.WriteString("# >>> forseti flow (generated) >>>\n")
		b.WriteString("# edit .forseti/flow.yaml, not this hook\n")
		for _, line := range lines {
			b.WriteString(line + "\n")
		}
		b.WriteString("# <<< forseti flow (generated) <<<")
		blocks[hooks[stage]] = b.String()
	}
	if len(blocks) == 0 {
		fail(fmt.Errorf("no hook-compatible stages declared (pre_commit, pre_push, commit_msg)"))
	}

	hooksDir, err := gitHooksDir(root)
	if err != nil {
		hooksDir = filepath.Join(root, ".git", "hooks")
	}
	if *printOnly {
		for _, name := range sortedKeys(blocks) {
			fmt.Printf("== %s ==\n#!/bin/sh\n%s\n", name, blocks[name])
		}
		return
	}
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		fail(err)
	}
	for _, name := range sortedKeys(blocks) {
		hookPath := filepath.Join(hooksDir, name)
		if err := installHookBlock(hookPath, blocks[name], *force); err != nil {
			fail(err)
		}
		fmt.Println("installed", hookPath)
	}
}

func installHookBlock(path, block string, force bool) error {
	existing, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	content := string(existing)
	switch {
	case strings.Contains(content, ">>> forseti flow"):
		content = replaceHookBlock(content, block)
	case content == "":
		content = "#!/bin/sh\n" + block + "\n"
	case force:
		if !strings.HasPrefix(content, "#!") {
			content = "#!/bin/sh\n" + content
		}
		content = strings.TrimRight(content, "\n") + "\n\n" + block + "\n"
	default:
		return fmt.Errorf("%s exists without a forseti flow block — rerun with --print to review, or --force to append", path)
	}
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		return err
	}
	return os.Chmod(path, 0o755)
}

func replaceHookBlock(content, block string) string {
	start := strings.Index(content, "# >>> forseti flow")
	end := strings.Index(content, "# <<< forseti flow")
	if start < 0 || end < start {
		return content
	}
	endLine := strings.Index(content[end:], "\n")
	if endLine < 0 {
		endLine = len(content) - end
	}
	return content[:start] + block + content[end+endLine:]
}

func gitHooksDir(root string) (string, error) {
	out, err := exec.Command("git", "-C", root, "rev-parse", "--git-path", "hooks").Output()
	if err != nil {
		return "", err
	}
	p := strings.TrimSpace(string(out))
	if p == "" {
		return "", fmt.Errorf("empty git hooks path")
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(root, p)
	}
	return p, nil
}

func cmdInit(args []string) {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	dir := fs.String("dir", "", "checkout root (default: current directory)")
	printOnly := fs.Bool("print", false, "print the drafted flow instead of writing it")
	force := fs.Bool("force", false, "overwrite an existing .forseti/flow.yaml")
	preset := fs.String("preset", "repo", "repo | review-loop | docs-required | release")
	_ = fs.Parse(args)

	switch *preset {
	case "repo", "review-loop", "docs-required", "release":
	default:
		fail(fmt.Errorf("unknown preset %q (repo, review-loop, docs-required, release)", *preset))
	}
	root := *dir
	if root == "" {
		if out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output(); err == nil {
			root = strings.TrimSpace(string(out))
		}
		if root == "" {
			root, _ = os.Getwd()
		}
	}
	root, _ = filepath.Abs(root)
	doc := draftFlow(root, *preset)
	if *printOnly {
		fmt.Print(doc)
		return
	}
	target := filepath.Join(root, ".forseti", "flow.yaml")
	if _, err := os.Stat(target); err == nil && !*force {
		fail(fmt.Errorf("%s already exists (use --force to overwrite)", target))
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		fail(err)
	}
	if err := os.WriteFile(target, []byte(doc), 0o644); err != nil {
		fail(err)
	}
	fmt.Printf("wrote %s (preset %s)\n", target, *preset)
	fmt.Printf("next: forseti flow validate && forseti flow install-hooks\n")
	fmt.Printf("note: add .forseti/flow/ to .gitignore (run state and evidence cache)\n")
}

func draftFlow(root, preset string) string {
	name := flowNameFor(root)
	testCmd, testKnown := detectTestCmd(root)
	docs := []string{"README.md"}
	for _, d := range []string{"docs", "documentation"} {
		if st, err := os.Stat(filepath.Join(root, d)); err == nil && st.IsDir() {
			docs = append(docs, d+"/**")
		}
	}
	if _, err := os.Stat(filepath.Join(root, "AGENTS.md")); err == nil {
		docs = append(docs, "AGENTS.md")
	}
	generated := []string{"**/*.gen.go", "**/dist/**"}

	hybrid := preset == "review-loop"
	review := preset == "release" || hybrid

	var b strings.Builder
	fmt.Fprintf(&b, "# Drafted by `forseti-flow init --preset %s` from %s.\n", preset, root)
	b.WriteString("# Edit this file; the gates name evidence, never prose.\n")
	fmt.Fprintf(&b, "version: 1\nname: %s\n", name)
	if hybrid {
		b.WriteString("mode: hybrid\n")
	} else {
		b.WriteString("mode: repo\n")
	}
	b.WriteString("\ndefaults:\n  severity: warn\n")
	if hybrid {
		b.WriteString("  max_continues: 2\n  max_cycles: 2\n")
	}
	b.WriteString("\nevidence:\n")
	if hybrid {
		b.WriteString("  plan_artifact:\n    kind: artifact\n    file: PLAN.md\n\n")
	}
	if testKnown {
		fmt.Fprintf(&b, "  tests:\n    kind: shell\n    run: %s\n    timeout: 5m\n    within: 30m\n\n", strconv.Quote(testCmd))
	}
	fmt.Fprintf(&b, "  docs_touched:\n    kind: diff\n    changed: [%s]\n\n", quotedList(docs))
	fmt.Fprintf(&b, "  no_generated:\n    kind: diff\n    unchanged: [%s]\n    severity: block\n\n", quotedList(generated))
	if review {
		b.WriteString("  review_lgtm:\n    kind: review\n    role: reviewer\n")
		b.WriteString("    criteria: \"Correctness, tests, docs drift, and scope creep. LGTM only if no blocking defect remains.\"\n")
		b.WriteString("    verdict: {file: \".forseti/out/review.md\", labels: [lgtm, lbtm]}\n")
		b.WriteString("    model: " + defaultReviewModel() + "\n    timeout: 5m\n\n")
	}

	b.WriteString("on:\n")
	b.WriteString("  post_turn:\n    require: [docs_touched]\n    severity: warn\n")
	preCommit := []string{}
	if testKnown {
		preCommit = append(preCommit, "tests")
	}
	preCommit = append(preCommit, "no_generated")
	if preset == "docs-required" {
		preCommit = append(preCommit, "docs_touched")
	}
	fmt.Fprintf(&b, "  pre_commit:\n    require: [%s]\n    severity: block\n", strings.Join(preCommit, ", "))
	switch {
	case review && testKnown:
		b.WriteString("  pre_push:\n    require: [tests, review_lgtm]\n    severity: block\n")
	case review:
		b.WriteString("  pre_push:\n    require: [review_lgtm]\n    severity: block\n")
	case testKnown:
		b.WriteString("  pre_push:\n    require: [tests]\n    severity: block\n")
	}
	b.WriteString("  commit_msg:\n    pattern: '^(feat|fix|docs|test|refactor|chore)(\\(.+\\))?: .+'\n")

	if hybrid {
		b.WriteString("\nphases:\n")
		b.WriteString("  - id: plan\n    actor: agent\n    role: planner\n")
		b.WriteString("    prompt: \"Read the request and write PLAN.md: scope, non-goals, and a test plan.\"\n")
		b.WriteString("    produce: [plan]\n    gate: {require: [plan_artifact]}\n\n")
		b.WriteString("  - id: implement\n    actor: agent\n    from: [plan]\n")
		b.WriteString("    prompt: \"Implement the plan. Keep the diff minimal and update the docs.\"\n")
		if testKnown {
			b.WriteString("    gate: {require: [tests]}\n")
		}
		b.WriteString("\n  - id: review\n    actor: reviewer\n    from: [implement]\n    role: reviewer\n")
		b.WriteString("    criteria: \"Correctness, tests, docs drift, and scope creep.\"\n")
		b.WriteString("    verdict: {lgtm: commit, lbtm: implement}\n    max_visits: 2\n\n")
		b.WriteString("  - id: commit\n    actor: shell\n    from: [review]\n")
		b.WriteString("    run: \"git commit -am implement\"\n")
		if testKnown {
			b.WriteString("    gate: {require: [tests, review_lgtm]}\n")
		} else {
			b.WriteString("    gate: {require: [review_lgtm]}\n")
		}
	}

	b.WriteString("\nwaivers:\n  enabled: true\n  reason_required: true\n  ledger: .forseti/waivers.jsonl\n  ttl: 24h\n  never: [no_generated]\n")
	return b.String()
}

func flowNameFor(root string) string {
	base := strings.ToLower(filepath.Base(root))
	var b strings.Builder
	for _, r := range base {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	name := strings.Trim(b.String(), "-")
	if name == "" || name[0] < 'a' || name[0] > 'z' {
		name = "repo-" + name
	}
	if len(name) > 31 {
		name = name[:31]
	}
	return strings.TrimRight(name, "-")
}

func detectTestCmd(root string) (string, bool) {
	if exists(filepath.Join(root, "go.mod")) {
		return "go test ./...", true
	}
	var sub []string
	for _, d := range []string{"cli", "crew"} {
		if exists(filepath.Join(root, d, "go.mod")) {
			sub = append(sub, fmt.Sprintf("(cd %s && go test ./...)", d))
		}
	}
	if len(sub) > 0 {
		return strings.Join(sub, " && "), true
	}
	switch {
	case exists(filepath.Join(root, "package.json")):
		return "npm test", true
	case exists(filepath.Join(root, "pyproject.toml")):
		return "pytest -q", true
	case exists(filepath.Join(root, "Cargo.toml")):
		return "cargo test", true
	}
	return "", false
}

func exists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && !st.IsDir()
}

func quotedList(items []string) string {
	out := make([]string, 0, len(items))
	for _, s := range items {
		out = append(out, strconv.Quote(s))
	}
	return strings.Join(out, ", ")
}

func defaultReviewModel() string {
	if m := os.Getenv("FORSETI_FLOW_MODEL"); m != "" {
		return m
	}
	return "opencode-go/beam-free"
}

// loadSelected resolves the file, loads it, and picks the requested flow.
func loadSelected(file, flowID string) (*flow.File, *flow.Flow, string, error) {
	path, err := resolveFile(file)
	if err != nil {
		return nil, nil, path, err
	}
	f, err := load(path)
	if err != nil {
		return nil, nil, path, err
	}
	if flowID == "" {
		if len(f.Flows) == 1 {
			return f, f.Flows[0], path, nil
		}
		return nil, nil, path, fmt.Errorf("%s declares %d flows; pass --flow <id>", path, len(f.Flows))
	}
	fl := f.FlowByID(flowID)
	if fl == nil {
		return nil, nil, path, fmt.Errorf("flow %q not found in %s", flowID, path)
	}
	return f, fl, path, nil
}

// flowRoot is the checkout a flow belongs to: the parent of .forseti when the
// file lives there, otherwise the current directory.
func flowRoot(path string) string {
	dir := filepath.Dir(path)
	if filepath.Base(dir) == ".forseti" {
		return filepath.Dir(dir)
	}
	if wd, err := os.Getwd(); err == nil {
		return wd
	}
	return dir
}

// resolveRootPath keeps an absolute configured path absolute.
func resolveRootPath(root, p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(root, p)
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "ERROR:", err)
	os.Exit(2)
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
