package handler

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/commentguard"
	"github.com/multica-ai/multica/server/internal/decisiontx"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func TestUnpromptedDecisionRecordRollback(test *testing.T) {
	for _, query := range []string{"CreateDecisionRequest", "CreateDecisionAnswer", "CreateComment", "CreateChatMessage", "MarkDecisionRequestAnswered"} {
		test.Run(query, func(test *testing.T) {
			fixture := newAskedRecordFixture(test)
			input := fixture.recordInput()
			input.ParentID, input.EscalationClass = pgtype.UUID{}, "spend"
			failure := errors.New("injected rollback")
			useRequestCoreStarter(test, &requestCoreStarter{base: testHandler.TxStarter, query: "-- name: " + query, fail: failure})
			result, err := testHandler.createAskedDecisionRecord(fixture.recordRequest(), fixture.issue, input)
			if !errors.Is(err, failure) || result.Answer.ID.Valid || dbfx.Count(test, `SELECT count(*) FROM decision_request WHERE issue_id = $1 AND origin = 'unprompted'`, fixture.issueID) != 0 {
				test.Fatalf("partial unprompted commit: %+v %v", result, err)
			}
			fixture.assertRecordCounts(test, 0)
		})
	}
}

func TestDecisionWithdrawalRollback(test *testing.T) {
	for _, query := range []string{"CreateComment", "WithdrawDecisionAnswer", "ReopenDecisionRequest", "CancelWithdrawnDecisionRequest"} {
		test.Run(query, func(test *testing.T) {
			fixture := newAskedRecordFixture(test)
			record, err := testHandler.createAskedDecisionRecord(fixture.recordRequest(), fixture.issue, fixture.recordInput())
			if err != nil {
				test.Fatal(err)
			}
			if query == "CancelWithdrawnDecisionRequest" {
				dbfx.Exec(test, `UPDATE chat_session SET status = 'archived' WHERE id = $1`, fixture.sessionID)
			}
			failure := errors.New("withdrawal rollback")
			useRequestCoreStarter(test, &requestCoreStarter{base: testHandler.TxStarter, query: "-- name: " + query, fail: failure})
			result, err := testHandler.disclaimDecisionAnswerTx(fixture.recordRequest(), fixture.issue.WorkspaceID, record.Answer.ID, parseUUID(fixture.humanID))
			if !errors.Is(err, failure) || result.Answer.ID.Valid {
				test.Fatalf("unexpected rollback result: %+v %v", result, err)
			}
			if dbfx.Count(test, `SELECT count(*) FROM decision_answer WHERE id = $1 AND disclaimed_at IS NULL AND withdrawal_comment_id IS NULL AND disclaim_comment_posted_at IS NULL`, record.Answer.ID) != 1 || dbfx.Count(test, `SELECT count(*) FROM comment WHERE issue_id = $1 AND decision_comment_kind IN ('disclaim','cancel')`, fixture.issueID) != 0 || dbfx.Count(test, `SELECT count(*) FROM decision_request WHERE id = $1 AND status = 'answered'`, record.Request.ID) != 1 {
				test.Fatal("withdrawal leaked comments, receipts or state")
			}
		})
	}
}

func TestDecisionWithdrawalArchiveOrders(test *testing.T) {
	for _, first := range []string{"archive", "withdraw"} {
		test.Run(first, func(test *testing.T) {
			fixture := newAskedRecordFixture(test)
			record, err := testHandler.createAskedDecisionRecord(fixture.recordRequest(), fixture.issue, fixture.recordInput())
			if err != nil {
				test.Fatal(err)
			}
			original := *testHandler
			query := "-- name: WithdrawDecisionAnswer"
			if first == "archive" {
				query = "-- name: DecisionOpenLaneExists"
			}
			paused, unblock := pauseDecisionTransaction(test, query)
			withdraw := func(handler *Handler) <-chan error {
				finished := make(chan error, 1)
				go func() {
					ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
					defer cancel()
					_, err := handler.disclaimDecisionAnswerTx(fixture.recordRequest().WithContext(ctx), fixture.issue.WorkspaceID, record.Answer.ID, parseUUID(fixture.humanID))
					finished <- err
				}()
				return finished
			}
			var archived <-chan int
			var withdrawn <-chan error
			request := decisionSessionRequestForHuman(test, fixture.requestCoreFixture, "archive")
			if first == "archive" {
				archived = startDecisionSessionAction(test, testHandler, request, "archive")
			} else {
				withdrawn = withdraw(testHandler)
			}
			holderPID := awaitDecisionPause(test, paused)
			if first == "archive" {
				withdrawn = withdraw(&original)
			} else {
				archived = startDecisionSessionAction(test, &original, request, "archive")
			}
			if !waitForWaiterBlockedBy(test, holderPID, 3*time.Second) {
				test.Fatal("archive/withdraw did not contend on the session fence")
			}
			unblock()
			if err := <-withdrawn; err != nil {
				test.Fatal(err)
			}
			wantedArchive, wantedRequest := http.StatusOK, "cancelled"
			if first == "withdraw" {
				wantedArchive, wantedRequest = http.StatusConflict, "open"
			}
			if status := awaitDecisionSessionAction(test, archived); status != wantedArchive {
				test.Fatalf("archive status=%d want=%d", status, wantedArchive)
			}
			if dbfx.Count(test, `SELECT count(*) FROM decision_request WHERE id = $1 AND status = $2`, record.Request.ID, wantedRequest) != 1 {
				test.Fatal("archive/withdraw ordering changed request outcome")
			}
		})
	}
}

func TestDecisionWithdrawalRaiseOrders(test *testing.T) {
	for _, first := range []string{"raise", "withdraw"} {
		test.Run(first, func(test *testing.T) {
			fixture := newAskedRecordFixture(test)
			record, err := testHandler.createAskedDecisionRecord(fixture.recordRequest(), fixture.issue, fixture.recordInput())
			if err != nil {
				test.Fatal(err)
			}
			original := *testHandler
			query := "-- name: WithdrawDecisionAnswer"
			if first == "raise" {
				query = "-- name: LockDecisionAnswer"
			}
			paused, unblock := pauseDecisionTransaction(test, query)
			start := func(handler *Handler, operation string) <-chan error {
				finished := make(chan error, 1)
				go func() {
					ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
					defer cancel()
					if operation == "withdraw" {
						_, err := handler.disclaimDecisionAnswerTx(fixture.recordRequest().WithContext(ctx), fixture.issue.WorkspaceID, record.Answer.ID, parseUUID(fixture.humanID))
						finished <- err
						return
					}
					transaction, err := handler.TxStarter.Begin(ctx)
					if err != nil {
						finished <- err
						return
					}
					defer transaction.Rollback(ctx)
					queries := handler.Queries.WithTx(transaction)
					if _, err = queries.LockWorkspaceForChatSessionCreate(ctx, fixture.issue.WorkspaceID); err == nil {
						_, err = queries.LockDecisionRequest(ctx, db.LockDecisionRequestParams{WorkspaceID: fixture.issue.WorkspaceID, ID: record.Request.ID})
					}
					if err == nil {
						_, err = queries.LockDecisionAnswer(ctx, db.LockDecisionAnswerParams{WorkspaceID: fixture.issue.WorkspaceID, ID: record.Answer.ID})
					}
					if err == nil {
						_, err = transaction.Exec(ctx, `UPDATE decision_request SET escalation_class = 'spend' WHERE id = $1 AND status <> 'cancelled'`, record.Request.ID)
					}
					if err == nil {
						_, err = transaction.Exec(ctx, `UPDATE decision_answer SET effective_at = clock_timestamp() + interval '10 minutes' WHERE id = $1 AND disclaimed_at IS NULL`, record.Answer.ID)
					}
					if err == nil {
						err = transaction.Commit(ctx)
					}
					finished <- err
				}()
				return finished
			}
			firstDone := start(testHandler, first)
			holderPID := awaitDecisionPause(test, paused)
			second := "raise"
			if first == "raise" {
				second = "withdraw"
			}
			secondDone := start(&original, second)
			if !waitForWaiterBlockedBy(test, holderPID, 3*time.Second) {
				test.Fatal("raise/withdraw did not contend on the request fence")
			}
			unblock()
			if err := <-firstDone; err != nil {
				test.Fatal(err)
			}
			if err := <-secondDone; err != nil {
				test.Fatal(err)
			}
			if dbfx.Count(test, `SELECT count(*) FROM decision_answer WHERE id = $1 AND disclaimed_at IS NOT NULL AND wake_at IS NULL`, record.Answer.ID) != 1 {
				test.Fatal("raise resurrected a withdrawn answer or made it effective")
			}
		})
	}
}

func TestDecisionWithdrawalReusableForWorker(test *testing.T) {
	fixture := newAskedRecordFixture(test)
	record, err := testHandler.createAskedDecisionRecord(fixture.recordRequest(), fixture.issue, fixture.recordInput())
	if err != nil {
		test.Fatal(err)
	}
	transaction, err := testPool.Begin(context.Background())
	if err != nil {
		test.Fatal(err)
	}
	defer transaction.Rollback(context.Background())
	queries := testHandler.Queries.WithTx(transaction)
	ctx := context.Background()
	if _, err := queries.LockWorkspaceForChatSessionCreate(ctx, fixture.issue.WorkspaceID); err != nil {
		test.Fatal(err)
	}
	if _, err := queries.LockDecisionSession(ctx, db.LockDecisionSessionParams{WorkspaceID: fixture.issue.WorkspaceID, ID: record.Request.ChatSessionID}); err != nil {
		test.Fatal(err)
	}
	if _, err := commentguard.LockThreadForReplyAndLoadRoot(ctx, queries, fixture.issue.WorkspaceID, record.Request.IssueCommentID); err != nil {
		test.Fatal(err)
	}
	if _, err := queries.LockDecisionIssue(ctx, db.LockDecisionIssueParams{WorkspaceID: fixture.issue.WorkspaceID, ID: fixture.issue.ID}); err != nil {
		test.Fatal(err)
	}
	if _, err := queries.LockDecisionRequest(ctx, db.LockDecisionRequestParams{WorkspaceID: fixture.issue.WorkspaceID, ID: record.Request.ID}); err != nil {
		test.Fatal(err)
	}
	if _, err := queries.LockDecisionAnswer(ctx, db.LockDecisionAnswerParams{WorkspaceID: fixture.issue.WorkspaceID, ID: record.Answer.ID}); err != nil {
		test.Fatal(err)
	}
	result, err := decisiontx.Withdraw(ctx, queries, record.Request, record.Answer, "active", "source_removed", pgtype.UUID{})
	if err != nil || result.Answer.DisclaimReason.String != "source_removed" || result.Request.Status != "open" || result.Answer.ContentSnapshot != record.Answer.ContentSnapshot || result.Answer.SourceChatSessionID != record.Answer.SourceChatSessionID || len(result.Comments) != 1 {
		test.Fatalf("worker withdrawal contract: %+v %v", result, err)
	}
	repeated, err := decisiontx.Withdraw(ctx, queries, result.Request, result.Answer, "active", "source_removed", pgtype.UUID{})
	if err != nil || len(repeated.Comments) != 0 || repeated.Answer.WithdrawalCommentID != result.Answer.WithdrawalCommentID {
		test.Fatalf("worker retry is not idempotent: %+v %v", repeated, err)
	}
	if err := transaction.Commit(context.Background()); err != nil {
		test.Fatal(err)
	}
}

func TestDecisionWithdrawalCancelledReasonsAndWakeHistory(test *testing.T) {
	for _, reason := range []string{"issue_closed", "human_removed", "agent_archived", "lane_archived"} {
		for _, wake := range []string{"pending", "delivered", "skipped"} {
			test.Run(reason+"/"+wake, func(test *testing.T) {
				fixture := newAskedRecordFixture(test)
				record, err := testHandler.createAskedDecisionRecord(fixture.recordRequest(), fixture.issue, fixture.recordInput())
				if err != nil {
					test.Fatal(err)
				}
				dbfx.Exec(test, `UPDATE decision_request SET status = 'cancelled', cancel_reason = $1, cancel_notice_posted_at = clock_timestamp() WHERE id = $2`, reason, record.Request.ID)
				dbfx.Exec(test, `UPDATE chat_session SET status = 'archived' WHERE id = $1`, fixture.sessionID)
				if wake != "pending" {
					outcome := "run_enqueued"
					if wake == "skipped" {
						outcome = "skipped_ineligible"
					}
					dbfx.Exec(test, `UPDATE decision_answer SET wake_at = clock_timestamp(), woken_at = clock_timestamp(), wake_outcome = $1 WHERE id = $2`, outcome, record.Answer.ID)
				}
				result, err := testHandler.disclaimDecisionAnswerTx(fixture.recordRequest(), fixture.issue.WorkspaceID, record.Answer.ID, parseUUID(fixture.humanID))
				if err != nil {
					test.Fatal(err)
				}
				if result.Request.Status != "cancelled" || result.Request.CancelReason.String != reason || result.Request.ChatSessionID != record.Request.ChatSessionID || !result.Answer.DisclaimedAt.Valid || result.Answer.ContentSnapshot != record.Answer.ContentSnapshot || result.Answer.SourceChatSessionID != record.Answer.SourceChatSessionID || result.Answer.ChatMessageIds[0] != record.Answer.ChatMessageIds[0] || result.Answer.WokenAt.Valid != (wake != "pending") || result.Answer.WakeAt.Valid != (wake != "pending") || result.Answer.RetractionDeliveredAt.Valid || len(result.Comments) != 1 {
					test.Fatalf("cancel/wake history changed: %+v", result)
				}
			})
		}
	}
}
