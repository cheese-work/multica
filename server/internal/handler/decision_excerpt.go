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
// matches once whitespace is normalised on both sides. Each excerpt must lie
// inside one source message: words that only appear across a join of two
// messages were never written. An excerpt no message holds, or that is blank,
// is refused: the record must never carry words the human did not write.
// first describes where the first excerpt matched, for naming its option.
func verbatimExcerpts(sources []string, excerpts []string) (spans []string, first excerptMatch, err error) {
	if len(excerpts) > maxDecisionExcerpts {
		return nil, first, decisionIntentRefusal{http.StatusBadRequest, "too many excerpts"}
	}
	for position, excerpt := range excerpts {
		wanted := strings.Join(strings.Fields(excerpt), " ")
		found := false
		for _, source := range sources {
			collapsedSource, from, to := collapseSpaces(source)
			at := strings.Index(collapsedSource, wanted)
			if wanted == "" || at < 0 {
				continue
			}
			start, end := from[at], to[at+len(wanted)-1]
			spans = append(spans, source[start:end])
			if position == 0 {
				first = excerptMatch{source: source, start: start, unique: strings.Count(collapsedSource, wanted) == 1}
			}
			found = true
			break
		}
		if !found {
			return nil, first, decisionIntentRefusal{http.StatusBadRequest, "each excerpt must be a verbatim part of one source message"}
		}
	}
	return spans, first, nil
}

// excerptMatch locates the first excerpt in the one message that holds it.
type excerptMatch struct {
	source string
	start  int
	unique bool
}

// sameExcerpts reports whether a retry names the excerpts the stored answer
// quoted: the stored reply text is those excerpts joined, so it must equal the
// retry's excerpts after whitespace is normalised.
func sameExcerpts(excerpted bool, storedReply string, excerpts []string) bool {
	if excerpted != (len(excerpts) > 0) {
		return false
	}
	return !excerpted || strings.Join(strings.Fields(storedReply), " ") == strings.Join(strings.Fields(strings.Join(excerpts, " ")), " ")
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
