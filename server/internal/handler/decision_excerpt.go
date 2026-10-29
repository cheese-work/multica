package handler

import (
	"net/http"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

const maxDecisionExcerpts = 10

// collapseSpaces folds every whitespace run into one space and trims the ends.
// from and to map each byte of the result back to the byte range of the source
// rune that produced it, so a match can be quoted as the source's own text.
func collapseSpaces(source string) (collapsed string, from, to []int) {
	var builder strings.Builder
	pendingSpace := false
	for index, char := range source {
		size := utf8.RuneLen(char)
		if size < 0 {
			size = 1
		}
		if unicode.IsSpace(char) {
			pendingSpace = builder.Len() > 0
			continue
		}
		if pendingSpace {
			builder.WriteByte(' ')
			from, to = append(from, index), append(to, index)
			pendingSpace = false
		}
		for range size {
			from, to = append(from, index), append(to, index+size)
		}
		builder.WriteRune(char)
	}
	return builder.String(), from, to
}

// verbatimExcerpts returns, for each excerpt, the source's own text that it
// matches once whitespace is normalised on both sides. An excerpt the source
// does not hold, or that is blank, is refused: the record must never carry
// words the human did not write.
func verbatimExcerpts(source string, excerpts []string) (spans []string, firstStart int, firstUnique bool, err error) {
	if len(excerpts) > maxDecisionExcerpts {
		return nil, 0, false, decisionIntentRefusal{http.StatusBadRequest, "too many excerpts"}
	}
	collapsedSource, from, to := collapseSpaces(source)
	for position, excerpt := range excerpts {
		wanted := strings.Join(strings.Fields(excerpt), " ")
		at := strings.Index(collapsedSource, wanted)
		if wanted == "" || at < 0 {
			return nil, 0, false, decisionIntentRefusal{http.StatusBadRequest, "each excerpt must be a verbatim part of the source message"}
		}
		start, end := from[at], to[at+len(wanted)-1]
		spans = append(spans, source[start:end])
		if position == 0 {
			firstStart, firstUnique = start, strings.Count(collapsedSource, wanted) == 1
		}
	}
	return spans, firstStart, firstUnique, nil
}

// quoteExcerpts renders each excerpt as its own block quote paragraph.
func quoteExcerpts(spans []string) string {
	quoted := make([]string, len(spans))
	for index, span := range spans {
		quoted[index] = blockQuote(span)
	}
	return strings.Join(quoted, "\n>\n")
}

var optionLabelPattern = regexp.MustCompile(`(?m)^[>\s*_-]*([A-Z]{1,2}[0-9]{1,2})\b`)

// offeredLabel names the option the excerpt answers: the last label (such as
// D1) that starts a line of the source up to the excerpt, provided the earlier
// assistant message offered that label. It is empty when nothing matches.
func offeredLabel(offered, source string, excerptStart int) string {
	known := map[string]bool{}
	for _, match := range optionLabelPattern.FindAllStringSubmatch(offered, -1) {
		known[match[1]] = true
	}
	region := source
	if end := strings.IndexByte(source[excerptStart:], '\n'); end >= 0 {
		region = source[:excerptStart+end]
	}
	label := ""
	for _, match := range optionLabelPattern.FindAllStringSubmatch(region, -1) {
		if known[match[1]] {
			label = match[1]
		}
	}
	return label
}
