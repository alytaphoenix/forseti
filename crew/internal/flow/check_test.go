package flow

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestCheckStageBlocked(t *testing.T) {
	fl := loadFlow(t, `
version: 1
name: t
evidence:
  proof: {kind: shell, run: "true"}
on: {pre_commit: {require: [proof], severity: block}}
`)
	ev := NewEvaluator(t.TempDir(), fl, nil)
	ev.Run = func(ctx context.Context, dir, script string) (string, error) { return "nope", errors.New("x") }
	res, err := CheckStage(context.Background(), ev, "pre_commit", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.OK || !res.Blocked || len(res.Messages) != 1 {
		t.Fatalf("want blocked, got %+v", res)
	}
}

func TestCheckStageWaived(t *testing.T) {
	fl := loadFlow(t, `
version: 1
name: t
evidence:
  proof: {kind: shell, run: "true"}
on: {pre_commit: {require: [proof], severity: block}}
`)
	now := time.Now()
	ev := NewEvaluator(t.TempDir(), fl, nil)
	ev.Now = func() time.Time { return now }
	ev.Run = func(ctx context.Context, dir, script string) (string, error) { return "nope", errors.New("x") }
	res, err := CheckStage(context.Background(), ev, "pre_commit", []Waiver{{
		At: now, Flow: "t", Stage: "pre_commit", Evidence: "proof", Reason: "known issue",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK || res.Blocked || len(res.Evidence) != 1 || res.Evidence[0].Status != StatusWaived {
		t.Fatalf("want waived ok, got %+v", res)
	}
}

func TestCheckStageWarn(t *testing.T) {
	fl := loadFlow(t, `
version: 1
name: t
evidence:
  proof: {kind: shell, run: "true"}
on: {post_turn: {require: [proof], severity: warn}}
`)
	ev := NewEvaluator(t.TempDir(), fl, nil)
	ev.Run = func(ctx context.Context, dir, script string) (string, error) { return "nope", errors.New("x") }
	res, err := CheckStage(context.Background(), ev, "post_turn", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK || res.Blocked || len(res.Warnings) != 1 {
		t.Fatalf("want warn ok, got %+v", res)
	}
}

func TestCheckStageHyphen(t *testing.T) {
	fl := loadFlow(t, `
version: 1
name: t
evidence: {proof: {kind: shell, run: "true"}}
on: {pre_commit: {require: [proof]}}
`)
	ev := NewEvaluator(t.TempDir(), fl, nil)
	ev.Run = func(ctx context.Context, dir, script string) (string, error) { return "", nil }
	if _, err := CheckStage(context.Background(), ev, "pre-commit", nil); err != nil {
		t.Fatalf("hyphen stage: %v", err)
	}
}

func TestCheckCommitMessage(t *testing.T) {
	fl := loadFlow(t, `
version: 1
name: t
evidence: {proof: {kind: shell, run: "true"}}
on:
  pre_commit: {require: [proof]}
  commit_msg: {pattern: '^feat: .+'}
`)
	if r, err := CheckCommitMessage(fl, "feat: add flow"); err != nil || r.Status != StatusPass {
		t.Fatalf("pass: %+v %v", r, err)
	}
	if r, err := CheckCommitMessage(fl, "nope"); err != nil || r.Status != StatusFail || !strings.Contains(r.Detail, "does not match") {
		t.Fatalf("fail: %+v %v", r, err)
	}
}
