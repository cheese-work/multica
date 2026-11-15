package handler

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// failStatusActivityInserts makes the status_changed insert for issueID fail,
// to prove the status write rolls back with it.
func failStatusActivityInserts(t *testing.T, issueID string) {
	t.Helper()
	fn := "che2841_fail_" + strings.ReplaceAll(issueID, "-", "")
	for _, stmt := range []string{
		fmt.Sprintf(`CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected activity failure'; END $$`, fn),
		fmt.Sprintf(`CREATE TRIGGER %s BEFORE INSERT ON activity_log FOR EACH ROW WHEN (NEW.issue_id = '%s' AND NEW.action = 'status_changed') EXECUTE FUNCTION %s()`, fn, issueID, fn),
	} {
		if _, err := testPool.Exec(context.Background(), stmt); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), fmt.Sprintf(`DROP TRIGGER IF EXISTS %s ON activity_log`, fn))
		testPool.Exec(context.Background(), fmt.Sprintf(`DROP FUNCTION IF EXISTS %s()`, fn))
	})
}

// wantBackgroundTrigger checks the one status row of a system writer: the
// cause kind, a system actor and a null auth_source.
func wantBackgroundTrigger(t *testing.T, issueID, kind string) map[string]any {
	t.Helper()
	rows := statusRows(t, issueID)
	if len(rows) != 1 {
		t.Fatalf("status_changed rows = %d, want 1", len(rows))
	}
	trigger, _ := rows[0]["trigger"].(map[string]any)
	cause, _ := trigger["cause"].(map[string]any)
	by, _ := cause["by"].(map[string]any)
	if trigger["v"] != float64(1) || cause["kind"] != kind || by["type"] != "system" || trigger["auth_source"] != nil {
		t.Fatalf("trigger = %v, want v1 cause %s by system with null auth_source", trigger, kind)
	}
	return trigger
}

// issueUpdatedActivityID captures the activity_id of the next issue:updated
// event for issueID.
func issueUpdatedActivityID(issueID string) *any {
	var id any
	testHandler.Bus.Subscribe(protocol.EventIssueUpdated, func(e events.Event) {
		p, _ := e.Payload.(map[string]any)
		if issue, _ := p["issue"].(IssueResponse); issue.ID == issueID {
			id = p["activity_id"]
		}
	})
	return &id
}

func statusRowID(t *testing.T, issueID string) string {
	t.Helper()
	var id string
	if err := testPool.QueryRow(context.Background(), `SELECT id FROM activity_log WHERE issue_id = $1 AND action = 'status_changed'`, issueID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func inProgressIssueWithMergedPR(t *testing.T, title, repo string) string {
	t.Helper()
	issue := newVCSIssue(t, title)
	t.Cleanup(func() { cleanupVCS(context.Background(), issue.ID) })
	linkMergedGitHubPRForTest(t, issue.ID, repo)
	return issue.ID
}

func TestPRAutoCompleteRecordsStatusChangeInTransaction(t *testing.T) {
	issueID := inProgressIssueWithMergedPR(t, "pr auto-complete history", "che2841-pr-auto")
	activityID := issueUpdatedActivityID(issueID)

	testHandler.maybeAutoCompleteIssue(context.Background(), parseUUID(testWorkspaceID), parseUUID(issueID), nil)

	wantIssueStatus(t, issueID, "done")
	trigger := wantBackgroundTrigger(t, issueID, "pr_merged")
	cause, _ := trigger["cause"].(map[string]any)
	ref, _ := cause["ref"].(map[string]any)
	if ref["pr"] != "#1" {
		t.Fatalf("cause.ref = %v, want pr #1", ref)
	}
	if *activityID != statusRowID(t, issueID) {
		t.Fatalf("event activity_id = %v, want the committed row", *activityID)
	}
}

func TestPRAutoCompleteRollsBackWithActivityFailure(t *testing.T) {
	issueID := inProgressIssueWithMergedPR(t, "pr auto-complete rollback", "che2841-pr-rollback")
	failStatusActivityInserts(t, issueID)

	testHandler.maybeAutoCompleteIssue(context.Background(), parseUUID(testWorkspaceID), parseUUID(issueID), nil)

	wantIssueStatus(t, issueID, "in_progress")
}

func TestGitHubMergeRecordsStatusChangeInTransaction(t *testing.T) {
	issue := newVCSIssue(t, "github merge history")
	t.Cleanup(func() { cleanupVCS(context.Background(), issue.ID) })
	activityID := issueUpdatedActivityID(issue.ID)
	row, err := testHandler.Queries.GetIssue(context.Background(), parseUUID(issue.ID))
	if err != nil {
		t.Fatal(err)
	}

	testHandler.advanceIssueToDone(context.Background(), row, testWorkspaceID)

	wantIssueStatus(t, issue.ID, "done")
	wantBackgroundTrigger(t, issue.ID, "pr_merged")
	if *activityID != statusRowID(t, issue.ID) {
		t.Fatalf("event activity_id = %v, want the committed row", *activityID)
	}
}

func TestGitHubMergeRollsBackWithActivityFailure(t *testing.T) {
	issue := newVCSIssue(t, "github merge rollback")
	t.Cleanup(func() { cleanupVCS(context.Background(), issue.ID) })
	failStatusActivityInserts(t, issue.ID)
	row, err := testHandler.Queries.GetIssue(context.Background(), parseUUID(issue.ID))
	if err != nil {
		t.Fatal(err)
	}

	testHandler.advanceIssueToDone(context.Background(), row, testWorkspaceID)

	wantIssueStatus(t, issue.ID, "in_progress")
}
