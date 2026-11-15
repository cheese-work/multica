package decisiontx

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

func labels(options []AskOption) []string {
	out := make([]string, len(options))
	for i, option := range options {
		out[i] = option.Label
	}
	return out
}

// recommended_option is the only source of the recommended option. The prose is never read.
func TestRequireRecommendedFirst(test *testing.T) {
	raw, _ := json.Marshal(validAskMap()) // options NOW, LATER
	ask, err := DecodeAsk(raw)
	if err != nil {
		test.Fatal(err)
	}
	cases := []struct {
		name, recommendation, option string
		want                         []string // nil: the ask is refused
	}{
		{"exact label moves first", "", "LATER", []string{"LATER", "NOW"}},
		{"case and space are ignored", "", " later ", []string{"LATER", "NOW"}},
		{"already first", "", "NOW", []string{"NOW", "LATER"}},
		{"prose that disagrees is ignored", "NOW is fine too.", "LATER", []string{"LATER", "NOW"}},
		{"prose that names the label is not enough", "LATER", "", nil},
		{"prose with a contrast is not enough", "I recommend LATER rather than NOW", "", nil},
		{"prose that misleads is not enough", "NOW is risky; I recommend LATER", "", nil},
		{"no option at all is refused", "Whichever is cheaper.", "", nil},
		{"blank option is refused", "LATER", "  ", nil},
		{"unknown label is refused", "LATER", "SOMETIME", nil},
	}
	for _, tc := range cases {
		test.Run(tc.name, func(test *testing.T) {
			candidate := ask
			candidate.Recommendation, candidate.RecommendedOption = tc.recommendation, tc.option
			got, err := candidate.RequireRecommendedFirst()
			if tc.want == nil {
				if err == nil || !strings.Contains(err.Error(), "recommended_option") {
					test.Fatalf("want a refusal naming recommended_option, got %v", err)
				}
				return
			}
			if err != nil || !slices.Equal(labels(got.Options), tc.want) || got.RecommendedIndex() != 0 {
				test.Fatalf("got %v (%v), want %v", labels(got.Options), err, tc.want)
			}
		})
	}
}

func TestRequireRecommendedFirstLeavesSingleOptionAsksAlone(test *testing.T) {
	ask := Ask{Options: []AskOption{{Label: "Only"}}}
	got, err := ask.RequireRecommendedFirst()
	if err != nil || len(got.Options) != 1 || got.RecommendedIndex() != -1 {
		test.Fatalf("got %+v (%v)", got, err)
	}
}

func TestDecodeAskRecommendedOption(test *testing.T) {
	m := validAskMap()
	m["recommended_option"] = "LATER"
	raw, _ := json.Marshal(m)
	ask, err := DecodeAsk(raw)
	if err != nil || ask.RecommendedOption != "LATER" {
		test.Fatalf("recommended_option = %q (%v)", ask.RecommendedOption, err)
	}
}
