package main

import (
	"os"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/migrations"
)

// The scope CHECK on issue_view_preference is dropped and re-added by every
// migration that widens it. Migrations apply in lexical filename order, so a
// fresh install ends with whichever widening sorts last. That last one must
// list every scope the handler accepts; otherwise a fresh schema silently
// diverges from an upgraded one.
func TestIssueViewPreferenceScopeCheckLastWideningKeepsEveryScope(t *testing.T) {
	files, err := migrations.Files("up")
	if err != nil {
		t.Fatalf("list migrations: %v", err)
	}
	var last, body string
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		if strings.Contains(string(b), "ADD CONSTRAINT issue_view_preference_scope_type_check") {
			last, body = f, string(b)
		}
	}
	if last == "" {
		t.Fatal("no migration adds issue_view_preference_scope_type_check")
	}
	for _, scope := range []string{"workspace", "my", "project", "sidebar", "sidebar_projects"} {
		if !strings.Contains(body, "'"+scope+"'") {
			t.Errorf("last widening %s drops scope %q on a fresh install", last, scope)
		}
	}
}
