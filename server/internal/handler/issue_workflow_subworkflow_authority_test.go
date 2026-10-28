package handler

import (
	"context"
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func TestSubWorkflowNestedPreservesRequesterAuthorization(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	readyAgent := seededReadyAgentID(t)
	privateAgent, ownerID, requesterID := privateAgentTestFixture(t)
	seedTestCatalog(t)
	leaf := createTestWorkflowSteps(t, "Reviewer private leaf "+workflowTestSuffix(), []map[string]any{
		{"status_key": "todo", "handler": map[string]any{"type": "agent", "id": readyAgent}, "instructions": "Review fixture only."},
		{"status_key": "in_progress"}, {"status_key": "in_review"}, {"status_key": "blocked"},
	})
	if _, err := testPool.Exec(ctx, `UPDATE issue_workflow SET steps=replace(steps::text,$2,$3)::jsonb WHERE id=$1`, leaf.ID, readyAgent, privateAgent); err != nil {
		t.Fatal(err)
	}
	parentCase, _ := newSubWorkflowCaseTo(t, 1, subWorkflowSteps(leaf.ID, 2))
	if _, err := testPool.Exec(ctx, `UPDATE issue SET creator_type='member',creator_id=$2,requested_by_id=$3 WHERE id=$1`, parentCase.issueID, ownerID, requesterID); err != nil {
		t.Fatal(err)
	}
	agent, err := testHandler.Queries.GetAgentInWorkspace(ctx, db.GetAgentInWorkspaceParams{ID: parseUUID(privateAgent), WorkspaceID: parseUUID(testWorkspaceID)})
	if err != nil {
		t.Fatal(err)
	}
	if testHandler.canInvokeAgent(ctx, agent, "system", "", requesterID, testWorkspaceID) {
		t.Fatal("negative control: requester unexpectedly allowed to invoke private agent")
	}
	parentCase.enter(t, "in_progress")
	tickEdges(t)
	var childID string
	var requesterPreserved bool
	dbfx.QueryRow(t, `SELECT id::text,COALESCE(requested_by_id=$2::uuid,false) FROM issue WHERE parent_issue_id=$1`, parentCase.issueID, requesterID).Scan(&childID, &requesterPreserved)
	t.Logf("intermediate child preserves original requester: %v", requesterPreserved)
	t.Cleanup(func() {
		testPool.Exec(ctx, `DELETE FROM agent_task_queue WHERE issue_id IN (SELECT id FROM issue WHERE parent_issue_id=$1)`, childID)
		testPool.Exec(ctx, `DELETE FROM issue_wakeup WHERE issue_id IN (SELECT id FROM issue WHERE parent_issue_id=$1)`, childID)
		testPool.Exec(ctx, `DELETE FROM issue WHERE parent_issue_id=$1`, childID)
	})
	childCase := edgeCase{issueID: childID}
	childCase.enter(t, "in_progress")
	tickEdges(t)
	queued := dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE agent_id=$1 AND issue_id IN (SELECT id FROM issue WHERE parent_issue_id=$2)`, privateAgent, childID)
	if queued != 0 {
		t.Fatalf("private agent runs queued for a requester denied by canInvokeAgent: got %d, want 0; requester preserved=%v", queued, requesterPreserved)
	}
	if !requesterPreserved {
		t.Fatal("spawned child dropped requested_by_id")
	}
}

func TestSubWorkflowDoesNotTrustClaimedRequesterAsInvoker(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	readyAgent := seededReadyAgentID(t)
	privateAgent, ownerID, memberID := privateAgentTestFixture(t)
	parentCase, targetID := newSubWorkflowCaseTo(t, 1, []map[string]any{
		{"status_key": "todo", "handler": map[string]any{"type": "agent", "id": readyAgent}, "instructions": "Review fixture only."},
		{"status_key": "in_progress"}, {"status_key": "in_review"}, {"status_key": "blocked"},
	})
	if _, err := testPool.Exec(ctx, `UPDATE issue_workflow SET steps=replace(steps::text,$2,$3)::jsonb WHERE id=$1`, targetID, readyAgent, privateAgent); err != nil {
		t.Fatal(err)
	}
	agent, err := testHandler.Queries.GetAgentInWorkspace(ctx, db.GetAgentInWorkspaceParams{ID: parseUUID(privateAgent), WorkspaceID: parseUUID(testWorkspaceID)})
	if err != nil {
		t.Fatal(err)
	}
	if testHandler.canInvokeAgent(ctx, agent, "member", memberID, memberID, testWorkspaceID) {
		t.Fatal("negative control: member unexpectedly allowed to invoke private agent")
	}
	var projectID string
	dbfx.QueryRow(t, `SELECT project_id::text FROM issue WHERE id=$1`, parentCase.issueID).Scan(&projectID)
	request := newRequestAs(memberID, "POST", "/api/issues?workspace_id="+testWorkspaceID, map[string]any{
		"title": "Reviewer claimed requester", "status": "in_progress", "project_id": projectID, "requested_by_id": ownerID,
	})
	var root IssueResponse
	testutil.Call(t, testHandler.CreateIssue, request).Want(http.StatusCreated).JSON(&root)
	t.Cleanup(func() {
		testPool.Exec(ctx, `DELETE FROM agent_task_queue WHERE issue_id IN (SELECT id FROM issue WHERE parent_issue_id=$1) OR issue_id=$1`, root.ID)
		testPool.Exec(ctx, `DELETE FROM issue_wakeup WHERE issue_id IN (SELECT id FROM issue WHERE parent_issue_id=$1) OR issue_id=$1`, root.ID)
		testPool.Exec(ctx, `DELETE FROM issue WHERE parent_issue_id=$1 OR id=$1`, root.ID)
	})
	tickEdges(t)
	queued := dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE agent_id=$1 AND issue_id IN (SELECT id FROM issue WHERE parent_issue_id=$2)`, privateAgent, root.ID)
	if queued != 0 {
		t.Fatalf("member denied by canInvokeAgent queued %d private agent run(s) by setting requested_by_id to the owner; want 0", queued)
	}
}

func TestSubWorkflowNestedHumanHandlerKeepsRequester(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	_, _, requesterID := privateAgentTestFixture(t)
	seedTestCatalog(t)
	leaf := createTestWorkflowSteps(t, "Reviewer human leaf "+workflowTestSuffix(), []map[string]any{
		{"status_key": "todo", "handler": map[string]any{"type": "human_by_role"}, "instructions": "Review fixture only."},
		{"status_key": "in_progress"}, {"status_key": "in_review"}, {"status_key": "blocked"},
	})
	parentCase, _ := newSubWorkflowCaseTo(t, 1, subWorkflowSteps(leaf.ID, 2))
	if _, err := testPool.Exec(ctx, `UPDATE issue SET requested_by_id=$2 WHERE id=$1`, parentCase.issueID, requesterID); err != nil {
		t.Fatal(err)
	}
	parentCase.enter(t, "in_progress")
	tickEdges(t)
	var childID string
	dbfx.QueryRow(t, `SELECT id::text FROM issue WHERE parent_issue_id=$1`, parentCase.issueID).Scan(&childID)
	t.Cleanup(func() {
		testPool.Exec(ctx, `DELETE FROM issue_wakeup WHERE issue_id IN (SELECT id FROM issue WHERE parent_issue_id=$1)`, childID)
		testPool.Exec(ctx, `DELETE FROM issue WHERE parent_issue_id=$1`, childID)
	})
	childCase := edgeCase{issueID: childID}
	childCase.enter(t, "in_progress")
	tickEdges(t)
	var assigneeID string
	dbfx.QueryRow(t, `SELECT assignee_id::text FROM issue WHERE parent_issue_id=$1`, childID).Scan(&assigneeID)
	if assigneeID != requesterID {
		t.Fatalf("nested human_by_role assignee=%s, want original requester=%s; workspace owner=%s", assigneeID, requesterID, testUserID)
	}
}
