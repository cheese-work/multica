package handler

import (
	"net/http"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// newPendingPause asks for agent.pause or agent.resume of the fixture agent and
// returns the card, decided by a human of the given workspace role.
func newPendingPause(test *testing.T, actionType, role string) pendingAction {
	test.Helper()
	fixture := newActionFixture(test)
	body := fixture.withAction(map[string]any{"type": actionType, "version": 1, "params": map[string]any{"agent_id": fixture.agentID}})
	var created actionResponse
	fixture.respond(test, body, http.StatusCreated, &created)
	dbfx.Cleanup(test, `DELETE FROM decision_answer WHERE decision_request_id = $1`, created.ID)
	action := pendingAction{fixture, created.ID, created.Action.ID}
	placeDecisionHuman(test, fixture.issueID, action.humanID, role, true)
	return action
}

func (action pendingAction) pausedAt(test *testing.T) (paused bool, by string) {
	test.Helper()
	dbfx.QueryRow(test, `SELECT scheduling_paused_at IS NOT NULL, COALESCE(scheduling_paused_by::text, '') FROM agent WHERE id = $1`, action.agentID).Scan(&paused, &by)
	return paused, by
}

func TestApproveAgentPauseThenResume(test *testing.T) {
	handler := withVerifier(acceptingVerifier)
	pause := newPendingPause(test, "agent.pause", "owner")
	pause.approve(test, handler, pause.claim(test, nil), http.StatusOK)
	if paused, by := pause.pausedAt(test); !paused || by != pause.humanID {
		test.Fatalf("after approved pause: paused=%v by=%q, want true by %s", paused, by, pause.humanID)
	}
	// The pause is live: assigning the agent is refused.
	assign := map[string]any{"title": "to a paused agent", "status": "todo", "priority": "medium", "assignee_type": "agent", "assignee_id": pause.agentID}
	res := testutil.Call(test, testHandler.CreateIssue, newRequest("POST", "/api/issues?workspace_id="+testWorkspaceID, assign)).Want(http.StatusConflict)
	if !strings.Contains(res.Text(), "agent_paused") {
		test.Fatalf("assignment body %q lacks agent_paused", res.Text())
	}
	resume := newPendingPause(test, "agent.resume", "owner")
	resume.approve(test, handler, resume.claim(test, nil), http.StatusOK)
	if paused, _ := resume.pausedAt(test); paused {
		test.Fatal("agent still paused after approved resume")
	}
}

func TestApproveAgentPauseNeedsOwnerOrAdmin(test *testing.T) {
	action := newPendingPause(test, "agent.pause", "member")
	action.setEnvAuthority(test, true) // owning the agent is not enough
	body := action.approve(test, withVerifier(acceptingVerifier), action.claim(test, nil), http.StatusForbidden)
	if !strings.Contains(body, "not_permitted") {
		test.Fatalf("body %q lacks not_permitted", body)
	}
	if paused, _ := action.pausedAt(test); paused {
		test.Fatal("agent paused by a member")
	}
}

func TestApproveAgentPauseDriftsWhenAlreadyPaused(test *testing.T) {
	action := newPendingPause(test, "agent.pause", "owner")
	dbfx.Exec(test, `UPDATE agent SET scheduling_paused_at = now() WHERE id = $1`, action.agentID)
	body := action.approve(test, withVerifier(acceptingVerifier), action.claim(test, nil), http.StatusConflict)
	if !strings.Contains(body, "precondition_drift") {
		test.Fatalf("body %q lacks precondition_drift", body)
	}
}
