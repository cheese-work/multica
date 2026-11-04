package decisiontx

import (
	"encoding/json"
	"slices"
	"testing"
)

func labels(options []AskOption) []string {
	out := make([]string, len(options))
	for i, option := range options {
		out[i] = option.Label
	}
	return out
}

func TestRecommendedIndex(test *testing.T) {
	cases := []struct {
		name, recommendation string
		labels               []string
		want                 int
	}{
		{"exact label", "LATER", []string{"NOW", "LATER"}, 1},
		{"label then prose", "D2: ship after the check because it is safer.", []string{"D1", "D2", "D3"}, 1},
		{"option prefix and markdown", "**Option B** — merge it.", []string{"A", "B"}, 1},
		{"case-insensitive", "later, since CI is red", []string{"Now", "Later"}, 1},
		{"label boundary", "D10 is better", []string{"D1", "D10"}, 1},
		{"prefix of longer word is no match", "Merge-now wins", []string{"Merge", "Hold"}, -1},
		{"no label named", "Do whatever is cheaper.", []string{"A", "B"}, -1},
		{"ordinary wording", "I recommend Hold because CI is red.", []string{"Proceed", "Hold"}, 1},
		{"label mid-sentence", "We should go with Later, since CI is red.", []string{"Now", "Later"}, 1},
		{"label inside longer label", "Use Hold off until CI is green.", []string{"Hold", "Hold off"}, 1},
		{"label at start wins over a later mention", "Hold, then Proceed once CI is green.", []string{"Proceed", "Hold"}, 1},
		{"two labels named mid-sentence is ambiguous", "Either Hold or Proceed works.", []string{"Proceed", "Hold"}, -1},
		{"other label negated", "Hold, not Proceed.", []string{"Proceed", "Hold"}, 1},
		{"negated mid-sentence label is no match", "Do not Proceed yet.", []string{"Proceed", "Hold"}, -1},
		{"negated start label is no match", "Proceed is not safe.", []string{"Proceed", "Hold"}, -1},
		{"word inside another word is no match", "Withhold the change.", []string{"Proceed", "Hold"}, -1},
	}
	for _, tc := range cases {
		test.Run(tc.name, func(test *testing.T) {
			if got := RecommendedIndex(tc.recommendation, tc.labels); got != tc.want {
				test.Fatalf("RecommendedIndex(%q, %v) = %d, want %d", tc.recommendation, tc.labels, got, tc.want)
			}
		})
	}
}

func TestRecommendedFirstMovesRecommendedOptionToFront(test *testing.T) {
	raw, _ := json.Marshal(validAskMap()) // options NOW, LATER; recommendation LATER
	ask, err := DecodeAsk(raw)
	if err != nil {
		test.Fatal(err)
	}
	reordered := ask.RecommendedFirst()
	if got := labels(reordered.Options); !slices.Equal(got, []string{"LATER", "NOW"}) {
		test.Fatalf("reordered = %v, want [LATER NOW]", got)
	}
	if got := labels(ask.Options); !slices.Equal(got, []string{"NOW", "LATER"}) {
		test.Fatalf("receiver mutated: %v", got)
	}
	if idx := RecommendedIndex(reordered.Recommendation, labels(reordered.Options)); idx != 0 {
		test.Fatalf("recommended index after reorder = %d, want 0", idx)
	}
	// an unidentifiable recommendation keeps the caller's order
	ask.Recommendation = "whichever is cheaper"
	if got := labels(ask.RecommendedFirst().Options); !slices.Equal(got, []string{"NOW", "LATER"}) {
		test.Fatalf("unmatched reorder = %v", got)
	}
}

func TestDecodeAskConfidence(test *testing.T) {
	for _, tc := range []struct {
		name    string
		value   any
		wantErr string
		want    float64
	}{
		{"valid", 0.8, "", 0.8},
		{"zero", 0, "", 0},
		{"above one", 1.5, "ask.confidence must be a number from 0 to 1", 0},
		{"negative", -0.1, "ask.confidence must be a number from 0 to 1", 0},
		{"string", "high", "ask.confidence must be a number from 0 to 1", 0},
	} {
		test.Run(tc.name, func(test *testing.T) {
			ask := validAskMap()
			ask["confidence"] = tc.value
			raw, _ := json.Marshal(ask)
			got, err := DecodeAsk(raw)
			if errString(err) != tc.wantErr {
				test.Fatalf("error = %q, want %q", errString(err), tc.wantErr)
			}
			if tc.wantErr == "" && (got.Confidence == nil || *got.Confidence != tc.want) {
				test.Fatalf("confidence = %v, want %v", got.Confidence, tc.want)
			}
		})
	}
	raw, _ := json.Marshal(validAskMap())
	if got, _ := DecodeAsk(raw); got.Confidence != nil {
		test.Fatalf("absent confidence = %v, want nil", got.Confidence)
	}
}
