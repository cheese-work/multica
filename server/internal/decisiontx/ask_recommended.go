package decisiontx

import (
	"slices"
	"strings"
)

// RecommendedIndex is the position of the option named by RecommendedOption
// (exact label, case-insensitive), or -1. The recommendation prose is never
// read: guessing an option from free text cannot be made reliable (CHE-3628).
func (ask Ask) RecommendedIndex() int {
	option := strings.TrimSpace(ask.RecommendedOption)
	if option == "" {
		return -1
	}
	return slices.IndexFunc(ask.Options, func(candidate AskOption) bool {
		return strings.EqualFold(strings.TrimSpace(candidate.Label), option)
	})
}

// RequireRecommendedFirst returns the ask with the recommended option moved to
// the front, others keeping their order (Cheese's UI rule, CHE-3628). An ask
// with two or more options must name it in RecommendedOption, or it is
// refused. Asks with fewer than two options keep their order.
func (ask Ask) RequireRecommendedFirst() (Ask, error) {
	if len(ask.Options) < 2 {
		return ask, nil
	}
	index := ask.RecommendedIndex()
	if index < 0 {
		return ask, &AskError{"ask.recommended_option", "is required with two or more options and must equal one option label exactly"}
	}
	ordered := slices.Clone(ask.Options)
	ask.Options = slices.Insert(slices.Delete(ordered, index, index+1), 0, ask.Options[index])
	return ask, nil
}
