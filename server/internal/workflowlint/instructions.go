package workflowlint

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/multica-ai/multica/server/internal/issueworkflow"
)

// WF009 and WF010 read the prose of a step's instructions for the moves it
// tells the handler to make. A phrase counts as a move when it reads
// "move [the issue] [back|straight] to <status>" or "multica issue status
// <id> <status>"; <status> must be a catalog status, by key or display name.
// Anything else ("move to the next step") is not a status and is ignored.

// moveRe matches the words that lead into a status: "move ... to" with a short
// gap, or the CLI form. Group 1 is the text after "to"; group 2 is the text
// after the CLI form's issue argument. Both stop at the next sentence break, so
// a later move on the same line is matched on its own.
var moveRe = regexp.MustCompile("(?i)\\bmove\\b[^.:;\\n]{0,40}?\\bto\\b[\\s`\"'(]*([^.;\\n]*)|\\bissue status\\s+(?:<[^>]*>|\\S+)\\s+[`\"']?([^.;\\n]*)")

// negationRe finds a prohibition in the clause before a move.
var negationRe = regexp.MustCompile(`(?i)\b(never|not|don't|dont|without)\b`)

type statusName struct{ key, norm string }

// normalize lower-cases and turns underscores into spaces so "In progress",
// "in_progress" and "IN PROGRESS" compare equal.
func normalize(s string) string {
	return strings.Join(strings.Fields(strings.ToLower(strings.ReplaceAll(s, "_", " "))), " ")
}

// statusNames lists every key and display name of the catalog, longest first,
// so "in review" wins over a shorter name that is its prefix.
func statusNames(catalog map[string]string) []statusName {
	var out []statusName
	for key, name := range catalog {
		out = append(out, statusName{key, normalize(key)})
		if n := normalize(name); n != "" && n != normalize(key) {
			out = append(out, statusName{key, n})
		}
	}
	slices.SortFunc(out, func(a, b statusName) int {
		if len(a.norm) != len(b.norm) {
			return len(b.norm) - len(a.norm)
		}
		return strings.Compare(a.norm, b.norm)
	})
	return out
}

// resolve returns the catalog status key that text starts with, on a word
// boundary, or "".
func resolve(text string, names []statusName) string {
	text = normalize(text)
	for _, n := range names {
		if rest, ok := strings.CutPrefix(text, n.norm); ok && (rest == "" || !isWordByte(rest[0])) {
			return n.key
		}
	}
	return ""
}

func isWordByte(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= '0' && b <= '9'
}

// instructionMove is one move an instruction text tells the handler to make.
type instructionMove struct {
	key        string
	prohibited bool
}

func movesIn(text string, names []statusName) []instructionMove {
	var out []instructionMove
	for _, m := range moveRe.FindAllStringSubmatchIndex(text, -1) {
		target := ""
		for _, g := range []int{2, 4} {
			if m[g] >= 0 {
				target = text[m[g]:m[g+1]]
			}
		}
		key := resolve(target, names)
		if key == "" {
			continue
		}
		// The clause is the text since the last sentence break.
		clause := text[:m[0]]
		if i := strings.LastIndexAny(clause, ".:;\n"); i >= 0 {
			clause = clause[i+1:]
		}
		out = append(out, instructionMove{key, negationRe.MatchString(clause) && m[2] >= 0})
	}
	return out
}

// lintInstructions is WF009 and WF010 for one step.
func (l *linter) lintInstructions(s issueworkflow.Step, env Env) {
	if env.Statuses == nil || s.Instructions == "" {
		return
	}
	edges := l.def.EdgesOf(s.StatusKey)
	seen := map[string]bool{}
	for _, mv := range movesIn(s.Instructions, statusNames(env.Statuses)) {
		if seen[mv.key] {
			continue
		}
		seen[mv.key] = true
		switch {
		case !l.def.Has(mv.key):
			l.add("WF009", LevelWarning, &s, fmt.Sprintf("The instructions of %q name %q, which this workflow does not define.", s.StatusKey, mv.key),
				"Change the instructions to a status of this workflow, or add the status.")
		case mv.prohibited || mv.key == s.StatusKey:
		case !slices.ContainsFunc(edges, func(e issueworkflow.Edge) bool { return e.To == mv.key }):
			l.add("WF010", LevelWarning, &s, fmt.Sprintf("The instructions of %q say to move to %q, but %q has no edge to it.", s.StatusKey, mv.key, s.StatusKey),
				"Add an edge to the status, or change the instructions.")
		}
	}
}
