package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/service"
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

// The channel is a client hint kept apart from authentication provenance: a
// CLI request keeps channel cli whatever credential it carries.
func TestStatusTriggerChannelIsIndependentOfAuthSource(t *testing.T) {
	cases := []struct{ source, platform, channel string }{
		{"task_token", "cli", "cli"},
		{"cloud_pat", "cli", "cli"},
		{"cli_jwt", "cli", "cli"},
		{"pat", "", "cli"},
		{"cli_jwt", "", "cli"},
		{"interactive_jwt", "desktop", "desktop"},
		{"interactive_jwt", "web", "web"},
		{"task_token", "", "unknown"},
		{"interactive_jwt", "", "unknown"},
	}
	for _, c := range cases {
		req := newRequest(http.MethodPatch, "/api/issues/x", nil)
		req.Header.Set("X-Actor-Source", c.source)
		if c.platform != "" {
			req.Header.Set("X-Client-Platform", c.platform)
		}
		got := statusTriggerFromRequest(req, "member", "").Channel
		if got != c.channel {
			t.Errorf("source=%q platform=%q: channel %q, want %q", c.source, c.platform, got, c.channel)
		}
	}
}

// A duplicate mark keeps the listener's mark row; the handler hands it the
// same trigger a plain status move records.
func TestDuplicateMarkEventCarriesStatusTrigger(t *testing.T) {
	issueID := createIssueForTimeline(t, "duplicate trigger")
	originalID := createIssueForTimeline(t, "duplicate trigger original")
	var trigger any
	testHandler.Bus.Subscribe(protocol.EventIssueUpdated, func(e events.Event) {
		p, _ := e.Payload.(map[string]any)
		if issue, _ := p["issue"].(IssueResponse); issue.ID == issueID {
			trigger = p["status_trigger"]
		}
	})
	req := withURLParam(newRequest(http.MethodPatch, "/api/issues/"+issueID, map[string]any{"duplicate_of_issue_id": originalID, "suppress_run": true}), "id", issueID)
	req.Header.Set("X-Actor-Source", "interactive_jwt")
	req.Header.Set("X-Client-Platform", "desktop")

	testutil.Call(t, testHandler.UpdateIssue, req).Want(http.StatusOK)

	got, ok := trigger.(service.StatusTrigger)
	if !ok || got.Version != 1 || got.Cause.Kind != service.StatusCauseUIOrAPI || got.Channel != "desktop" {
		t.Fatalf("status_trigger = %#v", trigger)
	}
}

// A status move may name the handoff comment that explains it. The link is
// stored when the comment belongs to the issue; otherwise it is dropped and the
// trigger says so.
func TestUpdateIssueStatusHandoffCommentLink(t *testing.T) {
	issueID := createIssueForTimeline(t, "handoff link")
	otherID := createIssueForTimeline(t, "handoff link other")
	own := dbfx.Comment(t, issueID, "handoff")
	elsewhere := dbfx.Comment(t, otherID, "other issue")

	move := func(issue, status, commentID string) {
		req := withURLParam(newRequest(http.MethodPatch, "/api/issues/"+issue, map[string]any{"status": status, "suppress_run": true, "handoff_comment_id": commentID}), "id", issue)
		req.Header.Set("X-Actor-Source", "interactive_jwt")
		testutil.Call(t, testHandler.UpdateIssue, req).Want(http.StatusOK)
	}
	move(issueID, "in_progress", own)
	move(otherID, "in_progress", own)
	move(issueID, "in_review", elsewhere)

	trigger := func(rows []map[string]any, i int) map[string]any {
		tr, _ := rows[i]["trigger"].(map[string]any)
		return tr
	}
	rows := statusRows(t, issueID)
	if len(rows) != 2 {
		t.Fatalf("status_changed rows = %d, want 2", len(rows))
	}
	if got := trigger(rows, 0); got["handoff_comment_id"] != own || got["ref_invalid"] != nil {
		t.Errorf("valid link: trigger = %v", got)
	}
	if got := trigger(rows, 1); got["handoff_comment_id"] != nil || got["ref_invalid"] != true {
		t.Errorf("link to a comment of another issue: trigger = %v", got)
	}
	other := statusRows(t, otherID)
	if got := trigger(other, 0); got["handoff_comment_id"] != nil || got["ref_invalid"] != true {
		t.Errorf("link to a comment of another issue: trigger = %v", got)
	}
}

// A move into a workflow step with a handler hands the issue off; the
// listener still writes that row, so the event must carry the validated link.
func TestUpdateIssueWorkflowHandoffKeepsHandoffCommentLink(t *testing.T) {
	reviewer := seededReadyAgentID(t)
	wf := createTestWorkflowSteps(t, "Handoff link "+workflowTestSuffix(), []map[string]any{
		{"status_key": "backlog"},
		{"status_key": "in_progress"},
		{"status_key": "in_review", "handler": map[string]any{"type": "agent", "id": reviewer}, "instructions": "Review the PR."},
	})
	projectID := createWorkflowTestProject(t, "Handoff link project")
	setProjectWorkflow(t, projectID, map[string]any{"workflow_id": wf.ID}).Want(http.StatusOK)

	run := func(link func(issue, other string) string) (map[string]any, map[string]string) {
		issueID := createIssueForTimeline(t, "handoff workflow link "+workflowTestSuffix())
		otherID := createIssueForTimeline(t, "handoff workflow link other "+workflowTestSuffix())
		if _, err := testPool.Exec(context.Background(), `UPDATE issue SET project_id=$2, status='in_progress' WHERE id=$1`, issueID, projectID); err != nil {
			t.Fatal(err)
		}
		commentID := link(dbfx.Comment(t, issueID, "handoff"), dbfx.Comment(t, otherID, "other issue"))
		var trigger map[string]any
		var handoff map[string]string
		testHandler.Bus.Subscribe(protocol.EventIssueUpdated, func(e events.Event) {
			p, _ := e.Payload.(map[string]any)
			if issue, _ := p["issue"].(IssueResponse); issue.ID != issueID {
				return
			}
			if tr, ok := p["status_trigger"].(service.StatusTrigger); ok {
				b, _ := json.Marshal(tr)
				_ = json.Unmarshal(b, &trigger)
			}
			handoff, _ = p["workflow_handoff"].(map[string]string)
		})
		req := withURLParam(newRequest(http.MethodPatch, "/api/issues/"+issueID, map[string]any{"status": "in_review", "suppress_run": true, "handoff_comment_id": commentID}), "id", issueID)
		req.Header.Set("X-Actor-Source", "interactive_jwt")
		testutil.Call(t, testHandler.UpdateIssue, req).Want(http.StatusOK)
		return trigger, handoff
	}

	own := ""
	trigger, handoff := run(func(issueComment, _ string) string { own = issueComment; return issueComment })
	if handoff == nil {
		t.Fatal("move did not hand off")
	}
	if trigger["handoff_comment_id"] != own || trigger["ref_invalid"] != nil || trigger["v"] != float64(1) {
		t.Errorf("valid link on a handoff: trigger = %v", trigger)
	}
	trigger, _ = run(func(_, otherComment string) string { return otherComment })
	if trigger["handoff_comment_id"] != nil || trigger["ref_invalid"] != true {
		t.Errorf("link to another issue's comment on a handoff: trigger = %v", trigger)
	}
}

// Reopening a duplicate straight into a handled workflow step is both an
// unmark and a handoff. The listener writes that row, so the event must keep
// the validated handoff link instead of the plain request trigger.
func TestUpdateIssueDuplicateReopenIntoHandoffKeepsHandoffCommentLink(t *testing.T) {
	reviewer := seededReadyAgentID(t)
	wf := createTestWorkflowSteps(t, "Reopen handoff "+workflowTestSuffix(), []map[string]any{
		{"status_key": "backlog"},
		{"status_key": "cancelled"},
		{"status_key": "in_review", "handler": map[string]any{"type": "agent", "id": reviewer}, "instructions": "Review the PR."},
	})
	projectID := createWorkflowTestProject(t, "Reopen handoff project")
	setProjectWorkflow(t, projectID, map[string]any{"workflow_id": wf.ID}).Want(http.StatusOK)

	issueID := createIssueForTimeline(t, "reopen handoff "+workflowTestSuffix())
	originalID := createIssueForTimeline(t, "reopen handoff original "+workflowTestSuffix())
	if _, err := testPool.Exec(context.Background(), `UPDATE issue SET project_id=$2, status='cancelled', duplicate_of_issue_id=$3 WHERE id=$1`, issueID, projectID, originalID); err != nil {
		t.Fatal(err)
	}
	commentID := dbfx.Comment(t, issueID, "handoff")

	var trigger service.StatusTrigger
	var handoff map[string]string
	testHandler.Bus.Subscribe(protocol.EventIssueUpdated, func(e events.Event) {
		p, _ := e.Payload.(map[string]any)
		if issue, _ := p["issue"].(IssueResponse); issue.ID != issueID {
			return
		}
		trigger, _ = p["status_trigger"].(service.StatusTrigger)
		handoff, _ = p["workflow_handoff"].(map[string]string)
	})
	req := withURLParam(newRequest(http.MethodPatch, "/api/issues/"+issueID, map[string]any{"status": "in_review", "suppress_run": true, "handoff_comment_id": commentID}), "id", issueID)
	req.Header.Set("X-Actor-Source", "interactive_jwt")
	testutil.Call(t, testHandler.UpdateIssue, req).Want(http.StatusOK)

	if handoff == nil {
		t.Fatal("reopen did not hand off")
	}
	if trigger.HandoffCommentID != commentID || trigger.Version != 1 {
		t.Errorf("trigger on a duplicate reopen into a handoff = %+v", trigger)
	}
}
