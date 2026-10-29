package handler

import (
	"context"
	"net/http"
	"testing"
)

// CHE-2398: with pr_merge_status set to a status, PR auto-complete used to move
// an issue before the workflow's pr.merged edge ran, skipping the target
// step's handoff. A step with a pr.merged edge now belongs to the workflow.

var prMergedEdgeSteps = []map[string]any{
	{"status_key": "backlog"},
	{"status_key": "todo"},
	{"status_key": "in_progress"},
	{"status_key": "in_review", "edges": []map[string]any{{"to": "merged", "guard": "pr.merged", "timeout": "72h", "on_timeout": "blocked"}}},
	{"status_key": "merged"},
	{"status_key": "blocked"},
}

// prMergeIssueInReview is an issue with a linked PR sitting in_review in a
// project on a workflow whose merged step hands off to an agent.
func prMergeIssueInReview(t *testing.T, steps []map[string]any, installationID int64) (issueID, identifier, agentID string) {
	t.Helper()
	seedTestCatalog(t)
	createTestCustomStatus(t, "merged", "started")
	agentID = seededReadyAgentID(t)
	wf := createTestWorkflowSteps(t, "Merge edge "+workflowTestSuffix(), steps)
	setWorkflowHandoff(t, wf.ID, "merged", handToAgent(agentID, "Verify landing.")).Want(http.StatusOK)
	projectID := createWorkflowTestProject(t, "Merge edge project")
	setProjectWorkflow(t, projectID, map[string]any{"workflow_id": wf.ID}).Want(http.StatusOK)
	issue := prAutoCompleteTestIssue(t, "merge edge", installationID)
	if _, err := testPool.Exec(context.Background(),
		`UPDATE issue SET project_id=$2, status='in_review', assignee_type=NULL, assignee_id=NULL WHERE id=$1`, issue.ID, projectID); err != nil {
		t.Fatal(err)
	}
	return issue.ID, issue.Identifier, agentID
}

func TestPRAutoCompleteLeavesPRMergedEdgeToWorkflow(t *testing.T) {
	if testHandler == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	const installationID int64 = 2398001
	secret := "che2398-secret"
	t.Setenv("GITHUB_WEBHOOK_SECRET", secret)
	issueID, identifier, agentID := prMergeIssueInReview(t, prMergedEdgeSteps, installationID)
	setWorkspacePRMergeStatusForTest(t, "merged")

	firePRWebhook(t, secret, installationID, 1, identifier+": work", "", "feat/che2398", "merged")

	if got := issueStatusForTest(t, issueID); got != "in_review" {
		t.Fatalf("auto-complete moved the issue to %q before the workflow edge ran", got)
	}
	if got := prAutoCompleteStateForTest(t, issueID).State; got != prAutoCompleteWorkflowOwned {
		t.Fatalf("auto_complete state = %q, want %q", got, prAutoCompleteWorkflowOwned)
	}
	tickEdges(t)
	status, assigneeType, assigneeID := issueStatusAndAssignee(t, issueID)
	if status != "merged" || assigneeType != "agent" || assigneeID != agentID {
		t.Fatalf("after the edge: status=%q assignee=%s/%s, want merged and agent %s", status, assigneeType, assigneeID, agentID)
	}
}

// A step without a pr.merged edge still auto-completes as before.
func TestPRAutoCompleteStillMovesStepWithoutPRMergedEdge(t *testing.T) {
	if testHandler == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	const installationID int64 = 2398002
	secret := "che2398-secret"
	t.Setenv("GITHUB_WEBHOOK_SECRET", secret)
	steps := []map[string]any{
		{"status_key": "backlog"}, {"status_key": "todo"}, {"status_key": "in_progress"},
		{"status_key": "in_review"}, {"status_key": "merged"}, {"status_key": "blocked"},
	}
	issueID, identifier, _ := prMergeIssueInReview(t, steps, installationID)
	setWorkspacePRMergeStatusForTest(t, "merged")

	firePRWebhook(t, secret, installationID, 2, identifier+": work", "", "feat/che2398b", "merged")

	if got := issueStatusForTest(t, issueID); got != "merged" {
		t.Fatalf("status = %q, want merged", got)
	}
}
