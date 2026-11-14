package handler

import (
	"net/http"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// CHE-5487 (S3 of CHE-2299): handoff_assignee hands one status move to an
// assignee the mover names instead of the step's handler.

type handoffAssigneeCase struct {
	handlerID, overrideID, projectID, runtimeID string
}

// newHandoffAssigneeCase is a project whose workflow hands in_progress to the
// handler agent, and a second ready agent the mover may name instead.
func newHandoffAssigneeCase(t *testing.T) handoffAssigneeCase {
	t.Helper()
	seedTestCatalog(t)
	c := handoffAssigneeCase{handlerID: seededReadyAgentID(t)}
	dbfx.QueryRow(t, `SELECT runtime_id::text FROM agent WHERE id = $1`, c.handlerID).Scan(&c.runtimeID)
	c.overrideID = dbfx.Agent(t, "Override target "+workflowTestSuffix(), c.runtimeID)
	c.projectID = deliveryProject(t, "Handoff assignee project "+workflowTestSuffix(), c.handlerID)
	return c
}

func (c handoffAssigneeCase) issue(t *testing.T) string {
	t.Helper()
	return dbfx.Issue(t, "handoff assignee", testutil.Cols{"project_id": c.projectID, "status": "todo"})
}

func (c handoffAssigneeCase) agentRuns(t *testing.T, issueID, agentID string) int {
	t.Helper()
	return dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND agent_id = $2`, issueID, agentID)
}

func overrideTo(agentID string) map[string]any {
	return map[string]any{"type": "agent", "id": agentID}
}

// Acceptance 1: the override is assigned and gets the one handoff run.
func TestHandoffAssignee_OverrideIsAssignedAndRun(t *testing.T) {
	c := newHandoffAssigneeCase(t)
	issueID := c.issue(t)

	updateIssueForTest(t, issueID, map[string]any{
		"status": "in_progress", "handoff_assignee": overrideTo(c.overrideID),
	}).Want(http.StatusOK)

	if status, typ, id := issueStatusAndAssignee(t, issueID); status != "in_progress" || typ != "agent" || id != c.overrideID {
		t.Fatalf("issue = (%s, %s, %s), want in_progress assigned to the override %s", status, typ, id, c.overrideID)
	}
	if n := c.agentRuns(t, issueID, c.overrideID); n != 1 {
		t.Errorf("override runs = %d, want 1", n)
	}
	if n := c.agentRuns(t, issueID, c.handlerID); n != 0 {
		t.Errorf("step handler runs = %d, want 0", n)
	}
	var override, stepHandler string
	dbfx.QueryRow(t, `SELECT COALESCE(details->>'handoff_override', ''), COALESCE(details->>'handoff_step_handler', '')
		FROM activity_log WHERE issue_id = $1 AND action = 'status_changed'`, issueID).Scan(&override, &stepHandler)
	if override != "true" || stepHandler != "agent:"+c.handlerID {
		t.Errorf("status history = (override %q, step_handler %q), want (true, agent:%s)", override, stepHandler, c.handlerID)
	}
}

// The override is valid only on a move that hands off.
func TestHandoffAssignee_RefusedWithoutAHandoffMove(t *testing.T) {
	c := newHandoffAssigneeCase(t)
	for name, body := range map[string]map[string]any{
		"no status change":       {"handoff_assignee": overrideTo(c.overrideID)},
		"step without a handoff": {"status": "done", "handoff_assignee": overrideTo(c.overrideID)},
		"with assignee_id":       {"status": "in_progress", "assignee_type": "agent", "assignee_id": c.handlerID, "handoff_assignee": overrideTo(c.overrideID)},
	} {
		t.Run(name, func(t *testing.T) {
			issueID := c.issue(t)
			resp := updateIssueForTest(t, issueID, body)
			resp.Want(http.StatusBadRequest)
			if !strings.Contains(resp.Body.String(), "handoff_assignee_not_applicable") {
				t.Fatalf("body = %s, want handoff_assignee_not_applicable", resp.Body.String())
			}
			if n := c.agentRuns(t, issueID, c.overrideID); n != 0 {
				t.Errorf("override runs = %d, want 0", n)
			}
		})
	}
}

// Acceptance 2: the override passes the gate a hand-picked assignee passes. A
// private agent the mover may not invoke is refused by name.
func TestHandoffAssignee_NotInvocableRefused(t *testing.T) {
	c := newHandoffAssigneeCase(t)
	privateAgentID, _, plainMember := privateAgentTestFixture(t)
	issueID := c.issue(t)

	req := withURLParam(newRequest(http.MethodPatch, "/api/issues/"+issueID, map[string]any{
		"status": "in_progress", "handoff_assignee": overrideTo(privateAgentID),
	}), "id", issueID)
	req.Header.Set("X-User-ID", plainMember)
	resp := testutil.Call(t, testHandler.UpdateIssue, req)
	resp.Want(http.StatusForbidden)
	if !strings.Contains(resp.Body.String(), "handoff_assignee_not_invocable") {
		t.Fatalf("body = %s, want handoff_assignee_not_invocable", resp.Body.String())
	}
	wantIssueStatus(t, issueID, "todo")
	if n := c.agentRuns(t, issueID, privateAgentID); n != 0 {
		t.Errorf("private agent runs = %d, want 0", n)
	}
}

// Acceptance 3: a confirm bound to another previewed assignee is refused.
func TestHandoffAssignee_ConfirmBoundToAnotherAssigneeIsStale(t *testing.T) {
	c := newHandoffAssigneeCase(t)
	binding := func(handlerID string) map[string]any {
		return map[string]any{
			"previous_assignee_type": nil, "previous_assignee_id": nil,
			"handler_type": "agent", "handler_id": handlerID,
		}
	}

	t.Run("previewed the step handler, confirmed an override", func(t *testing.T) {
		issueID := c.issue(t)
		resp := updateIssueForTest(t, issueID, map[string]any{
			"status": "in_progress", "handoff_assignee": overrideTo(c.overrideID),
			"handoff_preview": binding(c.handlerID),
		})
		resp.Want(http.StatusConflict)
		if !strings.Contains(resp.Body.String(), `"stale_preview"`) {
			t.Fatalf("body = %s, want stale_preview", resp.Body.String())
		}
		wantIssueStatus(t, issueID, "todo")
		if n := c.agentRuns(t, issueID, c.overrideID); n != 0 {
			t.Errorf("override runs = %d, want 0", n)
		}
	})

	t.Run("previewed the override, confirmed the same override", func(t *testing.T) {
		issueID := c.issue(t)
		updateIssueForTest(t, issueID, map[string]any{
			"status": "in_progress", "handoff_assignee": overrideTo(c.overrideID),
			"handoff_preview": binding(c.overrideID),
		}).Want(http.StatusOK)
	})
}

// Acceptance 4: a legacy PATCH that sets assignee and status together is not a
// handoff.
func TestHandoffAssignee_LegacyStatusAndAssigneeSkipsHandoff(t *testing.T) {
	c := newHandoffAssigneeCase(t)
	issueID := c.issue(t)

	updateIssueForTest(t, issueID, map[string]any{
		"status": "in_progress", "assignee_type": "agent", "assignee_id": c.overrideID,
	}).Want(http.StatusOK)

	if _, _, id := issueStatusAndAssignee(t, issueID); id != c.overrideID {
		t.Fatalf("assignee = %s, want the explicit choice %s", id, c.overrideID)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM activity_log WHERE issue_id = $1 AND details ? 'handoff_workflow'`, issueID); n != 0 {
		t.Errorf("handoff rows = %d, want 0: a legacy write is not a handoff", n)
	}
}

// The preview answers for the override: who receives the issue and what they
// are told.
func TestHandoffAssignee_PreviewNamesTheOverride(t *testing.T) {
	c := newHandoffAssigneeCase(t)
	issueID := c.issue(t)

	req := withURLParam(newRequest("GET", "/api/issues/"+issueID+"/workflow-handoff?status=in_progress&handoff_assignee_type=agent&handoff_assignee_id="+c.overrideID, nil), "id", issueID)
	var got WorkflowHandoffPreviewResponse
	testutil.Call(t, testHandler.PreviewWorkflowHandoff, req).Want(http.StatusOK).JSON(&got)
	if !got.Handoff || got.HandlerID != c.overrideID || !got.Override || got.StepHandlerID != c.handlerID {
		t.Fatalf("preview = %+v, want a handoff to the override %s over step handler %s", got, c.overrideID, c.handlerID)
	}
	if !strings.Contains(got.Brief, "Implement it and open a PR.") {
		t.Fatalf("preview brief is missing the step instructions:\n%s", got.Brief)
	}
}

// The route endpoint shares the decision: the same override works there.
func TestHandoffAssignee_RouteEndpointOverride(t *testing.T) {
	c := newRouteCase(t)
	other := c.otherAgent(t)

	c.route(t, routeBody("in_review", map[string]any{"handoff_assignee": overrideTo(other)}), nil).Want(http.StatusOK)

	if _, _, id := issueStatusAndAssignee(t, c.issueID); id != other {
		t.Fatalf("assignee = %s, want the override %s", id, other)
	}
	if runs := pendingStepRuns(t, c.issueID, c.handlerID); len(runs) != 0 {
		t.Errorf("step handler runs = %v, want none", runs)
	}
}
