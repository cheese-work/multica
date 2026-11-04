package handler

import "testing"

func TestCardAnswerQuoteLabelsTheChoiceAndTheNote(test *testing.T) {
	cases := []struct {
		name  string
		input decisionCardAnswerInput
		want  string
	}{
		{"option and note", decisionCardAnswerInput{Option: "Enabled", Text: "  Enabled. 17:31 ICT "}, "Chose: Enabled\n\nNote: Enabled. 17:31 ICT"},
		{"option only", decisionCardAnswerInput{Option: "Enabled"}, "Chose: Enabled"},
		{"text only", decisionCardAnswerInput{Text: " Ship C instead. "}, "Ship C instead."},
	}
	for _, tc := range cases {
		if got := cardAnswerQuoteText(tc.input); got != tc.want {
			test.Errorf("%s: quote text = %q, want %q", tc.name, got, tc.want)
		}
	}
}
