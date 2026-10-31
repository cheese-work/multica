package decisiontx

import (
	"fmt"
	"regexp"
	"strings"
)

var (
	lintLinks     = regexp.MustCompile(`\[[^\]]*\]\([^)]*\)|https?://\S+`)
	lintSHA       = regexp.MustCompile(`\b[0-9a-f]{7,40}\b`)
	lintCode      = regexp.MustCompile(`\b(?:L[1-9]|P[0-3]|(?:RGS|RHC|OER)-\d+|NOT-RUN|NOT-CAPTURED|historical-red)\b`)
	lintSentences = regexp.MustCompile(`[.!?]+(?:\s+|$)`)
)

// LintAsk returns advisory warnings for text a non-expert reader sees first.
// It never blocks a request.
func LintAsk(ask Ask) []string {
	var hints []string
	check := func(field, text string) {
		text = lintLinks.ReplaceAllString(text, " ")
		for _, sha := range lintSHA.FindAllString(text, -1) {
			if strings.ContainsAny(sha, "0123456789") {
				hints = append(hints, fmt.Sprintf("%s has a commit SHA (%s); say what it is in plain words", field, sha))
			}
		}
		for _, code := range lintCode.FindAllString(text, -1) {
			hints = append(hints, fmt.Sprintf("%s has an internal code (%s); say what it means in plain words", field, code))
		}
	}
	check("context_eli10", ask.ContextELI10)
	check("stakes_if_wrong", ask.StakesIfWrong)
	for index, option := range ask.Options {
		check(fmt.Sprintf("options[%d].what_happens", index), option.WhatHappens)
	}
	context := lintLinks.ReplaceAllString(ask.ContextELI10, " ")
	sentences := 0
	for _, part := range lintSentences.Split(strings.TrimSpace(context), -1) {
		if strings.TrimSpace(part) != "" {
			sentences++
		}
	}
	if sentences < 2 || sentences > 5 {
		hints = append(hints, fmt.Sprintf("context_eli10 has %d sentences; use 2 to 5", sentences))
	}
	return append(hints, lintHumanSteps(ask)...)
}
