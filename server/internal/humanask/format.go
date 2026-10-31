package humanask

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Format rules of human_ask.py (slice 2): warn-only in the CLI. RE2 has no
// lookaround and an ASCII `\b`, so a Python `\b` becomes b0/b1 below (consuming,
// fine for yes/no matches) or a rune check around a bare core pattern (bounded*).
const (
	b0 = `(?:^|[^\p{L}\p{N}_])`
	b1 = `(?:$|[^\p{L}\p{N}_])`
	// gap is Python `[^.;\n]{0,40}?` between two `\b` words: 1 to 40 chars, the
	// first and last of them non-word (a word char there would join the words).
	gap = `[^\p{L}\p{N}_.;\n](?:[^.;\n]{0,38}[^\p{L}\p{N}_.;\n])?`
)

var (
	labelOrder = [...]string{"Steps", "Checked", "Done when"}
	labelRE    = map[string]*regexp.Regexp{
		"Steps":     pyRE(`(?i)^\s*(?:#+\s*)?(?:\*\*)?\s*(Steps|Bước)`),
		"Checked":   pyRE(`(?i)^\s*(?:#+\s*)?(?:\*\*)?\s*(Checked|Đã kiểm tra)`),
		"Done when": pyRE(`(?i)^\s*(?:#+\s*)?(?:\*\*)?\s*(Done when|Xong khi)`),
	}
	htmlTags    = map[string]bool{"details": true, "summary": true, "br": true, "kbd": true, "sub": true, "sup": true, "p": true, "b": true, "i": true, "code": true, "pre": true}
	sourceRE    = pyRE(`(?i)\((?:source|nguồn):\s*[^()` + pyWS + `][^()]*\)`)
	implicitRE  = regexp.MustCompile(`(?i)similarly|likewise|same as|tương tự|etc|v\.v\.`)
	uuidRE      = regexp.MustCompile(`(?i)[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)
	placeholder = regexp.MustCompile(`<(/?)([A-Za-z][^<>\n]*)>`)
	numberedRE  = pyRE(`^\s*\d+[.)]\s`)
	bulletRE    = pyRE(`^\s*[-*]\s`)
	mergeRE     = regexp.MustCompile(`(?i)merge|gộp`)
	mentionTail = regexp.MustCompile(`\]\(mention://[^)]*\)`)
	notVerified = pyRE(`(?i)(not verified|chưa (?:xác minh|kiểm tra))\s*:\s*\S`)

	notDraft     = regexp.MustCompile(`(?i)not draft`)
	readyDraft   = regexp.MustCompile(`(?i)(?:marked )?ready from draft`)
	noUnready    = regexp.MustCompile(`(?i)(?:no|0|zero) (?:pending|running|queued|failed|failing|red) checks?`)
	hasReady     = pyRE(`(?i)` + b0 + `ready` + b1 + `|sẵn sàng`)
	reviewPass   = pyRE(`(?i)` + b0 + `(?:independent|different-lab|cross-lab)\s+review\s*:?\s+(?:PASS(?:ed)?|green)` + b1)
	ciGreen      = pyRE(`(?i)` + b0 + `(?:CI|checks?)` + gap + `(?:green|passed|success(?:ful)?|xanh)` + b1 + `|` + b0 + `(?:green|passed|xanh)` + gap + `(?:CI|checks?)` + b1)
	unreadyState = `(?:pending|running|queued|red|fail(?:ed|ing|ure)?|not (?:observed|verified|green|passed)|đỏ)`
	unready      = pyRE(`(?i)` + b0 + `(?:draft|not ready)` + b1 +
		`|` + b0 + `(?:CI|checks?|review|[\p{L}\p{N}_]+(?:-[\p{L}\p{N}_]+)+)\s*:?\s*(?:(?:are|is|still)\s+)*` + unreadyState + b1 +
		`|` + b0 + `(?:pending|running|queued|failed|failing|red)\s+checks?` + b1 +
		`|(?:not verified|not observed|chưa (?:xác minh|kiểm tra))\s*:(?:[^\n]*[^\p{L}\p{N}_\n])?(?:CI|checks?)` + b1)
)

func isWordRune(r rune) bool { return unicode.IsLetter(r) || unicode.IsNumber(r) || r == '_' }

func wordBefore(s string, i int) bool {
	r, _ := utf8.DecodeLastRuneInString(s[:i])
	return i > 0 && isWordRune(r)
}

func wordAfter(s string, i int) bool {
	r, _ := utf8.DecodeRuneInString(s[i:])
	return i < len(s) && isWordRune(r)
}

// findBounded is re.finditer(r"\b(?:core)\b"): matches of the bare core that
// start and end on a word boundary; a failed candidate retries one rune later.
// accept may veto or extend a candidate (returns the match end, or -1).
func findBounded(re *regexp.Regexp, s string, accept func(match string, end int) int) [][2]int {
	var out [][2]int
	for pos := 0; pos < len(s); {
		loc := re.FindStringIndex(s[pos:])
		if loc == nil {
			break
		}
		a, b := pos+loc[0], pos+loc[1]
		end := b
		if accept != nil {
			end = accept(s[a:b], b)
		} else if wordBefore(s, a) || wordAfter(s, b) {
			end = -1
		}
		if end < 0 {
			_, w := utf8.DecodeRuneInString(s[a:])
			pos = a + w
			continue
		}
		out = append(out, [2]int{a, end})
		pos = end
	}
	return out
}

func replaceBounded(re *regexp.Regexp, s, repl string) string {
	var b strings.Builder
	last := 0
	for _, m := range findBounded(re, s, nil) {
		b.WriteString(s[last:m[0]])
		b.WriteString(repl)
		last = m[1]
	}
	return b.String() + s[last:]
}

// sections maps a label to its lines, from the label line to the next label.
func sections(lines []string) map[string][]numbered {
	out := map[string][]numbered{}
	cur := ""
	for i, line := range lines {
		label, end := "", 0
		for _, name := range labelOrder {
			if m := labelRE[name].FindStringSubmatchIndex(line); m != nil && !wordAfter(line, m[3]) {
				label, end = name, m[3]
				break
			}
		}
		switch {
		case label != "":
			cur = label
			out[cur] = nil
			rest := strings.TrimLeft(line[end:], "* \t:")
			if strings.Trim(rest, " *") != "" {
				out[cur] = append(out[cur], numbered{i + 1, rest})
			}
		case cur != "":
			out[cur] = append(out[cur], numbered{i + 1, line})
		}
	}
	return out
}

func anyLine(lines []numbered, pred func(string) bool) bool {
	for _, l := range lines {
		if pred(l.line) {
			return true
		}
	}
	return false
}

func blank(s string) bool { return strings.TrimFunc(s, isPySpace) == "" }

// Warnings returns the format-level problems of a comment that asks a human to
// act (Steps, Checked, Done when, placeholders, bare UUIDs, implicit value
// words, merge readiness). The CLI prints them and still sends the comment.
func Warnings(text string) []Problem {
	d := parse(text)
	if !humanAction(text, d, missingMentions(d)) {
		return nil
	}
	var out []Problem
	add := func(n int, format string, args ...any) { out = append(out, Problem{n, fmt.Sprintf(format, args...)}) }
	secs := sections(d.lines)
	var steps []numbered
	for i, l := range d.lines {
		if numberedRE.MatchString(l) {
			steps = append(steps, numbered{i + 1, l})
		}
	}
	listed := len(steps)
	for _, s := range secs["Steps"] {
		dup := false
		for _, t := range steps[:listed] {
			dup = dup || t == s
		}
		if !dup {
			steps = append(steps, s)
		}
	}
	if !anyLine(secs["Steps"], numberedRE.MatchString) {
		add(1, "missing Steps section with at least one numbered step (Steps / Bước)")
	}
	checked := secs["Checked"]
	if !anyLine(checked, bulletRE.MatchString) && !anyLine(checked, notVerified.MatchString) {
		add(1, "missing Checked section: one evidence bullet, or `not verified: <reason>` (Checked / Đã kiểm tra)")
	}
	if !anyLine(secs["Done when"], func(l string) bool { return !blank(l) }) {
		add(1, "missing Done when section (Done when / Xong khi)")
	}

	for i, line := range d.lines {
		n := i + 1
		if !d.delims[n] {
			for _, m := range placeholder.FindAllStringSubmatch(line, -1) {
				fields := strings.FieldsFunc(m[2], isPySpace)
				tag := strings.ToLower(fields[0])
				if !htmlTags[tag] && !strings.HasPrefix(tag, "http") && !sourceRE.MatchString(line) {
					add(n, "placeholder <%s> has no (source: ...) with a value on its line", m[2])
				}
			}
		}
		if d.fenced[n] {
			continue
		}
		prose := mentionTail.ReplaceAllString(stripCodeSpans(line), "")
		for _, m := range findBounded(uuidRE, prose, nil) {
			add(n, "bare UUID %s: use a mention link or a code span and say what kind it is", prose[m[0]:m[1]])
		}
	}
	for _, s := range steps {
		for _, m := range findBounded(implicitRE, s.line, implicitWord(s.line)) {
			add(s.n, "implicit value word '%s': write every value out", s.line[m[0]:m[1]])
		}
	}
	for _, s := range steps {
		if len(findBounded(mergeRE, s.line, nil)) > 0 {
			if mergeNotReady(checked) {
				add(s.n, "merge step: Checked needs PR ready, CI green and independent review PASS on the exact head; pending, failed or unobserved gates cannot support a merge ask")
			}
			break
		}
	}
	sort.SliceStable(out, func(a, b int) bool {
		if out[a].Line != out[b].Line {
			return out[a].Line < out[b].Line
		}
		return out[a].Message < out[b].Message
	})
	return out
}

// implicitWord applies the \b rules of IMPLICIT: `v.v.` has none, `etc` may
// swallow one trailing dot (`\betc\b\.?`), the other words need both.
func implicitWord(line string) func(string, int) int {
	return func(match string, end int) int {
		start := end - len(match)
		if strings.EqualFold(match, "v.v.") {
			return end
		}
		if wordBefore(line, start) || wordAfter(line, end) {
			return -1
		}
		if strings.EqualFold(match, "etc") && strings.HasPrefix(line[end:], ".") {
			return end + 1
		}
		return end
	}
}

// mergeNotReady is the Checked-section attestation for a merge step: PR ready,
// CI green and independent review PASS, with no pending, failed or unobserved gate.
func mergeNotReady(checked []numbered) bool {
	parts := make([]string, len(checked))
	for i, c := range checked {
		parts[i] = c.line
	}
	state := strings.Join(parts, " ")
	state = replaceBounded(notDraft, state, "")
	state = replaceBounded(readyDraft, state, "ready")
	state = replaceBounded(noUnready, state, "")
	ready := hasReady.MatchString(state) && ciGreen.MatchString(state) && reviewPass.MatchString(state)
	return !ready || unready.MatchString(state)
}
