package handler

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

// CHE-2394 P4a: delivery-time validation (checks 1 to 4), the older-pending
// check on event_seq, and the two P2 carry-overs from the P3b review.

func receiptPending(test *testing.T, id pgtype.UUID) {
	test.Helper()
	if got := receiptStateOf(test, id); got.Status != "pending" {
		test.Fatalf("receipt = %+v, want pending", got)
	}
}

func TestReceiptDeliverySuppressedWhenIssueClosed(test *testing.T) {
	fixture, _, leader := squadActionFixture(test)
	dbfx.Exec(test, `UPDATE issue SET status = 'done' WHERE id = $1`, fixture.issueID)
	tasks := activeTaskCount(test, fixture.issueID)
	if err := testHandler.deliverDecisionWakeReceipts(context.Background()); err != nil {
		test.Fatal(err)
	}
	for _, agent := range []string{fixture.agentID, leader} {
		if got := receiptStateOf(test, receiptIDFor(test, fixture.decision.ID, agent)); got.Status != "suppressed_issue_closed" || got.Outcome != "" {
			test.Fatalf("receipt for %s = %+v, want suppressed_issue_closed", agent, got)
		}
	}
	if got := activeTaskCount(test, fixture.issueID); got != tasks {
		test.Fatalf("active tasks = %d, want %d: a closed issue was woken", got, tasks)
	}
}

func TestReceiptDeliverySupersededEventNeverDispatches(test *testing.T) {
	fixture, _, leader := squadActionFixture(test)
	dbfx.Exec(test, `UPDATE decision_wake_event SET superseded_at = clock_timestamp(), superseded_reason = 'answer_disclaimed' WHERE request_id = $1`, fixture.decision.ID)
	tasks := activeTaskCount(test, fixture.issueID)
	if err := testHandler.deliverDecisionWakeReceipts(context.Background()); err != nil {
		test.Fatal(err)
	}
	for _, agent := range []string{fixture.agentID, leader} {
		if got := receiptStateOf(test, receiptIDFor(test, fixture.decision.ID, agent)); got.Status != "superseded" {
			test.Fatalf("receipt for %s = %+v, want superseded", agent, got)
		}
	}
	if got := activeTaskCount(test, fixture.issueID); got != tasks {
		test.Fatalf("active tasks = %d, want %d", got, tasks)
	}
}

// P2 note 1 (Terra, decision_wake_receipt.go:192): a pending receipt must not
// dispatch once its answer is withdrawn or its request is cancelled.
func TestReceiptDeliveryDoesNotDispatchAfterAnswerWithdrawnOrRequestCancelled(test *testing.T) {
	for name, stmt := range map[string]string{
		"answer withdrawn":  `UPDATE decision_answer SET disclaimed_at = clock_timestamp() WHERE decision_request_id = $1`,
		"request cancelled": `UPDATE decision_request SET status = 'cancelled', cancel_reason = 'agent_withdrawn' WHERE id = $1`,
	} {
		test.Run(name, func(test *testing.T) {
			fixture, _, leader := squadActionFixture(test)
			dbfx.Exec(test, stmt, fixture.decision.ID)
			tasks := activeTaskCount(test, fixture.issueID)
			if err := testHandler.deliverDecisionWakeReceipts(context.Background()); err != nil {
				test.Fatal(err)
			}
			for _, agent := range []string{fixture.agentID, leader} {
				if got := receiptStateOf(test, receiptIDFor(test, fixture.decision.ID, agent)); got.Status != "superseded" {
					test.Fatalf("receipt for %s = %+v, want superseded", agent, got)
				}
			}
			if got := activeTaskCount(test, fixture.issueID); got != tasks {
				test.Fatalf("active tasks = %d, want %d", got, tasks)
			}
		})
	}
}

// P2 note 2 (Terra, decision_wake_receipt.sql:38): receipt selection respects
// the answer's wake_at, and the check is repeated under the answer lock.
func TestReceiptSelectionRespectsAnswerWakeAt(test *testing.T) {
	fixture, _, leader := squadActionFixture(test)
	dbfx.Exec(test, `UPDATE decision_answer SET wake_at = clock_timestamp() + interval '1 hour' WHERE decision_request_id = $1`, fixture.decision.ID)
	ids, err := testHandler.Queries.ListDecisionWakeReceiptCandidates(context.Background(), 100)
	if err != nil {
		test.Fatal(err)
	}
	for _, id := range ids {
		for _, agent := range []string{fixture.agentID, leader} {
			if id == receiptIDFor(test, fixture.decision.ID, agent) {
				test.Fatalf("receipt %v selected before the answer's wake_at", id)
			}
		}
	}
}

func TestReceiptDeliveryRechecksWakeAtUnderAnswerLock(test *testing.T) {
	fixture, _, leader := squadActionFixture(test)
	ctx := context.Background()
	hold, err := testPool.Begin(ctx)
	if err != nil {
		test.Fatal(err)
	}
	defer hold.Rollback(ctx)
	if _, err := hold.Exec(ctx, `UPDATE decision_answer SET wake_at = clock_timestamp() + interval '1 hour' WHERE decision_request_id = $1`, fixture.decision.ID); err != nil {
		test.Fatal(err)
	}
	receipt := receiptIDFor(test, fixture.decision.ID, leader)
	done := make(chan error, 1)
	go func() { done <- testHandler.deliverDecisionWakeReceipt(ctx, receipt) }()
	time.Sleep(700 * time.Millisecond) // the delivery has read the due answer and waits on its lock
	if err := hold.Commit(ctx); err != nil {
		test.Fatal(err)
	}
	if err := awaitDecisionDeliveryRaceDone(test, done, "receipt delivery"); err != nil {
		test.Fatal(err)
	}
	receiptPending(test, receipt)
	if n := dbfx.Count(test, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND agent_id = $2 AND status = 'queued'`, fixture.issueID, leader); n != 0 {
		test.Fatalf("leader got %d queued tasks before the answer's wake_at", n)
	}
}

// Terra P1 on PR #1244: an answer's wake_at stays NULL while its answering task
// runs. A NULL wake_at is not due: no selection, no dispatch. Delivery follows
// once finalization sets wake_at.
func TestReceiptNotDispatchedWhileAnswerWakeAtIsNull(test *testing.T) {
	fixture, _, leader := squadActionFixture(test)
	dbfx.Exec(test, `UPDATE decision_answer SET wake_at = NULL WHERE decision_request_id = $1`, fixture.decision.ID)
	receipt := receiptIDFor(test, fixture.decision.ID, leader)
	ids, err := testHandler.Queries.ListDecisionWakeReceiptCandidates(context.Background(), 100)
	if err != nil {
		test.Fatal(err)
	}
	for _, id := range ids {
		if id == receipt {
			test.Fatalf("receipt %v selected while the answer's wake_at is NULL", id)
		}
	}
	if err := testHandler.deliverDecisionWakeReceipt(context.Background(), receipt); err != nil {
		test.Fatal(err)
	}
	receiptPending(test, receipt)
	if n := dbfx.Count(test, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND agent_id = $2 AND status = 'queued'`, fixture.issueID, leader); n != 0 {
		test.Fatalf("leader got %d queued tasks while the answer's wake_at is NULL", n)
	}
	dbfx.Exec(test, `UPDATE decision_answer SET wake_at = clock_timestamp() WHERE decision_request_id = $1`, fixture.decision.ID)
	if err := testHandler.deliverDecisionWakeReceipt(context.Background(), receipt); err != nil {
		test.Fatal(err)
	}
	if got := receiptStateOf(test, receipt); got.Status == "pending" {
		test.Fatalf("receipt still pending after wake_at was set: %+v", got)
	}
}

// Sol (e): a disclaim and a delivery of the same receipt finish without a
// deadlock, and the delivery sees the disclaim.
func TestConcurrentDisclaimAndReceiptDeliveryFinishWithoutDeadlock(test *testing.T) {
	fixture, _, leader := squadActionFixture(test)
	ctx := context.Background()
	disclaim, err := testPool.Begin(ctx)
	if err != nil {
		test.Fatal(err)
	}
	defer disclaim.Rollback(ctx)
	// The writer's order: request, answer, event, receipt.
	for _, stmt := range []string{
		`UPDATE decision_request SET wake_seq = wake_seq WHERE id = $1`,
		`UPDATE decision_answer SET disclaimed_at = clock_timestamp() WHERE decision_request_id = $1`,
		`UPDATE decision_wake_event SET superseded_at = clock_timestamp(), superseded_reason = 'answer_disclaimed' WHERE request_id = $1`,
		`UPDATE decision_wake_receipt SET status = 'superseded' WHERE event_id IN (SELECT id FROM decision_wake_event WHERE request_id = $1)`,
	} {
		if _, err := disclaim.Exec(ctx, stmt, fixture.decision.ID); err != nil {
			test.Fatal(err)
		}
	}
	receipt := receiptIDFor(test, fixture.decision.ID, leader)
	done := make(chan error, 1)
	go func() { done <- testHandler.deliverDecisionWakeReceipt(ctx, receipt) }()
	time.Sleep(700 * time.Millisecond)
	if err := disclaim.Commit(ctx); err != nil {
		test.Fatal(err)
	}
	if err := awaitDecisionDeliveryRaceDone(test, done, "receipt delivery"); err != nil {
		test.Fatal(err)
	}
	if got := receiptStateOf(test, receipt); got.Status != "superseded" {
		test.Fatalf("receipt = %+v, want superseded", got)
	}
	if n := dbfx.Count(test, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND agent_id = $2 AND status = 'queued'`, fixture.issueID, leader); n != 0 {
		test.Fatalf("leader got %d queued tasks from a disclaimed answer", n)
	}
}

// Sol (f): UUID order is opposite to event order. Answer A is delivered and
// retracted, answer B is recorded, the retraction dispatch fails once. Each
// recipient gets the retraction before B, and B stays blocked while the
// retraction backs off.
func TestOlderPendingRetractionBlocksCorrectedAnswerDespiteUUIDOrder(test *testing.T) {
	fixture, recordedA, leader := squadActionFixture(test)
	ctx := context.Background()
	agents := []string{fixture.agentID, leader}
	if err := testHandler.deliverDecisionWakeReceipts(ctx); err != nil {
		test.Fatal(err)
	}
	// Disclaim A, write the retraction (its source comment is missing, so its
	// dispatch fails once), reopen, record B.
	dbfx.Exec(test, `UPDATE decision_answer SET disclaimed_at = clock_timestamp() WHERE id = $1`, recordedA.Answer.ID)
	dbfx.Exec(test, `UPDATE decision_wake_event SET superseded_at = clock_timestamp(), superseded_reason = 'answer_disclaimed' WHERE request_id = $1`, fixture.decision.ID)
	request, err := testHandler.Queries.GetDecisionRequestForWake(ctx, fixture.decision.ID)
	if err != nil {
		test.Fatal(err)
	}
	if err := testHandler.recordDecisionWakeEvent(ctx, testHandler.Queries, request, "retracted", recordedA.Answer.ID, pgtype.UUID{Bytes: [16]byte{1}, Valid: true}); err != nil {
		test.Fatal(err)
	}
	dbfx.Exec(test, `UPDATE decision_request SET status = 'open' WHERE id = $1`, fixture.decision.ID)
	dbfx.Exec(test, `UPDATE agent_task_queue SET status = 'running' WHERE id IN ($1, $2)`, fixture.taskID, fixture.chatTaskID)
	recordedB, err := testHandler.createAskedDecisionRecord(fixture.recordRequest(), fixture.issue, fixture.recordInput())
	if err != nil {
		test.Fatal(err)
	}
	dbfx.Exec(test, `UPDATE agent_task_queue SET status = 'completed' WHERE id IN ($1, $2)`, fixture.taskID, fixture.chatTaskID)
	finalizeAnswerWake(test, fixture)
	// Force UUID order opposite to event order: B's event ID sorts first.
	var retractionID, answerBID pgtype.UUID
	dbfx.QueryRow(test, `SELECT id FROM decision_wake_event WHERE request_id = $1 AND type = 'retracted'`, fixture.decision.ID).Scan(&retractionID)
	dbfx.QueryRow(test, `SELECT id FROM decision_wake_event WHERE request_id = $1 AND answer_id = $2`, fixture.decision.ID, recordedB.Answer.ID).Scan(&answerBID)
	suffix := fmt.Sprintf("%012x", time.Now().UnixNano()&0xffffffffffff)
	retraction, answerB := "ffffffff-ffff-7fff-bfff-"+suffix, "00000000-0000-7000-8000-"+suffix
	dbfx.Exec(test, `UPDATE decision_wake_receipt SET event_id = $2 WHERE event_id = $1`, retractionID, retraction)
	dbfx.Exec(test, `UPDATE decision_wake_event SET id = $2 WHERE id = $1`, retractionID, retraction)
	dbfx.Exec(test, `UPDATE decision_wake_receipt SET event_id = $2 WHERE event_id = $1`, answerBID, answerB)
	dbfx.Exec(test, `UPDATE decision_wake_event SET id = $2 WHERE id = $1`, answerBID, answerB)

	receiptFor := func(eventID, agent string) pgtype.UUID {
		var id pgtype.UUID
		dbfx.QueryRow(test, `SELECT id FROM decision_wake_receipt WHERE event_id = $1 AND recipient_agent_id = $2`, eventID, agent).Scan(&id)
		return id
	}

	if err := testHandler.deliverDecisionWakeReceipts(ctx); err != nil {
		test.Fatal(err)
	}
	for _, agent := range agents {
		receiptPending(test, receiptFor(retraction, agent))
		if retry := receiptRetryOf(test, receiptFor(retraction, agent)); retry.Attempts != 1 || !retry.Later {
			test.Fatalf("retraction retry for %s = %+v, want 1 attempt and a later next_attempt_at", agent, retry)
		}
		receiptPending(test, receiptFor(answerB, agent))
		if retry := receiptRetryOf(test, receiptFor(answerB, agent)); retry.Attempts != 0 {
			test.Fatalf("B for %s = %+v: a blocked receipt is not a failed attempt", agent, retry)
		}
	}

	// Worker restart: the retraction's source exists and its backoff is over.
	dbfx.Exec(test, `UPDATE decision_wake_event SET source_comment_id = $2 WHERE id = $1`, retraction, recordedA.Answer.IssueCommentID)
	dbfx.Exec(test, `UPDATE decision_wake_receipt SET next_attempt_at = clock_timestamp() WHERE event_id = $1`, retraction)
	for pass := 0; pass < 2; pass++ {
		if err := testHandler.deliverDecisionWakeReceipts(ctx); err != nil {
			test.Fatal(err)
		}
	}
	for _, agent := range agents {
		first, second := receiptStateOf(test, receiptFor(retraction, agent)), receiptStateOf(test, receiptFor(answerB, agent))
		if first.Status != "delivered" || second.Status != "delivered" {
			test.Fatalf("%s: retraction = %+v, B = %+v, want both delivered", agent, first, second)
		}
		if second.DeliveredAt.Time.Before(first.DeliveredAt.Time) {
			test.Fatalf("%s got B before the retraction", agent)
		}
	}
}

// Sol (decision_wake_receipt.sql:45): withdrawal clears wake_at to NULL on an
// unwoken answer. The withdrawn answer's pending receipt must stay selectable,
// so delivery settles it as superseded instead of leaving it pending and
// blocking every later receipt of the request through the older-pending barrier.
func TestWithdrawnAnswerReceiptStaysSelectableAndSettles(test *testing.T) {
	fixture, _, leader := squadActionFixture(test)
	dbfx.Exec(test, `UPDATE decision_answer SET disclaimed_at = clock_timestamp(), wake_at = NULL WHERE decision_request_id = $1`, fixture.decision.ID)
	receipt := receiptIDFor(test, fixture.decision.ID, leader)
	ids, err := testHandler.Queries.ListDecisionWakeReceiptCandidates(context.Background(), 100)
	if err != nil {
		test.Fatal(err)
	}
	selected := false
	for _, id := range ids {
		selected = selected || id == receipt
	}
	if !selected {
		test.Fatalf("receipt %v of a withdrawn answer (wake_at NULL) is not selected; it would stay pending and block later receipts", receipt)
	}
	if err := testHandler.deliverDecisionWakeReceipts(context.Background()); err != nil {
		test.Fatal(err)
	}
	if got := receiptStateOf(test, receipt); got.Status != "superseded" {
		test.Fatalf("receipt = %+v, want superseded", got)
	}
}
