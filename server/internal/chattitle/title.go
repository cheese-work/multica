package chattitle

import (
	"regexp"
	"strings"
	"unicode"
)

// FallbackMaxLen is the rune cut (ellipsis included) of the deterministic
// fallback title. It matches the client CHAT_TITLE_MAX.
const FallbackMaxLen = 60

var (
	markdownFence = regexp.MustCompile("```.*?```")
	markdownMarks = regexp.MustCompile("[#*`>~_]")
	markdownLink  = regexp.MustCompile(`!?\[([^\]]*)\]\([^)]*\)`)
	titleSpace    = regexp.MustCompile(`[[:space:]]+`)
)

// Derive mirrors the existing first-party deterministic title policy for
// channel-created Chats. Go slices valid Unicode code points rather than UTF-16
// code units, while preserving the same first-line, Markdown, whitespace, and
// single-ellipsis behavior at FallbackMaxLen characters.
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
	runes := []rune(line)
	if len(runes) <= FallbackMaxLen {
		return line
	}
	return strings.TrimRightFunc(string(runes[:FallbackMaxLen-1]), unicode.IsSpace) + "…"
}
