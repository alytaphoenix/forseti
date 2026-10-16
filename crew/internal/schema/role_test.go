package schema

import (
	"strings"
	"testing"
)

func TestRolePackBuiltinsExist(t *testing.T) {
	p := NewRolePack(nil)
	for _, want := range []string{"planner", "researcher", "engineer", "reviewer", "security_reviewer", "adversarial_reviewer", "judge", "verifier"} {
		if _, ok := p.Get(want); !ok {
			t.Errorf("built-in role %q missing (have: %s)", want, p.String())
		}
	}
}

func TestRolePrefixMetaGPTShape(t *testing.T) {
	r := Role{Name: "reviewer", Profile: "Reviewer", Goal: "find defects", Constraints: "be concrete"}
	got := r.Prefix("rev-a")
	want := "You are a Reviewer, named rev-a, your goal is find defects. the constraint is be concrete. "
	if got != want {
		t.Fatalf("prefix = %q, want %q", got, want)
	}
	// no constraints → no clause
	r.Constraints = ""
	if got := r.Prefix("x"); strings.Contains(got, "constraint") {
		t.Fatalf("empty constraints must not render the clause: %q", got)
	}
}

func TestRoleTierResolvesThroughModels(t *testing.T) {
	// reviewer is TierCapable; the crew maps capable → a model, so an explicit
	// model is unnecessary.
	c := &Crew{
		Name:   "tier-crew",
		Models: map[string]string{TierCapable: "valhalla/valhalla-flash-next"},
		Agents: []Agent{
			{Name: "a", Role: "reviewer", Prompt: "review this"},
		},
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if got := c.Agents[0].Model; got != "valhalla/valhalla-flash-next" {
		t.Fatalf("tier did not resolve: model = %q", got)
	}
	// explicit model wins over tier
	c2 := &Crew{
		Name:   "tier2",
		Models: map[string]string{TierCapable: "a/b"},
		Agents: []Agent{{Name: "a", Role: "reviewer", Prompt: "p", Model: "explicit/model"}},
	}
	if err := c2.Validate(); err != nil {
		t.Fatal(err)
	}
	if c2.Agents[0].Model != "explicit/model" {
		t.Fatalf("explicit model was overwritten: %q", c2.Agents[0].Model)
	}
}

func TestRoleTierWithoutModelsIsAnError(t *testing.T) {
	c := &Crew{Name: "no-models", Agents: []Agent{{Name: "a", Role: "reviewer", Prompt: "p"}}}
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "needs tier") {
		t.Fatalf("want a tier/models error, got %v", err)
	}
}

func TestRoleUnknownIsRejected(t *testing.T) {
	c := &Crew{Name: "r", Agents: []Agent{{Name: "a", Role: "wizard", Prompt: "p", Model: "m/x"}}}
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "unknown role") {
		t.Fatalf("want unknown-role error, got %v", err)
	}
}

func TestRoleInlineOverrideWins(t *testing.T) {
	c := &Crew{
		Name: "inline",
		Roles: []Role{{
			Name: "reviewer", Profile: "Strict Reviewer", Goal: "reject anything unproven",
			Instruction: "Review harshly.",
		}},
		Agents: []Agent{{Name: "a", Role: "reviewer"}}, // no prompt: role instruction must satisfy
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	p := NewRolePack(c.Roles)
	got := p.RolePrompt(&c.Agents[0], "")
	if !strings.Contains(got, "You are a Strict Reviewer, named a") {
		t.Fatalf("inline override prefix missing: %q", got)
	}
	if !strings.Contains(got, "Review harshly") {
		t.Fatalf("inline override body must replace the builtin instruction: %q", got)
	}
	if strings.Contains(got, "Review the artifact") {
		t.Fatalf("builtin instruction leaked through the override: %q", got)
	}
}

func TestRoleWatchWritesRequireRole(t *testing.T) {
	c := &Crew{Name: "watchy", Agents: []Agent{{Name: "a", Prompt: "p", Watch: []string{"plan"}}}}
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "need a role") {
		t.Fatalf("want a watch/writes-needs-a-role error, got %v", err)
	}
}

func TestRoleLeavesRolelessCrewUntouched(t *testing.T) {
	c := &Crew{Name: "plain", Agents: []Agent{{Name: "a", Prompt: "do the thing"}}}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	p := NewRolePack(nil)
	if got := p.RolePrompt(&c.Agents[0], "hello"); got != "hello" {
		t.Fatalf("role-less prompt must be unchanged, got %q", got)
	}
}

func TestRolePromptUsesInstructionWhenNodeHasNoPrompt(t *testing.T) {
	p := NewRolePack(nil)
	a := &Agent{Name: "rev", Role: "reviewer"}
	got := p.RolePrompt(a, "")
	if !strings.Contains(got, "You are a Reviewer, named rev") {
		t.Fatalf("missing prefix: %q", got)
	}
	if !strings.Contains(got, "Review the artifact") {
		t.Fatalf("missing instruction: %q", got)
	}
}
