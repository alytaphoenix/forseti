// Human-readable rendering of a validated flow. Deterministic: evidence ids are
// sorted, lifecycle stages follow StageOrder, phases follow declaration order.
package flow

import (
	"fmt"
	"strings"
)

// ExplainFile renders every flow in a document.
func ExplainFile(f *File) string {
	var b strings.Builder
	for i, fl := range f.Flows {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString(Explain(fl))
	}
	return b.String()
}

// Explain renders one resolved flow.
func Explain(fl *Flow) string {
	var b strings.Builder
	mode := fl.EffectiveMode()
	fmt.Fprintf(&b, "flow %s (%s", fl.ID, mode)
	if fl.Trigger != "" {
		fmt.Fprintf(&b, ", trigger=%s", fl.Trigger)
	}
	b.WriteString(")\n")
	fmt.Fprintf(&b, "  defaults severity=%s max_continues=%d max_cycles=%d timeout=%s\n",
		fl.EffectiveSeverity(), fl.MaxContinues(), fl.MaxCycles(), fl.DefaultTimeout())

	if len(fl.Evidence) > 0 {
		b.WriteString("  evidence:\n")
		for _, id := range fl.evidenceIDs() {
			fmt.Fprintf(&b, "    %s %s\n", id, evidenceSummary(fl.Evidence[id]))
		}
	}

	if len(fl.On) > 0 {
		b.WriteString("  on:\n")
		for _, stage := range StageOrder {
			r, ok := fl.On[stage]
			if !ok {
				continue
			}
			fmt.Fprintf(&b, "    %-11s %s\n", stage, ruleSummary(fl, r))
		}
	}

	if len(fl.Phases) > 0 {
		b.WriteString("  phases:\n")
		for i := range fl.Phases {
			p := &fl.Phases[i]
			meta := []string{p.Actor}
			if p.Role != "" {
				meta = append(meta, "role="+p.Role)
			}
			switch {
			case p.Route != "":
				meta = append(meta, "route="+p.Route)
			case p.Model != "":
				meta = append(meta, "model="+p.Model)
			}
			line := fmt.Sprintf("    %-12s (%s)", p.ID, strings.Join(meta, ", "))
			if len(p.From) > 0 {
				line += " ← " + strings.Join(p.From, ",")
			} else {
				line += " ENTRY"
			}
			b.WriteString(line + "\n")
			if p.Prompt != "" {
				fmt.Fprintf(&b, "      prompt: %q\n", p.Prompt)
			}
			if p.Run != "" {
				fmt.Fprintf(&b, "      run: %s\n", p.Run)
			}
			if p.Criteria != "" {
				fmt.Fprintf(&b, "      criteria: %q\n", p.Criteria)
			}
			if len(p.Produce) > 0 {
				fmt.Fprintf(&b, "      produce: %s\n", strings.Join(p.Produce, ","))
			}
			if len(p.Inputs) > 0 {
				fmt.Fprintf(&b, "      inputs: %s\n", strings.Join(p.Inputs, ","))
			}
			if p.Gate != nil {
				sev := p.Gate.Severity
				if sev == "" {
					sev = fl.EffectiveSeverity()
				}
				fmt.Fprintf(&b, "      gate [%s] require=%s\n", sev, strings.Join(p.Gate.Require, ","))
			}
			if len(p.Verdict) > 0 {
				labels := make([]string, 0, len(p.Verdict))
				for l := range p.Verdict {
					labels = append(labels, l)
				}
				sortStrings(labels)
				parts := make([]string, 0, len(labels))
				for _, l := range labels {
					part := l + " → " + p.Verdict[l]
					// max_visits bounds the loop edge only, not forward branches.
					if p.MaxVisits > 0 && fl.phaseIndex(p.Verdict[l]) <= i {
						part += fmt.Sprintf(" (max_visits=%d)", p.MaxVisits)
					}
					parts = append(parts, part)
				}
				fmt.Fprintf(&b, "      verdict: %s\n", strings.Join(parts, ", "))
			}
		}
	}

	if fl.Waivers != nil && !fl.WaiversEnabled() {
		b.WriteString("  waivers: disabled\n")
	} else {
		fmt.Fprintf(&b, "  waivers: ledger=%s ttl=%s", fl.WaiverLedger(), fl.WaiverTTL())
		if fl.Waivers != nil && len(fl.Waivers.Never) > 0 {
			fmt.Fprintf(&b, " never=%s", strings.Join(fl.Waivers.Never, ","))
		}
		b.WriteString("\n")
	}
	return b.String()
}

func evidenceSummary(e *Evidence) string {
	if e == nil {
		return "(empty)"
	}
	bits := []string{e.Kind}
	switch e.Kind {
	case "shell":
		bits = append(bits, "run="+quote(e.Run))
	case "diff":
		if e.Base != "" {
			bits = append(bits, "base="+e.Base)
		}
		if len(e.Changed) > 0 {
			bits = append(bits, "changed="+strings.Join(e.Changed, ","))
		}
		if len(e.Unchanged) > 0 {
			bits = append(bits, "unchanged="+strings.Join(e.Unchanged, ","))
		}
	case "artifact":
		if e.File != "" {
			bits = append(bits, "file="+e.File)
		}
		if e.Verdict != nil && e.Verdict.File != "" {
			bits = append(bits, "verdict="+e.Verdict.File)
		}
	case "review":
		bits = append(bits, "role="+e.Role, "criteria="+quote(e.Criteria))
		if e.Verdict != nil {
			bits = append(bits, "verdict="+e.Verdict.File, "labels="+strings.Join(e.Verdict.Labels, ","))
		}
		if e.Model != "" {
			bits = append(bits, "model="+e.Model)
		}
	case "human":
		bits = append(bits, "prompt="+quote(e.Prompt))
	case "all", "any":
		bits = append(bits, "of="+strings.Join(e.Of, ","))
	}
	if e.Timeout != "" {
		bits = append(bits, "timeout="+e.Timeout)
	}
	if e.Within != "" {
		bits = append(bits, "within="+e.Within)
	}
	if e.Severity != "" {
		bits = append(bits, "severity="+e.Severity)
	}
	return strings.Join(bits, " ")
}

func ruleSummary(fl *Flow, r *Rule) string {
	sev := r.Severity
	if sev == "" {
		sev = fl.EffectiveSeverity()
	}
	bits := []string{"[" + sev + "]"}
	if len(r.Require) > 0 {
		bits = append(bits, "require="+strings.Join(r.Require, ","))
	}
	if r.Pattern != "" {
		bits = append(bits, "pattern="+quote(r.Pattern))
	}
	for _, d := range r.Deny {
		part := "deny=" + quote(d.Match)
		if d.Reason != "" {
			part += " (" + d.Reason + ")"
		}
		bits = append(bits, part)
	}
	return strings.Join(bits, " ")
}

func quote(s string) string {
	if s == "" {
		return `""`
	}
	if strings.ContainsAny(s, " \t\"") {
		return fmt.Sprintf("%q", s)
	}
	return s
}

func sortStrings(ss []string) {
	for i := 1; i < len(ss); i++ {
		for j := i; j > 0 && ss[j] < ss[j-1]; j-- {
			ss[j], ss[j-1] = ss[j-1], ss[j]
		}
	}
}
