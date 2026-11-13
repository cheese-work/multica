package handler

import (
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
)

// The fork's title-only suggestion query (CHE-2402) must also drop issues in
// private projects the caller cannot see (upstream v0.8.2 private projects).
func TestBuildSuggestionIssueQueryHidesPrivateProjectsAndStaysTitleOnly(t *testing.T) {
	hidden := []pgtype.UUID{{Bytes: [16]byte{1}, Valid: true}}

	sql, args := buildSuggestionIssueQuery("deploy", []string{"deploy"}, 0, false, hidden...)
	if !strings.Contains(sql, "project_id IS NULL OR NOT (project_id = ANY($") {
		t.Fatalf("hidden-project predicate missing from %q", sql)
	}
	if got, ok := args[len(args)-1].([]pgtype.UUID); !ok || len(got) != 1 {
		t.Fatalf("last arg = %#v, want the hidden project ids", args[len(args)-1])
	}
	if strings.Contains(sql, "description") || strings.Contains(sql, "comment") {
		t.Fatalf("suggestion query reads more than title/number: %q", sql)
	}

	open, openArgs := buildSuggestionIssueQuery("deploy", []string{"deploy"}, 0, false)
	if strings.Contains(open, "project_id") || len(openArgs) != len(args)-1 {
		t.Fatalf("a caller with no hidden project gets an unfiltered query, got %q", open)
	}
}
