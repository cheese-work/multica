package handler

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
)

// CHE-2394 P4c: the issue-closed lifecycle cancel suppresses the pending
// receipts in its own transaction (Sol's cases c and d).

func closeIssueAndCancel(test *testing.T, fixture askedRecordFixture) {
	test.Helper()
	dbfx.Exec(test, `UPDATE issue SET status = 'done' WHERE id = $1`, fixture.issueID)
	if _, err := testHandler.CancelDecisionLifecycle(context.Background(), fixture.issue.WorkspaceID, fixture.decision.ID); err != nil {
		test.Fatal(err)
	}
	if dbfx.Count(test, `SELECT count(*) FROM decision_request WHERE id = $1 AND status = 'cancelled' AND cancel_reason = 'issue_closed'`, fixture.decision.ID) != 1 {
		test.Fatal("lifecycle did not cancel the request as issue_closed")
	}
}

// (c) Receipts written, issue closed, worker restarted: zero dispatches.
// The cancel itself settles the receipts; the worker pass finds none due.
func TestIssueClosedCancelSuppressesPendingReceiptsAndNothingDispatches(test *testing.T) {
	fixture, recorded, leader := squadActionFixture(test)
	tasks := activeTaskCount(test, fixture.issueID)
	closeIssueAndCancel(test, fixture)
	for _, agent := range []string{fixture.agentID, leader} {
		if got := receiptStateOf(test, receiptIDFor(test, fixture.decision.ID, agent)); got.Status != "suppressed_issue_closed" || got.Outcome != "" {
			test.Fatalf("receipt for %s = %+v, want suppressed_issue_closed right after the cancel", agent, got)
		}
	}
	if err := testHandler.deliverDecisionWakeReceipts(context.Background()); err != nil {
		test.Fatal(err)
	}
	if got := activeTaskCount(test, fixture.issueID); got != tasks {
		test.Fatalf("active tasks = %d, want %d: a closed issue was woken", got, tasks)
	}
	var outcome string
	dbfx.QueryRow(test, `SELECT coalesce(wake_outcome, '') FROM decision_answer WHERE id = $1`, recorded.Answer.ID).Scan(&outcome)
	if outcome != "skipped_ineligible" {
		test.Fatalf("answer wake_outcome = %q, want skipped_ineligible", outcome)
	}
}

// (d) Partial delivery, then closure: the remaining receipt is suppressed, the
// delivered one stays delivered, and the answer keeps the delivered outcome.
func TestIssueClosedCancelAfterPartialDeliverySuppressesOnlyTheRemainingReceipt(test *testing.T) {
	fixture, recorded, leader := squadActionFixture(test)
	dbfx.Exec(test, `UPDATE decision_wake_receipt SET next_attempt_at = clock_timestamp() + interval '1 hour' WHERE id = $1`, receiptIDFor(test, fixture.decision.ID, leader))
	if err := testHandler.deliverDecisionWakeReceipts(context.Background()); err != nil {
		test.Fatal(err)
	}
	requester := receiptIDFor(test, fixture.decision.ID, fixture.agentID)
	if got := receiptStateOf(test, requester); got.Status != "delivered" {
		test.Fatalf("requester receipt = %+v, want delivered", got)
	}
	tasks := activeTaskCount(test, fixture.issueID)
	closeIssueAndCancel(test, fixture)
	if got := receiptStateOf(test, requester); got.Status != "delivered" || got.Outcome != "run_enqueued" {
		test.Fatalf("requester receipt = %+v, want it left delivered", got)
	}
	if got := receiptStateOf(test, receiptIDFor(test, fixture.decision.ID, leader)); got.Status != "suppressed_issue_closed" {
		test.Fatalf("leader receipt = %+v, want suppressed_issue_closed", got)
	}
	if err := testHandler.deliverDecisionWakeReceipts(context.Background()); err != nil {
		test.Fatal(err)
	}
	if got := activeTaskCount(test, fixture.issueID); got != tasks {
		test.Fatalf("active tasks = %d, want %d", got, tasks)
	}
	var outcome string
	var woken pgtype.Timestamptz
	dbfx.QueryRow(test, `SELECT woken_at, coalesce(wake_outcome, '') FROM decision_answer WHERE id = $1`, recorded.Answer.ID).Scan(&woken, &outcome)
	if !woken.Valid || outcome != "run_enqueued" {
		test.Fatalf("answer woken = %v outcome = %q, want the delivered outcome run_enqueued", woken.Valid, outcome)
	}
}

// Receipts of other issues' requests are not touched by the sweep.
func TestIssueClosedCancelLeavesOtherRequestsReceiptsPending(test *testing.T) {
	fixture, _, _ := squadActionFixture(test)
	other, _ := answeredActionFixture(test)
	closeIssueAndCancel(test, fixture)
	if got := receiptStateOf(test, receiptIDFor(test, other.decision.ID, other.agentID)); got.Status != "pending" {
		test.Fatalf("other request receipt = %+v, want pending", got)
	}
}
