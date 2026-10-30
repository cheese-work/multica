package util

import (
	"strings"
	"testing"
)

func TestEstimateInstructionTokens(t *testing.T) {
	tests := []struct {
		name string
		text string
		want int
	}{
		{name: "empty", text: "", want: 0},
		{name: "one rune", text: "a", want: 1},
		{name: "two runes", text: "ab", want: 1},
		{name: "exact multiple", text: "abc", want: 2},
		{name: "partial multiple", text: "abcd", want: 2},
		{name: "ascii", text: "Short squad instructions", want: 9},
		{name: "multibyte", text: "你好世界", want: 2},
		{name: "emoji", text: "🎉🎉🎉", want: 2},
		{name: "combining runes", text: "e\u0301e\u0301e\u0301", want: 3},
		{name: "invalid utf8", text: "\xff\xfe\xfd", want: 2},
		{name: "squad limit ascii", text: strings.Repeat("a", 8999), want: 3000},
		{name: "squad over limit ascii", text: strings.Repeat("a", 9000), want: 3001},
		{name: "squad limit unicode", text: strings.Repeat("界", 8999), want: 3000},
		{name: "squad over limit unicode", text: strings.Repeat("界", 9000), want: 3001},
		{name: "agent limit ascii", text: strings.Repeat("a", 11999), want: 4000},
		{name: "agent over limit ascii", text: strings.Repeat("a", 12000), want: 4001},
		{name: "agent limit unicode", text: strings.Repeat("🎉", 11999), want: 4000},
		{name: "agent over limit unicode", text: strings.Repeat("🎉", 12000), want: 4001},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := EstimateInstructionTokens(test.text); got != test.want {
				t.Errorf("EstimateInstructionTokens() = %d, want %d", got, test.want)
			}
		})
	}
}
