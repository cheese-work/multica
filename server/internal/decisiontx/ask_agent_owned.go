package decisiontx

import (
	"regexp"
	"strings"
	"unicode"
)

// agentOwnedStep matches a step an agent owns under Standing Delivery
// Authorization (CHE-4869): mark ready, retarget, rebase, rerun CI, close or
// link an issue or PR, or merge into a named branch. A merge into "the default
// branch" stays askable: the server does not know a repo's default, so a named
// target is refused and the asker says "the default branch" when it is one.
//
// "-B" is the short form of "--base"; lowercase "-b" is "--body", so that letter
// stays case-sensitive.
//
// A period ends a sentence only when whitespace follows it, so a dotted branch
// such as release/1.2 or v1.2.3-rc.1 stays inside the step.
const stepGap = `(?:[^.\n]|\.\S)`

var agentOwnedStep = regexp.MustCompile(`(?i)` +
	`\bready for review\b|\bmark(?:ed|ing)?\b` + stepGap + `{0,30}\bready\b|\bundraft\b|\bgh pr ready\b` +
	`|\bretarget\b|\bgh pr edit\b[^\n]*(?:--base\b|\s(?-i:-B))|\brebase\b` +
	`|\bre-?run\b` + stepGap + `{0,40}\b(?:CI|checks?|workflow|jobs?|runs?)\b|\bgh run rerun\b` +
	`|\b(?:close|link)\b` + stepGap + `{0,30}(?:\b(?:issue|PR|pull request)\b|\b[A-Za-z][A-Za-z0-9]*-\d+\b|#\d+)`)

var (
	mergeVerb  = regexp.MustCompile(`(?i)\b(?:merge|gộp)\b`)
	ghPRMerge  = regexp.MustCompile(`(?i)\bgh pr merge\b`)
	intoClause = regexp.MustCompile(`(?i)\binto\s+(?:the\s+)?(\S+)(\s+branch\b)?`)
)

// branchEdge holds the quotes, brackets and sentence punctuation around a target.
const branchEdge = "`'\"()[].,;:!?"

const agentOwnedMessage = "is an agent-owned step; do it yourself and ask a human only for spend, production deploy or release, public publication, an irreversible action or an open product choice"

// mergeClauses splits text at each merge verb. A clause ends at the next merge
// verb, a ";" or a period followed by whitespace or the end. "gh pr merge" is a
// command, not a verb, so it is blanked first.
func mergeClauses(text string) []string {
	text = ghPRMerge.ReplaceAllString(text, " ")
	starts := mergeVerb.FindAllStringIndex(text, -1)
	clauses := make([]string, 0, len(starts))
	for i, start := range starts {
		end := len(text)
		if i+1 < len(starts) {
			end = starts[i+1][0]
		}
		clauses = append(clauses, text[start[0]:clauseEnd(text, start[0], end)])
	}
	return clauses
}

func clauseEnd(text string, from, to int) int {
	for i := from; i < to; i++ {
		if text[i] == ';' || text[i] == '.' && (i+1 == len(text) || unicode.IsSpace(rune(text[i+1]))) {
			return i
		}
	}
	return to
}

// agentOwned reports whether text hands a human an agent-owned action. Every
// merge clause needs exactly one "into the default branch": a missing target, a
// second merge after an allowed first one or a second "into" in the clause is refused.
func agentOwned(text string) bool {
	if agentOwnedStep.MatchString(text) {
		return true
	}
	for _, clause := range mergeClauses(text) {
		intos := intoClause.FindAllStringSubmatch(clause, -1)
		if len(intos) != 1 {
			return true
		}
		into := intos[0]
		if into[2] == "" || !strings.EqualFold(strings.Trim(into[1], branchEdge), "default") {
			return true
		}
	}
	return false
}
