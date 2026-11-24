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

<<<<<<< b441bc8a2d6331808b11e83175a1716c052bd1d2
// Derive is the deterministic DM title: first non-empty line, Markdown and
// whitespace cleaned, at most 80 Unicode code points, cut at a word boundary
// when that keeps at least 2/3 of the limit (else a hard cut). It never adds an ellipsis; the UI truncates at the container
// edge. Twin: deriveChatTitle in packages/core/chat/title.ts.
||||||| 2ae2f08005
// Derive mirrors the existing first-party deterministic title policy for
// channel-created Chats. Go slices valid Unicode code points rather than UTF-16
// code units, while preserving the same first-line, Markdown, whitespace, and
// 30-character single-ellipsis behavior.
=======
// Derive is the single deterministic title policy for a Chat's first message,
// shared by first-party clients (which create sessions untitled) and channels:
// first non-empty line that is not only mentions (or the first non-empty line
// when every line is), Markdown stripped, whitespace collapsed, capped at 30
// code points with a single ellipsis.
>>>>>>> 2fcb424b72b024f65161809081009063846ea44d
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
