package ciwatch

import (
	"regexp"
	"strings"
)

var (
	// A credential-like key (bare, single- or double-quoted, or escaped inside a
	// string) and its ":" or "=". Keys must END in the credential word, so
	// max_tokens and password_policy stay readable.
	credKey = regexp.MustCompile(`(?i)\b[a-z0-9_.-]*(?:password|passwd|secret|secret[_-]?key|access[_-]?key|api[_-]?key|private[_-]?key|credentials?|token)\\?["']?\s*[:=]\s*`)
	// A bare value stops at whitespace or , } ] &.
	bareValue = regexp.MustCompile(`(?i)^(?:bearer\s+)?[^\s,}\]&]+`)
	// A YAML block scalar header ("|", ">", "|-", ">+", "|2"): the value is the
	// indented lines that follow, not the header.
	blockIndicator = regexp.MustCompile(`^[|>](?:[+-][1-9]?|[1-9][+-]?)?(?:\s|$)`)
	// An Actions log line starts with a timestamp that is not part of the content.
	logTimestamp = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?Z `)
	// Any word that can start a credential key. An oversized line is never
	// parsed, so one of these inside it means a value boundary we cannot see.
	credWord = regexp.MustCompile(`(?i)password|passwd|secret|credential|token|api[_-]?key|access[_-]?key|private[_-]?key`)
)

// credWordCarry is longer than any credWord match, so a word split across two
// fragments of an oversized line is still seen.
const credWordCarry = 16

// lineIndent is the count of leading blanks after an optional log timestamp.
func lineIndent(s string) (indent int, blank bool) {
	s = logTimestamp.ReplaceAllString(s, "")
	rest := strings.TrimLeft(s, " \t")
	return len(s) - len(rest), strings.TrimSpace(rest) == ""
}

// credMasker masks the value after every credential key. It sees every line,
// so a value that nests ({ [), holds quoted braces or sits on a later line is
// masked whole. Where a value boundary is uncertain the mask fails closed: an
// unterminated bracket or quote masks to EOF, a block scalar masks to its
// dedent, and a line we could not parse masks to EOF.
type credMasker struct {
	depth     int  // open { and [ of the value being masked
	quote     byte // open string quote inside the value: ' " or 'E' for an escaped \" string
	esc       bool
	pending   bool // a key ended its line: mask the next non-empty line
	block     bool // a YAML block scalar is open: mask lines indented deeper than keyIndent
	keyIndent int  // indent of the line that opened the block
	eof       bool // value boundary unknown: mask everything that follows
}

// consume masks from s[i:] until the value ends and returns the index past it.
// A bracket or quoted value may outlive its line, so its state stays in m.
func (m *credMasker) consume(s string, i int) int {
	for ; i < len(s); i++ {
		c := s[i]
		switch {
		case m.esc:
			m.esc = false
		case m.quote == 'E':
			if c == '\\' && i+1 < len(s) && s[i+1] == '"' {
				i++
				m.quote = 0
				if m.depth == 0 {
					return i + 1
				}
			}
		case m.quote != 0:
			if c == '\\' {
				m.esc = true
			} else if c == m.quote {
				m.quote = 0
				if m.depth == 0 {
					return i + 1
				}
			}
		case c == '"' || c == '\'':
			m.quote = c
		case c == '\\' && i+1 < len(s) && s[i+1] == '"':
			i++
			m.quote = 'E'
		case c == '{' || c == '[':
			m.depth++
		case c == '}' || c == ']':
			if m.depth--; m.depth <= 0 {
				m.depth = 0
				return i + 1
			}
		}
	}
	return len(s)
}

func (m *credMasker) active() bool { return m.depth > 0 || m.quote != 0 }

// omitted records a line too long to parse. If it names a credential, or a
// key before it waits for its value, the value boundary is unknown.
func (m *credMasker) omitted(sawCredWord bool) {
	if sawCredWord || m.pending {
		m.eof, m.pending = true, false
	}
}

// line returns s with every credential value replaced by a placeholder.
func (m *credMasker) line(s string) string {
	if m.eof {
		return tokenPlaceholer
	}
	indent, blank := lineIndent(s)
	if m.block {
		if !blank && indent <= m.keyIndent {
			m.block = false
		} else {
			return tokenPlaceholer
		}
	}
	var out strings.Builder
	i := 0
	for i < len(s) {
		if m.active() {
			i = m.consume(s, i)
			out.WriteString(tokenPlaceholer)
			continue
		}
		if m.pending {
			rest := strings.TrimLeft(s[i:], " \t")
			if rest == "" {
				break // blank line: still waiting for the value
			}
			m.pending = false
			out.WriteString(s[i : len(s)-len(rest)])
			i = len(s) - len(rest)
			if blockIndicator.MatchString(s[i:]) {
				m.block = true
				i = len(s)
			} else if strings.ContainsRune(`{["'\`, rune(s[i])) {
				i = m.consume(s, i)
			} else {
				i = len(s)
			}
			out.WriteString(tokenPlaceholer)
			continue
		}
		loc := credKey.FindStringIndex(s[i:])
		if loc == nil {
			break
		}
		end := i + loc[1]
		out.WriteString(s[i:end])
		i = end
		m.keyIndent = indent
		if i == len(s) {
			m.pending = true
			break
		}
		if blockIndicator.MatchString(s[i:]) {
			m.block = true
			i = len(s)
		} else if v := bareValue.FindString(s[i:]); strings.ContainsRune(`{["'\`, rune(s[i])) {
			i = m.consume(s, i)
		} else if v != "" {
			i += len(v)
		} else {
			continue
		}
		out.WriteString(tokenPlaceholer)
	}
	out.WriteString(s[min(i, len(s)):])
	return out.String()
}
