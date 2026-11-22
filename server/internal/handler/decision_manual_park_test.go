package handler

import (
	"context"
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// manualParkFixture is an in_progress issue with Readiness "ready" and one
// open, unparked decision request (decision_ask_park=on_wait, no wait), so the
// next status write is the agent's own move to need_decision.
type manualParkFixture struct {
	askedRecordFixture
	readinessID string
}

func newManualParkFixture(test *testing.T, setting string, openRequest bool) manualParkFixture {
	test.Helper()
	resetDecisionLease(test)
	readinessID := ensureParkCatalog(test)
	dropStoredDefaultWorkflow(test) // a stored Default from an earlier test may not list need_decision
	resetDefaultWorkflow(test)
	if setting != "" {
		setDecisionAskPark(test, setting)
	}
	core := newRequestCoreFixture(test, false)
	dbfx.Exec(test, `UPDATE issue SET status = 'in_progress', properties = jsonb_build_object($1::text, 'ready') WHERE id = $2`, readinessID, core.issueID)
	core.issue, _ = testHandler.Queries.GetIssue(context.Background(), parseUUID(core.issueID))
	fixture := manualParkFixture{readinessID: readinessID}
	if !openRequest {
		fixture.askedRecordFixture = askedRecordFixture{requestCoreFixture: core}
		return fixture
	}
	result, err := testHandler.createDecisionRequest(core.request(), core.issue, core.input())
	if err != nil {
		test.Fatal(err)
	}
	if result.ParkedActivity != nil {
		test.Fatalf("the request parked the issue; this fixture needs an unparked open request (setting %q)", setting)
	}
	messageID := dbfx.Insert(test, "chat_message", testutil.Cols{"chat_session_id": core.sessionID, "role": "user", "content": "Yes.", "sent_via": "interactive_jwt"})
	chatTaskID := dbfx.Task(test, core.agentID, testutil.Cols{"status": "running", "chat_session_id": core.sessionID, "runtime_id": handlerTestRuntimeID(test)})
	dbfx.Exec(test, `UPDATE agent_task_queue SET started_at = clock_timestamp() WHERE id = $1`, chatTaskID)
	dbfx.Exec(test, `UPDATE decision_request SET ask_json = $2::jsonb, options = $3::jsonb WHERE id = $1`, result.Request.ID, cardAskJSON, cardOptions)
	dbfx.Exec(test, `UPDATE decision_intent SET sealed_at = clock_timestamp() WHERE id = $1`, core.intentID)
	fixture.askedRecordFixture = askedRecordFixture{core, result.Request, uuidToString(result.Comment.ID), messageID, chatTaskID}
	return fixture
}

func (fixture manualParkFixture) state(test *testing.T) (status, readiness, parkedFrom, parkedTo string) {
	test.Helper()
	dbfx.QueryRow(test, `SELECT status, COALESCE(properties ->> $1, ''),
		COALESCE((SELECT prior_status FROM issue_decision_park WHERE issue_id = issue.id), ''),
		COALESCE((SELECT to_status FROM issue_decision_park WHERE issue_id = issue.id), '')
		FROM issue WHERE id = $2`, fixture.readinessID, fixture.issueID).Scan(&status, &readiness, &parkedFrom, &parkedTo)
	return
}

func (fixture manualParkFixture) assignee(test *testing.T) string {
	test.Helper()
	var assignee string
	dbfx.QueryRow(test, `SELECT COALESCE(assignee_id::text, '') FROM issue WHERE id = $1`, fixture.issueID).Scan(&assignee)
	return assignee
}

// moveSingle is the agent's own "move this issue to need_decision" write.
func (fixture manualParkFixture) moveSingle(test *testing.T, status string) {
	test.Helper()
	request := withURLParam(newRequest(http.MethodPut, "/api/issues/"+fixture.issueID, map[string]any{"status": status}), "id", fixture.issueID)
	testutil.Call(test, testHandler.UpdateIssue, request).Want(http.StatusOK)
}

func (fixture manualParkFixture) moveBatch(test *testing.T, status string) {
	test.Helper()
	request := newRequest(http.MethodPost, "/api/issues/batch?workspace_id="+testWorkspaceID, map[string]any{
		"issue_ids": []string{fixture.issueID}, "updates": map[string]any{"status": status},
	})
	testutil.Call(test, testHandler.BatchUpdateIssues, request).Want(http.StatusOK)
}

// CHE-6460 D1c: under on_wait a manual move to the park target with an open
// request is a park; the answer restores the prior status.
func TestManualMoveToTheParkTargetUnderOnWaitIsAPark(test *testing.T) {
	for _, path := range []struct {
		name string
		move func(manualParkFixture, *testing.T, string)
	}{
		{"single", manualParkFixture.moveSingle},
		{"batch", manualParkFixture.moveBatch},
	} {
		test.Run(path.name, func(test *testing.T) {
			fixture := newManualParkFixture(test, "on_wait", true)
			assigneeBefore := fixture.assignee(test)
			path.move(fixture, test, "need_decision")
			status, readiness, from, to := fixture.state(test)
			if status != "need_decision" || readiness != "gate" || from != "in_progress" || to != "need_decision" {
				test.Fatalf("status=%q readiness=%q park=%q->%q, want need_decision/gate and a park from in_progress", status, readiness, from, to)
			}
			if got := fixture.assignee(test); got != assigneeBefore {
				test.Fatalf("assignee = %q, want %q (the park keeps the assignee)", got, assigneeBefore)
			}
			wantDecisionStatusRows(test, fixture.issueID, 1)
			fixture.answerAndDeliver(test, true)
			if status, readiness, from, _ := fixture.state(test); status != "in_progress" || readiness != "ready" || from != "" {
				test.Fatalf("after the answer: status=%q readiness=%q park=%q, want in_progress/ready and no record", status, readiness, from)
			}
		})
	}
}

// No open request: the write is an ordinary move, with no park record.
func TestManualMoveWithoutAnOpenRequestIsNotAPark(test *testing.T) {
	for _, path := range []string{"single", "batch"} {
		test.Run(path, func(test *testing.T) {
			fixture := newManualParkFixture(test, "on_wait", false)
			if path == "single" {
				fixture.moveSingle(test, "need_decision")
			} else {
				fixture.moveBatch(test, "need_decision")
			}
			status, readiness, from, _ := fixture.state(test)
			if status != "need_decision" || readiness != "ready" || from != "" {
				test.Fatalf("status=%q readiness=%q park=%q, want need_decision/ready and no park record", status, readiness, from)
			}
		})
	}
}

// decision_ask_park=always: a request parks at once, and a later manual move
// is unchanged: no second park record and no change to the first.
func TestManualMoveUnderAlwaysIsUnchanged(test *testing.T) {
	fixture := newManualParkFixture(test, "always", false)
	core := fixture.requestCoreFixture
	result, err := testHandler.createDecisionRequest(core.request(), core.issue, core.input())
	if err != nil {
		test.Fatal(err)
	}
	if result.ParkedActivity == nil {
		test.Fatal("always must park on the request")
	}
	if status, _, from, _ := fixture.state(test); status != "need_decision" || from != "in_progress" {
		test.Fatalf("status=%q park from %q, want need_decision parked from in_progress", status, from)
	}
	fixture.moveSingle(test, "need_decision")
	if got := dbfx.Count(test, `SELECT count(*) FROM issue_decision_park WHERE issue_id = $1`, fixture.issueID); got != 1 {
		test.Fatalf("park records = %d, want 1", got)
	}
}

// The key absent reads as always: an open request with no park record cannot
// exist, so a manual move is a plain move.
func TestManualMoveWithTheKeyAbsentIsNotAPark(test *testing.T) {
	fixture := newManualParkFixture(test, "", false)
	core := fixture.requestCoreFixture
	// A request opened while the issue was running under on_wait, then the
	// setting went back to absent: the move must not park behind its back.
	setDecisionAskPark(test, "on_wait")
	if _, err := testHandler.createDecisionRequest(core.request(), core.issue, core.input()); err != nil {
		test.Fatal(err)
	}
	dbfx.Exec(test, `UPDATE workspace SET settings = settings - 'decision_ask_park' WHERE id = $1`, testWorkspaceID)
	fixture.moveSingle(test, "need_decision")
	if status, _, from, _ := fixture.state(test); status != "need_decision" || from != "" {
		test.Fatalf("status=%q park from %q, want need_decision and no park record", status, from)
	}
}
