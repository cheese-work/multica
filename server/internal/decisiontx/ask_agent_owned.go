package decisiontx

import (
	"regexp"
	"strings"
)

// agentOwnedStep matches a step an agent owns under Standing Delivery
// Authorization (CHE-4869): mark ready, retarget, rebase, rerun CI, close or
// link an issue or PR, or merge into a named branch. A merge into "the default
// branch" stays askable: the server does not know a repo's default, so a named
// target is refused and the asker says "the default branch" when it is one.
//
// A period ends a sentence only when whitespace follows it, so a dotted branch
// such as release/1.2 or v1.2.3-rc.1 stays inside the step.
const stepGap = `(?:[^.\n]|\.\S)`

var agentOwnedStep = regexp.MustCompile(`(?i)` +
	`\bready for review\b|\bmark(?:ed|ing)?\b` + stepGap + `{0,30}\bready\b|\bundraft\b|\bgh pr ready\b` +
	`|\bretarget\b|\bgh pr edit\b[^\n]*--base\b|\brebase\b` +
	`|\bre-?run\b` + stepGap + `{0,40}\b(?:CI|checks?|workflow|jobs?|runs?)\b|\bgh run rerun\b` +
	`|\b(?:close|link)\b` + stepGap + `{0,30}(?:\b(?:issue|PR|pull request)\b|\b[A-Za-z][A-Za-z0-9]*-\d+\b|#\d+)`)

var mergeTarget = regexp.MustCompile(`(?i)\bmerge\b` + stepGap + `*?\binto\s+(?:the\s+)?(\S+)(\s+branch\b)?`)

// branchEdge holds the quotes, brackets and sentence punctuation around a target.
const branchEdge = "`'\"()[].,;:!?"

const agentOwnedMessage = "is an agent-owned step; do it yourself and ask a human only for spend, production deploy or release, public publication, an irreversible action or an open product choice"

// agentOwned reports whether text hands a human an agent-owned action.
func agentOwned(text string) bool {
	if agentOwnedStep.MatchString(text) {
		return true
	}
	into := mergeTarget.FindStringSubmatch(text)
	if len(into) < 3 {
		return false
	}
	return !(strings.EqualFold(strings.Trim(into[1], branchEdge), "default") && into[2] != "")
}
