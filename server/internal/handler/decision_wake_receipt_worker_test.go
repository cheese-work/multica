package handler

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// CHE-2394 P3b: worker stage, commit, retry, aggregate outcome, archive race.

// squadActionFixture is an answered kind=action request on a squad-assigned
// issue: a requester receipt and a leader (assignee) receipt, no active tasks.
// finalizeAnswerWake mirrors the answering task's finalization: wake_at is set.
// A NULL wake_at is not due.
func finalizeAnswerWake(test *testing.T, fixture askedRecordFixture) {
	test.Helper()
	dbfx.Exec(test, `UPDATE decision_answer SET wake_at = GREATEST(effective_at, clock_timestamp()) WHERE decision_request_id = $1 AND wake_at IS NULL`, fixture.decision.ID)
}

func squadActionFixture(test *testing.T) (fixture askedRecordFixture, recorded decisionRecordResult, leader string) {
	test.Helper()
	fixture = newAskedRecordFixture(test)
	leader = dbfx.Agent(test, "wake worker leader", handlerTestRuntimeID(test))
	squadID := dbfx.Squad(test, "Wake worker", leader)
	dbfx.Exec(test, `UPDATE issue SET assignee_type = 'squad', assignee_id = $1 WHERE id = $2`, squadID, fixture.issueID)
	dbfx.Exec(test, `UPDATE decision_request SET kind = 'action', expires_at = clock_timestamp() + interval '1 hour' WHERE id = $1`, fixture.decision.ID)
	recorded, err := testHandler.createAskedDecisionRecord(fixture.recordRequest(), fixture.issue, fixture.recordInput())
	if err != nil {
		test.Fatal(err)
	}
	dbfx.Exec(test, `UPDATE agent_task_queue SET status = 'completed' WHERE id IN ($1, $2)`, fixture.taskID, fixture.chatTaskID)
	finalizeAnswerWake(test, fixture)
	return fixture, recorded, leader
}

type receiptRetry struct {
	Attempts  int
	LastError string
	Later     bool
}

func receiptRetryOf(test *testing.T, id pgtype.UUID) receiptRetry {
	test.Helper()
	var retry receiptRetry
	dbfx.QueryRow(test, `SELECT attempt_count, coalesce(last_error, ''), next_attempt_at > clock_timestamp() FROM decision_wake_receipt WHERE id = $1`, id).Scan(&retry.Attempts, &retry.LastError, &retry.Later)
	return retry
}

func activeTaskCount(test *testing.T, issueID string) int {
	test.Helper()
	return dbfx.Count(test, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND status IN ('queued', 'running')`, issueID)
}

func TestReceiptStageRetriesFailedRecipientWithoutDelayingTheOther(test *testing.T) {
	fixture, _, leader := squadActionFixture(test)
	// The requester has a running task of the wrong kind (a leader task), so its
	// dispatch cannot reach it: the injected failure for recipient 1.
	blocker := dbfx.Task(test, fixture.agentID, testutil.Cols{"issue_id": fixture.issueID, "status": "running", "runtime_id": handlerTestRuntimeID(test), "is_leader_task": true})
	requester, assignee := receiptIDFor(test, fixture.decision.ID, fixture.agentID), receiptIDFor(test, fixture.decision.ID, leader)

	if err := testHandler.deliverDecisionWakeReceipts(context.Background()); err != nil {
		test.Fatal(err)
	}
	if got := receiptStateOf(test, assignee); got.Status != "delivered" || got.Outcome != "run_enqueued" {
		test.Fatalf("leader receipt = %+v, want delivered run_enqueued in the same pass", got)
	}
	if got := receiptStateOf(test, requester); got.Status != "pending" {
		test.Fatalf("requester receipt = %+v, want pending", got)
	}
	if retry := receiptRetryOf(test, requester); retry.Attempts != 1 || retry.LastError == "" || !retry.Later {
		test.Fatalf("requester retry = %+v, want 1 attempt, an error and a later next_attempt_at", retry)
	}

	// Backing off: the next pass leaves it alone.
	if err := testHandler.deliverDecisionWakeReceipts(context.Background()); err != nil {
		test.Fatal(err)
	}
	if retry := receiptRetryOf(test, requester); retry.Attempts != 1 {
		test.Fatalf("attempts = %d during backoff, want 1", retry.Attempts)
	}

	// Once due and no longer blocked, the retry delivers.
	dbfx.Exec(test, `UPDATE agent_task_queue SET status = 'completed' WHERE id = $1`, blocker)
	dbfx.Exec(test, `UPDATE decision_wake_receipt SET next_attempt_at = clock_timestamp() WHERE id = $1`, requester)
	if err := testHandler.deliverDecisionWakeReceipts(context.Background()); err != nil {
		test.Fatal(err)
	}
	if got := receiptStateOf(test, requester); got.Status != "delivered" || got.Outcome != "run_enqueued" {
		test.Fatalf("requester receipt after retry = %+v, want delivered run_enqueued", got)
	}
}

func TestReceiptStageRerunAfterCommitAddsNoSecondInput(test *testing.T) {
	fixture, _, leader := squadActionFixture(test)
	if err := testHandler.deliverDecisionWakeReceipts(context.Background()); err != nil {
		test.Fatal(err)
	}
	for _, agent := range []string{fixture.agentID, leader} {
		if got := receiptStateOf(test, receiptIDFor(test, fixture.decision.ID, agent)); got.Status != "delivered" {
			test.Fatalf("receipt for %s = %+v, want delivered", agent, got)
		}
	}
	tasks, delivered := activeTaskCount(test, fixture.issueID), receiptStateOf(test, receiptIDFor(test, fixture.decision.ID, leader)).DeliveredAt
	// A worker restart runs the stage again over the same rows.
	if err := testHandler.deliverDecisionWakeReceipts(context.Background()); err != nil {
		test.Fatal(err)
	}
	if got := activeTaskCount(test, fixture.issueID); got != tasks {
		test.Fatalf("active tasks = %d after rerun, want %d", got, tasks)
	}
	if got := receiptStateOf(test, receiptIDFor(test, fixture.decision.ID, leader)).DeliveredAt; got != delivered {
		test.Fatalf("delivered_at moved from %v to %v on rerun", delivered, got)
	}
}

func TestAllRecipientsSkippedGivesSkippedOutcomeAndUndeliveredCard(test *testing.T) {
	fixture, recorded, leader := squadActionFixture(test)
	dbfx.Exec(test, `UPDATE agent SET archived_at = clock_timestamp() WHERE id IN ($1, $2)`, fixture.agentID, leader)
	if err := testHandler.deliverDecisionWakeReceipts(context.Background()); err != nil {
		test.Fatal(err)
	}
	for _, agent := range []string{fixture.agentID, leader} {
		if got := receiptStateOf(test, receiptIDFor(test, fixture.decision.ID, agent)); got.Status != "skipped_ineligible" {
			test.Fatalf("receipt for %s = %+v, want skipped_ineligible", agent, got)
		}
	}
	var wokenAt pgtype.Timestamptz
	var outcome, disclaimed = pgtype.Text{}, pgtype.Timestamptz{}
	dbfx.QueryRow(test, `SELECT woken_at, wake_outcome, disclaimed_at FROM decision_answer WHERE id = $1`, recorded.Answer.ID).Scan(&wokenAt, &outcome, &disclaimed)
	if !wokenAt.Valid || outcome.String != "skipped_ineligible" {
		test.Fatalf("answer woken_at = %v, wake_outcome = %q, want set and skipped_ineligible", wokenAt, outcome.String)
	}
	if decisionAnswerDelivered(wokenAt, outcome, disclaimed) {
		test.Fatal("card reads delivered although every recipient was skipped")
	}
}

func TestAnswerWakeOutcomeIsRequesterThenAssigneeOnceAllSettled(test *testing.T) {
	fixture, recorded, leader := squadActionFixture(test)
	dbfx.Exec(test, `UPDATE agent SET archived_at = clock_timestamp() WHERE id = $1`, fixture.agentID)
	answerWoken := func() pgtype.Timestamptz {
		var wokenAt pgtype.Timestamptz
		dbfx.QueryRow(test, `SELECT woken_at FROM decision_answer WHERE id = $1`, recorded.Answer.ID).Scan(&wokenAt)
		return wokenAt
	}
	// One receipt still pending: the answer is not acknowledged yet.
	dbfx.Exec(test, `UPDATE decision_wake_receipt SET next_attempt_at = clock_timestamp() + interval '1 hour' WHERE id = $1`, receiptIDFor(test, fixture.decision.ID, leader))
	if err := testHandler.deliverDecisionWakeReceipts(context.Background()); err != nil {
		test.Fatal(err)
	}
	if answerWoken().Valid {
		test.Fatal("answer acknowledged while a receipt was still pending")
	}
	dbfx.Exec(test, `UPDATE decision_wake_receipt SET next_attempt_at = clock_timestamp() WHERE id = $1`, receiptIDFor(test, fixture.decision.ID, leader))
	if err := testHandler.deliverDecisionWakeReceipts(context.Background()); err != nil {
		test.Fatal(err)
	}
	var outcome string
	dbfx.QueryRow(test, `SELECT coalesce(wake_outcome, '') FROM decision_answer WHERE id = $1`, recorded.Answer.ID).Scan(&outcome)
	if !answerWoken().Valid || outcome != "run_enqueued" {
		test.Fatalf("answer wake_outcome = %q, want the delivered assignee outcome run_enqueued", outcome)
	}
}

// CHE-2394 P3a carry-over (Terra P2): an archive that commits after the
// eligibility reload must not let a dispatch settle as delivered.
func TestReceiptDeliveryDoesNotDeliverToRecipientArchivedMidDelivery(test *testing.T) {
	fixture, _, leader := squadActionFixture(test)
	ctx := context.Background()
	archive, err := testPool.Begin(ctx)
	if err != nil {
		test.Fatal(err)
	}
	defer archive.Rollback(ctx)
	if _, err := archive.Exec(ctx, `UPDATE agent SET archived_at = clock_timestamp() WHERE id = $1`, leader); err != nil {
		test.Fatal(err)
	}
	receipt := receiptIDFor(test, fixture.decision.ID, leader)
	done := make(chan error, 1)
	go func() { done <- testHandler.deliverDecisionWakeReceipt(ctx, receipt) }()
	time.Sleep(700 * time.Millisecond) // the delivery has read the unarchived agent and reached its locks
	if err := archive.Commit(ctx); err != nil {
		test.Fatal(err)
	}
	if err := awaitDecisionDeliveryRaceDone(test, done, "receipt delivery"); err != nil {
		test.Fatal(err)
	}
	if got := receiptStateOf(test, receipt); got.Status != "skipped_ineligible" {
		test.Fatalf("receipt = %+v, want skipped_ineligible for the recipient archived mid-delivery", got)
	}
	if n := dbfx.Count(test, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND agent_id = $2 AND status = 'queued'`, fixture.issueID, leader); n != 0 {
		test.Fatalf("archived leader got %d queued tasks", n)
	}
}
