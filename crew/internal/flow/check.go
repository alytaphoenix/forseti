// Stage evaluation: apply a lifecycle rule's evidence, waivers and severities.
package flow

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// StageResult is the outcome of one lifecycle stage.
type StageResult struct {
	Flow     string   `json:"flow"`
	Stage    string   `json:"stage"`
	OK       bool     `json:"ok"`
	Blocked  bool     `json:"blocked"`
	Messages []string `json:"messages,omitempty"`
	Warnings []string `json:"warnings,omitempty"`
	Evidence []Result `json:"evidence,omitempty"`
}

// NormalizeStage accepts both hook spellings (pre-commit) and YAML spellings
// (pre_commit).
func NormalizeStage(stage string) string {
	return strings.ReplaceAll(strings.TrimSpace(stage), "-", "_")
}

// CheckStage evaluates a rule's evidence. Waivers are applied per evidence id.
func CheckStage(ctx context.Context, ev *Evaluator, stage string, waivers []Waiver) (StageResult, error) {
	stage = NormalizeStage(stage)
	fl := ev.Flow
	rule, ok := fl.On[stage]
	if !ok {
		return StageResult{}, fmt.Errorf("on.%s is not declared by flow %q", stage, fl.ID)
	}
	if stage == "pre_tool" {
		return StageResult{}, fmt.Errorf("pre_tool is a tool gate, not a check stage")
	}
	if stage == "commit_msg" {
		return StageResult{}, fmt.Errorf("commit_msg is checked with CheckCommitMessage")
	}
	out := StageResult{Flow: fl.ID, Stage: stage, OK: true}
	for _, id := range rule.Require {
		res := ev.Eval(ctx, id)
		if w := ActiveWaiver(waivers, fl.ID, stage, id, ev.now()); w != nil {
			res.Status = StatusWaived
			res.Detail = "waived: " + w.Reason
		}
		sev := EffectiveSeverity(fl, rule.Severity, fl.Evidence[id])
		switch res.Status {
		case StatusPass, StatusWaived:
		case StatusFail, StatusSkip:
			switch sev {
			case "block":
				out.OK = false
				out.Blocked = true
				out.Messages = append(out.Messages, fmt.Sprintf("%s %s: %s", id, res.Status, res.Detail))
			case "warn":
				out.Warnings = append(out.Warnings, fmt.Sprintf("%s %s: %s", id, res.Status, res.Detail))
			}
		}
		out.Evidence = append(out.Evidence, res)
	}
	return out, nil
}

// CheckCommitMessage applies on.commit_msg.pattern to a commit message.
func CheckCommitMessage(fl *Flow, message string) (Result, error) {
	rule, ok := fl.On["commit_msg"]
	if !ok {
		return Result{}, fmt.Errorf("on.commit_msg is not declared by flow %q", fl.ID)
	}
	re, err := regexp.Compile(rule.Pattern)
	if err != nil {
		return Result{}, fmt.Errorf("on.commit_msg: bad pattern: %w", err)
	}
	at := time.Now()
	if re.MatchString(strings.TrimSpace(message)) {
		return Result{ID: "commit_msg", Status: StatusPass, At: at, Detail: "matches " + rule.Pattern}, nil
	}
	return Result{ID: "commit_msg", Status: StatusFail, At: at, Detail: "does not match " + rule.Pattern}, nil
}
