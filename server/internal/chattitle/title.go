package chattitle

import (
	"regexp"
	"strings"
	"unicode"
)

const (
	deterministicTitleLimit = 80
	// minWordCut: a word boundary is used only when it keeps at least 2/3 of
	// the limit; otherwise a long token (a URL) would be dropped whole.
	minWordCut = deterministicTitleLimit * 2 / 3
)

var (
	markdownFence = regexp.MustCompile("```.*?```")
	markdownMarks = regexp.MustCompile("[#*`>~_]")
	markdownLink  = regexp.MustCompile(`!?\[([^\]]*)\]\([^)]*\)`)
	titleSpace    = regexp.MustCompile(`[[:space:]]+`)
	// A line of nothing but mention links, such as the page a message was
	// sent from, which clients put ahead of what the user wrote.
	mentionsOnly = regexp.MustCompile(`^(?:\[(?:\\.|[^\]\\])*\]\(mention://[^)]*\)\s*)+$`)
)

// Derive is the single deterministic title policy for a Chat's first message,
// shared by first-party clients (which create sessions untitled) and channels:
// first non-empty line that is not only mentions (or the first non-empty line
// when every line is), Markdown and whitespace cleaned, at most 80 Unicode code
// points, cut at a word boundary when that keeps at least 2/3 of the limit
// (else a hard cut). It never adds an ellipsis; the UI truncates at the
// container edge. Twin: deriveChatTitle in packages/core/chat/title.ts.
func Derive(body string) string {
	line := ""
	for _, candidate := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(candidate)
		if trimmed == "" {
			continue
		}
		if line == "" {
			line = candidate
		}
		if !mentionsOnly.MatchString(trimmed) {
			line = candidate
			break
		}
	}
	line = markdownFence.ReplaceAllString(line, " ")
	line = markdownMarks.ReplaceAllString(line, "")
	line = markdownLink.ReplaceAllString(line, "$1")
	line = strings.TrimSpace(titleSpace.ReplaceAllString(line, " "))
	if line == "" {
		return ""
	}
	return Cut(line)
}

// Cut limits an already cleaned single-line title to 80 code points at a word
// boundary when that keeps at least 2/3 of the limit (else a hard cut), without
// an ellipsis.
func Cut(line string) string {
	runes := []rune(line)
	if len(runes) <= deterministicTitleLimit {
		return line
	}
	cut := runes[:deterministicTitleLimit]
	// Whitespace right after the cut means the last word is whole.
	if !unicode.IsSpace(runes[deterministicTitleLimit]) {
		if i := lastSpace(cut); i >= minWordCut {
			cut = cut[:i]
		}
	}
	return strings.TrimRightFunc(string(cut), unicode.IsSpace)
}

func lastSpace(r []rune) int {
	for i := len(r) - 1; i >= 0; i-- {
		if unicode.IsSpace(r[i]) {
			return i
		}
	}
	return -1
}
