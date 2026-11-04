package decisiontx

import (
	"slices"
	"strings"
	"unicode"
)

// recommendPrefix is the filler an agent writes before the label it recommends.
var recommendPrefix = []string{"recommended:", "recommend:", "recommended", "recommend", "option", "choose", "pick"}

var negations = map[string]bool{"not": true, "no": true, "never": true, "avoid": true, "without": true, "against": true, "don't": true, "dont": true, "isn't": true, "shouldn't": true}

type span struct{ label, start, end int }

// RecommendedIndex returns which label the recommendation text names, or -1.
// A label counts when it appears as whole words and no negation sits right
// before or after it ("not Proceed", "Proceed is not safe"). A label that
// starts the text (after markdown and a filler word) wins; otherwise exactly
// one label must be named. A label inside a longer label's match is ignored.
// -1 means the recommendation is ambiguous or names no option.
func RecommendedIndex(recommendation string, labels []string) int {
	text := strings.ToLower(strings.TrimLeft(strings.TrimSpace(recommendation), "*_#>- `\""))
	for _, filler := range recommendPrefix {
		if rest, ok := strings.CutPrefix(text, filler+" "); ok {
			text = strings.TrimLeft(rest, "*_ `\"")
			break
		}
	}
	var all []span
	for index, label := range labels {
		label = strings.ToLower(strings.TrimSpace(label))
		for from := 0; label != "" && from < len(text); {
			at := strings.Index(text[from:], label)
			if at < 0 {
				break
			}
			start := from + at
			end := start + len(label)
			from = start + 1
			if wholeWords(text, start, end) {
				all = append(all, span{index, start, end})
			}
		}
	}
	var named []span
	for _, candidate := range all {
		inside := slices.ContainsFunc(all, func(other span) bool {
			return other.label != candidate.label && other.start <= candidate.start && candidate.end <= other.end && other.end-other.start > candidate.end-candidate.start
		})
		if !inside && !negated(text, candidate) {
			named = append(named, candidate)
		}
	}
	best, bestEnd := -1, 0
	for _, candidate := range named {
		if candidate.start == 0 && candidate.end > bestEnd {
			best, bestEnd = candidate.label, candidate.end
		}
	}
	if best >= 0 {
		return best
	}
	if len(named) > 0 && !slices.ContainsFunc(named, func(s span) bool { return s.label != named[0].label }) {
		return named[0].label
	}
	return -1
}

func wholeWords(text string, start, end int) bool {
	before := []rune(text[:start])
	if len(before) > 0 && isWordRune(before[len(before)-1]) {
		return false
	}
	after := []rune(text[end:])
	if len(after) == 0 {
		return true
	}
	return !isWordRune(after[0]) && !(after[0] == '-' && len(after) > 1 && unicode.IsLetter(after[1]))
}

func isWordRune(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }

// negated reports a negation word within two words before the label, or within
// three words after it, with no punctuation in between.
func negated(text string, s span) bool {
	clause := func(part string, fromEnd bool, limit int) []string {
		if i := strings.LastIndexAny(part, ",;:.!?()—"); fromEnd && i >= 0 {
			part = part[i+1:]
		} else if i := strings.IndexAny(part, ",;:.!?()—"); !fromEnd && i >= 0 {
			part = part[:i]
		}
		words := strings.Fields(part)
		if len(words) > limit {
			if fromEnd {
				words = words[len(words)-limit:]
			} else {
				words = words[:limit]
			}
		}
		return words
	}
	return slices.ContainsFunc(clause(text[:s.start], true, 2), func(w string) bool { return negations[w] }) ||
		slices.ContainsFunc(clause(text[s.end:], false, 3), func(w string) bool { return negations[w] })
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
