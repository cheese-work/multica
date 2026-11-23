package agentrolescope

import (
	"slices"
	"testing"
)

const sol = "11111111-1111-4111-8111-111111111111"

func TestParseSeedAndClasses(t *testing.T) {
	cfg, err := Parse([]byte(`{"agents":{"` + sol + `":["plan","plan_review","decision","debate","security_cosign"]},"security_paths":["server/migrations/"]}`))
	if err != nil {
		t.Fatal(err)
	}
	for title, want := range map[string]string{
		"plan(x): shape":                    Plan,
		"decision: pick":                    Decision,
		"debate(x): weigh":                  Debate,
		"security(auth): cosign":            SecurityCosign,
		"fix: edit server/migrations/1.sql": SecurityCosign,
		"feat(x): ship":                     Execution,
		"no type here":                      Execution,
	} {
		if got := cfg.ClassOf(title); got != want {
			t.Errorf("ClassOf(%q) = %q, want %q", title, got, want)
		}
	}
	if got := cfg.ReviewClassOf("feat(x): ship"); got != CodeReview {
		t.Errorf("ReviewClassOf(feat) = %q, want code_review", got)
	}
	if got := cfg.ReviewClassOf("plan(x): shape"); got != Plan {
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
		"empty path":    `{"agents":{"` + sol + `":["plan"]},"security_paths":[" "]}`,
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
