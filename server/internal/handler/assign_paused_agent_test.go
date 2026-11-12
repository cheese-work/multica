package handler

import (
	"net/http"
	"strings"
	"testing"

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
	test.Cleanup(func() { dbfx.Exec(test, `UPDATE agent SET scheduling_paused_at = NULL, scheduling_paused_by = NULL WHERE id = $1`, agentID) })
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
