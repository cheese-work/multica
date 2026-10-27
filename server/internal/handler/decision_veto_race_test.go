package handler

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func TestDecisionVetoStateGuards(test *testing.T) {
	for _, state := range []string{"pending", "scheduled", "skipped", "archived", "cancelled", "unprompted"} {
		for _, issueTask := range []bool{false, true} {
			test.Run(state+map[bool]string{false: "/dm", true: "/issue"}[issueTask], func(test *testing.T) {
				fixture := newAskedRecordFixture(test)
				record, err := testHandler.createAskedDecisionRecord(fixture.recordRequest(), fixture.issue, fixture.recordInput())
				if err != nil {
					test.Fatal(err)
				}
				switch state {
				case "scheduled":
					dbfx.Exec(test, `UPDATE decision_answer SET wake_at = now() WHERE id = $1`, record.Answer.ID)
				case "skipped":
					dbfx.Exec(test, `UPDATE decision_answer SET wake_at = now(), woken_at = now(), wake_outcome = 'skipped_ineligible' WHERE id = $1`, record.Answer.ID)
				case "archived":
					dbfx.Exec(test, `UPDATE chat_session SET status = 'archived' WHERE id = $1`, fixture.sessionID)
				case "cancelled":
					dbfx.Exec(test, `UPDATE decision_request SET status = 'cancelled', cancel_reason = 'human_removed' WHERE id = $1`, record.Request.ID)
				case "unprompted":
					dbfx.Exec(test, `UPDATE decision_request SET origin = 'unprompted' WHERE id = $1`, record.Request.ID)
				}
				response := vetoDecisionHTTP(testHandler, fixture, uuidToString(record.Comment.ID), "Wrong source", issueTask)
				if response.Code != http.StatusCreated {
					test.Fatalf("veto status=%d body=%s", response.Code, response.Body.String())
				}
				answer, err := testHandler.Queries.GetDecisionAnswer(context.Background(), db.GetDecisionAnswerParams{WorkspaceID: fixture.issue.WorkspaceID, ID: record.Answer.ID})
				if err != nil {
					test.Fatal(err)
				}
				if !answer.DisclaimedAt.Valid || answer.WakeAt.Valid != (state == "skipped") || answer.RetractionDeliveredAt.Valid || answer.RetractionOutcome.Valid {
					test.Fatalf("veto resurrected a wake or suppressed an undelivered wake: %+v", answer)
				}
				status, reason, cancelComments := "open", "", 0
				if state == "archived" {
					status, reason, cancelComments = "cancelled", "lane_archived", 1
				} else if state == "cancelled" {
					status, reason = "cancelled", "human_removed"
				} else if state == "unprompted" {
					status, reason = "cancelled", "record_vetoed"
				}
				if dbfx.Count(test, `SELECT count(*) FROM decision_request WHERE id = $1 AND status = $2 AND COALESCE(cancel_reason, '') = $3`, record.Request.ID, status, reason) != 1 || dbfx.Count(test, `SELECT count(*) FROM comment WHERE decision_request_id = $1 AND decision_comment_kind = 'cancel'`, record.Request.ID) != cancelComments {
					test.Fatal("veto violated cancelled/archived/unprompted guards")
				}
				if state == "unprompted" && dbfx.Count(test, `SELECT count(*) FROM decision_request WHERE id = $1 AND cancel_notice_posted_at IS NOT NULL`, record.Request.ID) != 1 {
					test.Fatal("unprompted veto did not acknowledge its cancellation notice")
				}
				if response := vetoDecisionHTTP(testHandler, fixture, uuidToString(record.Comment.ID), "Again", issueTask); response.Code != http.StatusConflict {
					test.Fatalf("repeat veto status=%d", response.Code)
				}
			})
		}
	}
}

func TestDecisionVetoRollback(test *testing.T) {
	for _, query := range []string{"CreateComment", "WithdrawDecisionAnswer", "ReopenDecisionRequest", "CancelWithdrawnDecisionRequest", "ClearThreadResolutionForReply"} {
		test.Run(query, func(test *testing.T) {
			fixture := newAskedRecordFixture(test)
			record, err := testHandler.createAskedDecisionRecord(fixture.recordRequest(), fixture.issue, fixture.recordInput())
			if err != nil {
				test.Fatal(err)
			}
			if query == "CancelWithdrawnDecisionRequest" {
				dbfx.Exec(test, `UPDATE chat_session SET status = 'archived' WHERE id = $1`, fixture.sessionID)
			}
			failure := errors.New("veto rollback")
			useRequestCoreStarter(test, &requestCoreStarter{base: testHandler.TxStarter, query: "-- name: " + query, fail: failure})
			result, err := testHandler.vetoDecisionAnswerTx(fixture.request(), fixture.issue, record.Comment.ID, "Wrong source")
			if !errors.Is(err, failure) || result.Answer.ID.Valid || dbfx.Count(test, `SELECT count(*) FROM decision_answer WHERE id = $1 AND disclaimed_at IS NULL AND withdrawal_comment_id IS NULL`, record.Answer.ID) != 1 || dbfx.Count(test, `SELECT count(*) FROM comment WHERE decision_answer_id = $1 AND decision_comment_kind = 'veto'`, record.Answer.ID) != 0 {
				test.Fatalf("veto did not roll back atomically: %+v %v", result, err)
			}
		})
	}
}

func TestDecisionVetoNOWAITBudget(test *testing.T) {
	for _, issueTask := range []bool{false, true} {
		test.Run(map[bool]string{false: "dm", true: "issue"}[issueTask], func(test *testing.T) {
			fixture := newAskedRecordFixture(test)
			record, err := testHandler.createAskedDecisionRecord(fixture.recordRequest(), fixture.issue, fixture.recordInput())
			if err != nil {
				test.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			holder, err := testPool.Begin(ctx)
			if err != nil {
				test.Fatal(err)
			}
			defer holder.Rollback(ctx)
			taskID := fixture.chatTaskID
			if issueTask {
				taskID = fixture.taskID
			}
			if _, err := holder.Exec(ctx, `SELECT id FROM agent_task_queue WHERE id = $1 FOR UPDATE`, taskID); err != nil {
				test.Fatal(err)
			}
			starter := &requestCoreStarter{base: testHandler.TxStarter, query: "-- name: unused"}
			useRequestCoreStarter(test, starter)
			started := time.Now()
			response := vetoDecisionHTTP(testHandler, fixture, uuidToString(record.Comment.ID), "Wrong source", issueTask)
			if response.Code != http.StatusServiceUnavailable || response.Header().Get("Retry-After") == "" || starter.attempts != 3 || time.Since(started) > 2*time.Second {
				test.Fatalf("unbounded NOWAIT refusal: status=%d attempts=%d elapsed=%s", response.Code, starter.attempts, time.Since(started))
			}
			if dbfx.Count(test, `SELECT count(*) FROM decision_answer WHERE id = $1 AND disclaimed_at IS NULL`, record.Answer.ID) != 1 || dbfx.Count(test, `SELECT count(*) FROM comment WHERE decision_answer_id = $1 AND decision_comment_kind = 'veto'`, record.Answer.ID) != 0 {
				test.Fatal("NOWAIT refusal leaked a veto")
			}
		})
	}
}

func TestDecisionVetoDeadlockBudget(test *testing.T) {
	fixture := newAskedRecordFixture(test)
	record, err := testHandler.createAskedDecisionRecord(fixture.recordRequest(), fixture.issue, fixture.recordInput())
	if err != nil {
		test.Fatal(err)
	}
	starter := &requestCoreStarter{base: testHandler.TxStarter, query: "-- name: LockDecisionCallingTask", fail: &pgconn.PgError{Code: "40P01"}}
	useRequestCoreStarter(test, starter)
	response := vetoDecisionHTTP(testHandler, fixture, uuidToString(record.Comment.ID), "Wrong source", true)
	if response.Code != http.StatusServiceUnavailable || starter.attempts != 2 {
		test.Fatalf("deadlock retry status=%d attempts=%d", response.Code, starter.attempts)
	}
}

func startDecisionMutation(test *testing.T, handler *Handler, fixture askedRecordFixture, record decisionRecordResult, operation string) <-chan error {
	test.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	test.Cleanup(cancel)
	finished := make(chan error, 1)
	go func() {
		var err error
		switch operation {
		case "veto":
			_, err = handler.vetoDecisionAnswerTx(fixture.request().WithContext(ctx), fixture.issue, record.Comment.ID, "Wrong source")
		case "record":
			_, err = handler.createAskedDecisionRecord(fixture.recordRequest().WithContext(ctx), fixture.issue, fixture.recordInput())
		case "disclaim":
			_, err = handler.disclaimDecisionAnswerTx(fixture.recordRequest().WithContext(ctx), fixture.issue.WorkspaceID, record.Answer.ID, parseUUID(fixture.humanID))
		case "raise":
			_, err = handler.raiseDecisionClassTx(fixture.recordRequest().WithContext(ctx), fixture.issue.WorkspaceID, fixture.decision.ID, parseUUID(fixture.humanID), "spend")
		}
		finished <- err
	}()
	return finished
}

func awaitDecisionMutation(test *testing.T, finished <-chan error) error {
	test.Helper()
	select {
	case err := <-finished:
		return err
	case <-time.After(10 * time.Second):
		test.Fatal("decision mutation did not finish")
		return context.DeadlineExceeded
	}
}

func TestDecisionMutationOrders(test *testing.T) {
	for _, pair := range [][2]string{{"veto", "record"}, {"veto", "disclaim"}, {"veto", "veto"}, {"raise", "disclaim"}, {"raise", "record"}} {
		for _, reverse := range []bool{false, true} {
			first, second := pair[0], pair[1]
			if reverse {
				first, second = second, first
			}
			test.Run(first+"_before_"+second, func(test *testing.T) {
				fixture := newAskedRecordFixture(test)
				record := decisionRecordResult{}
				if pair != [2]string{"raise", "record"} {
					var err error
					record, err = testHandler.createAskedDecisionRecord(fixture.recordRequest(), fixture.issue, fixture.recordInput())
					if err != nil {
						test.Fatal(err)
					}
				}
				original := *testHandler
				query := "-- name: WithdrawDecisionAnswer"
				if first == "record" {
					query = "-- name: LockDecisionRequest"
					if second == "raise" {
						query = "-- name: CreateDecisionAnswer"
					}
				} else if first == "raise" {
					query = "-- name: RaiseDecisionClass"
				}
				paused, unblock := pauseDecisionTransaction(test, query)
				firstDone := startDecisionMutation(test, testHandler, fixture, record, first)
				holderPID := awaitDecisionPause(test, paused)
				secondDone := startDecisionMutation(test, &original, fixture, record, second)
				if !waitForWaiterBlockedBy(test, holderPID, 3*time.Second) {
					test.Fatal("decision mutation bypassed its serialization fence")
				}
				unblock()
				firstError, secondError := awaitDecisionMutation(test, firstDone), awaitDecisionMutation(test, secondDone)
				if firstError != nil {
					test.Fatal(firstError)
				}
				if pair == [2]string{"veto", "disclaim"} || pair == [2]string{"veto", "veto"} {
					requireDecisionConflict(test, secondError)
				} else if secondError != nil {
					test.Fatal(secondError)
				}
				if pair[0] == "raise" {
					if dbfx.Count(test, `SELECT count(*) FROM decision_class_change WHERE decision_request_id = $1`, fixture.decision.ID) != 1 || dbfx.Count(test, `SELECT count(*) FROM decision_answer WHERE decision_request_id = $1 AND wake_at IS NOT NULL`, fixture.decision.ID) != 0 {
						test.Fatal("raise/record/disclaim lost its audit or made a pending answer effective")
					}
				}
				if pair == [2]string{"veto", "record"} {
					wanted := 1
					if first == "veto" {
						wanted = 2
					}
					fixture.assertRecordCounts(test, wanted)
				}
			})
		}
	}
}

func requireDecisionConflict(test *testing.T, err error) {
	test.Helper()
	var refusal decisionIntentRefusal
	if !errors.As(err, &refusal) || refusal.status != http.StatusConflict {
		test.Fatalf("expected 409 conflict, got %v", err)
	}
}

func TestDecisionClassRollback(test *testing.T) {
	for _, query := range []string{"RaiseDecisionClass", "CreateDecisionClassChange", "HoldDecisionAnswerForClass"} {
		test.Run(query, func(test *testing.T) {
			fixture := newAskedRecordFixture(test)
			record, err := testHandler.createAskedDecisionRecord(fixture.recordRequest(), fixture.issue, fixture.recordInput())
			if err != nil {
				test.Fatal(err)
			}
			failure := errors.New("class rollback")
			useRequestCoreStarter(test, &requestCoreStarter{base: testHandler.TxStarter, query: "-- name: " + query, fail: failure})
			result, err := testHandler.raiseDecisionClassTx(fixture.recordRequest(), fixture.issue.WorkspaceID, record.Request.ID, parseUUID(fixture.humanID), "spend")
			if !errors.Is(err, failure) || result.ID.Valid || dbfx.Count(test, `SELECT count(*) FROM decision_request WHERE id = $1 AND escalation_class = 'reversible'`, record.Request.ID) != 1 || dbfx.Count(test, `SELECT count(*) FROM decision_class_change WHERE decision_request_id = $1`, record.Request.ID) != 0 {
				test.Fatalf("class change leaked through rollback: %+v %v", result, err)
			}
		})
	}
}

func TestDecisionVetoTaskTerminalOrder(test *testing.T) {
	fixture := newAskedRecordFixture(test)
	record, err := testHandler.createAskedDecisionRecord(fixture.recordRequest(), fixture.issue, fixture.recordInput())
	if err != nil {
		test.Fatal(err)
	}
	paused, unblock := pauseDecisionTransaction(test, "-- name: WithdrawDecisionAnswer")
	finished := startDecisionMutation(test, testHandler, fixture, record, "veto")
	holderPID := awaitDecisionPause(test, paused)
	terminal := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		_, err := testPool.Exec(ctx, `UPDATE agent_task_queue SET status = 'cancelled' WHERE id = $1`, fixture.taskID)
		terminal <- err
	}()
	if !waitForWaiterBlockedBy(test, holderPID, 3*time.Second) {
		test.Fatal("task finalization bypassed the veto's NOWAIT share lock")
	}
	unblock()
	if err := awaitDecisionMutation(test, finished); err != nil {
		test.Fatal(err)
	}
	if err := awaitDecisionMutation(test, terminal); err != nil {
		test.Fatal(err)
	}
	if dbfx.Count(test, `SELECT count(*) FROM decision_answer WHERE id = $1 AND disclaimed_at IS NOT NULL AND disclaimed_by_task_id = $2`, record.Answer.ID, fixture.taskID) != 1 {
		test.Fatal("terminal path changed the committed veto")
	}
}

func TestDecisionVetoLifecycleCancelOrders(test *testing.T) {
	for _, first := range []string{"veto", "cancel"} {
		test.Run(first+"_first", func(test *testing.T) {
			fixture := newAskedRecordFixture(test)
			record, err := testHandler.createAskedDecisionRecord(fixture.recordRequest(), fixture.issue, fixture.recordInput())
			if err != nil {
				test.Fatal(err)
			}
			original := *testHandler
			query := "-- name: WithdrawDecisionAnswer"
			if first == "cancel" {
				query = "-- name: LockDecisionRequest"
			}
			paused, unblock := pauseDecisionTransaction(test, query)
			start := func(handler *Handler, operation string) <-chan error {
				if operation == "veto" {
					return startDecisionMutation(test, handler, fixture, record, "veto")
				}
				finished := make(chan error, 1)
				go func() {
					ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
					defer cancel()
					transaction, err := handler.TxStarter.Begin(ctx)
					if err != nil {
						finished <- err
						return
					}
					defer transaction.Rollback(ctx)
					queries := handler.Queries.WithTx(transaction)
					if _, err = queries.LockWorkspaceForChatSessionCreate(ctx, fixture.issue.WorkspaceID); err == nil {
						_, err = queries.LockDecisionSession(ctx, db.LockDecisionSessionParams{WorkspaceID: fixture.issue.WorkspaceID, ID: record.Request.ChatSessionID})
					}
					if err == nil {
						_, err = queries.LockDecisionIssue(ctx, db.LockDecisionIssueParams{WorkspaceID: fixture.issue.WorkspaceID, ID: fixture.issue.ID})
					}
					if err == nil {
						_, err = queries.LockDecisionRequest(ctx, db.LockDecisionRequestParams{WorkspaceID: fixture.issue.WorkspaceID, ID: record.Request.ID})
					}
					if err == nil {
						_, err = transaction.Exec(ctx, `UPDATE decision_request SET status = 'cancelled', cancel_reason = 'issue_closed' WHERE id = $1`, record.Request.ID)
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
			second := "cancel"
			if first == "cancel" {
				second = "veto"
			}
			secondDone := start(&original, second)
			if !waitForWaiterBlockedBy(test, holderPID, 3*time.Second) {
				test.Fatal("lifecycle cancellation bypassed the current-session fence")
			}
			unblock()
			if err := awaitDecisionMutation(test, firstDone); err != nil {
				test.Fatal(err)
			}
			if err := awaitDecisionMutation(test, secondDone); err != nil {
				test.Fatal(err)
			}
			if dbfx.Count(test, `SELECT count(*) FROM decision_request WHERE id = $1 AND status = 'cancelled' AND cancel_reason = 'issue_closed'`, record.Request.ID) != 1 || dbfx.Count(test, `SELECT count(*) FROM decision_answer WHERE id = $1 AND disclaimed_at IS NOT NULL AND wake_at IS NULL`, record.Answer.ID) != 1 {
				test.Fatal("veto reopened a lifecycle-cancelled decision")
			}
		})
	}
}
