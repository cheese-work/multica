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
)

// Derive is the deterministic DM title: first non-empty line, Markdown and
// whitespace cleaned, at most 80 Unicode code points, cut at a word boundary
// when that keeps at least 2/3 of the limit (else a hard cut). It never adds an ellipsis; the UI truncates at the container
// edge. Twin: deriveChatTitle in packages/core/chat/title.ts.
func Derive(body string) string {
	line := ""
	for _, candidate := range strings.Split(body, "\n") {
		if strings.TrimSpace(candidate) != "" {
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
