package issueworkflow

import (
	"strings"
	"testing"
)

// adoption_linked (CHE-3487, CHE-2948 S3): the close gate of a Setup change.

const longReason = "checked the agent-facing prompts, skills and runtime config; none depend on the bumped component"

func adoptionRefusal(a *AdoptionFacts) string {
	got := EvaluateEntry(gates(EntryModeBlock, "adoption_linked"), Facts{Adoption: a})
	if len(got) == 0 {
		return ""
	}
	return got[0].Fact
}

func TestAdoptionLinkedRules(t *testing.T) {
	done := AdoptionLink{ID: "CHE-1", Terminal: true}
	open := AdoptionLink{ID: "CHE-2"}
	registry := []string{"hermes", "multica"}
	cases := []struct {
		name    string
		facts   *AdoptionFacts
		refused string // substring of the fact; empty: the gate holds
	}{
		{"facts not loaded", nil, "not loaded"},
		{"lookup error", &AdoptionFacts{Err: "db down", SetupChange: "none"}, "db down"},
		{"property unset", &AdoptionFacts{}, "Setup change is not set"},
		{"unparsable metadata", &AdoptionFacts{SetupChange: "yes", Err: "adoption_issues is not a list of issue ids"}, "adoption_issues"},
		{"yes, listed issue open", &AdoptionFacts{SetupChange: "yes", Registry: registry, Components: []string{"hermes"}, Links: []AdoptionLink{open}}, "CHE-2"},
		{"yes, listed issue open, with adoption_none", &AdoptionFacts{SetupChange: "yes", Registry: registry, Components: []string{"hermes"}, Links: []AdoptionLink{open}, AdoptionNone: longReason}, "CHE-2"},
		{"none, listed issue open", &AdoptionFacts{SetupChange: "none", Links: []AdoptionLink{open}}, "CHE-2"},
		{"none, listed issue open, with adoption_none", &AdoptionFacts{SetupChange: "none", Links: []AdoptionLink{open}, AdoptionNone: longReason}, "CHE-2"},
		{"listed issue missing", &AdoptionFacts{SetupChange: "yes", Registry: registry, Components: []string{"hermes"}, Links: []AdoptionLink{{ID: "CHE-9", Missing: true}}}, "CHE-9"},
		{"yes, no components", &AdoptionFacts{SetupChange: "yes", Registry: registry, Links: []AdoptionLink{done}}, "components"},
		{"yes, unknown component", &AdoptionFacts{SetupChange: "yes", Registry: registry, Components: []string{"nope"}, Links: []AdoptionLink{done}}, "components"},
		{"yes, empty registry", &AdoptionFacts{SetupChange: "yes", Components: []string{"hermes"}, Links: []AdoptionLink{done}}, "components"},
		{"yes, empty links, short reason", &AdoptionFacts{SetupChange: "yes", Registry: registry, Components: []string{"hermes"}, AdoptionNone: "too short"}, "adoption_none"},
		{"yes, empty links, long reason", &AdoptionFacts{SetupChange: "yes", Registry: registry, Components: []string{"hermes"}, AdoptionNone: longReason}, ""},
		{"yes, all terminal", &AdoptionFacts{SetupChange: "yes", Registry: registry, Components: []string{"hermes"}, Links: []AdoptionLink{done, done}}, ""},
		{"none, cross-check passes", &AdoptionFacts{SetupChange: "none"}, ""},
		{"none, all terminal", &AdoptionFacts{SetupChange: "none", Links: []AdoptionLink{done}}, ""},
		{"none, cross-check hit", &AdoptionFacts{SetupChange: "none", CrossCheckHit: "hermes"}, "hermes"},
		{"unknown value", &AdoptionFacts{SetupChange: "maybe"}, "Setup change"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := adoptionRefusal(c.facts)
			if c.refused == "" && got != "" {
				t.Fatalf("gate refused: %s", got)
			}
			if c.refused != "" && !strings.Contains(got, c.refused) {
				t.Fatalf("refusal = %q, want it to contain %q", got, c.refused)
			}
		})
	}
}

func TestAdoptionLinkedIsAnEntryGateNotAnEdge(t *testing.T) {
	if err := validateEntry(Step{StatusKey: "done", Entry: gates(EntryModeBlock, "adoption_linked")}); err != nil {
		t.Fatalf("entry gate: %v", err)
	}
	g, err := ParseGuard("adoption_linked(x)")
	if err == nil {
		t.Fatalf("adoption_linked takes no argument, parsed %+v", g)
	}
}
