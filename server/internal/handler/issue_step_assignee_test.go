package handler

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// CHE-5490 (S4 of CHE-2299): keep_for_step makes a handoff_assignee sticky for
// the target step, so later returns to that step go to the same assignee.

func keepFor(agentID string) map[string]any {
	return map[string]any{
		"status": "in_progress", "handoff_assignee": overrideTo(agentID), "keep_for_step": true,
	}
}

func stickyAssignee(t *testing.T, issueID, status string) (typ, id string, found bool) {
	t.Helper()
	rows := dbfx.Count(t, `SELECT count(*) FROM issue_step_assignee WHERE issue_id = $1 AND status_key = $2`, issueID, status)
	if rows == 0 {
		return "", "", false
	}
	dbfx.QueryRow(t, `SELECT assignee_type, assignee_id::text FROM issue_step_assignee WHERE issue_id = $1 AND status_key = $2`, issueID, status).Scan(&typ, &id)
	return typ, id, true
}

func TestStepAssignee_SetOnKeepForStep(t *testing.T) {
	c := newHandoffAssigneeCase(t)
	issueID := c.issue(t)

	updateIssueForTest(t, issueID, keepFor(c.overrideID)).Want(http.StatusOK)

	typ, id, found := stickyAssignee(t, issueID, "in_progress")
	if !found || typ != "agent" || id != c.overrideID {
		t.Fatalf("sticky row = (%s, %s, found=%v), want agent %s", typ, id, found, c.overrideID)
	}
}

func TestStepAssignee_NoRowWithoutKeepForStep(t *testing.T) {
	c := newHandoffAssigneeCase(t)
	issueID := c.issue(t)

	updateIssueForTest(t, issueID, map[string]any{
		"status": "in_progress", "handoff_assignee": overrideTo(c.overrideID),
	}).Want(http.StatusOK)

	if _, _, found := stickyAssignee(t, issueID, "in_progress"); found {
		t.Fatal("an override without keep_for_step left a sticky row")
	}
}

func TestStepAssignee_KeepForStepNeedsAnOverride(t *testing.T) {
	c := newHandoffAssigneeCase(t)
	issueID := c.issue(t)

	resp := updateIssueForTest(t, issueID, map[string]any{"status": "in_progress", "keep_for_step": true})
	resp.Want(http.StatusBadRequest)
	if !strings.Contains(resp.Body.String(), "keep_for_step") {
		t.Fatalf("body = %s, want the refusal to name keep_for_step", resp.Body.String())
	}
	wantIssueStatus(t, issueID, "todo")
}

func TestStepAssignee_ReplaceKeepsOneRow(t *testing.T) {
	c := newHandoffAssigneeCase(t)
	issueID := c.issue(t)
	second := dbfx.Agent(t, "Second override "+workflowTestSuffix(), c.runtimeID)

	updateIssueForTest(t, issueID, keepFor(c.overrideID)).Want(http.StatusOK)
	updateIssueForTest(t, issueID, map[string]any{"status": "todo"}).Want(http.StatusOK)
	updateIssueForTest(t, issueID, keepFor(second)).Want(http.StatusOK)

	if n := dbfx.Count(t, `SELECT count(*) FROM issue_step_assignee WHERE issue_id = $1`, issueID); n != 1 {
		t.Fatalf("sticky rows = %d, want 1", n)
	}
	if _, id, _ := stickyAssignee(t, issueID, "in_progress"); id != second {
		t.Fatalf("sticky assignee = %s, want the replacement %s", id, second)
	}
}

// Returning to a step with no override goes to the sticky assignee, not the
// step handler, and starts its run.
func TestStepAssignee_ReturnToStepUsesSticky(t *testing.T) {
	c := newHandoffAssigneeCase(t)
	issueID := c.issue(t)

	updateIssueForTest(t, issueID, keepFor(c.overrideID)).Want(http.StatusOK)
	updateIssueForTest(t, issueID, map[string]any{"status": "todo"}).Want(http.StatusOK)
	// The first run finishes, and someone else takes the issue meanwhile.
	dbfx.Exec(t, `UPDATE agent_task_queue SET status = 'completed' WHERE issue_id = $1`, issueID)
	dbfx.Exec(t, `UPDATE issue SET assignee_type = 'agent', assignee_id = $2 WHERE id = $1`, issueID, c.handlerID)
	runsBefore := c.agentRuns(t, issueID, c.overrideID)

	updateIssueForTest(t, issueID, map[string]any{"status": "in_progress"}).Want(http.StatusOK)

	if _, typ, id := issueStatusAndAssignee(t, issueID); typ != "agent" || id != c.overrideID {
		t.Fatalf("assignee = (%s, %s), want the sticky agent %s", typ, id, c.overrideID)
	}
	if n := c.agentRuns(t, issueID, c.overrideID); n != runsBefore+1 {
		t.Errorf("sticky agent runs = %d, want %d", n, runsBefore+1)
	}
	var override, stepHandler string
	dbfx.QueryRow(t, `SELECT COALESCE(details->>'handoff_override', ''), COALESCE(details->>'handoff_step_handler', '')
		FROM activity_log WHERE issue_id = $1 AND action = 'status_changed' ORDER BY created_at DESC LIMIT 1`, issueID).Scan(&override, &stepHandler)
	if override != "true" || stepHandler != "agent:"+c.handlerID {
		t.Errorf("status history = (override %q, step_handler %q), want (true, agent:%s)", override, stepHandler, c.handlerID)
	}
}

// An explicit override on the return beats the sticky row.
func TestStepAssignee_ExplicitOverrideBeatsSticky(t *testing.T) {
	c := newHandoffAssigneeCase(t)
	issueID := c.issue(t)
	other := dbfx.Agent(t, "Explicit override "+workflowTestSuffix(), c.runtimeID)

	updateIssueForTest(t, issueID, keepFor(c.overrideID)).Want(http.StatusOK)
	updateIssueForTest(t, issueID, map[string]any{"status": "todo"}).Want(http.StatusOK)
	updateIssueForTest(t, issueID, map[string]any{
		"status": "in_progress", "handoff_assignee": overrideTo(other),
	}).Want(http.StatusOK)

	if _, _, id := issueStatusAndAssignee(t, issueID); id != other {
		t.Fatalf("assignee = %s, want the explicit override %s", id, other)
	}
	if _, id, _ := stickyAssignee(t, issueID, "in_progress"); id != c.overrideID {
		t.Fatalf("sticky assignee = %s, want it unchanged at %s", id, c.overrideID)
	}
}

func previewKept(t *testing.T, issueID string) map[string]any {
	t.Helper()
	req := withURLParam(newRequest("GET", "/api/issues/"+issueID+"/workflow-handoff?status=in_progress", nil), "id", issueID)
	var got map[string]any
	testutil.Call(t, testHandler.PreviewWorkflowHandoff, req).Want(http.StatusOK).JSON(&got)
	return got
}

// The preview answers for the sticky row: who receives the issue, and that it
// is kept for this step.
func TestStepAssignee_PreviewShowsKeptForStep(t *testing.T) {
	c := newHandoffAssigneeCase(t)
	issueID := c.issue(t)

	got := previewKept(t, issueID)
	if got["handler_id"] != c.handlerID || got["kept_for_step"] == true {
		t.Fatalf("preview without a sticky row = %v, want the step handler %s, not kept", got, c.handlerID)
	}

	updateIssueForTest(t, issueID, keepFor(c.overrideID)).Want(http.StatusOK)
	updateIssueForTest(t, issueID, map[string]any{"status": "todo"}).Want(http.StatusOK)

	got = previewKept(t, issueID)
	if got["handler_id"] != c.overrideID || got["kept_for_step"] != true || got["step_handler_id"] != c.handlerID {
		t.Fatalf("preview = %v, want handler %s kept for this step over step handler %s", got, c.overrideID, c.handlerID)
	}
}

// A confirm bound to a preview of the old sticky assignee is refused once the
// sticky row has changed, and moves nothing.
func TestStepAssignee_ConfirmWithStalePreviewRefused(t *testing.T) {
	c := newHandoffAssigneeCase(t)
	issueID := c.issue(t)
	replacement := dbfx.Agent(t, "Sticky replacement "+workflowTestSuffix(), c.runtimeID)

	updateIssueForTest(t, issueID, keepFor(c.overrideID)).Want(http.StatusOK)
	updateIssueForTest(t, issueID, map[string]any{"status": "todo"}).Want(http.StatusOK)

	previewed := previewKept(t, issueID)
	// Another writer replaces the sticky row after the preview.
	dbfx.Exec(t, `UPDATE issue_step_assignee SET assignee_id = $2 WHERE issue_id = $1 AND status_key = 'in_progress'`, issueID, replacement)

	var prevType, prevID any
	if s, ok := previewed["previous_assignee_type"].(string); ok {
		prevType = s
	}
	if s, ok := previewed["previous_assignee_id"].(string); ok {
		prevID = s
	}
	resp := updateIssueForTest(t, issueID, map[string]any{
		"status": "in_progress",
		"handoff_preview": map[string]any{
			"previous_assignee_type": prevType, "previous_assignee_id": prevID,
			"handler_type": previewed["handler_type"], "handler_id": previewed["handler_id"],
		},
	})
	resp.Want(http.StatusConflict)
	var body struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &body); err != nil || body.Code != "stale_preview" {
		t.Fatalf("body = %s, want code stale_preview", resp.Body.String())
	}
	wantIssueStatus(t, issueID, "todo")
}

// The route endpoint shares the decision: keep_for_step works there too.
func TestStepAssignee_RouteEndpointKeepsForStep(t *testing.T) {
	c := newRouteCase(t)
	other := c.otherAgent(t)

	c.route(t, routeBody("in_review", map[string]any{
		"handoff_assignee": overrideTo(other), "keep_for_step": true,
	}), nil).Want(http.StatusOK)

	if typ, id, found := stickyAssignee(t, c.issueID, "in_review"); !found || typ != "agent" || id != other {
		t.Fatalf("sticky row = (%s, %s, found=%v), want agent %s on in_review", typ, id, found, other)
	}
}
