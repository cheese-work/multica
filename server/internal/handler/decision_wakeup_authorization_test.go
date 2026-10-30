package handler

import (
	"context"
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func TestCardDecisionDeliveryCanRegisterWakeup(t *testing.T) {
	for _, branch := range []string{"enqueue", "queued", "squad"} {
		t.Run(branch, func(t *testing.T) {
			resetDecisionLease(t)
			fixture := newCardFixture(t)
			dbfx.Exec(t, `UPDATE agent SET owner_id = $1 WHERE id = $2`, fixture.humanID, fixture.agentID)
			result, err := fixture.answerOnCard(decisionCardAnswerInput{Option: "Option B"})
			if err != nil {
				t.Fatal(err)
			}
			dbfx.Exec(t, `UPDATE agent_task_queue SET status = 'completed' WHERE id IN ($1, $2)`, fixture.taskID, fixture.chatTaskID)
			if branch == "queued" {
				dbfx.Task(t, fixture.agentID, testutil.Cols{"issue_id": fixture.issueID, "status": "queued", "runtime_id": handlerTestRuntimeID(t)})
			}
			if branch == "squad" {
				squadID := dbfx.Insert(t, "squad", testutil.Cols{"workspace_id": testWorkspaceID, "name": "Decision wakeup", "creator_id": fixture.humanID, "leader_id": fixture.agentID})
				dbfx.Exec(t, `UPDATE issue SET assignee_type = 'squad', assignee_id = $1 WHERE id = $2`, squadID, fixture.issueID)
			}
			_, runner := newDecisionRunnerFixture(t)
			decisionPass(t, runner)
			var taskID string
			dbfx.QueryRow(t, `SELECT id FROM agent_task_queue WHERE issue_id = $1 AND agent_id = $2 AND status = 'queued' AND (trigger_comment_id = $3 OR $3 = ANY(coalesced_comment_ids))`, fixture.issueID, fixture.agentID, result.Comment.ID).Scan(&taskID)
			dbfx.Exec(t, `UPDATE agent_task_queue SET status = 'running', started_at = now() WHERE id = $1`, taskID)
			dbfx.Cleanup(t, `DELETE FROM issue_wakeup WHERE issue_id = $1`, fixture.issueID)
			request := withURLParam(newRequest(http.MethodPost, "/", map[string]any{
				"kind": "event", "condition": map[string]string{"type": "pull_request", "event": "checks_finished"}, "instruction": "Read CI results",
			}), "id", fixture.issueID)
			request.Header.Set("X-Agent-ID", fixture.agentID)
			request.Header.Set("X-Task-ID", taskID)
			request.Header.Set("X-Actor-Source", "task_token")
			var wakeup db.IssueWakeup
			testutil.Call(t, testHandler.CreateIssueWakeup, request).Want(http.StatusCreated).JSON(&wakeup)
			if wakeup.AgentID != parseUUID(fixture.agentID) || wakeup.CreatedBy != parseUUID(fixture.humanID) || wakeup.SourceTaskID != parseUUID(taskID) {
				t.Fatalf("wakeup lost the decision human or source: %+v", wakeup)
			}
			task, err := testHandler.Queries.GetAgentTask(context.Background(), parseUUID(taskID))
			if err != nil {
				t.Fatal(err)
			}
			if task.OriginatorUserID != parseUUID(fixture.humanID) || task.AccountableUserID != parseUUID(fixture.humanID) {
				t.Fatalf("decision task attribution = %+v", task)
			}
			request = withURLParam(newRequest(http.MethodGet, "/", nil), "id", fixture.issueID)
			request.Header.Set("X-Agent-ID", fixture.agentID)
			request.Header.Set("X-Task-ID", taskID)
			request.Header.Set("X-Actor-Source", "task_token")
			var listed []db.ListIssueWakeupsRow
			testutil.Call(t, testHandler.ListIssueWakeups, request).Want(http.StatusOK).JSON(&listed)
			if len(listed) != 1 || listed[0].ID != wakeup.ID {
				t.Fatalf("created wakeup not listed: %+v", listed)
			}
		})
	}
}

func TestCardDecisionQuoteAttributionRejectsUnboundSource(t *testing.T) {
	for _, branch := range []string{"comment", "workspace", "issue", "author", "kind", "recorder"} {
		t.Run(branch, func(t *testing.T) {
			fixture := newCardFixture(t)
			result, err := fixture.answerOnCard(decisionCardAnswerInput{Option: "Option B"})
			if err != nil {
				t.Fatal(err)
			}
			workspaceID := fixture.issue.WorkspaceID
			switch branch {
			case "comment":
				dbfx.Exec(t, `UPDATE decision_answer SET issue_comment_id = $1 WHERE id = $2`, fixture.parentID, result.Answer.ID)
			case "workspace":
				workspaceID = parseUUID(fixture.humanID)
			case "issue":
				otherIssue := dbfx.Issue(t, "Unrelated decision")
				dbfx.Exec(t, `UPDATE comment SET issue_id = $1 WHERE id = $2`, otherIssue, result.Comment.ID)
			case "author":
				dbfx.Exec(t, `UPDATE comment SET author_id = $1 WHERE id = $2`, testUserID, result.Comment.ID)
			case "kind":
				dbfx.Exec(t, `UPDATE comment SET decision_comment_kind = 'delivery' WHERE id = $1`, result.Comment.ID)
			case "recorder":
				dbfx.Exec(t, `UPDATE decision_answer SET answered_by_task_id = $1 WHERE id = $2`, fixture.chatTaskID, result.Answer.ID)
			}
			originator := testHandler.TaskService.ResolveOriginatorFromTriggerComment(context.Background(), workspaceID, result.Comment.ID)
			if originator.Valid {
				t.Fatalf("unbound %s quote supplied a human originator: %s", branch, uuidToString(originator))
			}
		})
	}
}
