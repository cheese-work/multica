package system1

import (
	"slices"
	"testing"
)

func keys(key string, count int) []string { return slices.Repeat([]string{key}, count) }

func TestPredict(test *testing.T) {
	cases := []struct {
		name       string
		precedents []string
		want       Prediction
	}{
		{"no precedent abstains", nil, Prediction{}},
		{"below minimum abstains", keys("recommended", MinPrecedents-1), Prediction{Precedents: MinPrecedents - 1}},
		{"majority wins", append(keys("recommended", 9), "other"), Prediction{Key: "recommended", Confidence: 10.0 / 12.0, Precedents: 10}},
		{"tie abstains", append(keys("recommended", 3), keys("other", 3)...), Prediction{Precedents: 6}},
		{"unanimous at minimum", keys("agent:42", MinPrecedents), Prediction{Key: "agent:42", Confidence: 6.0 / 7.0, Precedents: MinPrecedents}},
	}
	for _, tc := range cases {
		test.Run(tc.name, func(test *testing.T) {
			if got := Predict(tc.precedents); got != tc.want {
				test.Fatalf("Predict = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestWaiverRule(t *testing.T) {
	for _, c := range []struct {
		crossLab bool
		evidence string
		want     string
	}{
		{false, "characterization_test", KeyRefuse},
		{false, "review_only", KeyRefuse},
		{true, "manual_repro", KeyWaive},
		{true, "review_only", ""},
	} {
		if got := WaiverRule(c.crossLab, c.evidence); got != c.want {
			t.Errorf("WaiverRule(%v, %q) = %q, want %q", c.crossLab, c.evidence, got, c.want)
		}
	}
}
