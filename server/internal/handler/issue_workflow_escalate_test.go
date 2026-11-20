package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
)

// CHE-7682 (slice 17 of CHE-7658): `issue workflow escalate`. An issue that
// follows Quick moves, with no card, to the full workflow named by its newest
// quick_entry row. A live task parks the switch in the CHE-7669 pending row;
// with none the switch applies at once. An agent move into Quick opens the
// issue.workflow card.

type escalateFixture struct {
	full, quick, other, handler string
}

func newEscalateFixture(t *testing.T) escalateFixture {
	t.Helper()
	seedTestCatalog(t)
	resetDefaultWorkflow(t)
	f := escalateFixture{handler: seededReadyAgentID(t)}
	steps := []string{"todo", "in_progress", "in_review"}
	f.full = createTestWorkflow(t, "escalate-full-"+workflowTestSuffix(), steps...).ID
	f.other = createTestWorkflow(t, "escalate-other-"+workflowTestSuffix(), steps...).ID
	f.quick = createTestWorkflow(t, "Quick", steps...).ID
	for _, status := range []string{"todo", "in_progress"} {
		setWorkflowHandoff(t, f.full, status, handToAgent(f.handler, "Full workflow "+status)).Want(http.StatusOK)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM issue_workflow_switch_pending WHERE workspace_id = $1`, testWorkspaceID)
	})
	return f
}

// issueOnQuick is an issue that followed `prior`, was switched to Quick by a
// member (which writes its quick_entry row) and sits on status.
func (f escalateFixture) issueOnQuick(t *testing.T, status, description string) string {
	t.Helper()
	id := dbfx.Issue(t, "escalate "+workflowTestSuffix(), testutil.Cols{"status": status, "description": description})
	setIssueOverride(t, id, f.full)
	putIssueWorkflow(t, id, f.quick).Want(http.StatusOK)
	return id
}

type escalateResult struct {
	State      string `json:"state"`
	WorkflowID string `json:"workflow_id"`
	Code       string `json:"code"`
}

// escalate calls the endpoint as an agent run (task token), the caller the
// command is for.
func escalateCall(t *testing.T, issueID string, body map[string]any) *testutil.Response {
	t.Helper()
	req := withActorSource(newRequest("POST", "/api/issues/"+issueID+"/workflow/escalate", body), "task_token")
	req.Header.Set("X-Agent-ID", seededReadyAgentID(t))
	return testutil.Call(t, testHandler.EscalateIssueWorkflow, withURLParam(req, "id", issueID))
}

func escalateActivities(t *testing.T, issueID, action string) []map[string]any {
	t.Helper()
	rows, err := testPool.Query(context.Background(),
		`SELECT details, COALESCE(actor_type, '') FROM activity_log WHERE issue_id = $1 AND action = $2 ORDER BY created_at, id`, issueID, action)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var raw []byte
		var actor string
		if err := rows.Scan(&raw, &actor); err != nil {
			t.Fatal(err)
		}
		d := map[string]any{}
		if err := json.Unmarshal(raw, &d); err != nil {
			t.Fatalf("%s details %s: %v", action, raw, err)
		}
		d["_actor_type"] = actor
		out = append(out, d)
	}
	return out
}

func escalateQueuedRuns(t *testing.T, issueID string) int {
	t.Helper()
	return dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND status = 'queued'`, issueID)
}

func escalatePendingRows(t *testing.T, issueID string) int {
	t.Helper()
	return dbfx.Count(t, `SELECT count(*) FROM issue_workflow_switch_pending WHERE issue_id = $1`, issueID)
}

// The pre-activation canary: an issue follows its workflow, a member overrides
// it into Quick, and escalate returns it to that prior binding with exactly
// one run for the full workflow's step.
func TestEscalateAfterOverrideReturnsToThePriorBindingWithOneRun(t *testing.T) {
	f := newEscalateFixture(t)
	id := f.issueOnQuick(t, "in_progress", "")
	if got := issueOverride(t, id); got != f.quick {
		t.Fatalf("setup: issue follows %s, want Quick %s", got, f.quick)
	}

	var res escalateResult
	escalateCall(t, id, map[string]any{"reason": "needs a schema change"}).Want(http.StatusOK).JSON(&res)

	if res.State != "applied" || res.WorkflowID != f.full {
		t.Fatalf("result = %+v, want applied on %s", res, f.full)
	}
	if got := issueOverride(t, id); got != f.full {
		t.Fatalf("issue follows %s, want the prior workflow %s", got, f.full)
	}
	if n := escalateQueuedRuns(t, id); n != 1 {
		t.Fatalf("queued runs = %d, want exactly one for the full workflow's step", n)
	}
	if _, at, aid := issueStatusAndAssignee(t, id); at != "agent" || aid != f.handler {
		t.Fatalf("assignee = %s/%s, want the full workflow's handler %s", at, aid, f.handler)
	}
	if n := escalatePendingRows(t, id); n != 0 {
		t.Fatalf("pending rows = %d after an idle apply", n)
	}
	esc := escalateActivities(t, id, "workflow_escalated")
	if len(esc) != 1 || esc[0]["reason"] != "needs a schema change" || esc[0]["workflow_id"] != f.full || esc[0]["_actor_type"] != "agent" {
		t.Fatalf("workflow_escalated rows = %+v", esc)
	}
	calls := escalateActivities(t, id, "intake_call")
	if len(calls) != 1 || calls[0]["call"] != "escalated" || calls[0]["step"] != "in_progress" {
		t.Fatalf("intake_call rows = %+v, want one escalated call on in_progress", calls)
	}
}

// A live task keeps the switch pending; the issue follows Quick until it ends,
// then the full workflow starts exactly one run.
func TestEscalateWithALiveTaskAppliesAfterTheRun(t *testing.T) {
	f := newEscalateFixture(t)
	id := f.issueOnQuick(t, "in_progress", "")
	running := dbfx.Task(t, f.handler, testutil.Cols{
		"issue_id": id, "status": "running",
		"runtime_id": testutil.Raw("(SELECT runtime_id FROM agent WHERE id='" + f.handler + "')"),
	})

	var res escalateResult
	escalateCall(t, id, map[string]any{"reason": "too big"}).Want(http.StatusAccepted).JSON(&res)
	if res.State != "pending" {
		t.Fatalf("result = %+v, want pending", res)
	}
	if got := issueOverride(t, id); got != f.quick {
		t.Fatalf("issue follows %s while its run lives, want Quick %s", got, f.quick)
	}
	if escalatePendingRows(t, id) != 1 || escalateQueuedRuns(t, id) != 0 {
		t.Fatalf("rows=%d queued=%d, want one pending row and no run yet", escalatePendingRows(t, id), escalateQueuedRuns(t, id))
	}
	// The reason and the call are logged when the request is made.
	if len(escalateActivities(t, id, "workflow_escalated")) != 1 || len(escalateActivities(t, id, "intake_call")) != 1 {
		t.Fatal("escalated activities are not written at request time")
	}

	setTaskState(t, running, "completed")
	testHandler.ApplyDeferredWorkflowSwitch(context.Background(), util.MustParseUUID(id))

	if got := issueOverride(t, id); got != f.full {
		t.Fatalf("issue follows %s after the run, want %s", got, f.full)
	}
	if n := escalateQueuedRuns(t, id); n != 1 {
		t.Fatalf("queued runs after the apply = %d, want 1", n)
	}
}

// The target is the newest quick_entry row, never the caller's word.
func TestEscalateRefusesACraftedTarget(t *testing.T) {
	f := newEscalateFixture(t)
	id := f.issueOnQuick(t, "in_progress", "")

	for _, key := range []string{"workflow_id", "full_workflow_id", "target"} {
		escalateCall(t, id, map[string]any{"reason": "x", key: f.other}).Want(http.StatusBadRequest)
	}
	if got := issueOverride(t, id); got != f.quick {
		t.Fatalf("a refused call changed the workflow to %s", got)
	}
	if len(escalateActivities(t, id, "workflow_escalated")) != 0 {
		t.Fatal("a refused call wrote an activity")
	}
	// The newest row wins over an older one.
	putIssueWorkflow(t, id, f.other).Want(http.StatusOK)
	putIssueWorkflow(t, id, f.quick).Want(http.StatusOK)
	var res escalateResult
	escalateCall(t, id, map[string]any{"reason": "x"}).Want(http.StatusOK).JSON(&res)
	if res.WorkflowID != f.other {
		t.Fatalf("escalated to %s, want the newest row's workflow %s", res.WorkflowID, f.other)
	}
}

func TestEscalateRefusesWithoutAReasonOrQuickEvidence(t *testing.T) {
	f := newEscalateFixture(t)

	t.Run("no reason", func(t *testing.T) {
		id := f.issueOnQuick(t, "in_progress", "")
		escalateCall(t, id, map[string]any{"reason": "  "}).Want(http.StatusBadRequest)
	})
	t.Run("bound to Quick with no quick_entry row", func(t *testing.T) {
		id := dbfx.Issue(t, "escalate no row "+workflowTestSuffix(), testutil.Cols{"status": "in_progress"})
		setIssueOverride(t, id, f.quick)
		var res escalateResult
		escalateCall(t, id, map[string]any{"reason": "x"}).Want(http.StatusConflict).JSON(&res)
		if res.Code != "no_full_workflow" {
			t.Fatalf("code = %q, want no_full_workflow", res.Code)
		}
		if got := issueOverride(t, id); got != f.quick {
			t.Fatalf("a refused call changed the workflow to %s", got)
		}
	})
	t.Run("a quick_entry row with no prior binding", func(t *testing.T) {
		id := dbfx.Issue(t, "escalate unbound "+workflowTestSuffix(), testutil.Cols{"status": "in_progress"})
		putIssueWorkflow(t, id, f.quick).Want(http.StatusOK)
		var res escalateResult
		escalateCall(t, id, map[string]any{"reason": "x"}).Want(http.StatusConflict).JSON(&res)
		if res.Code != "no_full_workflow" {
			t.Fatalf("code = %q, want no_full_workflow", res.Code)
		}
	})
	t.Run("an issue that is not on Quick", func(t *testing.T) {
		id := f.issueOnQuick(t, "in_progress", "")
		setIssueOverride(t, id, f.other)
		var res escalateResult
		escalateCall(t, id, map[string]any{"reason": "x"}).Want(http.StatusConflict).JSON(&res)
		if res.Code != "not_quick" {
			t.Fatalf("code = %q, want not_quick", res.Code)
		}
	})
}

func TestEscalateFromTodoNeedsAnEstimateUnlessRollback(t *testing.T) {
	f := newEscalateFixture(t)

	t.Run("missing estimate refused", func(t *testing.T) {
		id := f.issueOnQuick(t, "todo", "")
		var res escalateResult
		escalateCall(t, id, map[string]any{"reason": "too big"}).Want(http.StatusUnprocessableEntity).JSON(&res)
		if res.Code != "estimate_required" {
			t.Fatalf("code = %q, want estimate_required", res.Code)
		}
		if got := issueOverride(t, id); got != f.quick || escalatePendingRows(t, id) != 0 {
			t.Fatalf("a refused call changed the issue: workflow %s, pending %d", got, escalatePendingRows(t, id))
		}
		if len(escalateActivities(t, id, "workflow_escalated")) != 0 || len(escalateActivities(t, id, "intake_call")) != 0 {
			t.Fatal("a refused call wrote an activity")
		}
	})
	t.Run("an Estimate line is accepted and logged", func(t *testing.T) {
		id := f.issueOnQuick(t, "todo", "Estimate: 6 h\nRepos: 1")
		escalateCall(t, id, map[string]any{"reason": "too big"}).Want(http.StatusOK)
		calls := escalateActivities(t, id, "intake_call")
		if len(calls) != 1 || calls[0]["call"] != "escalated" || calls[0]["estimate"] != "6 h" || calls[0]["step"] != "todo" {
			t.Fatalf("intake_call rows = %+v", calls)
		}
	})
	t.Run("rollback needs no estimate", func(t *testing.T) {
		id := f.issueOnQuick(t, "todo", "")
		escalateCall(t, id, map[string]any{"reason": "rollback"}).Want(http.StatusOK)
		if got := issueOverride(t, id); got != f.full {
			t.Fatalf("issue follows %s, want %s", got, f.full)
		}
		calls := escalateActivities(t, id, "intake_call")
		if len(calls) != 1 || calls[0]["call"] != "escalated" {
			t.Fatalf("intake_call rows = %+v", calls)
		}
	})
	t.Run("in_progress needs no estimate", func(t *testing.T) {
		id := f.issueOnQuick(t, "in_progress", "")
		escalateCall(t, id, map[string]any{"reason": "found a migration"}).Want(http.StatusOK)
	})
}

// A done issue that was reopened and bound to Quick leaves through escalate
// like any other: it follows its prior workflow and is handed to no one.
func TestEscalateAFinishedQuickIssueFollowsItsPriorWorkflow(t *testing.T) {
	f := newEscalateFixture(t)
	id := f.issueOnQuick(t, "done", "")

	escalateCall(t, id, map[string]any{"reason": "reopened as a larger change"}).Want(http.StatusOK)

	if got := issueOverride(t, id); got != f.full {
		t.Fatalf("issue follows %s, want %s", got, f.full)
	}
	wantIssueStatus(t, id, "done")
	if n := escalateQueuedRuns(t, id); n != 0 {
		t.Fatalf("queued runs = %d, a finished issue is handed to no one", n)
	}
}

// Moves into Quick: an agent's opens the owner card (CHE-5228) whatever the
// budget does; a member still switches at once.
func TestAgentMoveIntoQuickOpensTheCard(t *testing.T) {
	f := newEscalateFixture(t)

	t.Run("agent", func(t *testing.T) {
		id := dbfx.Issue(t, "de-escalate "+workflowTestSuffix(), testutil.Cols{"status": "in_progress"})
		setIssueOverride(t, id, f.full)
		req := withURLParam(withActorSource(newRequest("PUT", "/api/issues/"+id+"/workflow", map[string]any{"workflow_id": f.quick}), "task_token"), "id", id)
		var opened workflowSetOpened
		testutil.Call(t, testHandler.SetIssueWorkflow, req).Want(http.StatusAccepted).JSON(&opened)
		if opened.ActionID == "" {
			t.Fatalf("no card opened: %+v", opened)
		}
		dbfx.Cleanup(t, `DELETE FROM decision_action WHERE id = $1`, opened.ActionID)
		dbfx.Cleanup(t, `DELETE FROM decision_answer WHERE decision_request_id = $1`, opened.RequestID)
		dbfx.Cleanup(t, `DELETE FROM decision_request WHERE id = $1`, opened.RequestID)
		if got := pendingWorkflowSetActions(t, id); got != 1 {
			t.Fatalf("pending issue.workflow.set actions = %d, want 1", got)
		}
		if got := issueOverride(t, id); got != f.full {
			t.Fatalf("opening the card changed the workflow to %s", got)
		}
	})
	t.Run("agent move that is not into Quick needs no card", func(t *testing.T) {
		id := dbfx.Issue(t, "elsewhere "+workflowTestSuffix(), testutil.Cols{"status": "in_progress"})
		// other was created after full, so the move back to full widens no budget.
		setIssueOverride(t, id, f.other)
		req := withURLParam(withActorSource(newRequest("PUT", "/api/issues/"+id+"/workflow", map[string]any{"workflow_id": f.full}), "task_token"), "id", id)
		testutil.Call(t, testHandler.SetIssueWorkflow, req).Want(http.StatusOK)
		if got := issueOverride(t, id); got != f.full {
			t.Fatalf("issue follows %s, want %s", got, f.full)
		}
	})
	t.Run("member switches freely", func(t *testing.T) {
		id := dbfx.Issue(t, "member "+workflowTestSuffix(), testutil.Cols{"status": "in_progress"})
		setIssueOverride(t, id, f.full)
		putIssueWorkflow(t, id, f.quick).Want(http.StatusOK)
		if got := issueOverride(t, id); got != f.quick {
			t.Fatalf("issue follows %s, want Quick %s", got, f.quick)
		}
	})
}
