package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
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
	dbfx.QueryRow(test, `SELECT status, COALESCE(properties ->> $1, ''), COALESCE((SELECT prior_status FROM issue_decision_park WHERE issue_id = issue.id), '') FROM issue WHERE id = $2`, fixture.readinessID, fixture.issueID).Scan(&status, &readiness, &parkedFrom)
	return
}

// wantDecisionStatusRows fails unless the issue has exactly n status rows and
// every one is a decision move (CHE-2854: the listener writes no stand-in).
func wantDecisionStatusRows(test *testing.T, issueID string, n int) {
	test.Helper()
	rows := statusRows(test, issueID)
	if len(rows) != n {
		test.Fatalf("status_changed rows = %d, want %d", len(rows), n)
	}
	for _, row := range rows {
		trigger, _ := row["trigger"].(map[string]any)
		if cause, _ := trigger["cause"].(map[string]any); cause["kind"] != "decision" {
			test.Fatalf("status row trigger = %v, want cause.kind decision", trigger)
		}
	}
}

func TestDecisionRequestParksTheIssue(test *testing.T) {
	fixture := newParkFixture(test)
	result, err := testHandler.createDecisionRequest(fixture.request(), fixture.issue, fixture.input())
	if err != nil {
		test.Fatal(err)
	}
	if result.ParkedActivity == nil {
		test.Fatal("result does not report the park")
	}
	if status, readiness, from := fixture.state(test); status != "need_decision" || readiness != "gate" || from != "in_progress" {
		test.Fatalf("status=%q readiness=%q parked_from=%q, want need_decision/gate/in_progress", status, readiness, from)
	}
	wantDecisionStatusRows(test, fixture.issueID, 1)
}

func TestParkedIssueMetadataHoldsOnlyPrimitives(test *testing.T) {
	fixture := newParkFixture(test)
	if _, err := testHandler.createDecisionRequest(fixture.request(), fixture.issue, fixture.input()); err != nil {
		test.Fatal(err)
	}
	if dbfx.Count(test, `SELECT count(*) FROM issue_decision_park WHERE issue_id = $1 AND prior_status = 'in_progress' AND to_status = 'need_decision'`, fixture.issueID) != 1 {
		test.Fatal("the park row is missing")
	}
	request := withURLParam(newRequest("GET", "/api/issues/"+fixture.issueID, nil), "id", fixture.issueID)
	recorder := httptest.NewRecorder()
	testHandler.GetIssue(recorder, request)
	if recorder.Code != http.StatusOK {
		test.Fatalf("GET issue = %d: %s", recorder.Code, recorder.Body.String())
	}
	var body struct {
		Metadata map[string]any `json:"metadata"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		test.Fatal(err)
	}
	for key, value := range body.Metadata {
		switch value.(type) {
		case string, float64, bool:
		default:
			test.Fatalf("metadata[%q] = %v (%T): the contract allows primitive values only", key, value, value)
		}
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
	if status, readiness, from := fixture.state(test); result.ParkedActivity != nil || status != "need_decision" || readiness != "ready" || from != "" {
		test.Fatalf("parked=%v status=%q readiness=%q parked_from=%q: an already parked issue must stay untouched", result.ParkedActivity != nil, status, readiness, from)
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
	if dbfx.Count(test, `SELECT count(*) FROM issue_decision_park WHERE issue_id = $1`, fixture.issueID) != 0 {
		test.Fatal("the park record outlived the answer")
	}
	wantDecisionStatusRows(test, fixture.issueID, 2) // park, then restore
}

func TestDecisionCancellationRestoresTheParkedIssue(test *testing.T) {
	fixture := newParkFixture(test)
	if _, err := testHandler.createDecisionRequest(fixture.request(), fixture.issue, fixture.input()); err != nil {
		test.Fatal(err)
	}
	dbfx.Exec(test, `UPDATE decision_request SET status = 'cancelled', cancel_reason = 'lane_archived' WHERE issue_id = $1`, fixture.issueID)
	parked, _ := testHandler.Queries.GetIssue(context.Background(), parseUUID(fixture.issueID))
	if err := testHandler.checkDecisionWait(context.Background(), parked); err != nil {
		test.Fatal(err)
	}
	if status, readiness, from := fixture.state(test); status != "in_progress" || readiness != "ready" || from != "" {
		test.Fatalf("status=%q readiness=%q parked_from=%q, want in_progress/ready and no record", status, readiness, from)
	}
	wantDecisionStatusRows(test, fixture.issueID, 2) // park, then restore
}

func TestDecisionAnswerKeepsAReadinessSetDuringTheWait(test *testing.T) {
	resetDecisionLease(test)
	readinessID := ensureParkCatalog(test)
	fixture := newCardFixture(test)
	dbfx.Exec(test, `UPDATE issue SET properties = jsonb_build_object($1::text, 'ready') WHERE id = $2`, readinessID, fixture.issueID)
	if _, err := fixture.answerOnCard(decisionCardAnswerInput{Option: "Option A"}); err != nil {
		test.Fatal(err)
	}
	dbfx.Exec(test, `UPDATE agent_task_queue SET status = 'completed' WHERE id IN ($1, $2)`, fixture.taskID, fixture.chatTaskID)
	_, runner := newDecisionRunnerFixture(test)
	decisionPass(test, runner)
	var readiness string
	dbfx.QueryRow(test, `SELECT COALESCE(properties ->> $1, '') FROM issue WHERE id = $2`, readinessID, fixture.issueID).Scan(&readiness)
	if readiness != "ready" {
		test.Fatalf("readiness = %q, want the value set during the wait (ready)", readiness)
	}
}

func TestDecisionWaitGuardRestoresPastAnEarlierAnsweredRequest(test *testing.T) {
	fixture := newParkFixture(test)
	if _, err := testHandler.createDecisionRequest(fixture.request(), fixture.issue, fixture.input()); err != nil {
		test.Fatal(err)
	}
	dbfx.Exec(test, `UPDATE decision_request SET status = 'answered' WHERE issue_id = $1`, fixture.issueID)
	parked, _ := testHandler.Queries.GetIssue(context.Background(), parseUUID(fixture.issueID))
	if err := testHandler.checkDecisionWait(context.Background(), parked); err != nil {
		test.Fatal(err)
	}
	if status, _, from := fixture.state(test); status != "in_progress" || from != "" {
		test.Fatalf("status=%q parked_from=%q: an answered request with no answer awaiting delivery must not hold the park", status, from)
	}
}

func TestDecisionWaitGuardKeepsTheParkWhileAnAnswerAwaitsDelivery(test *testing.T) {
	resetDecisionLease(test)
	ensureParkCatalog(test)
	fixture := newCardFixture(test)
	if _, err := fixture.answerOnCard(decisionCardAnswerInput{Option: "Option A"}); err != nil {
		test.Fatal(err)
	}
	parked, _ := testHandler.Queries.GetIssue(context.Background(), parseUUID(fixture.issueID))
	if err := testHandler.checkDecisionWait(context.Background(), parked); err != nil {
		test.Fatal(err)
	}
	var status string
	dbfx.QueryRow(test, `SELECT status FROM issue WHERE id = $1`, fixture.issueID).Scan(&status)
	if status != "need_decision" {
		test.Fatalf("status = %q, want the park kept until the answer is delivered", status)
	}
}

func TestDecisionWorkerPassRestoresABlockedFallbackPark(test *testing.T) {
	var kept int
	dbfx.QueryRow(test, `SELECT count(*) FROM issue_status WHERE workspace_id = $1 AND key = 'need_decision'`, testWorkspaceID).Scan(&kept)
	dbfx.Exec(test, `DELETE FROM issue_status WHERE workspace_id = $1 AND key = 'need_decision'`, testWorkspaceID)
	if kept > 0 {
		test.Cleanup(func() {
			dbfx.Insert(test, "issue_status", testutil.Cols{"workspace_id": testWorkspaceID, "key": "need_decision", "name": "Need Decision", "category": "started", "color": "#112233"})
		})
	}
	fixture := newRequestCoreFixture(test, false)
	dbfx.Exec(test, `UPDATE issue SET status = 'in_progress' WHERE id = $1`, fixture.issueID)
	fixture.issue, _ = testHandler.Queries.GetIssue(context.Background(), parseUUID(fixture.issueID))
	if _, err := testHandler.createDecisionRequest(fixture.request(), fixture.issue, fixture.input()); err != nil {
		test.Fatal(err)
	}
	var status string
	dbfx.QueryRow(test, `SELECT status FROM issue WHERE id = $1`, fixture.issueID).Scan(&status)
	if status != "blocked" {
		test.Fatalf("after the ask: status = %q, want the blocked fallback", status)
	}
	dbfx.Exec(test, `UPDATE decision_request SET status = 'cancelled', cancel_reason = 'lane_archived' WHERE issue_id = $1`, fixture.issueID)
	if err := testHandler.checkDecisionWaits(context.Background()); err != nil {
		test.Log(err)
	}
	dbfx.QueryRow(test, `SELECT status FROM issue WHERE id = $1`, fixture.issueID).Scan(&status)
	if status != "in_progress" {
		test.Fatalf("after the cancellation: status = %q, want in_progress", status)
	}
}

func TestDecisionAnswerKeepsTheParkWhileAnotherAnswerAwaitsDelivery(test *testing.T) {
	resetDecisionLease(test)
	ensureParkCatalog(test)
	fixture := newCardFixture(test)
	if _, err := fixture.answerOnCard(decisionCardAnswerInput{Option: "Option A"}); err != nil {
		test.Fatal(err)
	}
	otherID := dbfx.Insert(test, "decision_request", testutil.Cols{"workspace_id": testWorkspaceID, "issue_id": fixture.issueID, "asking_agent_id": fixture.agentID, "human_user_id": testUserID, "chat_session_id": fixture.sessionID, "issue_comment_id": fixture.parentID, "ask": "Second ask", "escalation_class": "reversible", "status": "answered", "origin": "asked"})
	dbfx.Insert(test, "decision_answer", testutil.Cols{"decision_request_id": otherID, "answered_by_task_id": fixture.chatTaskID, "source_chat_session_id": fixture.sessionID, "chat_message_ids": "{" + fixture.messageID + "}", "content_snapshot": "Yes", "effective_at": time.Now(), "wake_at": time.Now().Add(time.Hour)})
	dbfx.Exec(test, `UPDATE agent_task_queue SET status = 'completed' WHERE id IN ($1, $2)`, fixture.taskID, fixture.chatTaskID)
	_, runner := newDecisionRunnerFixture(test)
	decisionPass(test, runner)
	var status string
	dbfx.QueryRow(test, `SELECT status FROM issue WHERE id = $1`, fixture.issueID).Scan(&status)
	if status != "need_decision" || dbfx.Count(test, `SELECT count(*) FROM issue_decision_park WHERE issue_id = $1`, fixture.issueID) != 1 {
		test.Fatalf("status = %q: delivering one answer must keep the park while another is undelivered", status)
	}
}

// blockedCardFixture is a card asked on an issue that is already `blocked`
// (CHE-5239). With the need_decision step listed the park moves it there; without
// it the issue stays `blocked`. Either way the park must remember `blocked`.
func blockedCardFixture(test *testing.T, needDecision bool) askedRecordFixture {
	test.Helper()
	resetDecisionLease(test)
	ensureParkCatalog(test)
	if !needDecision {
		dbfx.Exec(test, `DELETE FROM issue_status WHERE workspace_id = $1 AND key = 'need_decision'`, testWorkspaceID)
		test.Cleanup(func() {
			dbfx.Insert(test, "issue_status", testutil.Cols{"workspace_id": testWorkspaceID, "key": "need_decision", "name": "Need Decision", "category": "started", "color": "#112233"})
		})
	}
	core := newRequestCoreFixture(test, false)
	dbfx.Exec(test, `UPDATE issue SET status = 'blocked' WHERE id = $1`, core.issueID)
	core.issue, _ = testHandler.Queries.GetIssue(context.Background(), parseUUID(core.issueID))
	result, err := testHandler.createDecisionRequest(core.request(), core.issue, core.input())
	if err != nil {
		test.Fatal(err)
	}
	if result.ParkedActivity != nil {
		core.issue = result.ParkedIssue
	}
	messageID := dbfx.Insert(test, "chat_message", testutil.Cols{"chat_session_id": core.sessionID, "role": "user", "content": "Yes.", "sent_via": "interactive_jwt"})
	chatTaskID := dbfx.Task(test, core.agentID, testutil.Cols{"status": "running", "chat_session_id": core.sessionID, "runtime_id": handlerTestRuntimeID(test)})
	dbfx.Exec(test, `UPDATE agent_task_queue SET started_at = clock_timestamp() WHERE id = $1`, chatTaskID)
	dbfx.Exec(test, `UPDATE decision_request SET ask_json = $2::jsonb, options = $3::jsonb WHERE id = $1`, result.Request.ID, cardAskJSON, cardOptions)
	dbfx.Exec(test, `UPDATE decision_intent SET sealed_at = clock_timestamp() WHERE id = $1`, core.intentID)
	return askedRecordFixture{core, result.Request, uuidToString(result.Comment.ID), messageID, chatTaskID}
}

func (fixture askedRecordFixture) answerAndDeliver(test *testing.T, finishRun bool) {
	test.Helper()
	if _, err := fixture.answerOnCard(decisionCardAnswerInput{Option: "Option A"}); err != nil {
		test.Fatal(err)
	}
	dbfx.Exec(test, `UPDATE agent_task_queue SET status = 'completed' WHERE id = $1`, fixture.chatTaskID)
	if finishRun {
		dbfx.Exec(test, `UPDATE agent_task_queue SET status = 'completed' WHERE id = $1`, fixture.taskID)
	}
	_, runner := newDecisionRunnerFixture(test)
	decisionPass(test, runner)
}

func (fixture askedRecordFixture) status(test *testing.T) string {
	test.Helper()
	var status string
	dbfx.QueryRow(test, `SELECT status FROM issue WHERE id = $1`, fixture.issueID).Scan(&status)
	return status
}

func (fixture askedRecordFixture) assigneeRuns(test *testing.T) int {
	test.Helper()
	return dbfx.Count(test, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND agent_id = $2 AND status IN ('queued','dispatched','running')`, fixture.issueID, fixture.agentID)
}

// The card was the only blocker: the answer resumes the issue and starts one run.
func TestDecisionAnswerResumesABlockedIssueWhenTheCardWasTheOnlyBlocker(test *testing.T) {
	fixture := blockedCardFixture(test, true)
	fixture.answerAndDeliver(test, true)
	if status := fixture.status(test); status != "in_progress" {
		test.Fatalf("status = %q, want in_progress", status)
	}
	if runs := fixture.assigneeRuns(test); runs != 1 {
		test.Fatalf("assignee runs = %d, want 1", runs)
	}
}

// The workspace lists no need_decision step: the blocked issue gets no status
// move, but the park is still recorded so the answer resumes it (CHE-5239).
func TestDecisionAnswerResumesABlockedIssueWithoutANeedDecisionStep(test *testing.T) {
	fixture := blockedCardFixture(test, false)
	if status := fixture.status(test); status != "blocked" {
		test.Fatalf("after the ask: status = %q, want blocked", status)
	}
	fixture.answerAndDeliver(test, true)
	if status := fixture.status(test); status != "in_progress" {
		test.Fatalf("status = %q, want in_progress", status)
	}
	if runs := fixture.assigneeRuns(test); runs != 1 {
		test.Fatalf("assignee runs = %d, want 1", runs)
	}
}

// An armed event wait with no condition (task.completed) is another blocker too.
func TestDecisionAnswerKeepsABlockedIssueBlockedWhileAnEventWaitIsArmed(test *testing.T) {
	fixture := blockedCardFixture(test, true)
	dbfx.Insert(test, "issue_wakeup", testutil.Cols{"id": uuid.NewString(), "workspace_id": testWorkspaceID, "issue_id": fixture.issueID, "agent_id": fixture.agentID, "instruction": "wait for the other run", "kind": "event", "mode": "once", "event_types": "{task.completed}"})
	fixture.answerAndDeliver(test, true)
	if status := fixture.status(test); status != "blocked" {
		test.Fatalf("status = %q, want blocked", status)
	}
	if runs := fixture.assigneeRuns(test); runs != 1 {
		test.Fatalf("assignee runs = %d, want 1", runs)
	}
}

// Another blocker is still recorded: the issue stays blocked, one run re-checks it.
func TestDecisionAnswerKeepsABlockedIssueBlockedWhileAnotherBlockerIsOpen(test *testing.T) {
	fixture := blockedCardFixture(test, true)
	dbfx.Insert(test, "issue_wakeup", testutil.Cols{"id": uuid.NewString(), "workspace_id": testWorkspaceID, "issue_id": fixture.issueID, "agent_id": fixture.agentID, "instruction": "wait for the PR", "kind": "event", "mode": "once", "condition": `{"type":"pr","state":"checks"}`})
	fixture.answerAndDeliver(test, true)
	if status := fixture.status(test); status != "blocked" {
		test.Fatalf("status = %q, want blocked", status)
	}
	if runs := fixture.assigneeRuns(test); runs != 1 {
		test.Fatalf("assignee runs = %d, want 1", runs)
	}
}

// CHE-5171 facts 1-4: a run stamped with the step the issue had while parked
// writes its status after the system unpark. The write must not be lost.
func TestAgentStatusWriteAfterTheSystemUnparkIsNotLost(test *testing.T) {
	fixture := blockedCardFixture(test, true)
	projectID := deliveryProject(test, "Unpark stale step project", fixture.agentID)
	dbfx.Exec(test, `UPDATE issue SET project_id = $1 WHERE id = $2`, projectID, fixture.issueID)
	// The run was queued while the issue was parked, so it is stamped with that step.
	dbfx.Exec(test, `UPDATE agent_task_queue SET status = 'queued', started_at = NULL, workflow_step = 'need_decision' WHERE id = $1`, fixture.taskID)
	fixture.answerAndDeliver(test, false)
	dbfx.Exec(test, `UPDATE agent_task_queue SET status = 'running', started_at = now() WHERE id = $1`, fixture.taskID)
	if status := fixture.status(test); status != "in_progress" {
		test.Fatalf("after the answer: status = %q, want in_progress", status)
	}
	agentUpdateIssue(test, fixture.issueID, fixture.agentID, fixture.taskID, map[string]any{"status": "in_review"}).Want(http.StatusOK)
	if status := fixture.status(test); status != "in_review" {
		test.Fatalf("after the agent's write: status = %q, want in_review", status)
	}
}
