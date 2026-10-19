// Compile turns an agentic/hybrid flow into an ordinary crew graph. Patterns
// are not involved: the flow's phases become agents/edges directly, so the
// crew scheduler, verdict parsing, checks and run log are reused unchanged.
package flow

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"forseti/crew/internal/schema"
)

var flowTemplateVar = regexp.MustCompile(`\{\{\s*([a-z][a-z0-9_-]*)\s*\}\}`)

// Compile renders the flow as a *schema.Crew and validates it.
//
// Mapping:
//   - agent/reviewer phase → crew node (role, model/route, prompt)
//   - `from:`              → an idle edge (a verdict edge wins on the same pair)
//   - reviewer `verdict:`  → `when: verdict:<label>` edges; a back edge is
//     marked loop and bounded by max_visits
//   - shell evidence in a phase gate → a crew check after that node
//
// Shell phases are deterministic post-crew steps (they must be terminal), and
// human phases are in-session only. Both decisions are enforced here.
func Compile(fl *Flow) (*schema.Crew, error) {
	if fl.EffectiveMode() == "repo" {
		return nil, fmt.Errorf("flow %q is repo mode; there is no agent graph to compile", fl.ID)
	}
	if len(fl.Phases) == 0 {
		return nil, fmt.Errorf("flow %q has no phases", fl.ID)
	}
	phaseIDs := map[string]bool{}
	for _, p := range fl.Phases {
		phaseIDs[p.ID] = true
	}
	for i := range fl.Phases {
		p := &fl.Phases[i]
		switch p.Actor {
		case "shell":
			for _, q := range fl.Phases {
				for _, dep := range q.From {
					if dep == p.ID {
						return nil, fmt.Errorf("phase %q: shell phase %q is terminal-only (phase %q depends on it)", q.ID, p.ID, q.ID)
					}
				}
			}
		case "human":
			return nil, fmt.Errorf("phase %q: human phases are in-session only and cannot compile to a crew", p.ID)
		}
		if len(p.Verdict) > 0 && p.Actor != "reviewer" {
			return nil, fmt.Errorf("phase %q: verdict edges require actor reviewer", p.ID)
		}
	}

	// Roles name a tier; the flow has no tier map, so pin both tiers to the
	// flow's reviewer model (or the environment default). An explicit phase
	// model/route still wins.
	model := defaultCompileModel(fl)
	crew := &schema.Crew{
		Name:   fl.ID,
		Models: map[string]string{"efficient": model, "capable": model},
	}
	compiled := map[string]bool{}
	for i := range fl.Phases {
		p := &fl.Phases[i]
		if p.Actor == "agent" || p.Actor == "reviewer" {
			compiled[p.ID] = true
		}
	}
	verdictPairs := map[string]bool{}
	for i := range fl.Phases {
		p := &fl.Phases[i]
		for _, target := range p.Verdict {
			if compiled[target] {
				verdictPairs[p.ID+">"+target] = true
			}
		}
	}

	pack := schema.NewRolePack(nil)
	for i := range fl.Phases {
		p := &fl.Phases[i]
		if p.Actor != "agent" && p.Actor != "reviewer" {
			continue
		}
		a := schema.Agent{Name: p.ID, Kind: "pi", Model: p.Model, Route: p.Route}
		if p.Role != "" {
			a.Role = p.Role
		} else if p.Actor == "reviewer" {
			a.Role = "reviewer"
		}
		if a.Role != "" {
			if _, ok := pack.Get(a.Role); !ok {
				return nil, fmt.Errorf("phase %q: role %q is not a crew built-in (planner, researcher, engineer, reviewer, security_reviewer, adversarial_reviewer, judge, verifier)", p.ID, a.Role)
			}
		}
		if a.Model == "" && a.Route == "" {
			a.Model = model
		}
		if len(p.Produce) == 1 && a.Role != "" {
			a.Writes = p.Produce[0]
		}
		if p.Actor == "reviewer" {
			a.Prompt = reviewerPrompt(fl, p)
		} else {
			a.Prompt = rewritePrompt(p.Prompt, phaseIDs)
		}
		if strings.TrimSpace(a.Prompt) == "" {
			return nil, fmt.Errorf("phase %q: empty prompt after compilation", p.ID)
		}
		crew.Agents = append(crew.Agents, a)
	}
	if len(crew.Agents) == 0 {
		return nil, fmt.Errorf("flow %q has no agent or reviewer phases to compile", fl.ID)
	}

	// Verdict edges first, so a plain from-edge on the same pair is dropped.
	for i := range fl.Phases {
		p := &fl.Phases[i]
		for _, label := range sortedLabels(p.Verdict) {
			target := p.Verdict[label]
			if !compiled[target] {
				continue // a terminal shell phase is executed after the crew
			}
			e := schema.Edge{From: p.ID, To: target, When: "verdict:" + label, MaxVisits: p.MaxVisits}
			if fl.phaseIndex(target) <= i {
				e.Loop = true
			}
			crew.Edges = append(crew.Edges, e)
		}
	}
	for i := range fl.Phases {
		p := &fl.Phases[i]
		if !compiled[p.ID] {
			continue
		}
		for _, from := range p.From {
			if !compiled[from] || verdictPairs[from+">"+p.ID] {
				continue
			}
			crew.Edges = append(crew.Edges, schema.Edge{From: from, To: p.ID, When: "idle"})
		}
	}

	for i := range fl.Phases {
		p := &fl.Phases[i]
		if p.Gate == nil || !compiled[p.ID] {
			continue
		}
		for _, id := range p.Gate.Require {
			e := fl.Evidence[id]
			if e != nil && e.Kind == "shell" {
				crew.Checks = append(crew.Checks, schema.Check{
					Name:  "flow-" + p.ID + "-" + id,
					After: p.ID,
					Run:   e.Run,
				})
			}
		}
	}

	if err := crew.Validate(); err != nil {
		return nil, fmt.Errorf("compiled crew is invalid: %w", err)
	}
	return crew, nil
}

// Marshal renders the compiled crew with a generated-file header. Output is
// deterministic (verdict labels are sorted), so it can be golden-tested.
func Marshal(fl *Flow) ([]byte, error) {
	crew, err := Compile(fl)
	if err != nil {
		return nil, err
	}
	data, err := yaml.Marshal(crew)
	if err != nil {
		return nil, err
	}
	header := "# GENERATED by forseti-flow from .forseti/flow.yaml — edit the flow, not this file.\n"
	return append([]byte(header), data...), nil
}

// ShellPhases returns the terminal deterministic steps to run after the crew.
func ShellPhases(fl *Flow) []Phase {
	var out []Phase
	for _, p := range fl.Phases {
		if p.Actor == "shell" {
			out = append(out, p)
		}
	}
	return out
}

func reviewerPrompt(fl *Flow, p *Phase) string {
	labels := sortedLabels(p.Verdict)
	return fmt.Sprintf(
		"Review the artifact against this acceptance standard:\n%s\n\n"+
			"Write your full review to %s, ending with exactly one line `VERDICT: <label>` "+
			"where <label> is one of: %s. Be concrete: quote the artifact and name the failure mode.",
		p.Criteria, reviewArtifactPath(fl, p), strings.Join(labels, ", "),
	)
}

func defaultCompileModel(fl *Flow) string {
	ids := make([]string, 0, len(fl.Evidence))
	for id := range fl.Evidence {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if e := fl.Evidence[id]; e != nil && e.Kind == "review" && e.Model != "" {
			return e.Model
		}
	}
	if m := os.Getenv("FORSETI_FLOW_MODEL"); m != "" {
		return m
	}
	return "opencode-go/beam-free"
}

func reviewArtifactPath(fl *Flow, p *Phase) string {
	ids := make([]string, 0, len(fl.Evidence))
	for id := range fl.Evidence {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		e := fl.Evidence[id]
		if e != nil && e.Kind == "review" && e.Verdict != nil && e.Verdict.File != "" {
			return e.Verdict.File
		}
	}
	return ".forseti/out/review-" + p.ID + ".md"
}

// rewritePrompt converts a bare flow variable (`{{ plan }}`) into the crew
// runner's node lookup (`{{ node "plan" }}`), so a flow prompt renders in the
// compiled graph. Unknown names are left untouched (and fail loudly at run).
func rewritePrompt(prompt string, phaseIDs map[string]bool) string {
	return flowTemplateVar.ReplaceAllStringFunc(prompt, func(m string) string {
		name := flowTemplateVar.FindStringSubmatch(m)[1]
		if phaseIDs[name] {
			return fmt.Sprintf(`{{ node %q }}`, name)
		}
		return m
	})
}

func sortedLabels(m map[string]string) []string {
	labels := make([]string, 0, len(m))
	for l := range m {
		labels = append(labels, l)
	}
	sort.Strings(labels)
	return labels
}
