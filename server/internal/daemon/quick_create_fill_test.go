package daemon

import (
	"os"
	"strings"
	"testing"
)

// CHE-6090: with quick_create_fill on, the prompt tells the agent to fill every
// field the modal left empty; off keeps the historical prompt byte for byte.

func TestQuickCreateFillOnFillsEmptyFields(t *testing.T) {
	out := buildQuickCreatePrompt(Task{
		QuickCreatePrompt:     "fix the login button color",
		QuickCreateFill:       true,
		QuickCreateTitleTypes: []string{"fix", "design"},
	})
	for _, want := range []string{
		"multica project list --output json",
		"--start-date",
		"--due-date",
		"--property",
		"multica property list --output json",
		"Readiness",
		"`fix`, `design`",
		"Acceptance criteria",
		"Estimate:",
		"derived by",
		"`--priority medium`",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("fill=on prompt missing %q\n%s", want, out)
		}
	}
	for _, banned := range []string{
		"If unspecified, omit",
		"**project**: omit",
		"never invent requirements, implementation details, or acceptance criteria",
	} {
		if strings.Contains(out, banned) {
			t.Errorf("fill=on prompt still contains %q", banned)
		}
	}
}

func TestQuickCreateFillOffIsByteIdentical(t *testing.T) {
	want, err := os.ReadFile("testdata/quick_create_fill_off.golden")
	if err != nil {
		t.Fatal(err)
	}
	got := buildQuickCreatePrompt(Task{QuickCreatePrompt: "fix the login button color"})
	if got != string(want) {
		t.Errorf("fill=off prompt differs from the ce8a292c60 golden\n--- got ---\n%s", got)
	}
}

func TestQuickCreateFillOnKeepsModalPicks(t *testing.T) {
	out := buildQuickCreatePrompt(Task{
		QuickCreatePrompt:   "x",
		QuickCreateFill:     true,
		QuickCreatePriority: "urgent",
		QuickCreateDueDate:  "2026-12-01",
		ProjectID:           "11111111-1111-1111-1111-111111111111",
	})
	for _, want := range []string{
		"Pass `--priority urgent`; the quick-create selection is authoritative",
		"Pass `--due-date 2026-12-01`; the quick-create selection is authoritative",
		"Pass `--project \"11111111-1111-1111-1111-111111111111\"`",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("modal pick not passed verbatim: missing %q", want)
		}
	}
	// A picked value must not also be re-inferred.
	for _, banned := range []string{"infer from the request", "pick the project whose description", "plus the estimate"} {
		if strings.Contains(out, banned) {
			t.Errorf("fill rule %q shown although the modal picked the field", banned)
		}
	}
}
