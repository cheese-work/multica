package handler

import (
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// setDecisionAskPark stores workspace.settings.decision_ask_park for one test.
func setDecisionAskPark(test *testing.T, mode string) {
	test.Helper()
	dbfx.Exec(test, `UPDATE workspace SET settings = jsonb_set(COALESCE(settings, '{}'), '{decision_ask_park}', to_jsonb($1::text)) WHERE id = $2`, mode, testWorkspaceID)
	test.Cleanup(func() {
		dbfx.Exec(test, `UPDATE workspace SET settings = settings - 'decision_ask_park' WHERE id = $1`, testWorkspaceID)
	})
}

// waitAsk posts one structured ask as the task would, optionally with wait.
func (fixture parkFixture) waitAsk(test *testing.T, singleCall, wait bool) db.DecisionRequest {
	test.Helper()
	fixture.prepare(test, singleCall)
	body := map[string]any{"issue_id": fixture.issueID, "session_id": fixture.sessionID, "escalation_class": "reversible", "ask": fixture.structuredAsk()}
	if singleCall {
		body["for"] = fixture.humanID
	}
	if wait {
		body["wait"] = true
	}
	request := newRequest(http.MethodPost, "/api/decisions", body)
	request.Header.Set("X-Actor-Source", "task_token")
	request.Header.Set("X-Agent-ID", fixture.agentID)
	request.Header.Set("X-Task-ID", fixture.taskID)
	var created db.DecisionRequest
	testutil.Call(test, testHandler.RequestDecision, request).Want(http.StatusCreated).JSON(&created)
	return created
}

func (fixture parkFixture) assignee(test *testing.T) string {
	test.Helper()
	var assignee string
	dbfx.QueryRow(test, `SELECT COALESCE(assignee_id::text, '') FROM issue WHERE id = $1`, fixture.issueID).Scan(&assignee)
	return assignee
}

// CHE-6460 D1a: with decision_ask_park=on_wait a request parks only with wait.
func TestDecisionAskParkSwitch(test *testing.T) {
	for _, tc := range []struct {
		name       string
		setting    string // "" leaves the key absent
		wait       bool
		wantParked bool
	}{
		{"on_wait without wait keeps the issue running", "on_wait", false, false},
		{"on_wait with wait parks", "on_wait", true, true},
		{"always parks", "always", false, true},
		{"key absent parks", "", false, true},
		{"key absent with wait parks", "", true, true},
	} {
		for _, singleCall := range []bool{false, true} {
			name := tc.name + "/two-step"
			if singleCall {
				name = tc.name + "/single-call"
			}
			test.Run(name, func(test *testing.T) {
				if tc.setting != "" {
					setDecisionAskPark(test, tc.setting)
				}
				fixture := newParkFixture(test)
				assigneeBefore := fixture.assignee(test)
				fixture.waitAsk(test, singleCall, tc.wait)
				status, readiness, parkedFrom := fixture.state(test)
				if tc.wantParked {
					if status != "need_decision" || readiness != "gate" || parkedFrom != "in_progress" {
						test.Fatalf("status=%q readiness=%q parked_from=%q, want need_decision/gate/in_progress", status, readiness, parkedFrom)
					}
				} else {
					if status != "in_progress" || readiness != "ready" || parkedFrom != "" {
						test.Fatalf("status=%q readiness=%q parked_from=%q, want in_progress/ready and no park record", status, readiness, parkedFrom)
					}
					if rows := statusRows(test, fixture.issueID); len(rows) != 0 {
						test.Fatalf("status_changed rows = %d, want 0", len(rows))
					}
				}
				if got := fixture.assignee(test); got != assigneeBefore {
					test.Fatalf("assignee = %q, want %q", got, assigneeBefore)
				}
			})
		}
	}
}

// A blocked issue parks as today under on_wait, and the answer resumes it.
func TestDecisionAskParkOnWaitStillParksABlockedIssue(test *testing.T) {
	setDecisionAskPark(test, "on_wait")
	fixture := blockedCardFixture(test, true)
	if got := dbfx.Count(test, `SELECT count(*) FROM issue_decision_park WHERE issue_id = $1`, fixture.issueID); got != 1 {
		test.Fatalf("park records after the ask = %d, want 1", got)
	}
	fixture.answerAndDeliver(test, true)
	if status := fixture.status(test); status != "in_progress" {
		test.Fatalf("status = %q, want in_progress", status)
	}
	if runs := fixture.assigneeRuns(test); runs != 1 {
		test.Fatalf("assignee runs = %d, want 1", runs)
	}
}

// The answer to an unparked request wakes its asker (existing path).
func TestDecisionAskParkUnparkedAnswerWakesTheAsker(test *testing.T) {
	setDecisionAskPark(test, "on_wait")
	fixture := newParkFixture(test)
	created := fixture.waitAsk(test, true, false)
	recipients, err := testHandler.resolveDecisionWakeRecipients(test.Context(), testHandler.Queries, created)
	if err != nil {
		test.Fatal(err)
	}
	if len(recipients) == 0 || recipients[0].role != "requester" || uuidToString(recipients[0].agentID) != fixture.agentID {
		test.Fatalf("recipients = %+v, want the asking agent first as requester", recipients)
	}
}
