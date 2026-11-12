package handler

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// pausedAgentID returns a workspace agent the test user may assign work to,
// paused for the length of the test.
func pausedAgentID(test *testing.T) string {
	test.Helper()
	var agentID string
	dbfx.QueryRow(test, `SELECT id::text FROM agent WHERE workspace_id = $1 AND archived_at IS NULL AND scheduling_paused_at IS NULL ORDER BY created_at LIMIT 1`, testWorkspaceID).Scan(&agentID)
	if agentID == "" {
		test.Skip("no agent in the test workspace")
	}
	dbfx.Exec(test, `UPDATE agent SET scheduling_paused_at = now() WHERE id = $1`, agentID)
	test.Cleanup(func() {
		dbfx.Exec(test, `UPDATE agent SET scheduling_paused_at = NULL, scheduling_paused_by = NULL WHERE id = $1`, agentID)
	})
	return agentID
}

func TestAssigningAPausedAgentIsRefusedWithAHint(test *testing.T) {
	agentID := pausedAgentID(test)
	response := testutil.Call(test, testHandler.CreateIssue, newRequest("POST", "/api/issues?workspace_id="+testWorkspaceID, map[string]any{
		"title": "paused assignee", "status": "todo", "priority": "medium",
		"assignee_type": "agent", "assignee_id": agentID,
	})).Want(http.StatusConflict)
	var body struct {
		Error string `json:"error"`
	}
	response.JSON(&body)
	if !strings.HasPrefix(body.Error, "agent_paused:") || !strings.Contains(body.Error, "pick another agent by role") {
		test.Fatalf("error = %q, want agent_paused with the pick-by-role hint", body.Error)
	}
}

func TestAssigningASquadWhoseLeaderIsPausedIsRefused(test *testing.T) {
	agentID := pausedAgentID(test)
	var squadID string
	dbfx.QueryRow(test, `INSERT INTO squad (workspace_id, name, description, leader_id, creator_id) VALUES ($1, 'paused-leader-squad', '', $2, $3) RETURNING id::text`, testWorkspaceID, agentID, testUserID).Scan(&squadID)
	dbfx.Cleanup(test, `DELETE FROM squad WHERE id = $1`, squadID)
	response := testutil.Call(test, testHandler.CreateIssue, newRequest("POST", "/api/issues?workspace_id="+testWorkspaceID, map[string]any{
		"title": "paused leader", "status": "todo", "priority": "medium",
		"assignee_type": "squad", "assignee_id": squadID,
	})).Want(http.StatusConflict)
	var body struct {
		Error string `json:"error"`
	}
	response.JSON(&body)
	if !strings.HasPrefix(body.Error, "agent_paused:") {
		test.Fatalf("error = %q, want agent_paused", body.Error)
	}
}

func pauseAgentForTest(test *testing.T, agentID string) {
	test.Helper()
	dbfx.Exec(test, `UPDATE agent SET scheduling_paused_at = now() WHERE id = $1`, agentID)
	test.Cleanup(func() {
		dbfx.Exec(test, `UPDATE agent SET scheduling_paused_at = NULL, scheduling_paused_by = NULL WHERE id = $1`, agentID)
	})
}

// A run for a paused agent is never queued, whichever path asks for it.
func TestEnqueueForAPausedAgentIsRefused(test *testing.T) {
	issueID := dbfx.Issue(test, "paused enqueue")
	test.Cleanup(func() { cleanupChildDoneIssue(issueID) })
	agentID := handlerTestAgentID(test)
	setIssueAssigneeDirect(test, issueID, "agent", agentID)
	pauseAgentForTest(test, agentID)
	issue, err := testHandler.Queries.GetIssue(context.Background(), parseUUID(issueID))
	if err != nil {
		test.Fatal(err)
	}
	if _, err := testHandler.TaskService.EnqueueTaskForIssue(context.Background(), issue); !errors.Is(err, service.ErrAgentPaused) {
		test.Fatalf("enqueue = %v, want ErrAgentPaused", err)
	}
	if n := dbfx.Count(test, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1`, issueID); n != 0 {
		test.Fatalf("task rows = %d, want none", n)
	}
}

func TestMentioningAPausedAgentStartsNoRun(test *testing.T) {
	issueID := dbfx.Issue(test, "paused mention")
	test.Cleanup(func() { cleanupChildDoneIssue(issueID) })
	agentID := handlerTestAgentID(test)
	pauseAgentForTest(test, agentID)
	commentOn(test, issueID, "[@Handler Test Agent](mention://agent/"+agentID+") please look", "", "")
	if n := dbfx.Count(test, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND agent_id = $2`, issueID, agentID); n != 0 {
		test.Fatalf("a mention of a paused agent queued %d runs", n)
	}
}

func TestWakeupForAPausedAgentIsSkippedAndLogged(test *testing.T) {
	issueID := dbfx.Issue(test, "paused wakeup")
	test.Cleanup(func() { cleanupChildDoneIssue(issueID) })
	agentID := handlerTestAgentID(test)
	rule := createRule(test, issueID, agentID, pgtype.UUID{}, service.WakeupInput{Instruction: "Reply to new comments", Kind: "event", Mode: "continuous", EventTypes: []string{"comment.created"}})
	pauseAgentForTest(test, agentID)
	commentOn(test, issueID, "Anyone there?", "", "")
	runWakeupTick(test)
	if n := wakeupRunsOf(test, rule); n != 0 {
		test.Fatalf("a paused agent's wakeup queued %d runs", n)
	}
	if n := dbfx.Count(test, `SELECT count(*) FROM activity_log WHERE issue_id = $1 AND action = 'wakeup_skipped' AND details->>'reason_code' = 'agent_paused'`, issueID); n != 1 {
		test.Fatalf("skip records = %d, want 1", n)
	}
}
