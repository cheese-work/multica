package handler

import (
	"context"
	"errors"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

type decisionRunner interface {
	ProcessPass(context.Context) (bool, error)
	Run(context.Context)
	Notify()
	WaitWithTimeout(time.Duration) bool
}

type absentDecisionRunner struct{}

func (absentDecisionRunner) ProcessPass(context.Context) (bool, error) { return false, nil }
func (absentDecisionRunner) Run(ctx context.Context)                   { <-ctx.Done() }
func (absentDecisionRunner) Notify()                                   {}
func (absentDecisionRunner) WaitWithTimeout(time.Duration) bool        { return true }

func decisionRunnerFor(test *testing.T, handler *Handler) decisionRunner {
	test.Helper()
	field := reflect.ValueOf(handler).Elem().FieldByName("DecisionWorker")
	if field.IsValid() && !field.IsNil() {
		return field.Interface().(decisionRunner)
	}
	return absentDecisionRunner{}
}

func newDecisionRunnerFixture(test *testing.T) (*Handler, decisionRunner) {
	test.Helper()
	handler := New(testHandler.Queries, testPool, nil, events.New(), nil, nil, nil, nil, Config{})
	return handler, decisionRunnerFor(test, handler)
}

func resetDecisionLease(test *testing.T) {
	test.Helper()
	dbfx.Exec(test, `DELETE FROM decision_worker_lease`)
	test.Cleanup(func() { dbfx.Exec(test, `DELETE FROM decision_worker_lease`) })
}

func decisionPass(test *testing.T, runner decisionRunner) {
	test.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := runner.ProcessPass(ctx); err != nil {
		test.Fatal(err)
	}
}

func TestDecisionWorkerLeaseTakeover(test *testing.T) {
	resetDecisionLease(test)
	_, first := newDecisionRunnerFixture(test)
	_, second := newDecisionRunnerFixture(test)
	decisionPass(test, first)
	if count := dbfx.Count(test, `SELECT count(*) FROM decision_worker_lease WHERE expires_at > clock_timestamp() AND expires_at <= clock_timestamp() + interval '30 seconds'`); count != 1 {
		test.Fatalf("active 30-second leases = %d, want 1", count)
	}
	if acquired, err := second.ProcessPass(context.Background()); err != nil || acquired {
		test.Fatalf("competing worker acquired live lease: acquired=%v err=%v", acquired, err)
	}
	dbfx.Exec(test, `UPDATE decision_worker_lease SET expires_at = clock_timestamp() - interval '1 second'`)
	if acquired, err := second.ProcessPass(context.Background()); err != nil || !acquired {
		test.Fatalf("expired lease not taken over: acquired=%v err=%v", acquired, err)
	}
	if acquired, err := first.ProcessPass(context.Background()); err != nil || acquired {
		test.Fatalf("old holder resumed after takeover: acquired=%v err=%v", acquired, err)
	}
}

func TestDecisionWorkerFinalize(test *testing.T) {
	for _, state := range []string{"running", "completed", "held", "failed", "cancelled", "archived", "unprompted", "request_cancelled"} {
		test.Run(state, func(test *testing.T) {
			resetDecisionLease(test)
			fixture := newAskedRecordFixture(test)
			recorded, err := testHandler.createAskedDecisionRecord(fixture.recordRequest(), fixture.issue, fixture.recordInput())
			if err != nil {
				test.Fatal(err)
			}
			status := "completed"
			if state == "running" || state == "failed" || state == "cancelled" {
				status = state
			}
			if state == "archived" || state == "unprompted" {
				status = "failed"
			}
			dbfx.Exec(test, `UPDATE agent_task_queue SET status = $2 WHERE id = $1`, fixture.chatTaskID, status)
			if state == "held" {
				dbfx.Exec(test, `UPDATE decision_answer SET effective_at = clock_timestamp() + interval '10 minutes' WHERE id = $1`, recorded.Answer.ID)
			}
			if state == "archived" {
				dbfx.Exec(test, `UPDATE chat_session SET status = 'archived' WHERE id = $1`, fixture.sessionID)
			}
			if state == "unprompted" {
				dbfx.Exec(test, `UPDATE decision_request SET origin = 'unprompted' WHERE id = $1`, recorded.Request.ID)
			}
			if state == "request_cancelled" {
				dbfx.Exec(test, `UPDATE decision_request SET status = 'cancelled', cancel_reason = 'issue_closed' WHERE id = $1`, recorded.Request.ID)
			}
			_, runner := newDecisionRunnerFixture(test)
			decisionPass(test, runner)
			answer, err := testHandler.Queries.GetDecisionAnswer(context.Background(), db.GetDecisionAnswerParams{WorkspaceID: recorded.Request.WorkspaceID, ID: recorded.Answer.ID})
			if err != nil {
				test.Fatal(err)
			}
			if state == "running" || state == "request_cancelled" {
				if answer.WakeAt.Valid || answer.DisclaimedAt.Valid {
					test.Fatalf("ineligible answer changed: %+v", answer)
				}
				return
			}
			if status == "completed" {
				if !answer.WakeAt.Valid || answer.WakeAt.Time.Before(answer.EffectiveAt.Time) || (state == "held" && !answer.WakeAt.Time.Equal(answer.EffectiveAt.Time)) {
					test.Fatalf("completed answer not scheduled with its hold: %+v", answer)
				}
			} else {
				if !answer.DisclaimedAt.Valid || answer.DisclaimReason.String != "answering_turn_"+status || !answer.DisclaimCommentPostedAt.Valid || !answer.WithdrawalCommentID.Valid || answer.WakeAt.Valid {
					test.Fatalf("failed/cancelled answer not withdrawn: %+v", answer)
				}
				request, err := testHandler.Queries.GetDecisionRequest(context.Background(), db.GetDecisionRequestParams{WorkspaceID: recorded.Request.WorkspaceID, ID: recorded.Request.ID})
				if err != nil {
					test.Fatal(err)
				}
				wanted := "open"
				if state == "archived" || state == "unprompted" {
					wanted = "cancelled"
				}
				if request.Status != wanted || (state == "archived" && (request.CancelReason.String != "lane_archived" || !request.CancelNoticePostedAt.Valid)) || (state == "unprompted" && request.CancelReason.String != "record_disclaimed") {
					test.Fatalf("wrong withdrawal request state: %+v", request)
				}
			}
			dbfx.Exec(test, `UPDATE decision_worker_lease SET expires_at = clock_timestamp() - interval '1 second'`)
			_, restarted := newDecisionRunnerFixture(test)
			decisionPass(test, restarted)
			again, err := testHandler.Queries.GetDecisionAnswer(context.Background(), db.GetDecisionAnswerParams{WorkspaceID: recorded.Request.WorkspaceID, ID: answer.ID})
			if err != nil || !reflect.DeepEqual(answer, again) {
				test.Fatalf("restart changed finalized answer: err=%v before=%+v after=%+v", err, answer, again)
			}
			wantedComments := 0
			if status != "completed" {
				wantedComments = 1
			}
			if count := dbfx.Count(test, `SELECT count(*) FROM comment WHERE decision_answer_id = $1 AND decision_comment_kind = 'disclaim'`, answer.ID); count != wantedComments {
				test.Fatalf("withdrawal comments = %d, want %d", count, wantedComments)
			}
		})
	}
}

func TestDecisionWorkerSourceSealPing(test *testing.T) {
	resetDecisionLease(test)
	fixture := newAskedRecordFixture(test)
	recorded, err := testHandler.createAskedDecisionRecord(fixture.recordRequest(), fixture.issue, fixture.recordInput())
	if err != nil {
		test.Fatal(err)
	}
	intentID := dbfx.Insert(test, "decision_intent", testutil.Cols{
		"workspace_id": testWorkspaceID, "issue_id": fixture.issueID, "task_id": fixture.taskID,
		"agent_id": fixture.agentID, "comment_id": fixture.tagID,
		"expires_at": testutil.Raw("clock_timestamp() - interval '1 second'"),
	})
	dbfx.Exec(test, `DELETE FROM chat_message WHERE id = $1`, fixture.messageID)
	handler, runner := newDecisionRunnerFixture(test)
	var posted atomic.Int32
	handler.Bus.Subscribe(protocol.EventCommentCreated, func(event events.Event) {
		if count := dbfx.Count(test, `SELECT count(*) FROM decision_answer WHERE id = $1 AND disclaimed_at IS NOT NULL`, recorded.Answer.ID); count != 1 {
			test.Error("event published before withdrawal committed")
		}
		posted.Add(1)
	})
	decisionPass(test, runner)
	decisionPass(test, runner)
	if count := dbfx.Count(test, `SELECT count(*) FROM decision_answer WHERE id = $1 AND disclaim_reason = 'source_removed'`, recorded.Answer.ID); count != 1 {
		test.Fatalf("source withdrawals = %d, want 1", count)
	}
	if count := dbfx.Count(test, `SELECT count(*) FROM decision_intent WHERE id = $1 AND sealed_at IS NOT NULL`, intentID); count != 1 {
		test.Fatalf("expired sealed intents = %d, want 1", count)
	}
	if count := dbfx.Count(test, `SELECT count(*) FROM inbox_item WHERE recipient_id = $1 AND details->>'decision_request_id' = $2`, fixture.humanID, uuidToString(recorded.Request.ID)); count != 1 {
		test.Fatalf("ping inbox notices = %d, want 1", count)
	}
	if posted.Load() != 1 {
		test.Fatalf("committed withdrawal events = %d, want 1", posted.Load())
	}
}

type decisionFailCommit struct{ txStarter }
type decisionFailCommitTx struct{ pgx.Tx }

func (starter decisionFailCommit) Begin(ctx context.Context) (pgx.Tx, error) {
	transaction, err := starter.txStarter.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return decisionFailCommitTx{transaction}, nil
}

func (decisionFailCommitTx) Commit(context.Context) error {
	return errors.New("injected commit failure")
}

func TestDecisionWorkerRollbackPublishesNothing(test *testing.T) {
	resetDecisionLease(test)
	fixture := newAskedRecordFixture(test)
	recorded, err := testHandler.createAskedDecisionRecord(fixture.recordRequest(), fixture.issue, fixture.recordInput())
	if err != nil {
		test.Fatal(err)
	}
	dbfx.Exec(test, `DELETE FROM chat_message WHERE id = $1`, fixture.messageID)
	handler, runner := newDecisionRunnerFixture(test)
	handler.TxStarter = decisionFailCommit{testPool}
	var posted atomic.Int32
	handler.Bus.SubscribeAll(func(events.Event) { posted.Add(1) })
	_, _ = runner.ProcessPass(context.Background())
	if posted.Load() != 0 || dbfx.Count(test, `SELECT count(*) FROM decision_answer WHERE id = $1 AND disclaimed_at IS NOT NULL`, recorded.Answer.ID) != 0 {
		test.Fatal("rolled-back consequence was published or persisted")
	}
	handler.TxStarter = testPool
	decisionPass(test, runner)
	if posted.Load() == 0 {
		test.Fatal("committed retry did not publish its consequence")
	}
}

func TestDecisionWorkerOrphans(test *testing.T) {
	for _, missing := range []string{"issue", "workspace", "lane"} {
		test.Run(missing, func(test *testing.T) {
			resetDecisionLease(test)
			fixture := newAskedRecordFixture(test)
			recorded, err := testHandler.createAskedDecisionRecord(fixture.recordRequest(), fixture.issue, fixture.recordInput())
			if err != nil {
				test.Fatal(err)
			}
			dbfx.Exec(test, `INSERT INTO decision_action (workspace_id, decision_request_id, action_type, action_version, params, payload_sha256, expires_at, requested_by_agent_id)
				SELECT workspace_id, id, 'agent.env.patch', 1, '{}', repeat('a', 64), now() + interval '1 day', $2 FROM decision_request WHERE id = $1`, recorded.Request.ID, fixture.agentID)
			if missing == "issue" {
				dbfx.Exec(test, `DELETE FROM issue WHERE id = $1`, fixture.issueID)
			} else if missing == "workspace" {
				workspaceID := dbfx.Insert(test, "workspace", testutil.Cols{"name": "orphan worker", "slug": "orphan-" + uuid.NewString()})
				dbfx.Exec(test, `UPDATE decision_request SET workspace_id = $2 WHERE id = $1`, recorded.Request.ID, workspaceID)
				dbfx.Exec(test, `UPDATE decision_intent SET workspace_id = $2 WHERE id = $1`, fixture.intentID, workspaceID)
				dbfx.Exec(test, `DELETE FROM workspace WHERE id = $1`, workspaceID)
			} else {
				dbfx.Exec(test, `DELETE FROM chat_session WHERE id = $1`, fixture.sessionID)
			}
			_, runner := newDecisionRunnerFixture(test)
			decisionPass(test, runner)
			decisionPass(test, runner)
			wanted := 0
			if missing == "lane" {
				wanted = 1
			}
			for _, query := range []string{
				`SELECT count(*) FROM decision_request WHERE id = $1`,
				`SELECT count(*) FROM decision_answer WHERE decision_request_id = $1`,
				`SELECT count(*) FROM decision_action WHERE decision_request_id = $1`,
			} {
				if count := dbfx.Count(test, query, recorded.Request.ID); count != wanted {
					test.Fatalf("%s orphan rows = %d, want %d", missing, count, wanted)
				}
			}
			if count := dbfx.Count(test, `SELECT count(*) FROM decision_intent WHERE id = $1`, fixture.intentID); count != wanted {
				test.Fatalf("orphan intents = %d, want %d", count, wanted)
			}
			if count := dbfx.Count(test, `SELECT count(*) FROM decision_intent_target WHERE decision_intent_id = $1`, fixture.intentID); count != wanted {
				test.Fatalf("orphan targets = %d, want %d", count, wanted)
			}
		})
	}
}

func TestDecisionWorkerRenewalLossStopsPass(test *testing.T) {
	resetDecisionLease(test)
	fixture := newAskedRecordFixture(test)
	recorded, err := testHandler.createAskedDecisionRecord(fixture.recordRequest(), fixture.issue, fixture.recordInput())
	if err != nil {
		test.Fatal(err)
	}
	dbfx.Exec(test, `UPDATE agent_task_queue SET status = 'completed' WHERE id = $1`, fixture.chatTaskID)
	blocker, err := testPool.Begin(context.Background())
	if err != nil {
		test.Fatal(err)
	}
	defer blocker.Rollback(context.Background())
	if _, err := blocker.Exec(context.Background(), `SELECT id FROM chat_session WHERE id = $1 FOR UPDATE`, fixture.sessionID); err != nil {
		test.Fatal(err)
	}
	_, runner := newDecisionRunnerFixture(test)
	ctx, cancel := context.WithTimeout(context.Background(), 14*time.Second)
	defer cancel()
	finished := make(chan error, 1)
	go func() { _, err := runner.ProcessPass(ctx); finished <- err }()
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	for dbfx.Count(test, `SELECT count(*) FROM decision_worker_lease`) == 0 {
		select {
		case <-finished:
			test.Fatal("pass ended without acquiring a lease")
		case <-deadline.C:
			test.Fatal("worker did not acquire a lease")
		case <-time.After(10 * time.Millisecond):
		}
	}
	dbfx.Exec(test, `UPDATE decision_worker_lease SET holder = 'replacement', token = $1, expires_at = clock_timestamp() + interval '30 seconds'`, uuid.NewString())
	select {
	case <-finished:
	case <-ctx.Done():
		test.Fatal("lost lease did not cancel the blocked pass at renewal")
	}
	if count := dbfx.Count(test, `SELECT count(*) FROM decision_answer WHERE id = $1 AND wake_at IS NOT NULL`, recorded.Answer.ID); count != 0 {
		test.Fatal("worker finalized after losing lease")
	}
}

func TestDecisionWorkerNotifyAndShutdown(test *testing.T) {
	resetDecisionLease(test)
	_, runner := newDecisionRunnerFixture(test)
	ctx, cancel := context.WithCancel(context.Background())
	go runner.Run(ctx)
	defer func() {
		cancel()
		if !runner.WaitWithTimeout(time.Second) {
			test.Error("worker did not join on shutdown")
		}
		if count := dbfx.Count(test, `SELECT count(*) FROM decision_worker_lease WHERE expires_at > clock_timestamp()`); count != 0 {
			test.Error("shutdown did not release the worker's lease")
		}
	}()
	fixture := newAskedRecordFixture(test)
	recorded, err := testHandler.createAskedDecisionRecord(fixture.recordRequest(), fixture.issue, fixture.recordInput())
	if err != nil {
		test.Fatal(err)
	}
	dbfx.Exec(test, `UPDATE agent_task_queue SET status = 'completed' WHERE id = $1`, fixture.chatTaskID)
	for range 100 {
		runner.Notify()
	}
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	for dbfx.Count(test, `SELECT count(*) FROM decision_answer WHERE id = $1 AND wake_at IS NOT NULL`, recorded.Answer.ID) != 1 {
		select {
		case <-deadline.C:
			test.Fatal("Notify did not start a pass before the 5-second tick")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestDecisionWorkerRetryBound(test *testing.T) {
	for _, code := range []string{"55P03", "40P01", "23505", "08006"} {
		test.Run(code, func(test *testing.T) {
			attempts := 0
			failure := &pgconn.PgError{Code: code}
			err := decisionAttempt(context.Background(), func() error { attempts++; return failure })
			wanted := 1
			if code == "55P03" || code == "40P01" {
				wanted = 2
			}
			if attempts != wanted || !errors.Is(err, failure) {
				test.Fatalf("attempts=%d want=%d err=%v", attempts, wanted, err)
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := decisionAttempt(ctx, func() error { test.Fatal("cancelled pass ran another item"); return nil })
	if !errors.Is(err, context.Canceled) {
		test.Fatalf("cancellation was not preserved: %v", err)
	}
}

func TestDecisionWorkerNotifiedOnlyAfterCommit(test *testing.T) {
	for _, fail := range []bool{false, true} {
		test.Run(map[bool]string{false: "commit", true: "rollback"}[fail], func(test *testing.T) {
			fixture := newAskedRecordFixture(test)
			for len(testHandler.DecisionWorker.notify) > 0 {
				<-testHandler.DecisionWorker.notify
			}
			if fail {
				testHandler.TxStarter = decisionFailCommit{testPool}
				defer func() { testHandler.TxStarter = testPool }()
			}
			_, err := testHandler.createAskedDecisionRecord(fixture.recordRequest(), fixture.issue, fixture.recordInput())
			if (err != nil) != fail {
				test.Fatalf("wrong commit result: %v", err)
			}
			if notified := len(testHandler.DecisionWorker.notify) > 0; notified == fail {
				test.Fatalf("post-commit hint=%v after rollback=%v", notified, fail)
			}
		})
	}
}
