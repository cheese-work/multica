package handler

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/multica-ai/multica/server/internal/decisiontx"
	"github.com/multica-ai/multica/server/internal/middleware"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func pauseDecisionTransaction(test *testing.T, query string) (<-chan int, func()) {
	test.Helper()
	paused, release := make(chan int, 1), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	test.Cleanup(unblock)
	starter := &requestCoreStarter{base: testHandler.TxStarter, query: query, before: func(transaction pgx.Tx) error {
		var backendPID int
		if err := transaction.QueryRow(context.Background(), `SELECT pg_backend_pid()`).Scan(&backendPID); err != nil {
			return err
		}
		paused <- backendPID
		select {
		case <-release:
			return nil
		case <-time.After(8 * time.Second):
			return errors.New("decision transaction probe timed out")
		}
	}}
	useRequestCoreStarter(test, starter)
	return paused, unblock
}

func awaitDecisionPause(test *testing.T, paused <-chan int) int {
	test.Helper()
	select {
	case backendPID := <-paused:
		return backendPID
	case <-time.After(4 * time.Second):
		test.Fatal("decision transaction did not reach its fence")
		return 0
	}
}

type recordCoreOutcome struct {
	result decisionRecordResult
	err    error
}

func startRecordCore(test *testing.T, handler *Handler, fixture askedRecordFixture) <-chan recordCoreOutcome {
	test.Helper()
	request := fixture.recordRequest()
	ctx, cancel := context.WithTimeout(request.Context(), 8*time.Second)
	test.Cleanup(cancel)
	finished := make(chan recordCoreOutcome, 1)
	go func() {
		result, err := handler.createAskedDecisionRecord(request.WithContext(ctx), fixture.issue, fixture.recordInput())
		finished <- recordCoreOutcome{result, err}
	}()
	return finished
}

func awaitRecordCore(test *testing.T, finished <-chan recordCoreOutcome) recordCoreOutcome {
	test.Helper()
	select {
	case outcome := <-finished:
		return outcome
	case <-time.After(10 * time.Second):
		test.Fatal("record transaction did not finish")
		return recordCoreOutcome{}
	}
}

func decisionSessionRequestForHuman(test *testing.T, fixture requestCoreFixture, action string) *http.Request {
	test.Helper()
	request := decisionGuardRequest(test, fixture.sessionID, action)
	request.Header.Set("X-User-ID", fixture.humanID)
	member, err := testHandler.Queries.GetMemberByUserAndWorkspace(request.Context(), db.GetMemberByUserAndWorkspaceParams{UserID: parseUUID(fixture.humanID), WorkspaceID: parseUUID(testWorkspaceID)})
	if err != nil {
		test.Fatal(err)
	}
	return request.WithContext(middleware.SetMemberContext(request.Context(), testWorkspaceID, member))
}

func startDecisionSessionAction(test *testing.T, handler *Handler, request *http.Request, action string) <-chan int {
	test.Helper()
	ctx, cancel := context.WithTimeout(request.Context(), 8*time.Second)
	test.Cleanup(cancel)
	finished := make(chan int, 1)
	go func() {
		response := httptest.NewRecorder()
		if action == "archive" {
			handler.SetChatSessionArchived(response, request.WithContext(ctx))
		} else if action == "continue" {
			handler.ContinueChatSession(response, request.WithContext(ctx))
		} else {
			handler.DeleteChatSession(response, request.WithContext(ctx))
		}
		finished <- response.Code
	}()
	return finished
}

func awaitDecisionSessionAction(test *testing.T, finished <-chan int) int {
	test.Helper()
	select {
	case status := <-finished:
		return status
	case <-time.After(10 * time.Second):
		test.Fatal("session action did not finish")
		return 0
	}
}

func TestDecisionPingArchiveOrders(test *testing.T) {
	for _, action := range []string{"archive", "continue"} {
		for _, first := range []string{"ping", "archive"} {
			test.Run(action+"/"+first+"_first", func(test *testing.T) {
				fixture := newRequestCoreFixture(test, false)
				original := *testHandler
				request := decisionSessionRequestForHuman(test, fixture, action)
				query := "-- name: CreateDecisionRequest"
				if first == "archive" {
					query = "-- name: DecisionOpenLaneExists"
				}
				paused, unblock := pauseDecisionTransaction(test, query)
				var ping <-chan requestCoreOutcome
				var archive <-chan int
				if first == "ping" {
					ping = startRequestCore(test, fixture, fixture.input())
				} else {
					archive = startDecisionSessionAction(test, testHandler, request, action)
				}
				holderPID := awaitDecisionPause(test, paused)
				if first == "ping" {
					archive = startDecisionSessionAction(test, &original, request, action)
				} else {
					finished := make(chan requestCoreOutcome, 1)
					go func() {
						result, err := original.createDecisionRequest(fixture.request(), fixture.issue, fixture.input())
						finished <- requestCoreOutcome{result, err}
					}()
					ping = finished
				}
				if !waitForWaiterBlockedBy(test, holderPID, 3*time.Second) {
					test.Fatal("second operation bypassed the session fence")
				}
				unblock()
				outcome, status := awaitRequestCore(test, ping), awaitDecisionSessionAction(test, archive)
				if first == "ping" {
					if outcome.err != nil || status != http.StatusConflict {
						test.Fatalf("ping-first outcome: %v, archive %d", outcome.err, status)
					}
					fixture.assertCounts(test, 1)
				} else {
					wanted := http.StatusOK
					if action == "continue" {
						wanted = http.StatusCreated
					}
					if outcome.err == nil || status != wanted {
						test.Fatalf("archive-first outcome: %v, archive %d", outcome.err, status)
					}
					fixture.assertCounts(test, 0)
				}
			})
		}
	}
}

func TestAskedDecisionRecordDeleteOrders(test *testing.T) {
	for _, first := range []string{"record", "delete"} {
		test.Run(first+"_first", func(test *testing.T) {
			fixture := newAskedRecordFixture(test)
			original := *testHandler
			request := decisionSessionRequestForHuman(test, fixture.requestCoreFixture, "delete")
			query := "-- name: CreateDecisionAnswer"
			if first == "delete" {
				query = "-- name: ListDecisionSessionReferences"
			}
			paused, unblock := pauseDecisionTransaction(test, query)
			var record <-chan recordCoreOutcome
			var deletion <-chan int
			if first == "record" {
				record = startRecordCore(test, testHandler, fixture)
			} else {
				deletion = startDecisionSessionAction(test, testHandler, request, "delete")
			}
			holderPID := awaitDecisionPause(test, paused)
			if first == "record" {
				deletion = startDecisionSessionAction(test, &original, request, "delete")
			} else {
				record = startRecordCore(test, &original, fixture)
			}
			if !waitForWaiterBlockedBy(test, holderPID, 3*time.Second) {
				test.Fatal("second operation bypassed the record/delete session fence")
			}
			unblock()
			outcome, status := awaitRecordCore(test, record), awaitDecisionSessionAction(test, deletion)
			if outcome.err != nil || status != http.StatusConflict {
				test.Fatalf("record/delete outcome: %v, deletion %d", outcome.err, status)
			}
			fixture.assertRecordCounts(test, 1)
		})
	}
}

func TestAskedDecisionRecordSourceDeleteOrders(test *testing.T) {
	for _, first := range []string{"source_delete", "record"} {
		test.Run(first+"_first", func(test *testing.T) {
			fixture := newAskedRecordFixture(test)
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			if first == "source_delete" {
				holder, err := testPool.Begin(ctx)
				if err != nil {
					test.Fatal(err)
				}
				defer holder.Rollback(ctx)
				var holderPID int
				if err := holder.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&holderPID); err != nil {
					test.Fatal(err)
				}
				if _, err := holder.Exec(ctx, `SELECT id FROM chat_session WHERE id = $1 FOR UPDATE`, fixture.sessionID); err != nil {
					test.Fatal(err)
				}
				if _, err := holder.Exec(ctx, `DELETE FROM chat_message WHERE id = $1`, fixture.messageID); err != nil {
					test.Fatal(err)
				}
				record := startRecordCore(test, testHandler, fixture)
				if !waitForWaiterBlockedBy(test, holderPID, 3*time.Second) {
					test.Fatal("record bypassed the source-delete session fence")
				}
				if err := holder.Commit(ctx); err != nil {
					test.Fatal(err)
				}
				outcome := awaitRecordCore(test, record)
				var refusal decisionIntentRefusal
				if !errors.As(outcome.err, &refusal) || refusal.status != http.StatusBadRequest {
					test.Fatalf("removed source was recorded: %v", outcome.err)
				}
				fixture.assertRecordCounts(test, 0)
				return
			}
			paused, unblock := pauseDecisionTransaction(test, "-- name: CreateDecisionAnswer")
			record := startRecordCore(test, testHandler, fixture)
			holderPID := awaitDecisionPause(test, paused)
			deleted := make(chan error, 1)
			go func() {
				transaction, err := testPool.Begin(ctx)
				if err != nil {
					deleted <- err
					return
				}
				defer transaction.Rollback(ctx)
				_, err = transaction.Exec(ctx, `SELECT id FROM chat_session WHERE id = $1 FOR UPDATE`, fixture.sessionID)
				if err == nil {
					_, err = transaction.Exec(ctx, `DELETE FROM chat_message WHERE id = $1`, fixture.messageID)
				}
				if err == nil {
					err = transaction.Commit(ctx)
				}
				deleted <- err
			}()
			if !waitForWaiterBlockedBy(test, holderPID, 3*time.Second) {
				test.Fatal("source delete bypassed the record's session fence")
			}
			unblock()
			outcome := awaitRecordCore(test, record)
			if outcome.err != nil {
				test.Fatal(outcome.err)
			}
			if err := <-deleted; err != nil {
				test.Fatal(err)
			}
			fixture.assertRecordCounts(test, 1)
			if outcome.result.Answer.ContentSnapshot == "" || outcome.result.Answer.WakeAt.Valid || dbfx.Count(test, `SELECT count(*) FROM chat_message WHERE id = $1`, fixture.messageID) != 0 {
				test.Fatal("source deletion changed the snapshot or finalized the answer")
			}
		})
	}
}

func TestAskedDecisionRecordRollback(test *testing.T) {
	for _, query := range []string{"CreateComment", "CreateChatMessage", "MarkDecisionRequestAnswered"} {
		test.Run(query, func(test *testing.T) {
			fixture := newAskedRecordFixture(test)
			starter := &requestCoreStarter{base: testHandler.TxStarter, query: "-- name: " + query, fail: &pgconn.PgError{Code: "23514", Message: "injected record write failure"}}
			useRequestCoreStarter(test, starter)
			result, err := testHandler.createAskedDecisionRecord(fixture.recordRequest(), fixture.issue, fixture.recordInput())
			if err == nil || result.Answer.ID.Valid {
				test.Fatalf("write failure returned a partial result: %+v %v", result, err)
			}
			fixture.assertRecordCounts(test, 0)
			if dbfx.Count(test, `SELECT count(*) FROM decision_request WHERE id = $1 AND status = 'open'`, fixture.decision.ID) != 1 {
				test.Fatal("failed transaction answered the request")
			}
		})
	}
}

func TestAskedDecisionRecordTaskNOWAIT(test *testing.T) {
	fixture := newAskedRecordFixture(test)
	holder, err := testPool.Begin(context.Background())
	if err != nil {
		test.Fatal(err)
	}
	defer holder.Rollback(context.Background())
	if _, err := holder.Exec(context.Background(), `SELECT id FROM agent_task_queue WHERE id = $1 FOR UPDATE`, fixture.chatTaskID); err != nil {
		test.Fatal(err)
	}
	starter := &intentRetryStarter{base: testHandler.TxStarter}
	useRequestCoreStarter(test, starter)
	_, err = testHandler.createAskedDecisionRecord(fixture.recordRequest(), fixture.issue, fixture.recordInput())
	if !decisiontx.IsRetryable(err) || starter.attempts != 3 {
		test.Fatalf("NOWAIT retry bound not respected: attempts=%d err=%v", starter.attempts, err)
	}
	response := httptest.NewRecorder()
	writeDecisionError(response, err, "decision record")
	if response.Code != http.StatusServiceUnavailable {
		test.Fatalf("busy task status=%d, want 503", response.Code)
	}
	fixture.assertRecordCounts(test, 0)
}

func TestAskedDecisionRecordTerminalWriterFirst(test *testing.T) {
	for _, status := range []string{"completed", "failed", "cancelled"} {
		test.Run(status, func(test *testing.T) {
			fixture := newAskedRecordFixture(test)
			request := fixture.recordRequest()
			holder, err := testPool.Begin(context.Background())
			if err != nil {
				test.Fatal(err)
			}
			defer holder.Rollback(context.Background())
			if _, err := holder.Exec(context.Background(), `UPDATE agent_task_queue SET status = $1, completed_at = now() WHERE id = $2 AND status = 'running'`, status, fixture.chatTaskID); err != nil {
				test.Fatal(err)
			}
			var once sync.Once
			starter := &intentRetryStarter{base: testHandler.TxStarter, onBusy: func() {
				once.Do(func() {
					if err := holder.Commit(context.Background()); err != nil {
						test.Fatal(err)
					}
				})
			}}
			useRequestCoreStarter(test, starter)
			_, err = testHandler.createAskedDecisionRecord(request, fixture.issue, fixture.recordInput())
			var refusal decisionIntentRefusal
			if !errors.As(err, &refusal) || refusal.status != http.StatusForbidden || starter.attempts != 1 {
				test.Fatalf("call admitted before terminal commit was not refused: attempts=%d err=%v", starter.attempts, err)
			}
			fixture.assertRecordCounts(test, 0)
		})
	}
}

func TestAskedDecisionRecordBeforeTerminalWriter(test *testing.T) {
	fixture := newAskedRecordFixture(test)
	paused, unblock := pauseDecisionTransaction(test, "-- name: CreateDecisionAnswer")
	record := startRecordCore(test, testHandler, fixture)
	holderPID := awaitDecisionPause(test, paused)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	completed := make(chan error, 1)
	go func() {
		_, err := testHandler.Queries.CompleteAgentTask(ctx, db.CompleteAgentTaskParams{ID: parseUUID(fixture.chatTaskID)})
		completed <- err
	}()
	if !waitForWaiterBlockedBy(test, holderPID, 3*time.Second) {
		test.Fatal("terminal writer bypassed the calling-task fence")
	}
	unblock()
	outcome := awaitRecordCore(test, record)
	if outcome.err != nil {
		test.Fatal(outcome.err)
	}
	if err := <-completed; err != nil {
		test.Fatal(err)
	}
	fixture.assertRecordCounts(test, 1)
	if dbfx.Count(test, `SELECT count(*) FROM decision_answer WHERE id = $1 AND wake_at IS NULL AND disclaimed_at IS NULL`, outcome.result.Answer.ID) != 1 {
		test.Fatal("baseline terminal writer finalized the pending answer")
	}
}

func TestAskedDecisionRecordDeadlockBound(test *testing.T) {
	fixture := newAskedRecordFixture(test)
	starter := &requestCoreStarter{base: testHandler.TxStarter, query: "-- name: CreateDecisionAnswer", fail: &pgconn.PgError{Code: "40P01", Message: "injected deadlock"}}
	useRequestCoreStarter(test, starter)
	_, err := testHandler.createAskedDecisionRecord(fixture.recordRequest(), fixture.issue, fixture.recordInput())
	if !decisiontx.IsRetryable(err) || starter.attempts != 2 {
		test.Fatalf("deadlock retried more than once: attempts=%d err=%v", starter.attempts, err)
	}
	fixture.assertRecordCounts(test, 0)
}

func TestDecisionCommentDeleteHasNoDecisionConsequence(test *testing.T) {
	for _, target := range []string{"ask", "tag"} {
		test.Run(target, func(test *testing.T) {
			fixture := newAskedRecordFixture(test)
			commentID := fixture.parentID
			if target == "tag" {
				commentID = fixture.tagID
			}
			holder, err := testPool.Begin(context.Background())
			if err != nil {
				test.Fatal(err)
			}
			defer holder.Rollback(context.Background())
			if _, err := holder.Exec(context.Background(), `SELECT id FROM decision_request WHERE id = $1 FOR UPDATE`, fixture.decision.ID); err != nil {
				test.Fatal(err)
			}
			request := withURLParam(newRequest(http.MethodDelete, "/api/comments/"+commentID, nil), "commentId", commentID)
			ctx, cancel := context.WithTimeout(request.Context(), time.Second)
			defer cancel()
			testutil.Call(test, testHandler.DeleteComment, request.WithContext(ctx)).Want(http.StatusNoContent)
			if dbfx.Count(test, `SELECT count(*) FROM decision_request WHERE id = $1 AND status = 'open'`, fixture.decision.ID) != 1 || dbfx.Count(test, `SELECT count(*) FROM decision_intent WHERE id = $1 AND closed_at IS NULL`, fixture.intentID) != 1 {
				test.Fatal("baseline comment deletion wrote a decision consequence")
			}
		})
	}
}
