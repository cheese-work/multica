package browserlease

import (
	"strings"
	"testing"
	"time"
)

// An ask refuses a bare ID in its decision text, so the request W3-10 opens links each ID.
// The validator must read the same scope back from the linked lines.
func TestLinkedScopeDecisionParsesBackToTheScope(t *testing.T) {
	want := Scope{AgentID: newUUID(), IssueID: newUUID(), ProfileID: newUUID(), Class: ClassS2, Duration: 2 * time.Hour}
	text := LinkedScopeDecision(want, ScopeNames{Slug: "acme", Agent: "Bot [x]", Issue: "Fix (login)\nnow", Profile: "figma"})
	if uuidBare(text) {
		t.Fatalf("decision carries a bare ID:\n%s", text)
	}
	got, ok := parseScope(text)
	if !ok || got != want {
		t.Fatalf("parseScope = %+v, %v; want %+v\n%s", got, ok, want, text)
	}
}

func TestParseScopeRefusesALinkWithoutAnID(t *testing.T) {
	text := strings.Replace(LinkedScopeDecision(Scope{AgentID: newUUID(), IssueID: newUUID(), ProfileID: newUUID(), Class: ClassS2, Duration: time.Hour},
		ScopeNames{Slug: "acme", Agent: "a", Issue: "i", Profile: "p"}), "/agents/", "/agents/x", 1)
	if _, ok := parseScope(text); ok {
		t.Fatalf("a link whose destination does not end in an ID parsed:\n%s", text)
	}
}

// uuidBare reports a UUID outside a link destination.
func uuidBare(s string) bool {
	for _, line := range strings.Split(s, "\n") {
		if i := strings.Index(line, "]("); i >= 0 {
			line = line[:i]
		}
		if _, v, ok := strings.Cut(line, ": "); ok && len(strings.Fields(v)) == 1 && len(v) == 36 {
			return true
		}
	}
	return false
}
