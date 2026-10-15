package schema

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"
)

// Phase 18C: forseti derives the typed keys it can from (efficient, capable)
// and passes `options` through verbatim, so every switchyard 0.3.0 route type
// and key is reachable without re-encoding the router's schema.

func crewWithRoute(route string) string {
	return "name: t\nroutes:\n  - " + route + "\nagents:\n  - {name: a, route: r1, prompt: x}\n"
}

// routeKeysLine renders the emitted route keys as a deterministic string
// (sorted keys, JSON-encoded values) for exact comparison.
func routeKeysLine(t *testing.T, route string) string {
	t.Helper()
	c, err := Load([]byte(crewWithRoute(route)))
	if err != nil {
		t.Fatalf("want valid route, got %v", err)
	}
	keys, err := c.Route("r1").RouteKeys()
	if err != nil {
		t.Fatalf("RouteKeys: %v", err)
	}
	names := make([]string, 0, len(keys))
	for k := range keys {
		names = append(names, k)
	}
	sort.Strings(names)
	var b strings.Builder
	for _, k := range names {
		v, _ := json.Marshal(keys[k])
		fmt.Fprintf(&b, "%s=%s ", k, v)
	}
	return strings.TrimSpace(b.String())
}

func TestRouteKeysPerType(t *testing.T) {
	cases := []struct{ name, route, want string }{
		{"stage_router defaults", "{id: r1, efficient: p/weak, capable: p/strong}",
			`capable_target="r1_cap" confidence_threshold=0.5 efficient_target="r1_eff" picker="efficient_first"`},
		{"stage_router overrides", "{id: r1, efficient: p/weak, capable: p/strong, picker: capable_first, confidence: 1}",
			`capable_target="r1_cap" confidence_threshold=1 efficient_target="r1_eff" picker="capable_first"`},
		{"plan_execute", "{id: r1, type: plan_execute, efficient: p/weak, capable: p/strong}",
			`capable_target="r1_cap" efficient_target="r1_eff"`},
		{"auto preset", "{id: r1, type: auto, efficient: p/weak, capable: p/strong}",
			`capable_target="r1_cap" efficient_target="r1_eff"`},
		{"passthrough", "{id: r1, type: passthrough, capable: p/strong}", `target="r1_cap"`},
		{"random", "{id: r1, type: random, efficient: p/weak, capable: p/strong}", `targets=["r1_cap","r1_eff"]`},
		{"noop", "{id: r1, type: noop}", ""},
		{"llm_classifier with judge target", "{id: r1, type: llm_classifier, efficient: p/weak, capable: p/strong, judge: p/judge}",
			`base_threshold=0.5 classifier_target="r1_judge" strong_target="r1_cap" weak_target="r1_eff"`},
		{"llm_classifier with wired judge", "{id: r1, type: llm_classifier, efficient: p/weak, capable: p/strong, options: {classifier_target: strong, base_threshold: 0.7}}",
			`base_threshold=0.7 classifier_target="strong" strong_target="r1_cap" weak_target="r1_eff"`},
		{"advisor via options", "{id: r1, type: advisor, options: {executor_target: strong, max_reviews: 2}}",
			`executor_target="strong" max_reviews=2`},
		{"stage_router tool_semantics", "{id: r1, efficient: p/weak, capable: p/strong, options: {tool_semantics: {new: [send_message]}}}",
			`capable_target="r1_cap" confidence_threshold=0.5 efficient_target="r1_eff" picker="efficient_first" tool_semantics={"new":["send_message"]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := routeKeysLine(t, tc.route); got != tc.want {
				t.Fatalf("keys:\n got %q\nwant %q", got, tc.want)
			}
		})
	}
}

func TestRouteValidationErrors(t *testing.T) {
	cases := []struct{ name, route, wantErr string }{
		{"unknown type", "{id: r1, type: bogus, efficient: p/a, capable: p/b}", "unknown switchyard type"},
		{"stage_router needs tiers", "{id: r1, efficient: p/a}", "needs both efficient and capable"},
		{"passthrough needs target", "{id: r1, type: passthrough}", "needs capable"},
		{"llm_classifier needs judge", "{id: r1, type: llm_classifier, efficient: p/a, capable: p/b}", "classifier_target"},
		{"option-driven needs options", "{id: r1, type: advisor}", "option-driven"},
		{"picker only on stage_router", "{id: r1, type: passthrough, capable: p/a, picker: capable_first}", "picker only applies"},
		{"confidence only on stage_router", "{id: r1, type: plan_execute, efficient: p/a, capable: p/b, confidence: 0.5}", "confidence only applies"},
		{"options cannot set id", "{id: r1, type: noop, options: {id: x}}", "is set by forseti"},
		{"options cannot override derived", "{id: r1, efficient: p/a, capable: p/b, options: {picker: capable_first}}", "duplicates"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load([]byte(crewWithRoute(tc.route)))
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestTargetNames(t *testing.T) {
	r := Route{ID: "pool"}
	eff, cap := r.TargetNames()
	if eff != "pool_eff" || cap != "pool_cap" {
		t.Fatalf("TargetNames = %q, %q", eff, cap)
	}
}
