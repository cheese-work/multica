package handler

import (
	"testing"

	"github.com/multica-ai/multica/server/internal/decisiontx"
)

func TestRecommendedOptionIndexNamesOnlyAnUnambiguousChoice(test *testing.T) {
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
		{"a label inside another word is not a match", labels("A", "B"), "B, because it is safe", 1},
		{"short label inside prose only", labels("A", "B"), "Go with because", -1},
		{"whole label, any case", labels("Ship now", "Wait"), "ship NOW.", 0},
		{"overlapping labels pick the longer one", labels("Ship", "Ship now"), "Ship now", 1},
		{"overlapping labels, shorter one named", labels("Ship", "Ship now"), "Ship", 0},
		{"shorter label named apart from the longer one is ambiguous", labels("Ship", "Ship now"), "Ship rather than Ship now", -1},
		{"only the first clause counts", labels("Ship", "Ship now"), "Ship now, not Ship", 1},
		{"shorter label named twice is still one choice", labels("Ship", "Ship now"), "Ship or Ship", 0},
		{"two different options named is ambiguous", labels("A", "B"), "A or B", -1},
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
