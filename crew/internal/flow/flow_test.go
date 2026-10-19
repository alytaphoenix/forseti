package flow

import (
	"strings"
	"testing"
)

func mustLoad(t *testing.T, src string) *File {
	t.Helper()
	f, err := Load([]byte(src))
	if err != nil {
		t.Fatalf("want valid, got %v", err)
	}
	return f
}

func wantErr(t *testing.T, src, substr string) {
	t.Helper()
	_, err := Load([]byte(src))
	if err == nil {
		t.Fatalf("want error containing %q, got nil", substr)
	}
	if !strings.Contains(err.Error(), substr) {
		t.Fatalf("want error containing %q, got %v", substr, err)
	}
}

func TestValidRepo(t *testing.T) {
	f := mustLoad(t, `
version: 1
name: t
evidence:
  tests: {kind: shell, run: "go test ./..."}
  docs:
    kind: diff
    changed: ["docs/**"]
on:
  post_turn: {require: [docs]}
  pre_commit: {require: [tests], severity: block}
`)
	fl := f.Flows[0]
	if fl.EffectiveMode() != "repo" {
		t.Fatalf("mode = %q", fl.EffectiveMode())
	}
	if fl.MaxContinues() != 2 || fl.MaxCycles() != 3 {
		t.Fatalf("defaults = %d/%d", fl.MaxContinues(), fl.MaxCycles())
	}
	if fl.WaiverLedger() != ".forseti/waivers.jsonl" || fl.WaiverTTL() != "24h" {
		t.Fatalf("waiver defaults = %q/%q", fl.WaiverLedger(), fl.WaiverTTL())
	}
}

func TestValidHybrid(t *testing.T) {
	f := mustLoad(t, `
version: 1
name: t
mode: hybrid
defaults: {severity: block, max_cycles: 2}
evidence:
  plan: {kind: artifact, file: PLAN.md}
  tests: {kind: shell, run: "go test ./..."}
  review_lgtm:
    kind: review
    role: reviewer
    criteria: "no blocking defects"
    verdict: {file: out/review.md, labels: [lgtm, lbtm]}
phases:
  - id: plan
    prompt: "write the plan"
    produce: [plan]
    gate: {require: [plan]}
  - id: implement
    from: [plan]
    prompt: "implement"
    gate: {require: [tests]}
  - id: review
    actor: reviewer
    from: [implement]
    criteria: "correctness"
    verdict: {lgtm: commit, lbtm: implement}
    max_visits: 2
  - id: commit
    actor: shell
    from: [review]
    run: "git commit"
on:
  pre_commit: {require: [tests]}
`)
	fl := f.Flows[0]
	if fl.EffectiveMode() != "hybrid" {
		t.Fatalf("mode = %q", fl.EffectiveMode())
	}
	if len(fl.Phases) != 4 {
		t.Fatalf("phases = %d", len(fl.Phases))
	}
	if fl.Phases[2].Actor != "reviewer" {
		t.Fatalf("actor = %q", fl.Phases[2].Actor)
	}
}

func TestNamedFlows(t *testing.T) {
	f := mustLoad(t, `
version: 1
flows:
  - id: edit
    evidence:
      tests: {kind: shell, run: "go test ./..."}
    on:
      pre_commit: {require: [tests]}
  - id: release
    trigger: pre-push
    evidence:
      tests: {kind: shell, run: "go test ./..."}
    on:
      pre_push: {require: [tests], severity: block}
`)
	if len(f.Flows) != 2 {
		t.Fatalf("flows = %d", len(f.Flows))
	}
	if f.FlowByID("release") == nil || f.FlowByID("ghost") != nil {
		t.Fatalf("FlowByID broken")
	}
}

func TestExplain(t *testing.T) {
	f := mustLoad(t, `
version: 1
name: t
mode: hybrid
evidence:
  plan: {kind: artifact, file: PLAN.md}
  tests: {kind: shell, run: "go test ./..."}
phases:
  - id: plan
    prompt: "write the plan"
    gate: {require: [plan]}
  - id: implement
    from: [plan]
    prompt: "implement"
    gate: {require: [tests], severity: block}
on:
  pre_commit: {require: [tests]}
`)
	out := Explain(f.Flows[0])
	for _, want := range []string{
		"flow t (hybrid)",
		"evidence:",
		"tests shell run=\"go test ./...\"",
		"pre_commit",
		"phases:",
		"plan",
		"ENTRY",
		"implement",
		"← plan",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("explain missing %q:\n%s", want, out)
		}
	}
}

func TestStrictUnknownKey(t *testing.T) {
	wantErr(t, "version: 1\nname: t\nnope: 1\non: {pre_commit: {require: [x]}}\n", "nope")
}

func TestVersion(t *testing.T) {
	wantErr(t, "version: 2\nname: t\non: {pre_commit: {require: [x]}}\n", "version must be 1")
}

func TestUnknownStage(t *testing.T) {
	wantErr(t, `
version: 1
name: t
evidence: {x: {kind: shell, run: "true"}}
on: {pre_merge: {require: [x]}}
`, "unknown stage")
}

func TestUnknownEvidenceRef(t *testing.T) {
	wantErr(t, `
version: 1
name: t
evidence: {x: {kind: shell, run: "true"}}
on: {pre_commit: {require: [ghost]}}
`, "unknown evidence")
}

func TestReviewNeedsCriteria(t *testing.T) {
	wantErr(t, `
version: 1
name: t
evidence:
  r:
    kind: review
    role: reviewer
    verdict: {file: out.md, labels: [lgtm]}
on: {pre_commit: {require: [r]}}
`, "requires criteria")
}

func TestReviewNeedsVerdictLabel(t *testing.T) {
	wantErr(t, `
version: 1
name: t
evidence:
  r:
    kind: review
    role: reviewer
    criteria: "x"
    verdict: {file: out.md}
on: {pre_commit: {require: [r]}}
`, "at least one verdict label")
}

func TestEvidenceCycle(t *testing.T) {
	wantErr(t, `
version: 1
name: t
evidence:
  a: {kind: all, of: [b]}
  b: {kind: all, of: [a]}
on: {pre_commit: {require: [a]}}
`, "evidence cycle")
}

func TestRepoCannotHavePhases(t *testing.T) {
	wantErr(t, `
version: 1
name: t
mode: repo
evidence: {x: {kind: shell, run: "true"}}
phases:
  - id: p
    prompt: "x"
    gate: {require: [x]}
`, "mode repo cannot declare phases")
}

func TestHybridNeedsPhases(t *testing.T) {
	wantErr(t, `
version: 1
name: t
mode: hybrid
evidence: {x: {kind: shell, run: "true"}}
on: {pre_commit: {require: [x]}}
`, "requires phases")
}

func TestFromMustBeEarlier(t *testing.T) {
	wantErr(t, `
version: 1
name: t
mode: agentic
evidence: {x: {kind: shell, run: "true"}}
phases:
  - id: a
    from: [b]
    prompt: "a"
    gate: {require: [x]}
  - id: b
    prompt: "b"
    gate: {require: [x]}
`, "must reference an earlier phase")
}

func TestDuplicatePhase(t *testing.T) {
	wantErr(t, `
version: 1
name: t
mode: agentic
evidence: {x: {kind: shell, run: "true"}}
phases:
  - id: a
    prompt: "a"
    gate: {require: [x]}
  - id: a
    prompt: "b"
    gate: {require: [x]}
`, "duplicate phase id")
}

func TestVerdictBackEdgeNeedsMaxVisits(t *testing.T) {
	wantErr(t, `
version: 1
name: t
mode: agentic
evidence: {x: {kind: shell, run: "true"}}
phases:
  - id: a
    prompt: "a"
    gate: {require: [x]}
  - id: r
    actor: reviewer
    from: [a]
    criteria: "x"
    verdict: {lbtm: a}
`, "set max_visits")
}

func TestShellPhaseNeedsRun(t *testing.T) {
	wantErr(t, `
version: 1
name: t
mode: agentic
evidence: {x: {kind: shell, run: "true"}}
phases:
  - id: a
    actor: shell
    gate: {require: [x]}
`, "shell requires run")
}

func TestFlatAndFlowsRejected(t *testing.T) {
	wantErr(t, `
version: 1
name: t
evidence: {x: {kind: shell, run: "true"}}
flows:
  - id: a
    on: {pre_commit: {require: [x]}}
`, "either top-level flow fields or flows:")
}

func TestUnknownEvidenceKind(t *testing.T) {
	wantErr(t, `
version: 1
name: t
evidence: {x: {kind: telepathy}}
on: {pre_commit: {require: [x]}}
`, "kind \"telepathy\"")
}

func TestBadCommitPattern(t *testing.T) {
	wantErr(t, `
version: 1
name: t
evidence: {x: {kind: shell, run: "true"}}
on:
  commit_msg: {pattern: "["}
`, "bad pattern")
}

func TestDenyRegexValidation(t *testing.T) {
	mustLoad(t, `
version: 1
name: t
evidence: {x: {kind: shell, run: "true"}}
on:
  pre_tool:
    deny:
      - {match: "bash-re:^rm\\s+-rf", reason: "no recursive deletes"}
  pre_commit: {require: [x]}
`)
	wantErr(t, `
version: 1
name: t
evidence: {x: {kind: shell, run: "true"}}
on:
  pre_tool:
    deny:
      - {match: "bash-re:[", reason: "bad"}
  pre_commit: {require: [x]}
`, "bad regex")
}

func TestDuplicateFlowID(t *testing.T) {
	wantErr(t, `
version: 1
flows:
  - id: a
    evidence: {x: {kind: shell, run: "true"}}
    on: {pre_commit: {require: [x]}}
  - id: a
    evidence: {x: {kind: shell, run: "true"}}
    on: {pre_commit: {require: [x]}}
`, "duplicate flow id")
}
