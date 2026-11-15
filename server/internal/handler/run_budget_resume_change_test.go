package handler

import (
	"encoding/json"
	"testing"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

// A workflow-wide budget change or an issue rebinding gives refused issues runs
// again: the executed action links their refusals and the P3 worker resumes
// each once (CHE-5508).

func budgetChangeAction(t *testing.T, typ string, params map[string]string) db.DecisionAction {
	t.Helper()
	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	return db.DecisionAction{ID: dbid.NewV7(), WorkspaceID: parseUUID(testWorkspaceID), ActionType: typ, Params: raw, State: "executed"}
}

func applyBudgetChange(t *testing.T, action db.DecisionAction) {
	t.Helper()
	if err := testHandler.resumeRefusalsAfterBudgetChange(t.Context(), action); err != nil {
		t.Fatal(err)
	}
}

func setWorkflowCap(t *testing.T, workflowID string, cap int) {
	t.Helper()
	dbfx.Exec(t, `UPDATE issue_workflow SET max_runs_per_issue=$2 WHERE id=$1`, workflowID, cap)
}

func TestWorkflowBudgetSetResumesEveryRefusedIssueOnceAndOnlyThatWorkflows(t *testing.T) {
	c, id := blockedCase(t, 5508001)
	other, otherID := blockedCase(t, 5508002)
	setWorkflowCap(t, c.workflowID, 30)
	setWorkflowCap(t, other.workflowID, 30) // the other workflow also has room, but this action is not about it
	action := budgetChangeAction(t, "workflow.budget.set", map[string]string{"workflow_id": c.workflowID})

	applyBudgetChange(t, action)
	if r := wantResume(t, id, "status_restored", ""); r.ActionID != action.ID {
		t.Fatalf("refusal linked to %v, want this action %v", r.ActionID, action.ID)
	}
	if got, n := c.status(t), resumeQueued(t, c); got != "in_progress" || n != 0 && n != 1 {
		t.Fatalf("issue %s with %d queued runs", got, n)
	}
	wantResume(t, otherID, "open", "")
	if got := other.status(t); got != "blocked" {
		t.Fatalf("issue of another workflow is %s, want blocked", got)
	}

	before := resumeQueued(t, c)
	applyBudgetChange(t, budgetChangeAction(t, "workflow.budget.set", map[string]string{"workflow_id": c.workflowID}))
	if after := resumeQueued(t, c); after != before {
		t.Fatalf("a second change queued %d more runs, want 0", after-before)
	}
	wantResume(t, id, "status_restored", "")
}

func TestWorkflowBudgetSetLeavesAnIssueStillOverBudgetBlocked(t *testing.T) {
	c, id := blockedCase(t, 5508003)
	setWorkflowCap(t, c.workflowID, 2) // no more room than before
	applyBudgetChange(t, budgetChangeAction(t, "workflow.budget.set", map[string]string{"workflow_id": c.workflowID}))
	if r := wantResume(t, id, "open", ""); r.ActionID.Valid {
		t.Fatalf("refusal linked to %v, want none", r.ActionID)
	}
	if got := c.status(t); got != "blocked" {
		t.Fatalf("issue is %s, want blocked", got)
	}
}

func TestIssueWorkflowSetResumesThatIssueOnce(t *testing.T) {
	c, id := blockedCase(t, 5508004)
	setWorkflowCap(t, c.workflowID, 30)
	action := budgetChangeAction(t, "issue.workflow.set", map[string]string{"issue_id": c.issueID})
	applyBudgetChange(t, action)
	if r := wantResume(t, id, "status_restored", ""); r.ActionID != action.ID {
		t.Fatalf("refusal linked to %v, want %v", r.ActionID, action.ID)
	}
	if got := c.status(t); got != "in_progress" {
		t.Fatalf("issue is %s, want in_progress", got)
	}
	applyBudgetChange(t, budgetChangeAction(t, "issue.workflow.set", map[string]string{"issue_id": c.issueID}))
	wantResume(t, id, "status_restored", "")
}
