package handler

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Structured blocker (CHE-6972): `status blocked --blocked-on <ref>` stores the
// ref in the status_changed details and in metadata.blocked_on.

func moveBlockedOn(t *testing.T, issueID string, body map[string]any) *testutil.Response {
	t.Helper()
	body["suppress_run"] = true
	return testutil.Call(t, testHandler.UpdateIssue, updateIssueRequest(issueID, body))
}

func blockedOnMetadata(t *testing.T, issueID string) (string, bool) {
	t.Helper()
	var v *string
	dbfx.QueryRow(t, `SELECT metadata ->> 'blocked_on' FROM issue WHERE id = $1`, issueID).Scan(&v)
	if v == nil {
		return "", false
	}
	return *v, true
}

func TestBlockedOnIsRecordedOnTheMoveAndInMetadata(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	other := dbfx.Issue(t, "blocked-on-target", testutil.Cols{"status": "in_progress"})
	var decision string
	dbfx.QueryRow(t, `INSERT INTO decision_request (workspace_id, issue_id, asking_agent_id, human_user_id, chat_session_id, ask, escalation_class, origin)
		VALUES ($1, $2, gen_random_uuid(), $3, gen_random_uuid(), 'Pick', 'reversible', 'asked') RETURNING id::text`,
		testWorkspaceID, other, testUserID).Scan(&decision)

	for _, ref := range []string{
		"issue:" + other,
		"issue:" + other + "@in_review",
		"pr:checks", "pr:merged", "pr:conflicts", "pr:dequeued",
		"children", "children@2",
		"gh-run:cheese-work/multica:123456",
		"decision:" + decision,
	} {
		issue := dbfx.Issue(t, "blocked-on-"+ref, testutil.Cols{"status": "in_progress"})
		moveBlockedOn(t, issue, map[string]any{"status": "blocked", "blocked_on": ref}).Want(http.StatusOK)
		if got, ok := blockedOnMetadata(t, issue); !ok || got != ref {
			t.Fatalf("%s: metadata.blocked_on = %q (set %v)", ref, got, ok)
		}
		rows := statusRows(t, issue)
		if len(rows) != 1 || rows[0]["blocked_on"] != ref {
			t.Fatalf("%s: status_changed details = %v, want blocked_on", ref, rows)
		}
	}
}

func TestBlockedOnRefusedForMalformedRefs(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	issue := dbfx.Issue(t, "blocked-on-malformed", testutil.Cols{"status": "in_progress"})
	for _, ref := range []string{
		"", "nonsense", "member:abc", "pr:open", "pr:", "children@0", "children@x", "gh-run:nope", "gh-run:o/r:abc",
		"issue:not-a-uuid", "issue:" + issue + "@wrong", "decision:not-a-uuid",
	} {
		resp := moveBlockedOn(t, issue, map[string]any{"status": "blocked", "blocked_on": ref}).Want(http.StatusBadRequest)
		for _, form := range []string{"issue:", "pr:", "children", "gh-run:", "decision:"} {
			if !strings.Contains(resp.Body.String(), form) {
				t.Fatalf("%q: error %q does not name the form %s", ref, resp.Body.String(), form)
			}
		}
	}
	if _, ok := blockedOnMetadata(t, issue); ok {
		t.Fatal("a refused write must not store blocked_on")
	}
}

func TestBlockedOnRefusedOnOtherStatusesAndWithoutStatus(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	issue := dbfx.Issue(t, "blocked-on-wrong-status", testutil.Cols{"status": "in_progress"})
	moveBlockedOn(t, issue, map[string]any{"status": "in_review", "blocked_on": "pr:checks"}).Want(http.StatusBadRequest)
	moveBlockedOn(t, issue, map[string]any{"blocked_on": "pr:checks"}).Want(http.StatusBadRequest)
	if _, ok := blockedOnMetadata(t, issue); ok {
		t.Fatal("a refused write must not store blocked_on")
	}
}

func TestBlockedOnRefusedForUnknownTargets(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	issue := dbfx.Issue(t, "blocked-on-unknown", testutil.Cols{"status": "in_progress"})
	var foreignWS, foreignIssue string
	dbfx.QueryRow(t, `INSERT INTO workspace (name, slug, issue_prefix) VALUES ('Foreign', 'foreign-blocked-on', 'FBO') RETURNING id::text`).Scan(&foreignWS)
	t.Cleanup(func() {
		dbfx.Exec(t, `DELETE FROM issue WHERE workspace_id = $1`, foreignWS)
		dbfx.Exec(t, `DELETE FROM workspace WHERE id = $1`, foreignWS)
	})
	dbfx.QueryRow(t, `INSERT INTO issue (workspace_id, title, status, priority, creator_type, creator_id, number) VALUES ($1, 'foreign', 'todo', 'none', 'member', $2, 1) RETURNING id::text`,
		foreignWS, testUserID).Scan(&foreignIssue)
	for _, ref := range []string{
		"issue:" + foreignIssue, // another workspace
		"issue:00000000-0000-0000-0000-000000000001",
		"decision:00000000-0000-0000-0000-000000000001",
	} {
		moveBlockedOn(t, issue, map[string]any{"status": "blocked", "blocked_on": ref}).Want(http.StatusBadRequest)
	}
}

func TestBlockedOnIsClearedWhenTheIssueLeavesBlocked(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	issue := dbfx.Issue(t, "blocked-on-cleared", testutil.Cols{"status": "in_progress"})
	moveBlockedOn(t, issue, map[string]any{"status": "blocked", "blocked_on": "pr:merged"}).Want(http.StatusOK)
	moveBlockedOn(t, issue, map[string]any{"status": "in_progress"}).Want(http.StatusOK)
	if got, ok := blockedOnMetadata(t, issue); ok {
		t.Fatalf("metadata.blocked_on = %q after leaving blocked, want cleared", got)
	}
}

func TestBlockedWithoutBlockedOnBehavesAsBefore(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	issue := dbfx.Issue(t, "blocked-no-ref", testutil.Cols{"status": "in_progress"})
	moveBlockedOn(t, issue, map[string]any{"status": "blocked"}).Want(http.StatusOK)
	if _, ok := blockedOnMetadata(t, issue); ok {
		t.Fatal("a move without --blocked-on must not store blocked_on")
	}
	if rows := statusRows(t, issue); len(rows) != 1 || rows[0]["blocked_on"] != nil {
		t.Fatalf("details = %v, want no blocked_on", rows)
	}
}

// Background writers (GitHub merge, task reset, decision park) move status
// through UpdateIssueStatus, not the PUT handler. They must not leave a stale
// blocked_on on an issue that left blocked.
func TestBlockedOnIsClearedByBackgroundStatusWriters(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	issue := dbfx.Issue(t, "blocked-on-background", testutil.Cols{"status": "in_progress"})
	moveBlockedOn(t, issue, map[string]any{"status": "blocked", "blocked_on": "pr:merged"}).Want(http.StatusOK)
	if _, err := testHandler.Queries.UpdateIssueStatus(context.Background(), db.UpdateIssueStatusParams{
		ID: parseUUID(issue), Status: "todo", WorkspaceID: parseUUID(testWorkspaceID),
	}); err != nil {
		t.Fatal(err)
	}
	if got, ok := blockedOnMetadata(t, issue); ok {
		t.Fatalf("metadata.blocked_on = %q after a background move out of blocked, want cleared", got)
	}
}
