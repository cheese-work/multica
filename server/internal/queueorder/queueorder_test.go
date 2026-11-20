package queueorder

import (
	"strings"
	"testing"
)

func TestDefaultsKeepTodaysOrder(t *testing.T) {
	for name, settings := range map[string]map[string]any{
		"nil":   nil,
		"empty": {},
		"other": {"compact_prompt": "x"},
	} {
		if got := Order(settings); got != ModePriority {
			t.Errorf("%s: Order = %q, want %q", name, got, ModePriority)
		}
		if got := AgingMinutes(settings); got != 30 {
			t.Errorf("%s: AgingMinutes = %d, want 30", name, got)
		}
	}
	if got := Order(map[string]any{KeyOrder: "project_rank"}); got != ModeProjectRank {
		t.Errorf("Order = %q, want project_rank", got)
	}
	if got := AgingMinutes(map[string]any{KeyAging: float64(45)}); got != 45 {
		t.Errorf("AgingMinutes = %d, want 45", got)
	}
}

func TestValidateAcceptsAllowedValues(t *testing.T) {
	for _, settings := range []map[string]any{
		{},
		{KeyOrder: "priority"},
		{KeyOrder: "project_rank", KeyAging: float64(5)},
		{KeyAging: float64(720)},
		{KeyAging: 30},
	} {
		if err := ValidateSetting(settings); err != nil {
			t.Errorf("%v refused: %v", settings, err)
		}
	}
}

// A refused value names what is allowed, so a caller can fix it without the docs.
func TestValidateRefusalNamesAllowedValues(t *testing.T) {
	for _, tc := range []struct {
		name     string
		settings map[string]any
		want     []string
	}{
		{"unknown mode", map[string]any{KeyOrder: "fifo"}, []string{KeyOrder, "priority", "project_rank"}},
		{"wrong case", map[string]any{KeyOrder: "Project_Rank"}, []string{"priority", "project_rank"}},
		{"mode not a string", map[string]any{KeyOrder: float64(1)}, []string{"priority", "project_rank"}},
		{"mode null", map[string]any{KeyOrder: nil}, []string{"priority", "project_rank"}},
		{"aging below 5", map[string]any{KeyAging: float64(4)}, []string{KeyAging, "5", "720"}},
		{"aging above 720", map[string]any{KeyAging: float64(721)}, []string{"5", "720"}},
		{"aging fraction", map[string]any{KeyAging: 30.5}, []string{"5", "720"}},
		{"aging string", map[string]any{KeyAging: "30"}, []string{"5", "720"}},
		{"aging null", map[string]any{KeyAging: nil}, []string{"5", "720"}},
	} {
		err := ValidateSetting(tc.settings)
		if err == nil {
			t.Errorf("%s: accepted", tc.name)
			continue
		}
		for _, want := range tc.want {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%s: error %q does not name %q", tc.name, err, want)
			}
		}
	}
}

func TestValidateIgnoresNonObject(t *testing.T) {
	if err := ValidateSetting("not an object"); err != nil {
		t.Fatalf("non-object refused: %v", err)
	}
}
