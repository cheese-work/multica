package decisiontx

import (
	"slices"
	"strings"
	"unicode"
)

// recommendPrefix is the filler an agent writes before the label it recommends.
var recommendPrefix = []string{"recommended:", "recommend:", "recommended", "recommend", "option", "choose", "pick"}

// RecommendedIndex returns which label the recommendation text names: the text
// starts with the label (after markdown and a filler word) and the label ends
// at a non-alphanumeric rune. Several matches: the longest label wins. -1 when
// no label starts the text.
func RecommendedIndex(recommendation string, labels []string) int {
	text := strings.ToLower(strings.TrimLeft(strings.TrimSpace(recommendation), "*_#>- `\""))
	for _, filler := range recommendPrefix {
		if rest, ok := strings.CutPrefix(text, filler+" "); ok {
			text = strings.TrimLeft(rest, "*_ `\"")
			break
		}
	}
	best := -1
	for index, label := range labels {
		label = strings.ToLower(strings.TrimSpace(label))
		rest, ok := strings.CutPrefix(text, label)
		if !ok || label == "" {
			continue
		}
		if next := []rune(rest); len(next) > 0 && (unicode.IsLetter(next[0]) || unicode.IsDigit(next[0]) || next[0] == '-' && len(next) > 1 && unicode.IsLetter(next[1])) {
			continue
		}
		if best < 0 || len(label) > len(strings.TrimSpace(labels[best])) {
			best = index
		}
	}
	return best
}

// RecommendedFirst returns the ask with the recommended option moved to the
// front, others keeping their order (Cheese's UI rule, CHE-3628). Without an
// identifiable recommendation the caller's order stands.
func (ask Ask) RecommendedFirst() Ask {
	labels := make([]string, len(ask.Options))
	for i, option := range ask.Options {
		labels[i] = option.Label
	}
	index := RecommendedIndex(ask.Recommendation, labels)
	if index <= 0 {
		return ask
	}
	ordered := slices.Clone(ask.Options)
	ordered = slices.Insert(slices.Delete(ordered, index, index+1), 0, ask.Options[index])
	ask.Options = ordered
	return ask
}
