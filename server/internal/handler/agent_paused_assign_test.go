package handler

import (
	"net/http"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// CHE-5066: assigning work to a scheduling-paused agent is refused with
// agent_paused and a hint to pick another agent; resuming lifts the refusal.
func TestCreateIssue_PausedAgentAssignmentRefused(t *testing.T) {
	agentID := createHandlerTestAgent(t, "PausedAssignTarget", []byte("[]"))
	body := map[string]any{
		"title": "assign to paused agent", "status": "todo", "priority": "medium",
		"assignee_type": "agent", "assignee_id": agentID,
	}
	create := func() *testutil.Response {
		return testutil.Call(t, testHandler.CreateIssue, newRequest("POST", "/api/issues?workspace_id="+testWorkspaceID, body))
	}

	dbfx.Exec(t, `UPDATE agent SET scheduling_paused_at = now() WHERE id = $1`, agentID)
	res := create()
	res.Want(http.StatusConflict)
	if !strings.Contains(res.Text(), "agent_paused") || !strings.Contains(res.Text(), "pick another agent by role") {
		t.Fatalf("expected agent_paused hint, got %s", res.Text())
	}

	dbfx.Exec(t, `UPDATE agent SET scheduling_paused_at = NULL WHERE id = $1`, agentID)
	create().Want(http.StatusCreated)
}
