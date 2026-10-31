package decisiontx

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
)

// Blocking readability rules (CHE-2181 D1c). They are pattern checks, not
// meaning checks: a free-prose multi-question ask can pass, and a false
// refusal costs the agent one rewrite.
var (
	// manyAnswers: "answer 1 to 5", "answer 1-5", "answer per number/each/all", "decisions 5, 6, 7, 9".
	manyAnswers = regexp.MustCompile(`(?i)\banswer\s+(?:\d+\s*(?:to|-|–)\s*\d+|per number|each|all)\b|\bdecisions\s+\d+\s*(?:,|-|–|and)\s*\d+`)
	numberedAsk = regexp.MustCompile(`(?i)\bnumbered answers\b|\banswer per number\b`)
	// inlineList: one line holding enumerators 1 and 2 (`1.` or `1)`, then a space).
	inlineList  = regexp.MustCompile(`(?:^|[\s(])1[.)]\s.*\s2[.)]\s`)
	anyLink     = regexp.MustCompile(`\[[^\]]*\]\([^)]*\)|https?://\S+`)
	uuidPattern = regexp.MustCompile(`(?i)\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b`)
	hexPattern  = regexp.MustCompile(`(?i)\b[0-9a-f]{7,}\b`)
)

const minAgentDidWords = 4

type proseField struct{ path, text string }

// proseFields lists every visible prose field with its indexed AskError path.
// agent_did is excluded: it sits in the collapsed Details block (rule 4).
func (ask Ask) proseFields() []proseField {
	if ask.isAction() {
		fields := []proseField{
			{"ask.context_eli10", ask.ContextELI10}, {"ask.why_human", ask.WhyHuman},
			{"ask.done_when", ask.DoneWhen}, {"ask.default_if_no_reply", ask.DefaultIfNoReply},
		}
		for index, step := range ask.Steps {
			fields = append(fields, proseField{fmt.Sprintf("ask.steps[%d]", index), step})
		}
		return fields
	}
	fields := []proseField{
		{"ask.context_eli10", ask.ContextELI10}, {"ask.decision", ask.Decision}, {"ask.stakes_if_wrong", ask.StakesIfWrong},
		{"ask.recommendation", ask.Recommendation}, {"ask.default_if_no_reply", ask.DefaultIfNoReply},
	}
	for index, option := range ask.Options {
		prefix := fmt.Sprintf("ask.options[%d]", index)
		fields = append(fields, proseField{prefix + ".label", option.Label}, proseField{prefix + ".what_happens", option.WhatHappens}, proseField{prefix + ".cost_or_time", option.CostOrTime})
		for j, pro := range option.Pros {
			fields = append(fields, proseField{fmt.Sprintf("%s.pros[%d]", prefix, j), pro})
		}
		for j, con := range option.Cons {
			fields = append(fields, proseField{fmt.Sprintf("%s.cons[%d]", prefix, j), con})
		}
	}
	return fields
}

func (ask Ask) validateReadable() error {
	if asksSeveral(ask) {
		return &AskError{"ask.decision", "asks more than one question; split into one request per question"}
	}
	fields := ask.proseFields()
	for _, field := range fields {
		for _, line := range strings.Split(field.text, "\n") {
			if inlineList.MatchString(line) {
				return &AskError{field.path, "has an inline numbered list; put each item on its own line as a Markdown list"}
			}
		}
	}
	for _, field := range fields {
		// Only a Markdown link destination is exempt; link text and bare URLs are not.
		if uuidPattern.MatchString(maskLinkDestinations(field.text)) {
			return &AskError{field.path, "has a bare ID; link it as [what it is](link)"}
		}
	}
	if plainWords(ask.AgentDid) < minAgentDidWords {
		return &AskError{"ask.agent_did", "must say in at least one plain sentence what the agent did; an ID alone is not enough"}
	}
	return nil
}

// maskLinkDestinations drops the (destination) of each real Markdown link,
// [label](destination), and keeps all other text. A code span is literal text,
// so its content is kept; a "](" with no opening "[", or one opened by an
// escaped "\[", is not a link either.
func maskLinkDestinations(text string) string {
	var out strings.Builder
	for i := 0; i < len(text); {
		switch text[i] {
		case '`':
			run := len(text[i:]) - len(strings.TrimLeft(text[i:], "`"))
			end := strings.Index(text[i+run:], text[i:i+run])
			if end < 0 {
				end = -run // no closing run: the backticks are literal
			}
			out.WriteString(text[i : i+run+end+run])
			i += run + end + run
		case '\\': // an escaped character is literal, so "\[" never opens a link
			end := min(i+2, len(text))
			out.WriteString(text[i:end])
			i = end
		case '[':
			if label := closingIndex(text, i, '[', ']'); label > 0 && label+1 < len(text) && text[label+1] == '(' {
				if dest := closingIndex(text, label+1, '(', ')'); dest > 0 {
					out.WriteString(text[i : label+1])
					i = dest + 1
					continue
				}
			}
			out.WriteByte('[')
			i++
		default:
			out.WriteByte(text[i])
			i++
		}
	}
	return out.String()
}

// closingIndex returns the index of the delimiter that closes the one at text[start],
// counting nested pairs and skipping backslash-escaped characters, or -1.
func closingIndex(text string, start int, open, closer byte) int {
	depth := 0
	for i := start; i < len(text); i++ {
		switch text[i] {
		case '\\':
			i++
		case open:
			depth++
		case closer:
			if depth--; depth == 0 {
				return i
			}
		}
	}
	return -1
}

func asksSeveral(ask Ask) bool {
	if manyAnswers.MatchString(ask.Decision) || strings.Count(ask.Decision, "?") >= 2 {
		return true
	}
	for _, option := range ask.Options {
		if numberedAsk.MatchString(option.Label) || numberedAsk.MatchString(option.WhatHappens) {
			return true
		}
	}
	return false
}

// plainWords counts words with letters once links, UUIDs and long hex values are removed.
func plainWords(text string) int {
	text = hexPattern.ReplaceAllString(uuidPattern.ReplaceAllString(anyLink.ReplaceAllString(text, " "), " "), " ")
	count := 0
	for _, word := range strings.Fields(text) {
		if strings.IndexFunc(word, unicode.IsLetter) >= 0 {
			count++
		}
	}
	return count
}
