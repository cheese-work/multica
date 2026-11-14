package handler

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
)

// CHE-2394 P4b: disclaiming an action answer supersedes its answered event and
// pending receipts, and retracts only to recipients that already got it.

func disclaimAnswer(test *testing.T, fixture askedRecordFixture, answer pgtype.UUID) {
	test.Helper()
	if _, err := testHandler.disclaimDecisionAnswerTx(fixture.recordRequest(), fixture.issue.WorkspaceID, answer, parseUUID(fixture.humanID)); err != nil {
		test.Fatal(err)
	}
}

func receiptsOfEvent(test *testing.T, request, answer pgtype.UUID, eventType string) map[string]receiptState {
	test.Helper()
	rows, err := testPool.Query(context.Background(), `SELECT r.recipient_agent_id::text, r.status, coalesce(r.outcome, ''), r.delivered_at
		FROM decision_wake_receipt r JOIN decision_wake_event e ON e.id = r.event_id
		WHERE e.request_id = $1 AND e.answer_id = $2 AND e.type = $3`, request, answer, eventType)
	if err != nil {
		test.Fatal(err)
	}
	defer rows.Close()
	states := map[string]receiptState{}
	for rows.Next() {
		var agent string
		var state receiptState
		if err := rows.Scan(&agent, &state.Status, &state.Outcome, &state.DeliveredAt); err != nil {
			test.Fatal(err)
		}
		states[agent] = state
	}
	return states
}

// Sol (a): answer A, deliver A, disclaim A, answer B. Each recipient gets the
// retraction of A, then B exactly once, with B's source.
func TestDisclaimActionAnswerRetractsDeliveredThenCorrectedAnswerOnce(test *testing.T) {
	fixture, recordedA, leader := squadActionFixture(test)
	ctx := context.Background()
	agents := []string{fixture.agentID, leader}
	if err := testHandler.deliverDecisionWakeReceipts(ctx); err != nil {
		test.Fatal(err)
	}
	for agent, got := range receiptsOfEvent(test, fixture.decision.ID, recordedA.Answer.ID, "answered") {
		if got.Status != "delivered" {
			test.Fatalf("A for %s = %+v, want delivered", agent, got)
		}
	}

	disclaimAnswer(test, fixture, recordedA.Answer.ID)
	if n := dbfx.Count(test, `SELECT count(*) FROM decision_wake_event WHERE answer_id = $1 AND type = 'answered' AND superseded_reason = 'answer_disclaimed' AND superseded_at IS NOT NULL`, recordedA.Answer.ID); n != 1 {
		test.Fatalf("answered event of A superseded rows = %d, want 1", n)
	}
	retractions := receiptsOfEvent(test, fixture.decision.ID, recordedA.Answer.ID, "retracted")
	for _, agent := range agents {
		if got, ok := retractions[agent]; !ok || got.Status != "pending" {
			test.Fatalf("retraction for %s = %+v (present %v), want one pending", agent, got, ok)
		}
	}
	var withdrawal pgtype.UUID
	dbfx.QueryRow(test, `SELECT withdrawal_comment_id FROM decision_answer WHERE id = $1`, recordedA.Answer.ID).Scan(&withdrawal)
	if n := dbfx.Count(test, `SELECT count(*) FROM decision_wake_event WHERE answer_id = $1 AND type = 'retracted' AND source_comment_id = $2`, recordedA.Answer.ID, withdrawal); n != 1 {
		test.Fatalf("retracted events sourced by the disclaim comment = %d, want 1", n)
	}

	dbfx.Exec(test, `UPDATE agent_task_queue SET status = 'running' WHERE id IN ($1, $2)`, fixture.taskID, fixture.chatTaskID)
	recordedB, err := testHandler.createAskedDecisionRecord(fixture.recordRequest(), fixture.issue, fixture.recordInput())
	if err != nil {
		test.Fatal(err)
	}
	dbfx.Exec(test, `UPDATE agent_task_queue SET status = 'completed' WHERE id IN ($1, $2)`, fixture.taskID, fixture.chatTaskID)
	finalizeAnswerWake(test, fixture)
	for pass := 0; pass < 3; pass++ {
		if err := testHandler.deliverDecisionWakeReceipts(ctx); err != nil {
			test.Fatal(err)
		}
	}
	retractions = receiptsOfEvent(test, fixture.decision.ID, recordedA.Answer.ID, "retracted")
	answersB := receiptsOfEvent(test, fixture.decision.ID, recordedB.Answer.ID, "answered")
	for _, agent := range agents {
		first, second := retractions[agent], answersB[agent]
		if first.Status != "delivered" || second.Status != "delivered" {
			test.Fatalf("%s: retraction = %+v, B = %+v, want both delivered", agent, first, second)
		}
		if second.DeliveredAt.Time.Before(first.DeliveredAt.Time) {
			test.Fatalf("%s got B before the retraction of A", agent)
		}
	}
	if len(answersB) != len(agents) {
		test.Fatalf("B receipts = %d, want exactly one per recipient (%d)", len(answersB), len(agents))
	}
	if n := dbfx.Count(test, `SELECT count(*) FROM decision_wake_event WHERE answer_id = $1 AND type = 'answered' AND source_comment_id = $2`, recordedB.Answer.ID, recordedB.Answer.IssueCommentID); n != 1 {
		test.Fatalf("B event does not carry B's source comment")
	}
}

// Sol (b): disclaim A after only recipient 1 got A. Recipient 1 gets the
// retraction. Recipient 2's A receipt is superseded and never presents A.
func TestDisclaimActionAnswerSupersedesUndeliveredReceipt(test *testing.T) {
	fixture, recordedA, leader := squadActionFixture(test)
	ctx := context.Background()
	requester, assignee := receiptIDFor(test, fixture.decision.ID, fixture.agentID), receiptIDFor(test, fixture.decision.ID, leader)
	if err := testHandler.deliverDecisionWakeReceipt(ctx, requester); err != nil {
		test.Fatal(err)
	}
	if got := receiptStateOf(test, requester); got.Status != "delivered" {
		test.Fatalf("requester A receipt = %+v, want delivered", got)
	}

	disclaimAnswer(test, fixture, recordedA.Answer.ID)
	if got := receiptStateOf(test, assignee); got.Status != "superseded" {
		test.Fatalf("assignee A receipt = %+v, want superseded", got)
	}
	retractions := receiptsOfEvent(test, fixture.decision.ID, recordedA.Answer.ID, "retracted")
	if got, ok := retractions[fixture.agentID]; !ok || got.Status != "pending" {
		test.Fatalf("recipient 1 retraction = %+v (present %v), want pending", got, ok)
	}
	if _, ok := retractions[leader]; ok || len(retractions) != 1 {
		test.Fatalf("retractions = %v, want only recipient 1", retractions)
	}

	if err := testHandler.deliverDecisionWakeReceipts(ctx); err != nil {
		test.Fatal(err)
	}
	if got := receiptsOfEvent(test, fixture.decision.ID, recordedA.Answer.ID, "retracted")[fixture.agentID]; got.Status != "delivered" {
		test.Fatalf("recipient 1 retraction = %+v, want delivered", got)
	}
	if got := receiptStateOf(test, assignee); got.Status != "superseded" || got.DeliveredAt.Valid {
		test.Fatalf("assignee A receipt = %+v, want superseded and never delivered", got)
	}
	if n := activeTaskCount(test, fixture.issueID); n != 1 {
		test.Fatalf("active tasks = %d, want only recipient 1's retraction run", n)
	}
}
