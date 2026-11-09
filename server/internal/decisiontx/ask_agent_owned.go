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
var agentOwnedStep = regexp.MustCompile(`(?i)` +
	`\bready for review\b|\bmark(?:ed|ing)?\b[^.\n]{0,30}\bready\b|\bundraft\b|\bgh pr ready\b` +
	`|\bretarget\b|\bgh pr edit\b[^\n]*--base\b|\brebase\b` +
	`|\bre-?run\b[^.\n]{0,40}\b(?:CI|checks?|workflow|jobs?|runs?)\b|\bgh run rerun\b` +
	`|\b(?:close|link)\b[^.\n]{0,30}(?:\b(?:issue|PR|pull request)\b|\b[A-Za-z][A-Za-z0-9]*-\d+\b|#\d+)`)

var mergeTarget = regexp.MustCompile(`(?i)\bmerge\b[^.\n]*?\binto\s+(?:the\s+)?(\S+)(?:\s+branch)?`)

const agentOwnedMessage = "is an agent-owned step; do it yourself and ask a human only for spend, production deploy or release, public publication, an irreversible action or an open product choice"

// agentOwned reports whether text hands a human an agent-owned action.
func agentOwned(text string) bool {
	if agentOwnedStep.MatchString(text) {
		return true
	}
	into := mergeTarget.FindStringSubmatch(text)
	return len(into) > 1 && !strings.EqualFold(into[1], "default")
}
