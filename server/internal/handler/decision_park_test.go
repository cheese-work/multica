package handler

import (
	"context"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// ensureParkCatalog lists need_decision and a Readiness property with a
// gated-human option for the test, and returns the property id.
func ensureParkCatalog(test *testing.T) string {
	test.Helper()
	if dbfx.Count(test, `SELECT count(*) FROM issue_status WHERE workspace_id = $1 AND key = 'need_decision'`, testWorkspaceID) == 0 {
		dbfx.Insert(test, "issue_status", testutil.Cols{"workspace_id": testWorkspaceID, "key": "need_decision", "name": "Need Decision", "category": "started", "color": "#112233"})
	}
	readinessID := dbfx.Insert(test, "issue_property", testutil.Cols{"workspace_id": testWorkspaceID, "name": "Readiness", "type": "select", "config": `{"options":[{"id":"ready","name":"ready"},{"id":"gate","name":"gated-human"}]}`})
	test.Cleanup(func() { dbfx.Exec(test, `DELETE FROM issue_property WHERE id = $1`, readinessID) })
	return readinessID
}

// parkFixture is a request fixture whose issue is in_progress with Readiness
// "ready", on a catalog that lists need_decision.
type parkFixture struct {
	requestCoreFixture
	readinessID string
}

func newParkFixture(test *testing.T) parkFixture {
	test.Helper()
	readinessID := ensureParkCatalog(test)
	fixture := newRequestCoreFixture(test, false)
	dbfx.Exec(test, `UPDATE issue SET status = 'in_progress', properties = jsonb_build_object($1::text, 'ready') WHERE id = $2`, readinessID, fixture.issueID)
	fixture.issue, _ = testHandler.Queries.GetIssue(context.Background(), parseUUID(fixture.issueID))
	return parkFixture{fixture, readinessID}
}

func (fixture parkFixture) state(test *testing.T) (status, readiness, parkedFrom string) {
	test.Helper()
	dbfx.QueryRow(test, `SELECT status, COALESCE(properties ->> $1, ''), COALESCE(metadata -> 'decision_park' ->> 'status', '') FROM issue WHERE id = $2`, fixture.readinessID, fixture.issueID).Scan(&status, &readiness, &parkedFrom)
	return
}

func TestDecisionRequestParksTheIssue(test *testing.T) {
	fixture := newParkFixture(test)
	result, err := testHandler.createDecisionRequest(fixture.request(), fixture.issue, fixture.input())
	if err != nil {
		test.Fatal(err)
	}
	if !result.Parked {
		test.Fatal("result does not report the park")
	}
	if status, readiness, from := fixture.state(test); status != "need_decision" || readiness != "gate" || from != "in_progress" {
		test.Fatalf("status=%q readiness=%q parked_from=%q, want need_decision/gate/in_progress", status, readiness, from)
	}
}

func TestDecisionRequestKeepsTheFirstParkOfAnAlreadyParkedIssue(test *testing.T) {
	fixture := newParkFixture(test)
	dbfx.Exec(test, `UPDATE issue SET status = 'need_decision' WHERE id = $1`, fixture.issueID)
	fixture.issue, _ = testHandler.Queries.GetIssue(context.Background(), parseUUID(fixture.issueID))
	result, err := testHandler.createDecisionRequest(fixture.request(), fixture.issue, fixture.input())
	if err != nil {
		test.Fatal(err)
	}
	if status, readiness, from := fixture.state(test); result.Parked || status != "need_decision" || readiness != "ready" || from != "" {
		test.Fatalf("parked=%v status=%q readiness=%q parked_from=%q: an already parked issue must stay untouched", result.Parked, status, readiness, from)
	}
}

func TestDecisionRequestOnATerminalIssueIsRefusedAndNotParked(test *testing.T) {
	fixture := newParkFixture(test)
	dbfx.Exec(test, `UPDATE issue SET status = 'done' WHERE id = $1`, fixture.issueID)
	fixture.issue, _ = testHandler.Queries.GetIssue(context.Background(), parseUUID(fixture.issueID))
	_, err := testHandler.createDecisionRequest(fixture.request(), fixture.issue, fixture.input())
	requireRequestRefusal(test, err)
	if status, _, _ := fixture.state(test); status != "done" {
		test.Fatalf("status = %q, want done", status)
	}
}

func TestDecisionAnswerRestoresTheParkedIssue(test *testing.T) {
	resetDecisionLease(test)
	readinessID := ensureParkCatalog(test)
	fixture := newCardFixture(test)
	var status, readiness string
	read := func() {
		dbfx.QueryRow(test, `SELECT status, COALESCE(properties ->> $1, '') FROM issue WHERE id = $2`, readinessID, fixture.issueID).Scan(&status, &readiness)
	}
	read()
	if status != "need_decision" || readiness != "gate" {
		test.Fatalf("after the ask: status=%q readiness=%q, want need_decision/gate", status, readiness)
	}
	if _, err := fixture.answerOnCard(decisionCardAnswerInput{Option: "Option A"}); err != nil {
		test.Fatal(err)
	}
	dbfx.Exec(test, `UPDATE agent_task_queue SET status = 'completed' WHERE id IN ($1, $2)`, fixture.taskID, fixture.chatTaskID)
	_, runner := newDecisionRunnerFixture(test)
	decisionPass(test, runner)
	read()
	if status != "todo" || readiness != "" {
		test.Fatalf("after the answer: status=%q readiness=%q, want todo and no Readiness", status, readiness)
	}
	if dbfx.Count(test, `SELECT count(*) FROM issue WHERE id = $1 AND metadata ? 'decision_park'`, fixture.issueID) != 0 {
		test.Fatal("the park record outlived the answer")
	}
}
