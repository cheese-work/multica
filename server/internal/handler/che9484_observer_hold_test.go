package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// CHE-9484 E6 pins the platform contract a long-lived observer issue relies on:
// an agent parks the issue with a decision request, its own cron rule keeps
// firing while parked, the asker withdraws, the human answers, and the idle
// guard leaves the issue alone only while a rule is armed. Every case goes
// through the real handlers; none changes runtime code.

// observerHold is an in_progress issue assigned to agent A (the asker), in a
// project whose workflow hands need_decision to another agent B. A system park
// writes no handoff, so B must never receive a run.
type observerHold struct {
	parkFixture
	handlerID string
}

func newObserverHold(test *testing.T) observerHold {
	test.Helper()
	// The live workspace parks only on `wait`.
	setDecisionAskPark(test, "on_wait")
	resetDecisionLease(test)
	fixture := newParkFixture(test)
	handlerID := createHandlerTestAgent(test, "need-decision handler "+fixture.agentName, []byte("[]"))
	projectID := createWorkflowTestProject(test, "CHE-9484 observer hold")
	setColumnHandoff(test, projectID, "need_decision", handToAgent(handlerID, "Decide it.")).Want(http.StatusOK)
	dbfx.Exec(test, `UPDATE issue SET project_id = $1 WHERE id = $2`, projectID, fixture.issueID)
	dbfx.Cleanup(test, `DELETE FROM issue_wakeup_receipt WHERE wakeup_id IN (SELECT id FROM issue_wakeup WHERE issue_id = $1)`, fixture.issueID)
	dbfx.Cleanup(test, `DELETE FROM issue_wakeup WHERE issue_id = $1`, fixture.issueID)
	dbfx.Cleanup(test, `DELETE FROM agent_task_queue WHERE issue_id = $1`, fixture.issueID)
	return observerHold{parkFixture: fixture, handlerID: handlerID}
}

// asTask makes the request the one agent A's running task sends.
func (hold observerHold) asTask(request *http.Request) *http.Request {
	request.Header.Set("X-Actor-Source", "task_token")
	request.Header.Set("X-Agent-ID", hold.agentID)
	request.Header.Set("X-Task-ID", hold.taskID)
	return request
}

// ask sends one decision request with wait and returns it.
func (hold observerHold) ask(test *testing.T) db.DecisionRequest {
	test.Helper()
	return hold.waitAsk(test, true, true)
}

// armCron creates the observer rule through the API, as A's task does.
func (hold observerHold) armCron(test *testing.T) db.IssueWakeup {
	test.Helper()
	body := map[string]any{
		"kind": "cron", "cron_expression": "30 10 * * 1,3,5", "timezone": "Asia/Ho_Chi_Minh",
		"instruction": "Observe.", "watch_inputs": []string{"always"}, "agent_id": hold.agentID,
	}
	request := withURLParam(hold.asTask(newRequest(http.MethodPost, "/api/issues/"+hold.issueID+"/wakeups", body)), "id", hold.issueID)
	var rule db.IssueWakeup
	testutil.Call(test, testHandler.CreateIssueWakeup, request).Want(http.StatusCreated).JSON(&rule)
	return rule
}

func (hold observerHold) withdraw(test *testing.T, requestID pgtype.UUID) {
	test.Helper()
	request := withURLParam(hold.asTask(newRequest(http.MethodPost, "/api/decisions/"+uuidToString(requestID)+"/withdraw", map[string]any{"reason": "Unanswered for 168 h."})), "id", uuidToString(requestID))
	testutil.Call(test, testHandler.WithdrawDecision, request).Want(http.StatusOK)
}

// answer is the request's human clicking the first option on the card.
func (hold observerHold) answer(test *testing.T, requestID pgtype.UUID) {
	test.Helper()
	option := hold.structuredAsk().Options[0].Label
	request := withURLParam(newRequest(http.MethodPost, "/api/decision-requests/"+uuidToString(requestID)+"/answer", map[string]any{"option": option}), "id", uuidToString(requestID))
	request.Header.Set("X-Actor-Source", "interactive_jwt")
	request.Header.Set("X-User-ID", hold.humanID)
	testutil.Call(test, testHandler.AnswerDecisionRequest, request).Want(http.StatusCreated)
}

// finishRun ends A's running task, as it ends when the observer run exits.
func (hold observerHold) finishRun(test *testing.T) {
	test.Helper()
	dbfx.Exec(test, `UPDATE agent_task_queue SET status = 'completed', completed_at = now() WHERE id = $1`, hold.taskID)
}

func (hold observerHold) status(test *testing.T) string {
	test.Helper()
	var status string
	dbfx.QueryRow(test, `SELECT status FROM issue WHERE id = $1`, hold.issueID).Scan(&status)
	return status
}

// queued counts the runs waiting or running for one agent on the issue.
func (hold observerHold) queued(test *testing.T, agentID string) int {
	test.Helper()
	return dbfx.Count(test, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND agent_id = $2 AND status IN ('queued', 'dispatched', 'running') AND id <> $3`, hold.issueID, agentID, hold.taskID)
}

func (hold observerHold) queuedAny(test *testing.T) int {
	test.Helper()
	return dbfx.Count(test, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND status IN ('queued', 'dispatched', 'running') AND id <> $2`, hold.issueID, hold.taskID)
}

func (hold observerHold) assertAssignee(test *testing.T) {
	test.Helper()
	if got := hold.assignee(test); got != hold.agentID {
		test.Fatalf("assignee = %q, want agent A %q", got, hold.agentID)
	}
}

// assertSystemDecisionStatus fails unless the newest status change was written
// by the system with cause decision and moved the issue to `to`.
func (hold observerHold) assertSystemDecisionStatus(test *testing.T, to string) {
	test.Helper()
	rows := statusRows(test, hold.issueID)
	if len(rows) == 0 {
		test.Fatalf("no status_changed rows, want a system move to %s", to)
	}
	last := rows[len(rows)-1]
	trigger, _ := last["trigger"].(map[string]any)
	cause, _ := trigger["cause"].(map[string]any)
	if last["to"] != to || last["actor_type"] != nil && last["actor_type"] != "system" || cause["kind"] != "decision" {
		test.Fatalf("last status row = %v, want a move to %s by system with cause decision", last, to)
	}
}

func (hold observerHold) enabledCron(test *testing.T, ruleID pgtype.UUID) (enabled, hasNext bool) {
	test.Helper()
	dbfx.QueryRow(test, `SELECT enabled, next_fire_at IS NOT NULL FROM issue_wakeup WHERE id = $1`, ruleID).Scan(&enabled, &hasNext)
	return
}

// (1) A's task asks with wait: the issue parks, A stays the assignee, one park row.
func TestCHE9484ObserverHoldWaitAskParksTheIssue(test *testing.T) {
	hold := newObserverHold(test)
	hold.ask(test)
	if status := hold.status(test); status != "need_decision" {
		test.Fatalf("status = %q, want need_decision", status)
	}
	hold.assertAssignee(test)
	if count := dbfx.Count(test, `SELECT count(*) FROM issue_decision_park WHERE issue_id = $1 AND prior_status = 'in_progress' AND to_status = 'need_decision'`, hold.issueID); count != 1 {
		test.Fatalf("park rows = %d, want 1", count)
	}
	if count := dbfx.Count(test, `SELECT count(*) FROM issue_decision_park WHERE issue_id = $1`, hold.issueID); count != 1 {
		test.Fatalf("all park rows = %d, want 1", count)
	}
	if runs := hold.queuedAny(test); runs != 0 {
		test.Fatalf("the park queued %d runs, want 0", runs)
	}
}

// (2) The observer's cron rule keeps firing while the issue is parked. The fire
// starts A, writes no status and hands nothing to the need_decision handler.
func TestCHE9484ObserverHoldCronFiresWhileParked(test *testing.T) {
	hold := newObserverHold(test)
	rule := hold.armCron(test)
	if enabled, hasNext := hold.enabledCron(test, rule.ID); !enabled || !hasNext {
		test.Fatalf("rule enabled=%v next_fire_at set=%v, want both before the ask", enabled, hasNext)
	}
	hold.ask(test)
	hold.finishRun(test)
	if enabled, hasNext := hold.enabledCron(test, rule.ID); !enabled || !hasNext {
		test.Fatalf("while parked: rule enabled=%v next_fire_at set=%v, want both", enabled, hasNext)
	}
	statusRowsBefore := len(statusRows(test, hold.issueID))
	dbfx.Exec(test, `UPDATE issue_wakeup SET next_fire_at = now() - interval '1 minute' WHERE id = $1`, rule.ID)
	if err := (&service.IssueWakeupService{Tasks: testHandler.TaskService}).TickWorkspaces(context.Background(), parseUUID(testWorkspaceID)); err != nil {
		test.Fatal(err)
	}
	if runs := hold.queued(test, hold.agentID); runs != 1 {
		test.Fatalf("runs queued for A by the fire = %d, want 1", runs)
	}
	if runs := hold.queued(test, hold.handlerID); runs != 0 {
		test.Fatalf("runs queued for the need_decision handler = %d, want 0", runs)
	}
	if runs := hold.queuedAny(test); runs != 1 {
		test.Fatalf("runs queued for any agent = %d, want 1", runs)
	}
	if status := hold.status(test); status != "need_decision" {
		test.Fatalf("status after the fire = %q, want need_decision", status)
	}
	if rows := len(statusRows(test, hold.issueID)); rows != statusRowsBefore {
		test.Fatalf("status_changed rows %d -> %d: the fire wrote a status", statusRowsBefore, rows)
	}
	hold.assertAssignee(test)
	if enabled, hasNext := hold.enabledCron(test, rule.ID); !enabled || !hasNext {
		test.Fatalf("after the fire: rule enabled=%v next_fire_at set=%v, want both", enabled, hasNext)
	}
}

// (3) The asker withdraws: the platform puts the issue back and starts no run,
// the cron rule stays armed, and the same task may still arm a condition rule
// and write metadata.
func TestCHE9484ObserverHoldWithdrawRestoresInProgress(test *testing.T) {
	hold := newObserverHold(test)
	rule := hold.armCron(test)
	asked := hold.ask(test)
	hold.withdraw(test, asked.ID)
	_, runner := newDecisionRunnerFixture(test)
	decisionPass(test, runner)
	if status := hold.status(test); status != "in_progress" {
		test.Fatalf("status after the withdrawal = %q, want in_progress", status)
	}
	hold.assertSystemDecisionStatus(test, "in_progress")
	hold.assertAssignee(test)
	if count := dbfx.Count(test, `SELECT count(*) FROM issue_decision_park WHERE issue_id = $1`, hold.issueID); count != 0 {
		test.Fatalf("park rows after the withdrawal = %d, want 0", count)
	}
	if runs := hold.queuedAny(test); runs != 0 {
		test.Fatalf("runs queued for any agent after the withdrawal = %d, want 0", runs)
	}
	if enabled, hasNext := hold.enabledCron(test, rule.ID); !enabled || !hasNext {
		test.Fatalf("after the withdrawal: rule enabled=%v next_fire_at set=%v, want both", enabled, hasNext)
	}

	watched := dbfx.Issue(test, "CHE-9449 stand-in", testutil.Cols{})
	condition, err := json.Marshal(map[string]any{"type": "other_issue", "issue_id": watched, "state": "done"})
	if err != nil {
		test.Fatal(err)
	}
	body := map[string]any{"kind": "event", "mode": "once", "agent_id": hold.agentID, "instruction": "CHE-9449 closed.", "condition": json.RawMessage(condition)}
	request := withURLParam(hold.asTask(newRequest(http.MethodPost, "/api/issues/"+hold.issueID+"/wakeups", body)), "id", hold.issueID)
	testutil.Call(test, testHandler.CreateIssueWakeup, request).Want(http.StatusCreated)
	if count := dbfx.Count(test, `SELECT count(*) FROM issue_wakeup WHERE issue_id = $1 AND enabled AND condition ->> 'type' = 'other_issue'`, hold.issueID); count != 1 {
		test.Fatalf("enabled condition rules = %d, want 1", count)
	}

	metadata := withURLParam(hold.asTask(newRequest(http.MethodPut, "/api/issues/"+hold.issueID+"/metadata/hold", map[string]any{"value": "waiting_che9449"})), "id", hold.issueID)
	metadata = withURLParam(metadata, "key", "hold")
	testutil.Call(test, testHandler.SetIssueMetadataKey, metadata).Want(http.StatusOK)
	var held string
	dbfx.QueryRow(test, `SELECT COALESCE(metadata ->> 'hold', '') FROM issue WHERE id = $1`, hold.issueID).Scan(&held)
	if held != "waiting_che9449" {
		test.Fatalf("metadata hold = %q, want waiting_che9449", held)
	}
}

// (4) A member answers a request on the card: the issue stays parked until the
// answer is delivered, then returns to in_progress with one run for A.
func TestCHE9484ObserverHoldAnswerRestoresAfterDelivery(test *testing.T) {
	hold := newObserverHold(test)
	asked := hold.ask(test)
	hold.finishRun(test)
	hold.answer(test, asked.ID)
	if status := hold.status(test); status != "need_decision" {
		test.Fatalf("status after the answer, before delivery = %q, want need_decision", status)
	}
	_, runner := newDecisionRunnerFixture(test)
	decisionPass(test, runner)
	if status := hold.status(test); status != "in_progress" {
		test.Fatalf("status after delivery = %q, want in_progress", status)
	}
	hold.assertAssignee(test)
	if runs := hold.queued(test, hold.agentID); runs != 1 {
		test.Fatalf("runs queued for A after delivery = %d, want 1", runs)
	}
	if runs := hold.queued(test, hold.handlerID); runs != 0 {
		test.Fatalf("runs queued for the need_decision handler = %d, want 0", runs)
	}
}

// (5) The idle guard's own scan leaves an issue with an armed cron rule alone
// and selects it once the rule is disabled and the idle time has passed.
func TestCHE9484ObserverHoldIdleGuardSelectsOnlyWithoutARule(test *testing.T) {
	hold := newObserverHold(test)
	rule := hold.armCron(test)
	asked := hold.ask(test)
	hold.withdraw(test, asked.ID)
	_, runner := newDecisionRunnerFixture(test)
	decisionPass(test, runner)
	hold.finishRun(test)
	selected := func() bool {
		test.Helper()
		dbfx.Exec(test, `UPDATE issue SET created_at = now() - interval '3 days' WHERE id = $1`, hold.issueID)
		dbfx.Exec(test, `UPDATE comment SET created_at = now() - interval '3 days' WHERE issue_id = $1`, hold.issueID)
		dbfx.Exec(test, `UPDATE agent_task_queue SET created_at = now() - interval '3 days', completed_at = now() - interval '3 days' WHERE issue_id = $1`, hold.issueID)
		dbfx.Exec(test, `UPDATE activity_log SET created_at = now() - interval '3 days' WHERE issue_id = $1`, hold.issueID)
		rows, err := testHandler.Queries.ListIdleStartedGuardIssues(context.Background(), db.ListIdleStartedGuardIssuesParams{
			WorkspaceID: parseUUID(testWorkspaceID), IssueID: parseUUID(hold.issueID), Statuses: []string{"in_progress"}, AfterSeconds: 3600,
		})
		if err != nil {
			test.Fatal(err)
		}
		return len(rows) == 1
	}
	if selected() {
		test.Fatal("the idle guard selected the issue while the cron rule is enabled")
	}
	disable := withURLParam(newRequest(http.MethodPost, "/api/issues/"+hold.issueID+"/wakeups/"+uuidToString(rule.ID)+"/disable", nil), "id", hold.issueID)
	disable = withURLParam(disable, "wakeupID", uuidToString(rule.ID))
	testutil.Call(test, testHandler.DisableIssueWakeup, disable).Want(http.StatusOK)
	if enabled, _ := hold.enabledCron(test, rule.ID); enabled {
		test.Fatal("the rule is still enabled after the disable call")
	}
	if !selected() {
		test.Fatal("the idle guard did not select the idle issue after the rule was disabled")
	}
}
