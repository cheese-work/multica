// Package humanask is the Go port of the send-time rules of human_ask.py
// (cheese-work/multica-dotfiles, skills/multica-cheese-work-ste-writing/scripts).
// A comment that asks a human to act must name a real member mention link and
// open with context. Slice 1 ports the trigger and the two refuse rules; the
// Python file stays the source of truth, and testdata/cases.json is the shared
// drift check.
package humanask

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode"
)

// Problem is one failed rule at a 1-based line of the comment text.
type Problem struct {
	Line    int
	Message string
}

// Roster of human members, from `multica workspace member list --output json`,
// CHE-2596, 30 October 2026: the sole human member is cheese (mention target: user_id).
const cheeseTarget = "mention://member/f0c4c4cf-b6cd-4939-bfba-a5e14290c1ff"

var members = map[string]string{"cheese": cheeseTarget}

// pyWS is Python's str `\s`: Unicode spaces plus \x1c-\x1f and \x85. RE2 `\s`
// is ASCII only, so patterns go through pyRE to keep Python parity.
const pyWS = `\t\n\v\f\r \p{Z}\x{1c}-\x{1f}\x{85}`

// notWord ends a word like Python's Unicode `\b` after a letter (RE2 `\b` is ASCII).
const notWord = `(?:$|[^\p{L}\p{N}_])`

func pyRE(pattern string) *regexp.Regexp {
	pattern = strings.ReplaceAll(pattern, `\S`, `[^`+pyWS+`]`)
	pattern = strings.ReplaceAll(pattern, `\s`, `[`+pyWS+`]`)
	return regexp.MustCompile(strings.ReplaceAll(pattern, `\d`, `\p{Nd}`))
}

var (
	// Go has no lookbehind: (?:^|[^\p{L}\p{N}_-]) stands for (?<![\w-]).
	address = pyRE(`(?i)^\s*(?:[-*]|\d+[.)]|#+)?\s*@?cheese\s*:` +
		`|(?:^|[^\p{L}\p{N}_-])for\s+cheese\s+to` + notWord +
		`|(?:^|[^\p{L}\p{N}_-])cheese\s+(?:may|can|should)` + notWord)
	memberLink = pyRE(`(?i)\[([^\]\n]+)\]\((mention://member/[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12})\)`)
	humanNone  = pyRE(`(?im)^\s*Human action:\s*none` + notWord)
	fenceOpen  = pyRE("^\\s*(`{3,}|~{3,})")
	quote      = regexp.MustCompile(`^(?:[ \t]*>)+[ \t]?`)
	labels     = [...]string{"What this is", "Why you, why now"}
)

func isPySpace(r rune) bool {
	return unicode.IsSpace(r) || unicode.Is(unicode.Z, r) || (r >= 0x1c && r <= 0x1f)
}

// splitLines splits like Python str.splitlines (\n, \r\n, \r, \v, \f, \x1c-\x1e, \x85,  ,  ).
func splitLines(text string) []string {
	var lines []string
	start := 0
	rs := []rune(text)
	for i := 0; i < len(rs); i++ {
		switch rs[i] {
		case '\n', '\r', '\v', '\f', 0x1c, 0x1d, 0x1e, 0x85, 0x2028, 0x2029:
			lines = append(lines, string(rs[start:i]))
			if rs[i] == '\r' && i+1 < len(rs) && rs[i+1] == '\n' {
				i++
			}
			start = i + 1
		}
	}
	if start < len(rs) {
		lines = append(lines, string(rs[start:]))
	}
	return lines
}

// closesFence reports whether line is only a run of the fence character at
// least as long as fence, with surrounding Python whitespace.
func closesFence(line, fence string) bool {
	run := strings.TrimFunc(line, isPySpace)
	return len(run) >= len(fence) && strings.Trim(run, fence[:1]) == ""
}

// fencedLines returns the 1-based numbers of lines inside fenced blocks, fences
// included, quoted fences too. An unclosed fence runs to the end.
func fencedLines(lines []string) map[int]bool {
	out := map[int]bool{}
	for i := 0; i < len(lines); {
		m := fenceOpen.FindStringSubmatch(quote.ReplaceAllString(lines[i], ""))
		if m == nil {
			i++
			continue
		}
		fence := m[1]
		j := i + 1
		for j < len(lines) && !closesFence(quote.ReplaceAllString(lines[j], ""), fence) {
			j++
		}
		if j > len(lines)-1 {
			j = len(lines) - 1
		}
		for k := i; k <= j; k++ {
			out[k+1] = true
		}
		i = j + 1
	}
	return out
}

type numbered struct {
	n    int
	line string
}

// visibleLines is unfenced, unquoted prose with original line numbers.
func visibleLines(lines []string) []numbered {
	fenced := fencedLines(lines)
	var out []numbered
	for i, line := range lines {
		if !fenced[i+1] && !strings.HasPrefix(strings.TrimLeftFunc(line, isPySpace), ">") {
			out = append(out, numbered{i + 1, line})
		}
	}
	return out
}

// stripCodeSpans drops `code` spans: a maximal backtick run closes at the next
// maximal run of the same length (CODE_SPAN in human_ask.py).
func stripCodeSpans(line string) string {
	type run struct{ start, end int }
	var runs []run
	for i := 0; i < len(line); {
		if line[i] != '`' {
			i++
			continue
		}
		j := i
		for j < len(line) && line[j] == '`' {
			j++
		}
		runs = append(runs, run{i, j})
		i = j
	}
	var b strings.Builder
	pos := 0
	for k := 0; k < len(runs); k++ {
		for l := k + 1; l < len(runs); l++ {
			if runs[l].end-runs[l].start == runs[k].end-runs[k].start {
				b.WriteString(line[pos:runs[k].start])
				pos = runs[l].end
				k = l
				break
			}
		}
	}
	b.WriteString(line[pos:])
	return b.String()
}

// stripValidMemberLink removes a link whose label and target are consistent;
// a link naming a known member with the wrong target (or a known target with
// the wrong label) stays and counts as unmatched.
func stripValidMemberLink(match string) string {
	parts := memberLink.FindStringSubmatch(match)
	name := strings.ToLower(strings.TrimPrefix(strings.TrimFunc(parts[1], isPySpace), "@"))
	target := strings.ToLower(parts[2])
	expected, known := members[name]
	targetKnown := false
	for _, t := range members {
		targetKnown = targetKnown || t == target
	}
	if (known || targetKnown) && (!known || target != expected) {
		return match
	}
	return ""
}

func missingMentions(lines []string) []Problem {
	var out []Problem
	for _, v := range visibleLines(lines) {
		prose := memberLink.ReplaceAllStringFunc(stripCodeSpans(v.line), stripValidMemberLink)
		prose = strings.ReplaceAll(prose, "**", "")
		if address.MatchString(prose) || strings.Contains(prose, "mention://member/") {
			out = append(out, Problem{v.n, "human addressee needs a real member mention link"})
		}
	}
	return out
}

func humanAction(text string, lines []string, missing []Problem) bool {
	if humanNone.MatchString(text) {
		return false
	}
	if len(missing) > 0 {
		return true
	}
	for _, v := range visibleLines(lines) {
		if memberLink.MatchString(stripCodeSpans(v.line)) {
			return true
		}
	}
	return false
}

// Check returns the refuse-level problems for a comment that asks a human to
// act, or nil when the comment asks no one or passes.
func Check(text string) []Problem {
	lines := splitLines(text)
	problems := missingMentions(lines)
	if !humanAction(text, lines, problems) {
		return nil
	}
	var opening []numbered
	for _, v := range visibleLines(lines) {
		if strings.TrimFunc(v.line, isPySpace) != "" {
			opening = append(opening, v)
		}
	}
	for i, label := range labels {
		n, line := 1, ""
		if len(opening) > i {
			n, line = opening[i].n, opening[i].line
		}
		ok := pyRE(`(?i)^\s*` + regexp.QuoteMeta(label) + `\s*:\s*\S`)
		if !ok.MatchString(strings.ReplaceAll(line, "**", "")) {
			problems = append(problems, Problem{n, fmt.Sprintf("opening line %d needs %s: with context on the same line", i+1, label)})
		}
	}
	sort.SliceStable(problems, func(a, b int) bool {
		if problems[a].Line != problems[b].Line {
			return problems[a].Line < problems[b].Line
		}
		return problems[a].Message < problems[b].Message
	})
	return problems
}
