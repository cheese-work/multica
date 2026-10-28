package handler

import (
	"context"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func TestSubWorkflowAgentCreatorUsesTrustedOriginator(t *testing.T) {
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
	var taskID string
	if err := testPool.QueryRow(ctx, `INSERT INTO agent_task_queue(agent_id,runtime_id,status,priority,originator_source,originator_user_id,accountable_user_id) VALUES($1,(SELECT runtime_id FROM agent WHERE id=$1),'running',0,'direct_human',$2,$2) RETURNING id::text`, readyAgent, memberID).Scan(&taskID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { testPool.Exec(ctx, `DELETE FROM agent_task_queue WHERE id=$1`, taskID) })
	var projectID string
	dbfx.QueryRow(t, `SELECT project_id::text FROM issue WHERE id=$1`, parentCase.issueID).Scan(&projectID)
	request := newRequest("POST", "/api/issues?workspace_id="+testWorkspaceID, map[string]any{
		"title": "Agent made root", "status": "in_progress", "project_id": projectID, "requested_by_id": ownerID,
	})
	request.Header.Set("X-Actor-Source", "task_token")
	request.Header.Set("X-Agent-ID", readyAgent)
	request.Header.Set("X-Task-ID", taskID)
	actorType, actorID := testHandler.resolveActor(request, testUserID, testWorkspaceID)
	if actorType != "agent" || actorID != readyAgent {
		t.Fatal("fixture did not resolve the admitted agent actor")
	}
	originator := testHandler.invokeOriginatorFromRequest(request, actorType, actorID)
	if originator != memberID {
		t.Fatalf("request originator=%s, want actual member=%s", originator, memberID)
	}
	agent, err := testHandler.Queries.GetAgentInWorkspace(ctx, db.GetAgentInWorkspaceParams{ID: parseUUID(privateAgent), WorkspaceID: parseUUID(testWorkspaceID)})
	if err != nil {
		t.Fatal(err)
	}
	if testHandler.canInvokeAgent(ctx, agent, actorType, actorID, originator, testWorkspaceID) {
		t.Fatal("negative control: actual originator unexpectedly allowed to invoke private agent")
	}
	var created IssueResponse
	testutil.Call(t, testHandler.CreateIssue, request).Want(http.StatusCreated).JSON(&created)
	t.Cleanup(func() {
		testPool.Exec(ctx, `DELETE FROM agent_task_queue WHERE issue_id IN(SELECT id FROM issue WHERE parent_issue_id=$1) OR issue_id=$1`, created.ID)
		testPool.Exec(ctx, `DELETE FROM issue_wakeup WHERE issue_id IN(SELECT id FROM issue WHERE parent_issue_id=$1) OR issue_id=$1`, created.ID)
		testPool.Exec(ctx, `DELETE FROM issue WHERE parent_issue_id=$1 OR id=$1`, created.ID)
	})
	parent, err := testHandler.Queries.GetIssue(ctx, parseUUID(created.ID))
	if err != nil {
		t.Fatal(err)
	}
	if parent.CreatorType != "agent" || parent.OriginType.String != "agent_create" || parent.OriginID != parseUUID(taskID) {
		t.Fatal("created issue did not preserve the server-stamped agent/task provenance")
	}
	trusted := testHandler.TaskService.OriginatorForIssueTask(ctx, parent, pgtype.UUID{})
	if trusted != parseUUID(memberID) {
		t.Fatalf("trusted originator=%s, want actual member=%s", uuidToString(trusted), memberID)
	}
	t.Log("HTTP and persisted task provenance both identify a human denied by the private-agent invoke gate")
	tickEdges(t)
	queued := dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE agent_id=$1 AND issue_id IN(SELECT id FROM issue WHERE parent_issue_id=$2)`, privateAgent, created.ID)
	if queued != 0 {
		t.Fatalf("admitted agent created %d private-agent run(s) for a forbidden true originator by nominating another requester; want 0", queued)
	}
}

func TestSubWorkflowNestedAgentCreatorCarriesTrustedOriginator(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	readyAgent := seededReadyAgentID(t)
	privateAgent, ownerID, _ := privateAgentTestFixture(t)
	seedTestCatalog(t)
	leaf := createTestWorkflowSteps(t, "Agent chain private leaf "+workflowTestSuffix(), []map[string]any{
		{"status_key": "todo", "handler": map[string]any{"type": "agent", "id": readyAgent}, "instructions": "Fixture only."},
		{"status_key": "in_progress"}, {"status_key": "in_review"}, {"status_key": "blocked"},
	})
	if _, err := testPool.Exec(ctx, `UPDATE issue_workflow SET steps=replace(steps::text,$2,$3)::jsonb WHERE id=$1`, leaf.ID, readyAgent, privateAgent); err != nil {
		t.Fatal(err)
	}
	parentCase, _ := newSubWorkflowCaseTo(t, 1, subWorkflowSteps(leaf.ID, 2))
	var taskID string
	if err := testPool.QueryRow(ctx, `INSERT INTO agent_task_queue(agent_id,runtime_id,status,priority,originator_source,originator_user_id,accountable_user_id) VALUES($1,(SELECT runtime_id FROM agent WHERE id=$1),'running',0,'direct_human',$2,$2) RETURNING id::text`, readyAgent, ownerID).Scan(&taskID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { testPool.Exec(ctx, `DELETE FROM agent_task_queue WHERE id=$1`, taskID) })
	var projectID string
	dbfx.QueryRow(t, `SELECT project_id::text FROM issue WHERE id=$1`, parentCase.issueID).Scan(&projectID)
	request := newRequest("POST", "/api/issues?workspace_id="+testWorkspaceID, map[string]any{
		"title": "Agent made root", "status": "in_progress", "project_id": projectID,
	})
	request.Header.Set("X-Actor-Source", "task_token")
	request.Header.Set("X-Agent-ID", readyAgent)
	request.Header.Set("X-Task-ID", taskID)
	actorType, actorID := testHandler.resolveActor(request, testUserID, testWorkspaceID)
	if actorType != "agent" || actorID != readyAgent {
		t.Fatal("fixture did not resolve the admitted agent actor")
	}
	var created IssueResponse
	testutil.Call(t, testHandler.CreateIssue, request).Want(http.StatusCreated).JSON(&created)
	t.Cleanup(func() {
		testPool.Exec(ctx, `DELETE FROM agent_task_queue WHERE issue_id IN(SELECT id FROM issue WHERE parent_issue_id=$1) OR issue_id=$1`, created.ID)
		testPool.Exec(ctx, `DELETE FROM issue_wakeup WHERE issue_id IN(SELECT id FROM issue WHERE parent_issue_id=$1) OR issue_id=$1`, created.ID)
		testPool.Exec(ctx, `DELETE FROM issue WHERE parent_issue_id=$1 OR id=$1`, created.ID)
	})
	tickEdges(t)
	var childID string
	dbfx.QueryRow(t, `SELECT id::text FROM issue WHERE parent_issue_id=$1`, created.ID).Scan(&childID)
	t.Cleanup(func() {
		testPool.Exec(ctx, `DELETE FROM agent_task_queue WHERE issue_id IN(SELECT id FROM issue WHERE parent_issue_id=$1)`, childID)
		testPool.Exec(ctx, `DELETE FROM issue_wakeup WHERE issue_id IN(SELECT id FROM issue WHERE parent_issue_id=$1)`, childID)
		testPool.Exec(ctx, `DELETE FROM issue WHERE parent_issue_id=$1`, childID)
	})
	childCase := edgeCase{issueID: childID}
	childCase.enter(t, "in_progress")
	tickEdges(t)
	queued := dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE agent_id=$1 AND issue_id IN(SELECT id FROM issue WHERE parent_issue_id=$2)`, privateAgent, childID)
	if queued != 1 {
		t.Fatalf("a grandchild of an agent-made issue queued %d private-agent run(s) for the allowed true originator; want 1", queued)
	}
}
