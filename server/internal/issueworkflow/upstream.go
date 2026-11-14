package issueworkflow

import (
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgtype"
)

// upstream_checked (CHE-5583, CHE-5574 step 6): a create-time entry gate. A new
// feat or plan issue in a listed project must cite an upstream search. The
// gate checks the form of the cited value, not the search itself.

const (
	upstreamLinkPrefix  = "https://github.com/multica-ai/multica/"
	upstreamNoneFound   = "none found:"
	upstreamNotMultica  = "not multica:"
	upstreamMinTextRune = 3
)

// UpstreamCheckValid reports whether value is a link into multica-ai/multica
// below the repository root, "none found: <query>" or "not multica: <reason>",
// with the query or reason at least 3 characters.
func UpstreamCheckValid(value string) bool {
	value = strings.TrimSpace(value)
	if rest, ok := strings.CutPrefix(value, upstreamLinkPrefix); ok {
		return strings.TrimSpace(rest) != "" && !strings.ContainsAny(value, " \t\r\n")
	}
	for _, prefix := range []string{upstreamNoneFound, upstreamNotMultica} {
		if rest, ok := strings.CutPrefix(value, prefix); ok {
			return utf8.RuneCountInString(strings.TrimSpace(rest)) >= upstreamMinTextRune
		}
	}
	return false
}

// UpstreamGatedTitle reports whether a title needs the upstream check: type
// feat or plan, or no type at all (fail closed: otherwise omitting the prefix
// skips the check). untyped marks the second case.
func UpstreamGatedTitle(title string) (gated, untyped bool) {
	typ, ok := TitleType(title)
	if !ok {
		return true, true
	}
	return typ == "feat" || typ == "plan", false
}

// UpstreamGateMode is the strictest mode of an upstream_checked entry gate in
// the workflow that lists projectID; "" when none does.
func UpstreamGateMode(def *Definition, projectID string) string {
	target, ok := parseProjectID(projectID)
	if !ok {
		return ""
	}
	mode := ""
	for _, step := range def.Steps {
		for _, g := range step.Entry {
			if g.Guard != GuardUpstreamChecked || g.Mode == EntryModeOff || !listsProject(g.Projects, target) {
				continue
			}
			if g.Mode == EntryModeBlock || mode == "" {
				mode = g.Mode
			}
		}
	}
	return mode
}

func listsProject(projects []string, target pgtype.UUID) bool {
	for _, p := range projects {
		if id, ok := parseProjectID(p); ok && id == target {
			return true
		}
	}
	return false
}

func parseProjectID(s string) (pgtype.UUID, bool) {
	var id pgtype.UUID
	if id.Scan(strings.TrimSpace(s)) != nil || !id.Valid {
		return pgtype.UUID{}, false
	}
	return id, true
}

// validateGateProjects: only upstream_checked takes projects, and it needs at
// least one valid project id. An unscoped gate would refuse every project on
// the workflow, so it is rejected when the workflow is saved.
func validateGateProjects(g Guard, e EntryGate) error {
	if g.Kind != GuardUpstreamChecked {
		if len(e.Projects) > 0 {
			return invalid("entry gate %q takes no projects", e.Guard)
		}
		return nil
	}
	if len(e.Projects) == 0 {
		return invalid("entry gate %q needs projects: a list of project ids", e.Guard)
	}
	for _, p := range e.Projects {
		if _, ok := parseProjectID(p); !ok {
			return invalid("entry gate %q has a projects entry that is not a project id: %q", e.Guard, p)
		}
	}
	return nil
}
