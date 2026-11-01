package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

func statusRows(t *testing.T, issueID string) []map[string]any {
	t.Helper()
	rows, err := testPool.Query(context.Background(), `SELECT details FROM activity_log WHERE issue_id = $1 AND action = 'status_changed' ORDER BY created_at, id`, issueID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var d map[string]any
		if err := json.Unmarshal(raw, &d); err != nil {
			t.Fatal(err)
		}
		out = append(out, d)
	}
	return out
}

func patchStatus(issueID, status string) *http.Request {
	req := withURLParam(newRequest(http.MethodPatch, "/api/issues/"+issueID, map[string]any{"status": status, "suppress_run": true}), "id", issueID)
	req.Header.Set("X-Actor-Source", "interactive_jwt")
	req.Header.Set("X-Client-Platform", "desktop")
	return req
}

// A single issue update writes its status_changed row in the status
// transaction and names that row in the event, so the listener skips it.
func TestUpdateIssueStatusRecordsActivityInTransaction(t *testing.T) {
	issueID := createIssueForTimeline(t, "status history migrated")
	var activityID any
	testHandler.Bus.Subscribe(protocol.EventIssueUpdated, func(e events.Event) {
		p, _ := e.Payload.(map[string]any)
		if issue, _ := p["issue"].(IssueResponse); issue.ID == issueID {
			activityID = p["activity_id"]
		}
	})

	testutil.Call(t, testHandler.UpdateIssue, patchStatus(issueID, "in_progress")).Want(http.StatusOK)

	rows := statusRows(t, issueID)
	if len(rows) != 1 {
		t.Fatalf("status_changed rows = %d, want 1", len(rows))
	}
	trigger, _ := rows[0]["trigger"].(map[string]any)
	cause, _ := trigger["cause"].(map[string]any)
	by, _ := cause["by"].(map[string]any)
	if trigger["v"] != float64(1) || cause["kind"] != "ui_or_api" || by["type"] != "member" || by["id"] != testUserID ||
		trigger["auth_source"] != "interactive_jwt" || trigger["channel"] != "desktop" {
		t.Fatalf("trigger = %v", trigger)
	}
	var rowID string
	if err := testPool.QueryRow(context.Background(), `SELECT id FROM activity_log WHERE issue_id = $1 AND action = 'status_changed'`, issueID).Scan(&rowID); err != nil {
		t.Fatal(err)
	}
	if activityID != rowID {
		t.Fatalf("event activity_id = %v, want %s", activityID, rowID)
	}
}

// The activity write failing rolls the status back with it.
func TestUpdateIssueStatusRollsBackWithActivityFailure(t *testing.T) {
	issueID := createIssueForTimeline(t, "status history rollback")
	fn := "che2835_fail_" + strings.ReplaceAll(issueID, "-", "")
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

	testutil.Call(t, testHandler.UpdateIssue, patchStatus(issueID, "in_progress")).Want(http.StatusInternalServerError)

	var status string
	if err := testPool.QueryRow(context.Background(), `SELECT status FROM issue WHERE id = $1`, issueID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "todo" {
		t.Fatalf("status = %q after failed activity write, want todo", status)
	}
}
