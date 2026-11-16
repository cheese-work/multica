package handler

import (
	"net/http"
	"strconv"
	"testing"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func newPendingAgentAction(test *testing.T, actionType string) pendingAction {
	test.Helper()
	fixture := newActionFixture(test)
	body := fixture.withAction(map[string]any{
		"type": actionType, "version": 1,
		"params": map[string]any{"agent_id": fixture.agentID},
	})
	var created actionResponse
	fixture.respond(test, body, http.StatusCreated, &created)
	dbfx.Cleanup(test, `DELETE FROM decision_answer WHERE decision_request_id = $1`, created.ID)
	action := pendingAction{fixture, created.ID, created.Action.ID}
	action.setEnvAuthority(test, true)
	return action
}

func (action pendingAction) schedulingPaused(test *testing.T) bool {
	test.Helper()
	var paused bool
	dbfx.QueryRow(test, `SELECT scheduling_paused_at IS NOT NULL FROM agent WHERE id = $1`, action.agentID).Scan(&paused)
	return paused
}

func TestAgentPauseActionPausesOnApproveThenResumeClearsIt(test *testing.T) {
	handler := withVerifier(acceptingVerifier)
	pause := newPendingAgentAction(test, "agent.pause")
	if pause.schedulingPaused(test) {
		test.Fatal("agent paused before approval")
	}
	var out decisionActionResponse
	decodeJSON(test, pause.approve(test, handler, pause.claim(test, nil), http.StatusOK), &out)
	if out.State != "executed" || !pause.schedulingPaused(test) {
		test.Fatalf("state=%s paused=%v, want executed and paused", out.State, pause.schedulingPaused(test))
	}
	if n := dbfx.Count(test, `SELECT count(*) FROM agent WHERE id = $1 AND scheduling_paused_by = $2`, pause.agentID, pause.humanID); n != 1 {
		test.Fatal("pause not attributed to the approver")
	}

	resume := newPendingAgentAction(test, "agent.resume")
	decodeJSON(test, resume.approve(test, handler, resume.claim(test, nil), http.StatusOK), &out)
	if out.State != "executed" || resume.schedulingPaused(test) {
		test.Fatalf("state=%s paused=%v, want executed and resumed", out.State, resume.schedulingPaused(test))
	}
}

func TestAgentPauseActionRefusesASecretSlot(test *testing.T) {
	fixture := newActionFixture(test)
	body := fixture.withAction(map[string]any{
		"type": "agent.pause", "version": 1,
		"params":       map[string]any{"agent_id": fixture.agentID},
		"secret_slots": []map[string]any{{"slot": "KEY", "target": map[string]string{"agent_id": fixture.agentID, "env_key": "KEY"}}},
	})
	fixture.respond(test, body, http.StatusBadRequest, nil)
}

// The lifecycle sweeper already cancels a request whose human is a non-owner
// without a project delegation, so the role rule is the second wall: a project
// delegate who is only a member still may not pause an agent.
func TestAgentPauseActionPermitsOnlyOwnersAndAdmins(test *testing.T) {
	for _, actionType := range []string{"agent.pause", "agent.resume"} {
		for role, want := range map[string]bool{"owner": true, "admin": true, "member": false} {
			if got := decisionActionPermitted(actionType, db.Agent{}, db.Member{Role: role}); got != want {
				test.Fatalf("%s as %s: permitted=%v, want %v", actionType, role, got, want)
			}
		}
	}
}

func TestAgentPauseActionCardShowsTheBlastRadius(test *testing.T) {
	action := newPendingAgentAction(test, "agent.pause")
	row, err := testHandler.Queries.GetDecisionAction(test.Context(), db.GetDecisionActionParams{ID: parseUUID(action.actionID), WorkspaceID: parseUUID(testWorkspaceID)})
	if err != nil {
		test.Fatal(err)
	}
	fields, err := testHandler.agentSchedulingPreview(test.Context(), row, parseUUID(action.humanID))
	if err != nil {
		test.Fatal(err)
	}
	got := map[string]string{}
	for _, field := range fields {
		got[field.Label] = field.Proposed
	}
	for _, label := range []string{"Running runs (finish)", "Queued runs (wait)", "Open assigned issues", "Enabled wakeups"} {
		if got[label] == "" {
			test.Fatalf("preview lacks %q: %v", label, got)
		}
	}
	if got["Scheduling"] != "Paused" {
		test.Fatalf("scheduling = %q", got["Scheduling"])
	}
	var running, queued int
	dbfx.QueryRow(test, `SELECT count(*) FILTER (WHERE status IN ('dispatched','running','waiting_local_directory')), count(*) FILTER (WHERE status IN ('queued','deferred')) FROM agent_task_queue WHERE agent_id = $1`, action.agentID).Scan(&running, &queued)
	if got["Running runs (finish)"] != itoa(running) || got["Queued runs (wait)"] != itoa(queued) {
		test.Fatalf("counts %v, want running %d queued %d", got, running, queued)
	}
}

func itoa(n int) string { return strconv.Itoa(n) }
