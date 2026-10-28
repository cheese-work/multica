package decisiontx

import (
	"regexp"
	"slices"
	"strings"
)

// maxExcerpts bounds the comment lookups one ask can cause.
const maxExcerpts = 5

const excerptRunes = 100

var (
	// commentLink matches a Markdown link to a comment in any path form.
	commentLink = regexp.MustCompile(`\[[^\]\n]*\]\([^)\s]*#comment-([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})\)`)
	// Excerpts hold no newline and no closing quote, so identity can strip them.
	askedByLine       = regexp.MustCompile(`\*\*Asked by\*\* [^\n]*\n\n`)
	excerptSuffix     = regexp.MustCompile(` \(“[^”\n]*”\)`)
	mentionOrLink     = regexp.MustCompile(`\[([^\]]*)\]\([^)]*\)`)
	leadingMarkers    = regexp.MustCompile(`^[#>\-\s]+`)
	markdownPunctuate = strings.NewReplacer("*", "", "`", "", "~", "", "[", "", "]", "", "“", `"`, "”", `"`)
)

// WithoutEnrichment drops what the server adds after the caller wrote the
// ask (source tail, asking agent, comment excerpts), so a re-ask compares equal.
func WithoutEnrichment(content string) string {
	return excerptSuffix.ReplaceAllString(askedByLine.ReplaceAllString(WithoutSourceTail(content), ""), "")
}

// Excerpt reduces a comment to its first nonblank line as plain text, at most
// excerptRunes characters plus an ellipsis. It returns "" when nothing is left.
func Excerpt(content string) string {
	for _, line := range strings.Split(content, "\n") {
		line = mentionOrLink.ReplaceAllString(line, "$1")
		line = strings.Join(strings.Fields(markdownPunctuate.Replace(leadingMarkers.ReplaceAllString(strings.TrimSpace(line), ""))), " ")
		if line == "" {
			continue
		}
		if runes := []rune(line); len(runes) > excerptRunes {
			return string(runes[:excerptRunes]) + "…"
		}
		return line
	}
	return ""
}

// WithExcerpts returns a copy of the ask with ` (“excerpt”)` after each of the
// first maxExcerpts comment links in the visible prose fields. load returns the
// comment text, or "" for a comment that must show no excerpt.
func (ask Ask) WithExcerpts(load func(commentID string) (string, error)) (Ask, error) {
	remaining := maxExcerpts
	var failure error
	add := func(text string) string {
		return commentLink.ReplaceAllStringFunc(text, func(link string) string {
			if remaining == 0 || failure != nil {
				return link
			}
			remaining--
			content, err := load(commentLink.FindStringSubmatch(link)[1])
			if err != nil {
				failure = err
				return link
			}
			if excerpt := Excerpt(content); excerpt != "" {
				return link + " (“" + excerpt + "”)"
			}
			return link
		})
	}
	ask.ContextELI10, ask.Decision, ask.StakesIfWrong = add(ask.ContextELI10), add(ask.Decision), add(ask.StakesIfWrong)
	ask.Recommendation, ask.DefaultIfNoReply = add(ask.Recommendation), add(ask.DefaultIfNoReply)
	ask.Options = slices.Clone(ask.Options)
	for index, option := range ask.Options {
		option.Label, option.WhatHappens, option.CostOrTime = add(option.Label), add(option.WhatHappens), add(option.CostOrTime)
		option.Pros, option.Cons = mapStrings(option.Pros, add), mapStrings(option.Cons, add)
		ask.Options[index] = option
	}
	return ask, failure
}

func mapStrings(items []string, fn func(string) string) []string {
	out := make([]string, len(items))
	for index, item := range items {
		out[index] = fn(item)
	}
	return out
}
