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
)

// credMasker masks the value after every credential key. It sees every line,
// so a value that nests ({ [), holds quoted braces or sits on a later line is
// masked whole. An unterminated bracket value masks to EOF (fail closed).
type credMasker struct {
	depth   int  // open { and [ of the value being masked
	quote   byte // open string quote inside the value: ' " or 'E' for an escaped \" string
	esc     bool
	pending bool // a key ended its line: mask the next non-empty line
}

// consume masks from s[i:] until the value ends and returns the index past it.
// A string value never outlives its line; a bracket value does.
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
	if m.depth == 0 {
		m.quote, m.esc = 0, false
	}
	return len(s)
}

func (m *credMasker) active() bool { return m.depth > 0 }

// line returns s with every credential value replaced by a placeholder.
func (m *credMasker) line(s string) string {
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
			if strings.ContainsRune(`{["'\`, rune(s[i])) {
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
		if i == len(s) {
			m.pending = true
			break
		}
		if v := bareValue.FindString(s[i:]); strings.ContainsRune(`{["'\`, rune(s[i])) {
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
