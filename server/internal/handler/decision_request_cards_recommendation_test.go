package handler

import (
	"testing"

	"github.com/multica-ai/multica/server/internal/decisiontx"
)

func TestRecommendedOptionIndexMarksOnlyAnExactLabelMatch(test *testing.T) {
	labels := func(names ...string) []decisiontx.AskOption {
		options := make([]decisiontx.AskOption, len(names))
		for index, name := range names {
			options[index].Label = name
		}
		return options
	}
	cases := []struct {
		name           string
		options        []decisiontx.AskOption
		recommendation string
		want           int // -1: nothing is marked
	}{
		{"whole recommendation equals a label, any case", labels("Ship now", "Wait"), "  ship NOW ", 0},
		{"second option", labels("A", "B"), "B", 1},
		{"a label followed by a reason is not exact", labels("A", "B"), "B, because it is safe", -1},
		{"negation names the rejected option", labels("A", "B"), "Do not choose A; choose B", -1},
		{"rather-than names two options", labels("Ship", "Ship now"), "Ship rather than Ship now", -1},
		{"partial overlap", labels("Ship", "Ship now"), "Ship now, not Ship", -1},
		{"two different options named", labels("A", "B"), "A or B", -1},
		{"two options with the same label", labels("A", "a"), "A", -1},
		{"no label named", labels("A", "B"), "Neither, wait a day", -1},
		{"empty recommendation", labels("A", "B"), "", -1},
	}
	for _, tc := range cases {
		got := recommendedOptionIndex(tc.options, tc.recommendation)
		switch {
		case tc.want < 0 && got != nil:
			test.Errorf("%s: marked option %d, want none", tc.name, *got)
		case tc.want >= 0 && (got == nil || *got != tc.want):
			test.Errorf("%s: got %v, want %d", tc.name, got, tc.want)
		}
	}
}
