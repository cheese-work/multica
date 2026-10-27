package handler

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/decisiontx"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

type decisionLifecycleCanceller interface {
	CancelDecisionLifecycle(context.Context, pgtype.UUID, pgtype.UUID) (decisiontx.Withdrawal, error)
}

func cancelDecisionForTest(handler *Handler, fixture askedRecordFixture) error {
	if canceller, ok := any(handler).(decisionLifecycleCanceller); ok {
		_, err := canceller.CancelDecisionLifecycle(context.Background(), fixture.issue.WorkspaceID, fixture.decision.ID)
		return err
	}
	return nil
}

func TestDecisionLifecycleCancellation(test *testing.T) {
	for _, condition := range []string{"done", "cancelled", "agent_accepted", "approved", "human_removed", "agent_archived", "archived_lane", "open_request"} {
		test.Run(condition, func(test *testing.T) {
			resetDecisionLease(test)
			fixture, recorded := readyDecisionDelivery(test)
			reason := "issue_closed"
			switch condition {
			case "human_removed":
				dbfx.Exec(test, `DELETE FROM member WHERE workspace_id = $1 AND user_id = $2`, testWorkspaceID, fixture.humanID)
				reason = condition
			case "agent_archived":
				dbfx.Exec(test, `UPDATE agent SET archived_at = clock_timestamp() WHERE id = $1`, fixture.agentID)
				reason = condition
			case "archived_lane", "open_request":
				dbfx.Exec(test, `UPDATE issue SET status = 'done' WHERE id = $1`, fixture.issueID)
				if condition == "archived_lane" {
					dbfx.Exec(test, `UPDATE chat_session SET status = 'archived' WHERE id = $1`, fixture.sessionID)
				} else {
					dbfx.Exec(test, `DELETE FROM decision_answer WHERE id = $1`, recorded.Answer.ID)
					dbfx.Exec(test, `UPDATE decision_request SET status = 'open' WHERE id = $1`, fixture.decision.ID)
				}
			default:
				dbfx.Exec(test, `UPDATE issue SET status = $2 WHERE id = $1`, fixture.issueID, condition)
			}
			messages := dbfx.Count(test, `SELECT count(*) FROM chat_message WHERE chat_session_id = $1`, fixture.sessionID)
			_, runner := newDecisionRunnerFixture(test)
			decisionPass(test, runner)
			decisionPass(test, runner)
			stored, err := testHandler.Queries.GetDecisionRequest(context.Background(), db.GetDecisionRequestParams{WorkspaceID: fixture.issue.WorkspaceID, ID: fixture.decision.ID})
			if err != nil || stored.Status != "cancelled" || stored.CancelReason.String != reason || !stored.CancelNoticePostedAt.Valid {
				test.Fatalf("lifecycle did not durably cancel: request=%+v error=%v", stored, err)
			}
			if count := dbfx.Count(test, `SELECT count(*) FROM comment WHERE decision_request_id = $1 AND decision_comment_kind = 'cancel'`, fixture.decision.ID); count != 1 {
				test.Fatalf("cancel comments=%d, want 1", count)
			}
			wantNotes := 1
			if reason == "issue_closed" && condition != "open_request" {
				wantNotes = 0
			}
			if count := dbfx.Count(test, `SELECT count(*) FROM chat_message WHERE chat_session_id = $1`, fixture.sessionID) - messages; count != wantNotes {
				test.Fatalf("cancellation DM notes=%d, want %d", count, wantNotes)
			}
			if condition != "open_request" && dbfx.Count(test, `SELECT count(*) FROM decision_answer WHERE id = $1 AND woken_at IS NOT NULL AND wake_outcome = 'skipped_ineligible'`, recorded.Answer.ID) != 1 {
				test.Fatal("pending answer was not acknowledged as skipped_ineligible")
			}
			if condition == "human_removed" && dbfx.Count(test, `SELECT count(*) FROM decision_request WHERE id = $1 AND reask_delivered_at IS NOT NULL AND reask_outcome = 'run_enqueued'`, fixture.decision.ID) != 1 {
				test.Fatal("removed human did not cause an assignee re-ask")
			}
		})
	}
}

func TestDecisionLifecycleRestoredEligibility(test *testing.T) {
	for _, condition := range []string{"status", "member", "agent", "changes_requested", "blockings_found"} {
		test.Run(condition, func(test *testing.T) {
			resetDecisionLease(test)
			fixture, recorded := readyDecisionDelivery(test)
			handler, runner := newDecisionRunnerFixture(test)
			switch condition {
			case "status":
				dbfx.Exec(test, `UPDATE issue SET status = 'done' WHERE id = $1`, fixture.issueID)
				handler.TxStarter = &decisionDeliveryMutation{txStarter: testPool, mutate: func() { dbfx.Exec(test, `UPDATE issue SET status = 'in_progress' WHERE id = $1`, fixture.issueID) }}
			case "member":
				dbfx.Exec(test, `DELETE FROM member WHERE workspace_id = $1 AND user_id = $2`, testWorkspaceID, fixture.humanID)
				handler.TxStarter = &decisionDeliveryMutation{txStarter: testPool, mutate: func() {
					dbfx.Insert(test, "member", testutil.Cols{"workspace_id": testWorkspaceID, "user_id": fixture.humanID, "role": "member"})
				}}
			case "agent":
				dbfx.Exec(test, `UPDATE agent SET archived_at = clock_timestamp() WHERE id = $1`, fixture.agentID)
				handler.TxStarter = &decisionDeliveryMutation{txStarter: testPool, mutate: func() { dbfx.Exec(test, `UPDATE agent SET archived_at = NULL WHERE id = $1`, fixture.agentID) }}
			default:
				dbfx.Exec(test, `UPDATE issue SET status = $2 WHERE id = $1`, fixture.issueID, condition)
			}
			decisionPass(test, runner)
			if count := dbfx.Count(test, `SELECT count(*) FROM decision_request WHERE id = $1 AND status = 'answered' AND cancel_notice_posted_at IS NULL`, fixture.decision.ID); count != 1 {
				test.Fatal("restored eligibility or re-entrant status cancelled the request")
			}
			if count := dbfx.Count(test, `SELECT count(*) FROM decision_answer WHERE id = $1 AND wake_outcome = 'run_enqueued'`, recorded.Answer.ID); count != 1 {
				test.Fatal("eligible answer was not delivered")
			}
		})
	}
}

func TestDecisionLifecycleRollbackAndReopenGuard(test *testing.T) {
	fixture, recorded := readyDecisionDelivery(test)
	dbfx.Exec(test, `UPDATE issue SET status = 'done' WHERE id = $1`, fixture.issueID)
	handler, _ := newDecisionRunnerFixture(test)
	handler.TxStarter = decisionFailCommit{testPool}
	var published atomic.Int32
	handler.Bus.SubscribeAll(func(events.Event) { published.Add(1) })
	if err := cancelDecisionForTest(handler, fixture); err == nil {
		test.Fatal("expected cancellation commit failure")
	}
	if published.Load() != 0 || dbfx.Count(test, `SELECT count(*) FROM comment WHERE decision_request_id = $1 AND decision_comment_kind = 'cancel'`, fixture.decision.ID) != 0 {
		test.Fatal("rolled-back cancellation escaped its transaction")
	}
	handler, _ = newDecisionRunnerFixture(test)
	if err := cancelDecisionForTest(handler, fixture); err != nil {
		test.Fatal(err)
	}
	dbfx.Exec(test, `UPDATE issue SET status = 'in_progress' WHERE id = $1`, fixture.issueID)
	request := fixture.request()
	if _, err := handler.disclaimDecisionAnswerTx(request, fixture.issue.WorkspaceID, recorded.Answer.ID, parseUUID(fixture.humanID)); err != nil {
		test.Fatal(err)
	}
	if count := dbfx.Count(test, `SELECT count(*) FROM decision_request WHERE id = $1 AND status = 'cancelled' AND cancel_reason = 'issue_closed'`, fixture.decision.ID); count != 1 {
		test.Fatal("disclaim reopened a lifecycle-cancelled request")
	}
}

func TestDecisionLifecycleDeliveryRace(test *testing.T) {
	for _, order := range []string{"cancel_first", "delivery_first"} {
		test.Run(order, func(test *testing.T) {
			fixture, recorded := readyDecisionDelivery(test)
			handler, _ := newDecisionRunnerFixture(test)
			if order == "delivery_first" {
				if err := deliverDecisionForTest(handler, fixture); err != nil {
					test.Fatal(err)
				}
			}
			dbfx.Exec(test, `UPDATE issue SET status = 'done' WHERE id = $1`, fixture.issueID)
			if err := cancelDecisionForTest(handler, fixture); err != nil {
				test.Fatal(err)
			}
			if err := deliverDecisionForTest(handler, fixture); err != nil {
				test.Fatal(err)
			}
			outcome := "skipped_ineligible"
			if order == "delivery_first" {
				outcome = "run_enqueued"
			}
			if count := dbfx.Count(test, `SELECT count(*) FROM decision_request WHERE id = $1 AND status = 'cancelled'`, fixture.decision.ID); count != 1 {
				test.Fatal("lifecycle race left a non-cancelled request")
			}
			if count := dbfx.Count(test, `SELECT count(*) FROM decision_answer WHERE id = $1 AND wake_outcome = $2`, recorded.Answer.ID, outcome); count != 1 {
				test.Fatalf("wrong durable race outcome, want %s", outcome)
			}
		})
	}
}

func TestDecisionLifecycleArchiveLockDefers(test *testing.T) {
	fixture, _ := readyDecisionDelivery(test)
	dbfx.Exec(test, `UPDATE agent SET archived_at = clock_timestamp() WHERE id = $1`, fixture.agentID)
	transaction, err := testPool.Begin(context.Background())
	if err != nil {
		test.Fatal(err)
	}
	defer transaction.Rollback(context.Background())
	if _, err := transaction.Exec(context.Background(), `UPDATE agent SET archived_at = NULL WHERE id = $1`, fixture.agentID); err != nil {
		test.Fatal(err)
	}
	handler, _ := newDecisionRunnerFixture(test)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	canceller, ok := any(handler).(decisionLifecycleCanceller)
	if !ok {
		test.Fatal("lifecycle cancellation is missing")
	}
	if _, err := canceller.CancelDecisionLifecycle(ctx, fixture.issue.WorkspaceID, fixture.decision.ID); !decisiontx.IsRetryable(err) {
		test.Fatalf("busy archive state should defer without cancellation, got %v", err)
	}
	if err := transaction.Commit(context.Background()); err != nil {
		test.Fatal(err)
	}
	if err := cancelDecisionForTest(handler, fixture); err != nil {
		test.Fatal(err)
	}
	if count := dbfx.Count(test, `SELECT count(*) FROM decision_request WHERE id = $1 AND status = 'answered'`, fixture.decision.ID); count != 1 {
		test.Fatal("unarchive before the stable recheck did not preserve the request")
	}
}

func TestDecisionLifecycleIssueFenceThroughCommit(test *testing.T) {
	for _, first := range []string{"delivery", "cancellation"} {
		test.Run(first, func(test *testing.T) {
			fixture, recorded := readyDecisionDelivery(test)
			if first == "cancellation" {
				dbfx.Exec(test, `UPDATE issue SET status = 'done' WHERE id = $1`, fixture.issueID)
			}
			handler, _ := newDecisionRunnerFixture(test)
			release := make(chan struct{})
			defer close(release)
			barrier := &decisionDeliveryBarrier{txStarter: testPool, ready: make(chan int, 1), release: release}
			handler.TxStarter = barrier
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			completed := make(chan error, 1)
			go func() {
				if first == "delivery" {
					completed <- handler.DeliverDecisionWake(ctx, fixture.issue.WorkspaceID, fixture.decision.ID)
				} else {
					_, err := handler.CancelDecisionLifecycle(ctx, fixture.issue.WorkspaceID, fixture.decision.ID)
					completed <- err
				}
			}()
			var backendPID int
			select {
			case backendPID = <-barrier.ready:
			case err := <-completed:
				test.Fatalf("transaction ended before the commit barrier: %v", err)
			case <-ctx.Done():
				test.Fatal(ctx.Err())
			}
			updated := make(chan error, 1)
			go func() {
				status := "done"
				if first == "cancellation" {
					status = "in_progress"
				}
				_, err := testPool.Exec(ctx, `UPDATE issue SET status = $2 WHERE id = $1`, fixture.issueID, status)
				updated <- err
			}()
			if !waitForWaiterBlockedBy(test, backendPID, 3*time.Second) {
				test.Fatal("issue status writer bypassed the transaction's issue fence")
			}
			release <- struct{}{}
			if err := <-completed; err != nil {
				test.Fatal(err)
			}
			if err := <-updated; err != nil {
				test.Fatal(err)
			}
			handler.TxStarter = testPool
			if first == "delivery" {
				if err := cancelDecisionForTest(handler, fixture); err != nil {
					test.Fatal(err)
				}
			}
			outcome := "run_enqueued"
			if first == "cancellation" {
				outcome = "skipped_ineligible"
			}
			if dbfx.Count(test, `SELECT count(*) FROM decision_request WHERE id = $1 AND status = 'cancelled' AND cancel_reason = 'issue_closed'`, fixture.decision.ID) != 1 || dbfx.Count(test, `SELECT count(*) FROM decision_answer WHERE id = $1 AND wake_outcome = $2`, recorded.Answer.ID, outcome) != 1 {
				test.Fatalf("concurrent issue writer changed the committed %s outcome", first)
			}
		})
	}
}
