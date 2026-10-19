// Evidence evaluation: the only currency a gate may require. Results are
// cached by input hash; `within` bounds how long a pass may be reused.
package flow

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"forseti/crew/internal/schema"
)

// Result statuses. "skip" means the evidence could not be evaluated (no git,
// no interactive runtime); a blocking gate treats skip like fail.
const (
	StatusPass   = "pass"
	StatusFail   = "fail"
	StatusWarn   = "warn"
	StatusSkip   = "skip"
	StatusWaived = "waived"
)

// Result is one evidence evaluation.
type Result struct {
	ID        string    `json:"id"`
	Status    string    `json:"status"`
	At        time.Time `json:"at"`
	Source    string    `json:"source,omitempty"` // shell|diff|artifact|review|human|all|any|cache
	Detail    string    `json:"detail,omitempty"`
	InputHash string    `json:"input_hash,omitempty"`
	Cached    bool      `json:"cached,omitempty"`
}

// Runner executes a shell script in dir (injectable for tests).
type Runner func(ctx context.Context, dir, script string) (string, error)

func shellRunner(ctx context.Context, dir, script string) (string, error) {
	cmd := exec.CommandContext(ctx, "sh", "-c", script)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// Evaluator evaluates a flow's evidence against one checkout.
type Evaluator struct {
	Dir   string
	Flow  *Flow
	State *State
	Now   func() time.Time
	Run   Runner
	// BaseOverride replaces each diff evidence's `base` (simulate --diff).
	BaseOverride string
	// Human carries confirmations for `kind: human` evidence (id → yes/no).
	// Absent means "not confirmed" and evaluates to skip.
	Human map[string]bool

	memo   map[string]Result
	hashes map[string]string
}

// NewEvaluator returns an evaluator rooted at dir.
func NewEvaluator(dir string, fl *Flow, st *State) *Evaluator {
	return &Evaluator{
		Dir: dir, Flow: fl, State: st, Now: time.Now, Run: shellRunner,
		memo: map[string]Result{}, hashes: map[string]string{},
	}
}

// Eval evaluates one evidence id, reusing a fresh cached result when allowed.
func (ev *Evaluator) Eval(ctx context.Context, id string) Result {
	if r, ok := ev.memo[id]; ok {
		return r
	}
	e := ev.Flow.Evidence[id]
	if e == nil {
		return Result{ID: id, Status: StatusFail, At: ev.now(), Detail: "unknown evidence"}
	}
	hash := ev.inputHash(id)
	if r, ok := ev.cached(id, hash); ok {
		ev.memo[id] = r
		return r
	}
	r := ev.eval(ctx, id, e)
	r.ID = id
	r.At = ev.now()
	r.InputHash = hash
	if r.Source == "" {
		r.Source = e.Kind
	}
	if ev.State != nil {
		ev.State.Set(ev.Flow.ID, id, r)
	}
	ev.memo[id] = r
	return r
}

func (ev *Evaluator) eval(ctx context.Context, id string, e *Evidence) Result {
	switch e.Kind {
	case "shell":
		return ev.evalShell(ctx, e)
	case "diff":
		return ev.evalDiff(e)
	case "artifact":
		return ev.evalArtifact(e)
	case "all", "any":
		return ev.evalComposition(ctx, e)
	case "review":
		return ev.evalReview(e)
	case "human":
		if yes, ok := ev.Human[id]; ok {
			if yes {
				return Result{Status: StatusPass, Detail: "confirmed by the user"}
			}
			return Result{Status: StatusFail, Detail: "declined by the user"}
		}
		return Result{Status: StatusSkip, Detail: "human evidence requires a confirmation"}
	default:
		return Result{Status: StatusFail, Detail: "unknown evidence kind " + e.Kind}
	}
}

func (ev *Evaluator) evalShell(ctx context.Context, e *Evidence) Result {
	timeout := e.Timeout
	if timeout == "" {
		timeout = ev.Flow.DefaultTimeout()
	}
	d, err := time.ParseDuration(timeout)
	if err != nil || d <= 0 {
		d = 10 * time.Minute
	}
	cctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	run := ev.Run
	if run == nil {
		run = shellRunner
	}
	out, err := run(cctx, ev.Dir, e.Run)
	detail := tail(strings.TrimSpace(out), 2000)
	if errors.Is(cctx.Err(), context.DeadlineExceeded) {
		return Result{Status: StatusFail, Detail: "timeout after " + d.String() + detailSuffix(detail)}
	}
	if err != nil {
		code := 1
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			code = exit.ExitCode()
		}
		return Result{Status: StatusFail, Detail: fmt.Sprintf("exit %d%s", code, detailSuffix(detail))}
	}
	if detail == "" {
		detail = "exit 0"
	}
	return Result{Status: StatusPass, Detail: detail}
}

func (ev *Evaluator) evalDiff(e *Evidence) Result {
	base := e.Base
	if ev.BaseOverride != "" {
		base = ev.BaseOverride
	}
	files, err := ev.changedFiles(base)
	if err != nil {
		return Result{Status: StatusSkip, Detail: err.Error()}
	}
	if len(e.Changed) > 0 {
		hit := firstMatch(files, e.Changed)
		if hit == "" {
			return Result{Status: StatusFail, Detail: fmt.Sprintf("no changed file matches %s (changed: %s)", strings.Join(e.Changed, ","), summarizeList(files))}
		}
	}
	if len(e.Unchanged) > 0 {
		hit := firstMatch(files, e.Unchanged)
		if hit != "" {
			return Result{Status: StatusFail, Detail: fmt.Sprintf("%s matches a forbidden pattern (%s)", hit, strings.Join(e.Unchanged, ","))}
		}
	}
	return Result{Status: StatusPass, Detail: summarizeList(files)}
}

func (ev *Evaluator) evalArtifact(e *Evidence) Result {
	file := e.File
	verdict := e.Verdict
	if file == "" && verdict != nil {
		file = verdict.File
	}
	path := file
	if !filepath.IsAbs(path) {
		path = filepath.Join(ev.Dir, file)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Result{Status: StatusFail, Detail: "missing artifact " + file}
	}
	if verdict != nil {
		label, ok := schema.ParseVerdict(string(data))
		if !ok {
			return Result{Status: StatusFail, Detail: "no VERDICT line in " + file}
		}
		allowed := false
		for _, l := range verdict.Labels {
			if strings.EqualFold(l, label) {
				allowed = true
				break
			}
		}
		if !allowed {
			return Result{Status: StatusFail, Detail: fmt.Sprintf("verdict %q is not one of %s", label, strings.Join(verdict.Labels, ","))}
		}
		return Result{Status: StatusPass, Detail: fmt.Sprintf("%s verdict=%s", file, label)}
	}
	return Result{Status: StatusPass, Detail: file}
}

// evalReview verifies the artifact a fresh reviewer wrote. The reviewer itself
// runs outside this evaluator (the pi extension or the compiled crew); check
// only parses and validates the verdict, so a CI/hook run can trust it.
func (ev *Evaluator) evalReview(e *Evidence) Result {
	file := e.File
	if e.Verdict != nil && e.Verdict.File != "" {
		file = e.Verdict.File
	}
	if file == "" {
		return Result{Status: StatusFail, Detail: "review evidence has no verdict.file"}
	}
	path := file
	if !filepath.IsAbs(path) {
		path = filepath.Join(ev.Dir, file)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Result{Status: StatusSkip, Detail: "review artifact missing: " + file + " (run the flow reviewer)"}
	}
	label, ok := schema.ParseVerdict(string(data))
	if !ok {
		return Result{Status: StatusFail, Detail: "no VERDICT line in " + file}
	}
	if e.Verdict != nil {
		for _, l := range e.Verdict.Labels {
			if strings.EqualFold(l, label) {
				return Result{Status: StatusPass, Detail: fmt.Sprintf("%s verdict=%s", file, label)}
			}
		}
		return Result{Status: StatusFail, Detail: fmt.Sprintf("verdict %q is not one of %s", label, strings.Join(e.Verdict.Labels, ","))}
	}
	return Result{Status: StatusPass, Detail: fmt.Sprintf("%s verdict=%s", file, label)}
}

func (ev *Evaluator) evalComposition(ctx context.Context, e *Evidence) Result {
	var failures, skips []string
	passes := 0
	for _, ref := range e.Of {
		r := ev.Eval(ctx, ref)
		switch r.Status {
		case StatusPass, StatusWaived:
			passes++
		case StatusSkip:
			skips = append(skips, ref+": "+r.Detail)
		default:
			failures = append(failures, ref+": "+r.Detail)
		}
	}
	if e.Kind == "any" {
		if passes > 0 {
			return Result{Status: StatusPass, Detail: fmt.Sprintf("%d/%d passed", passes, len(e.Of))}
		}
		if len(failures) == 0 && len(skips) > 0 {
			return Result{Status: StatusSkip, Detail: strings.Join(skips, "; ")}
		}
		return Result{Status: StatusFail, Detail: strings.Join(append(failures, skips...), "; ")}
	}
	if len(failures) > 0 {
		return Result{Status: StatusFail, Detail: strings.Join(failures, "; ")}
	}
	if len(skips) > 0 {
		return Result{Status: StatusSkip, Detail: strings.Join(skips, "; ")}
	}
	return Result{Status: StatusPass, Detail: fmt.Sprintf("%d/%d passed", passes, len(e.Of))}
}

// cached returns a stored pass/fail that is still fresh and matches the input
// hash. Only evidence with `within` opts into cross-run reuse.
func (ev *Evaluator) cached(id, hash string) (Result, bool) {
	if ev.State == nil {
		return Result{}, false
	}
	within, ok := parseWithin(ev.Flow.Evidence[id].Within)
	if !ok || within <= 0 {
		return Result{}, false
	}
	r, ok := ev.State.Get(ev.Flow.ID, id)
	if !ok || r.InputHash != hash {
		return Result{}, false
	}
	if ev.now().Sub(r.At) > within {
		return Result{}, false
	}
	r.Cached = true
	r.Source = "cache"
	return r, true
}

// inputHash covers the evidence definition, the repo tree, and any artifact it
// reads. A changed tree invalidates shell/diff evidence.
func (ev *Evaluator) inputHash(id string) string {
	if h, ok := ev.hashes[id]; ok {
		return h
	}
	h := sha256.New()
	e := ev.Flow.Evidence[id]
	fmt.Fprintf(h, "%s\x00%+v\x00", ev.Flow.ID, *e)
	io.WriteString(h, ev.gitOut("rev-parse", "HEAD"))
	io.WriteString(h, "\x00")
	io.WriteString(h, ev.gitOut("status", "--porcelain", "-z", "--", ".", ":(exclude).forseti/flow"))
	io.WriteString(h, "\x00")
	io.WriteString(h, ev.gitOut("diff", "--no-color", "--", ".", ":(exclude).forseti/flow"))
	io.WriteString(h, "\x00")
	// Untracked file contents matter to a test run, but the flow's own state
	// directory must not invalidate every cache entry. Cap the work.
	if untracked, err := ev.git("ls-files", "--others", "--exclude-standard", "--", ".", ":(exclude).forseti/flow"); err == nil {
		budget := 8 << 20
		for i, p := range strings.Split(untracked, "\n") {
			p = strings.TrimSpace(p)
			if p == "" || strings.HasPrefix(p, ".forseti/") {
				continue
			}
			if i >= 200 || budget <= 0 {
				break
			}
			path := filepath.Join(ev.Dir, p)
			st, err := os.Stat(path)
			if err != nil || st.IsDir() {
				continue
			}
			if st.Size() > 1<<20 || int64(budget) < st.Size() {
				fmt.Fprintf(h, "%s:%d\n", p, st.Size())
				continue
			}
			if data, err := os.ReadFile(path); err == nil {
				io.WriteString(h, p)
				h.Write(data)
				budget -= len(data)
			}
		}
	}
	if e.Kind == "artifact" || e.Kind == "review" || (e.Verdict != nil && e.Verdict.File != "") {
		file := e.File
		if e.Verdict != nil {
			file = e.Verdict.File
		}
		if file != "" {
			path := file
			if !filepath.IsAbs(path) {
				path = filepath.Join(ev.Dir, file)
			}
			if st, err := os.Stat(path); err == nil && !st.IsDir() {
				if st.Size() <= 2<<20 {
					data, _ := os.ReadFile(path)
					h.Write(data)
				} else {
					fmt.Fprintf(h, "%d/%d", st.Size(), st.ModTime().UnixNano())
				}
			}
		}
	}
	for _, ref := range e.Of {
		io.WriteString(h, ev.inputHash(ref))
	}
	out := hex.EncodeToString(h.Sum(nil))[:16]
	ev.hashes[id] = out
	return out
}

func (ev *Evaluator) now() time.Time {
	if ev.Now != nil {
		return ev.Now()
	}
	return time.Now()
}

// changedFiles lists working-tree changes against base (default HEAD),
// including untracked files. A missing HEAD (fresh repo) falls back to status.
func (ev *Evaluator) changedFiles(base string) ([]string, error) {
	if base == "" {
		base = "HEAD"
	}
	set := map[string]bool{}
	out, err := ev.git("diff", "--name-only", base, "--", ".", ":(exclude).forseti/flow")
	if err != nil {
		// No HEAD or not a repo: fall back to porcelain below.
		out = ""
	}
	for _, line := range strings.Split(out, "\n") {
		if p := strings.TrimSpace(line); p != "" {
			set[p] = true
		}
	}
	if untracked, err := ev.git("ls-files", "--others", "--exclude-standard", "--", ".", ":(exclude).forseti/flow"); err == nil {
		for _, line := range strings.Split(untracked, "\n") {
			if p := strings.TrimSpace(line); p != "" {
				set[p] = true
			}
		}
	}
	if len(set) == 0 {
		// Include staged/renamed/deleted files even when diff output is empty.
		if status, err := ev.git("status", "--porcelain", "-z", "--", ".", ":(exclude).forseti/flow"); err == nil {
			fields := strings.Split(status, "\x00")
			for i := 0; i < len(fields); i++ {
				entry := fields[i]
				if len(entry) < 4 {
					continue
				}
				st := entry[:2]
				p := entry[3:]
				if st[0] == 'R' || st[0] == 'C' {
					i++ // the following field is the original path
				}
				if p != "" {
					set[p] = true
				}
			}
		}
	}
	files := make([]string, 0, len(set))
	for p := range set {
		files = append(files, p)
	}
	sort.Strings(files)
	if _, err := ev.git("rev-parse", "--git-dir"); err != nil {
		return nil, fmt.Errorf("not a git checkout: %w", err)
	}
	return files, nil
}

func (ev *Evaluator) git(args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = ev.Dir
	out, err := cmd.Output()
	return string(out), err
}

func (ev *Evaluator) gitOut(args ...string) string {
	out, err := ev.git(args...)
	if err != nil {
		return ""
	}
	return out
}

// EffectiveSeverity resolves a gate's severity; an evidence-level severity may
// raise it but never lower it.
func EffectiveSeverity(fl *Flow, gateSeverity string, e *Evidence) string {
	sev := gateSeverity
	if sev == "" {
		sev = fl.EffectiveSeverity()
	}
	if e != nil && e.Severity != "" && severityRank(e.Severity) > severityRank(sev) {
		sev = e.Severity
	}
	return sev
}

func severityRank(sev string) int {
	switch sev {
	case "block":
		return 2
	case "warn":
		return 1
	default:
		return 0
	}
}

func parseWithin(raw string) (time.Duration, bool) {
	if strings.TrimSpace(raw) == "" {
		return 0, false
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, false
	}
	return d, true
}

func firstMatch(files, globs []string) string {
	for _, f := range files {
		for _, g := range globs {
			if matchGlob(g, f) {
				return f
			}
		}
	}
	return ""
}

// matchGlob supports `**` (any depth), `*` (one path segment) and `?`.
func matchGlob(pattern, name string) bool {
	re, err := globToRegexp(pattern)
	if err != nil {
		return false
	}
	return re.MatchString(filepath.ToSlash(name))
}

func globToRegexp(pattern string) (*regexp.Regexp, error) {
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(pattern); i++ {
		switch pattern[i] {
		case '*':
			if i+1 < len(pattern) && pattern[i+1] == '*' {
				i++
				if i+1 < len(pattern) && pattern[i+1] == '/' {
					i++
					b.WriteString("(?:.*/)?")
				} else {
					b.WriteString(".*")
				}
			} else {
				b.WriteString("[^/]*")
			}
		case '?':
			b.WriteString("[^/]")
		case '/':
			b.WriteString("/")
		default:
			b.WriteString(regexp.QuoteMeta(string(pattern[i])))
		}
	}
	b.WriteString("$")
	return regexp.Compile(b.String())
}

func summarizeList(items []string) string {
	const max = 8
	if len(items) == 0 {
		return "(none)"
	}
	out := strings.Join(items[:min(max, len(items))], ",")
	if len(items) > max {
		out += fmt.Sprintf(",… (%d)", len(items))
	}
	return out
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}

func detailSuffix(detail string) string {
	if detail == "" {
		return ""
	}
	return ": " + detail
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
