package agentrolescope

import (
	"slices"
	"testing"
)

const sol = "11111111-1111-4111-8111-111111111111"

func TestParseSeedAndClasses(t *testing.T) {
	cfg, err := Parse([]byte(`{"agents":{"` + sol + `":["plan","plan_review","decision","debate","security_cosign"]}}`))
	if err != nil {
		t.Fatal(err)
	}
	const parent = "CHE-9192"
	for _, tc := range []struct{ title, parent, want string }{
		{"plan(x): shape", "", Plan},
		{"decision: pick", "", Decision},
		{"debate(x): weigh", "", Debate},
		// A co-sign is a gate issue under the work it gates; a bare type or a
		// title alone is not one.
		{"security gate: CHE-9192", parent, SecurityCosign},
		{"security gate: che-9192 (D8)", parent, SecurityCosign},
		{"security gate: CHE-9192", "", Execution},
		{"security gate: CHE-1", parent, Execution},
		{"security(auth): implement login", "", Execution},
		{"security(auth): implement login", parent, Execution},
		{"fix: edit server/migrations/1.sql", "", Execution},
		{"feat(x): ship", "", Execution},
		{"no type here", "", Execution},
		{"review(plan): the rollout plan", "", PlanReview},
		{"review(design): the schema", "", PlanReview},
		{"review(code): review PR 2121", "", CodeReview},
		{"review: review PR 2121", "", CodeReview},
		{"review(security): review the auth diff", "", CodeReview},
	} {
		if got := cfg.ClassOf(tc.title, tc.parent); got != tc.want {
			t.Errorf("ClassOf(%q, %q) = %q, want %q", tc.title, tc.parent, got, tc.want)
		}
	}
	if got := cfg.ReviewClassOf("feat(x): ship", ""); got != CodeReview {
		t.Errorf("ReviewClassOf(feat) = %q, want code_review", got)
	}
	if got := cfg.ReviewClassOf("plan(x): shape", ""); got != Plan {
		t.Errorf("ReviewClassOf(plan) = %q, want plan", got)
	}
	if cfg.Allows(sol, Execution) || cfg.Allows(sol, CodeReview) {
		t.Error("Sol must not be allowed execution or code review")
	}
	if !cfg.Allows(sol, SecurityCosign) || !cfg.Allows("22222222-2222-4222-8222-222222222222", Execution) {
		t.Error("Sol keeps its seat; an agent with no entry is unrestricted")
	}
}

func TestParseRejects(t *testing.T) {
	for name, raw := range map[string]string{
		"not an object": `[]`,
		"no agents":     `{}`,
		"bad id":        `{"agents":{"sol":["plan"]}}`,
		"empty list":    `{"agents":{"` + sol + `":[]}}`,
		"unknown class": `{"agents":{"` + sol + `":["coding"]}}`,
		"unknown field": `{"agents":{"` + sol + `":["plan"]},"extra":1}`,
		"paths removed": `{"agents":{"` + sol + `":["plan"]},"security_paths":["server/migrations/"]}`,
	} {
		if _, err := Parse([]byte(raw)); err == nil {
			t.Errorf("%s: Parse accepted %s", name, raw)
		}
	}
}

func TestFromSettingsIsLenient(t *testing.T) {
	if got := FromSettings(map[string]any{SettingsKey: "junk"}); len(got.Agents) != 0 {
		t.Errorf("malformed setting restricted %v", got.AgentIDs())
	}
	if got := FromSettings(nil); !got.Allows(sol, Execution) {
		t.Error("unset setting must restrict nobody")
	}
	cfg := FromSettings(map[string]any{SettingsKey: map[string]any{"agents": map[string]any{sol: []any{"plan"}}}})
	if !slices.Equal(cfg.AgentIDs(), []string{sol}) {
		t.Errorf("AgentIDs = %v", cfg.AgentIDs())
	}
}
