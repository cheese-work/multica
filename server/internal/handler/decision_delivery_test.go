package handler

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func TestDecisionDeliveryBranches(test *testing.T) {
	for _, branch := range []string{"enqueue", "queued", "running", "squad", "member", "unassigned", "unbound", "different_head", "closed", "human_removed", "asking_archived"} {
		test.Run(branch, func(test *testing.T) {
			resetDecisionLease(test)
			fixture := newAskedRecordFixture(test)
			recorded, err := testHandler.createAskedDecisionRecord(fixture.recordRequest(), fixture.issue, fixture.recordInput())
			if err != nil {
				test.Fatal(err)
			}
			dbfx.Exec(test, `UPDATE agent_task_queue SET status = 'completed' WHERE id IN ($1, $2)`, fixture.taskID, fixture.chatTaskID)
			outcome := "run_enqueued"
			switch branch {
			case "queued", "running", "different_head":
				state := branch
				if branch == "different_head" {
					state = "running"
				}
				active := dbfx.Task(test, fixture.agentID, testutil.Cols{"issue_id": fixture.issueID, "status": state, "runtime_id": handlerTestRuntimeID(test)})
				dbfx.Exec(test, `UPDATE agent_task_queue SET comment_thread_id = $1 WHERE id = $2`, fixture.tagID, active)
				if branch == "different_head" {
					dbfx.Exec(test, `UPDATE agent_task_queue SET context = '{"head_sha":"other-head"}' WHERE id = $1`, active)
					pullID := dbfx.Insert(test, "github_pull_request", testutil.Cols{"workspace_id": testWorkspaceID, "installation_id": 1, "repo_owner": "test", "repo_name": "decision", "pr_number": 1750, "title": "Decision", "state": "open", "html_url": "https://example.test/pr/1750", "pr_created_at": time.Now(), "pr_updated_at": time.Now(), "head_sha": "current-head"})
					dbfx.Exec(test, `INSERT INTO issue_pull_request (issue_id, pull_request_id) VALUES ($1, $2)`, fixture.issueID, pullID)
					outcome = ""
				} else if branch == "queued" {
					outcome = "merged_into_queued"
				} else {
					outcome = "planned_input"
				}
			case "squad":
				squadID := dbfx.Insert(test, "squad", testutil.Cols{"workspace_id": testWorkspaceID, "name": "Decision recipients", "creator_id": testUserID, "leader_id": fixture.agentID})
				dbfx.Exec(test, `UPDATE issue SET assignee_type = 'squad', assignee_id = $1 WHERE id = $2`, squadID, fixture.issueID)
			case "member":
				dbfx.Exec(test, `UPDATE issue SET assignee_type = 'member', assignee_id = $1 WHERE id = $2`, testUserID, fixture.issueID)
				outcome = "comment_member"
			case "unassigned":
				dbfx.Exec(test, `UPDATE issue SET assignee_id = NULL, assignee_type = NULL WHERE id = $1`, fixture.issueID)
				outcome = "comment_unassigned"
			case "unbound":
				dbfx.Exec(test, `UPDATE agent SET runtime_id = NULL WHERE id = $1`, fixture.agentID)
				outcome = ""
			case "closed":
				dbfx.Exec(test, `UPDATE issue SET status = 'done' WHERE id = $1`, fixture.issueID)
				outcome = "skipped_ineligible"
			case "human_removed":
				dbfx.Exec(test, `DELETE FROM member WHERE workspace_id = $1 AND user_id = $2`, testWorkspaceID, fixture.humanID)
				outcome = "skipped_ineligible"
			case "asking_archived":
				dbfx.Exec(test, `UPDATE agent SET archived_at = clock_timestamp() WHERE id = $1`, fixture.agentID)
				outcome = "skipped_ineligible"
			}
			_, runner := newDecisionRunnerFixture(test)
			decisionPass(test, runner)
			stored, err := testHandler.Queries.GetDecisionAnswer(context.Background(), db.GetDecisionAnswerParams{WorkspaceID: fixture.issue.WorkspaceID, ID: recorded.Answer.ID})
			if err != nil {
				test.Fatal(err)
			}
			if stored.WakeOutcome.String != outcome || stored.WokenAt.Valid != (outcome != "") {
				test.Fatalf("wake outcome=%q acknowledged=%v, want %q", stored.WakeOutcome.String, stored.WokenAt.Valid, outcome)
			}
			if outcome == "" || outcome == "skipped_ineligible" {
				if count := dbfx.Count(test, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND status IN ('queued', 'running') AND trigger_comment_id = $2`, fixture.issueID, recorded.Comment.ID); count != 0 {
					test.Fatalf("ineligible/deferred delivery created %d triggered runs", count)
				}
				return
			}
			if branch == "member" || branch == "unassigned" {
				if count := dbfx.Count(test, `SELECT count(*) FROM comment WHERE decision_answer_id = $1 AND decision_comment_kind = 'delivery'`, recorded.Answer.ID); count != 1 {
					test.Fatalf("delivery comments=%d, want 1", count)
				}
			} else {
				if count := dbfx.Count(test, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND agent_id = $2 AND status IN ('queued', 'running') AND (trigger_comment_id = $3 OR $3 = ANY(coalesced_comment_ids))`, fixture.issueID, fixture.agentID, recorded.Comment.ID); count != 1 {
					test.Fatalf("durably covered quote count=%d, want 1", count)
				}
			}
			decisionPass(test, runner)
			if count := dbfx.Count(test, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND status = 'queued'`, fixture.issueID); count > 1 {
				test.Fatalf("retry duplicated runs: %d", count)
			}
		})
	}
}

type decisionDeliverer interface {
	DeliverDecisionWake(context.Context, pgtype.UUID, pgtype.UUID) error
}

func deliverDecisionForTest(handler *Handler, fixture askedRecordFixture) error {
	if deliverer, ok := any(handler).(decisionDeliverer); ok {
		return deliverer.DeliverDecisionWake(context.Background(), fixture.issue.WorkspaceID, fixture.decision.ID)
	}
	return nil
}

func readyDecisionDelivery(test *testing.T) (askedRecordFixture, decisionRecordResult) {
	test.Helper()
	fixture := newAskedRecordFixture(test)
	recorded, err := testHandler.createAskedDecisionRecord(fixture.recordRequest(), fixture.issue, fixture.recordInput())
	if err != nil {
		test.Fatal(err)
	}
	dbfx.Exec(test, `UPDATE agent_task_queue SET status = 'completed' WHERE id IN ($1, $2)`, fixture.taskID, fixture.chatTaskID)
	dbfx.Exec(test, `UPDATE decision_answer SET wake_at = clock_timestamp() WHERE id = $1`, recorded.Answer.ID)
	dbfx.Exec(test, `UPDATE decision_intent SET sealed_at = clock_timestamp() WHERE id = $1`, fixture.intentID)
	return fixture, recorded
}

type decisionDeliveryGroupFixture struct {
	core    requestCoreFixture
	members []askedRecordFixture
	records []decisionRecordResult
}

func readyDecisionDeliveryGroup(test *testing.T) decisionDeliveryGroupFixture {
	test.Helper()
	core := newRequestCoreFixture(test, true)
	group := decisionDeliveryGroupFixture{core: core}
	for _, humanID := range []string{core.humanID, testUserID} {
		person := core
		person.sessionID = dbfx.Insert(test, "chat_session", testutil.Cols{"workspace_id": testWorkspaceID, "agent_id": core.agentID, "creator_id": humanID, "title": "Concurrent group answer"})
		request, err := testHandler.createDecisionRequest(person.request(), core.issue, person.input())
		if err != nil {
			test.Fatal(err)
		}
		messageID := dbfx.Insert(test, "chat_message", testutil.Cols{"chat_session_id": person.sessionID, "role": "user", "content": "Answer from " + humanID, "sent_via": "interactive_jwt"})
		chatTaskID := dbfx.Task(test, core.agentID, testutil.Cols{"chat_session_id": person.sessionID, "status": "running", "runtime_id": handlerTestRuntimeID(test)})
		dbfx.Exec(test, `UPDATE agent_task_queue SET started_at = clock_timestamp() WHERE id = $1`, chatTaskID)
		member := askedRecordFixture{
			requestCoreFixture: person,
			decision:           request.Request,
			parentID:           uuidToString(request.Comment.ID),
			messageID:          messageID,
			chatTaskID:         chatTaskID,
		}
		recorded, err := testHandler.createAskedDecisionRecord(member.recordRequest(), core.issue, member.recordInput())
		if err != nil {
			test.Fatal(err)
		}
		dbfx.Exec(test, `UPDATE agent_task_queue SET status = 'completed' WHERE id = $1`, chatTaskID)
		dbfx.Exec(test, `UPDATE decision_answer SET wake_at = clock_timestamp() WHERE id = $1`, recorded.Answer.ID)
		group.members = append(group.members, member)
		group.records = append(group.records, recorded)
	}
	dbfx.Exec(test, `UPDATE agent_task_queue SET status = 'completed' WHERE id = $1`, core.taskID)
	dbfx.Exec(test, `UPDATE decision_intent SET sealed_at = clock_timestamp() WHERE id = $1`, core.intentID)
	return group
}

func TestDecisionDeliveryRollbackAndRestart(test *testing.T) {
	for _, recipient := range []string{"agent", "member"} {
		test.Run(recipient, func(test *testing.T) {
			fixture, recorded := readyDecisionDelivery(test)
			if recipient == "member" {
				dbfx.Exec(test, `UPDATE issue SET assignee_type = 'member', assignee_id = $1 WHERE id = $2`, testUserID, fixture.issueID)
			}
			handler, _ := newDecisionRunnerFixture(test)
			handler.TxStarter = decisionFailCommit{testPool}
			var published atomic.Int32
			handler.Bus.SubscribeAll(func(events.Event) { published.Add(1) })
			_ = deliverDecisionForTest(handler, fixture)
			if published.Load() != 0 {
				test.Fatal("rolled-back delivery published events")
			}
			if count := dbfx.Count(test, `SELECT count(*) FROM decision_answer WHERE id = $1 AND woken_at IS NOT NULL`, recorded.Answer.ID); count != 0 {
				test.Fatal("rollback acknowledged the answer")
			}
			if count := dbfx.Count(test, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND trigger_comment_id = $2`, fixture.issueID, recorded.Comment.ID); count != 0 {
				test.Fatal("rollback left a queued run")
			}
			if count := dbfx.Count(test, `SELECT count(*) FROM comment WHERE decision_answer_id = $1 AND decision_comment_kind = 'delivery'`, recorded.Answer.ID); count != 0 {
				test.Fatal("rollback left a delivery comment")
			}
			handler, _ = newDecisionRunnerFixture(test)
			if err := deliverDecisionForTest(handler, fixture); err != nil {
				test.Fatal(err)
			}
			if err := deliverDecisionForTest(handler, fixture); err != nil {
				test.Fatal(err)
			}
			if count := dbfx.Count(test, `SELECT count(*) FROM decision_answer WHERE id = $1 AND woken_at IS NOT NULL`, recorded.Answer.ID); count != 1 {
				test.Fatalf("restart acknowledged %d answers, want 1", count)
			}
		})
	}
}

func TestDecisionDeliverySourceFence(test *testing.T) {
	for _, order := range []string{"delete_first", "source_busy", "deliver_first"} {
		test.Run(order, func(test *testing.T) {
			fixture, recorded := readyDecisionDelivery(test)
			handler, _ := newDecisionRunnerFixture(test)
			if order == "delete_first" {
				dbfx.Exec(test, `DELETE FROM chat_message WHERE id = $1`, fixture.messageID)
			} else if order == "source_busy" {
				transaction, err := testPool.Begin(context.Background())
				if err != nil {
					test.Fatal(err)
				}
				defer transaction.Rollback(context.Background())
				if _, err := transaction.Exec(context.Background(), `DELETE FROM chat_message WHERE id = $1`, fixture.messageID); err != nil {
					test.Fatal(err)
				}
				_ = deliverDecisionForTest(handler, fixture)
				if count := dbfx.Count(test, `SELECT count(*) FROM decision_answer WHERE id = $1 AND woken_at IS NOT NULL`, recorded.Answer.ID); count != 0 {
					test.Fatal("busy source was acknowledged")
				}
				if err := transaction.Rollback(context.Background()); err != nil {
					test.Fatal(err)
				}
			}
			if err := deliverDecisionForTest(handler, fixture); err != nil {
				test.Fatal(err)
			}
			want := 1
			if order == "delete_first" {
				want = 0
			}
			if count := dbfx.Count(test, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND trigger_comment_id = $2`, fixture.issueID, recorded.Comment.ID); count != want {
				test.Fatalf("source-fenced runs=%d, want %d", count, want)
			}
		})
	}
}

type decisionDeliveryMutation struct {
	txStarter
	mutate func()
}

func (starter *decisionDeliveryMutation) Begin(ctx context.Context) (pgx.Tx, error) {
	if starter.mutate != nil {
		mutate := starter.mutate
		starter.mutate = nil
		mutate()
	}
	return starter.txStarter.Begin(ctx)
}

func TestDecisionDeliveryReassignment(test *testing.T) {
	for _, changed := range []string{"owner", "runtime"} {
		test.Run(changed, func(test *testing.T) {
			fixture, recorded := readyDecisionDelivery(test)
			handler, _ := newDecisionRunnerFixture(test)
			agentID := fixture.agentID
			if changed == "owner" {
				agentID = createHandlerTestAgent(test, "New decision recipient", []byte("[]"))
			}
			runtimeID := dbfx.Insert(test, "agent_runtime", testutil.Cols{"workspace_id": testWorkspaceID, "name": "New decision runtime", "runtime_mode": "local", "provider": "test", "status": "online", "owner_id": testUserID})
			handler.TxStarter = &decisionDeliveryMutation{txStarter: testPool, mutate: func() {
				dbfx.Exec(test, `UPDATE agent SET runtime_id = $1 WHERE id = $2`, runtimeID, agentID)
				if changed == "owner" {
					dbfx.Exec(test, `UPDATE issue SET assignee_id = $1 WHERE id = $2`, agentID, fixture.issueID)
				}
			}}
			if err := deliverDecisionForTest(handler, fixture); err != nil {
				test.Fatal(err)
			}
			if count := dbfx.Count(test, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND trigger_comment_id = $2 AND agent_id = $3 AND runtime_id = $4`, fixture.issueID, recorded.Comment.ID, agentID, runtimeID); count != 1 {
				test.Fatalf("delivery to current owner/runtime=%d, want 1", count)
			}
		})
	}
}

func TestDecisionDeliveryCompleteGroupAndReanswer(test *testing.T) {
	fixture := newRequestCoreFixture(test, true)
	var records []decisionRecordResult
	var members []askedRecordFixture
	for _, humanID := range []string{fixture.humanID, testUserID} {
		person := fixture
		person.sessionID = dbfx.Insert(test, "chat_session", testutil.Cols{"workspace_id": testWorkspaceID, "agent_id": fixture.agentID, "creator_id": humanID, "title": "Group answer"})
		request, err := testHandler.createDecisionRequest(person.request(), fixture.issue, person.input())
		if err != nil {
			test.Fatal(err)
		}
		messageID := dbfx.Insert(test, "chat_message", testutil.Cols{"chat_session_id": person.sessionID, "role": "user", "content": "Group answer", "sent_via": "interactive_jwt"})
		taskID := dbfx.Task(test, fixture.agentID, testutil.Cols{"chat_session_id": person.sessionID, "status": "running", "runtime_id": handlerTestRuntimeID(test)})
		dbfx.Exec(test, `UPDATE agent_task_queue SET started_at = clock_timestamp() WHERE id = $1`, taskID)
		member := askedRecordFixture{person, request.Request, uuidToString(request.Comment.ID), messageID, taskID}
		recorded, err := testHandler.createAskedDecisionRecord(member.recordRequest(), fixture.issue, member.recordInput())
		if err != nil {
			test.Fatal(err)
		}
		records = append(records, recorded)
		members = append(members, member)
		dbfx.Exec(test, `UPDATE agent_task_queue SET status = 'completed' WHERE id = $1`, taskID)
		dbfx.Exec(test, `UPDATE decision_answer SET wake_at = clock_timestamp() WHERE id = $1`, recorded.Answer.ID)
	}
	dbfx.Exec(test, `UPDATE agent_task_queue SET status = 'completed' WHERE id = $1`, fixture.taskID)
	dbfx.Exec(test, `UPDATE decision_intent SET sealed_at = clock_timestamp() WHERE id = $1`, fixture.intentID)
	handler, _ := newDecisionRunnerFixture(test)
	if err := deliverDecisionForTest(handler, members[0]); err != nil {
		test.Fatal(err)
	}
	if count := dbfx.Count(test, `SELECT count(*) FROM decision_answer WHERE decision_request_id IN ($1, $2) AND woken_at IS NOT NULL`, records[0].Request.ID, records[1].Request.ID); count != 2 {
		test.Fatalf("complete group acknowledged %d answers, want 2", count)
	}
	for _, record := range records {
		if count := dbfx.Count(test, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND status = 'queued' AND (trigger_comment_id = $2 OR $2 = ANY(coalesced_comment_ids))`, fixture.issueID, record.Comment.ID); count != 1 {
			test.Fatalf("group quote not in the single run: %d", count)
		}
		if count := dbfx.Count(test, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND handoff_note LIKE $2`, fixture.issueID, "%"+uuidToString(record.Comment.ID)+"%"); count != 1 {
			test.Fatalf("group prompt omitted quote %s", uuidToString(record.Comment.ID))
		}
	}
	dbfx.Exec(test, `UPDATE decision_answer SET disclaimed_at = clock_timestamp() WHERE id = $1`, records[0].Answer.ID)
	dbfx.Exec(test, `UPDATE decision_request SET status = 'open' WHERE id = $1`, records[0].Request.ID)
	member := members[0]
	member.chatTaskID = dbfx.Task(test, fixture.agentID, testutil.Cols{"chat_session_id": member.sessionID, "status": "running", "runtime_id": handlerTestRuntimeID(test)})
	dbfx.Exec(test, `UPDATE agent_task_queue SET started_at = clock_timestamp() WHERE id = $1`, member.chatTaskID)
	reanswer, err := testHandler.createAskedDecisionRecord(member.recordRequest(), fixture.issue, member.recordInput())
	if err != nil {
		test.Fatal(err)
	}
	dbfx.Exec(test, `UPDATE agent_task_queue SET status = 'completed' WHERE id = $1`, member.chatTaskID)
	dbfx.Exec(test, `UPDATE decision_answer SET wake_at = clock_timestamp() WHERE id = $1`, reanswer.Answer.ID)
	if err := deliverDecisionForTest(handler, member); err != nil {
		test.Fatal(err)
	}
	if count := dbfx.Count(test, `SELECT count(*) FROM decision_answer WHERE decision_request_id IN ($1, $2) AND woken_at IS NOT NULL`, records[0].Request.ID, records[1].Request.ID); count != 3 {
		test.Fatalf("individual re-answer receipts=%d, want 3", count)
	}
}

type decisionDeliveryBarrier struct {
	txStarter
	ready   chan int
	release <-chan struct{}
}

type decisionDeliveryBarrierTx struct {
	pgx.Tx
	starter *decisionDeliveryBarrier
}

func (starter *decisionDeliveryBarrier) Begin(ctx context.Context) (pgx.Tx, error) {
	transaction, err := starter.txStarter.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return decisionDeliveryBarrierTx{transaction, starter}, nil
}

func (transaction decisionDeliveryBarrierTx) Commit(ctx context.Context) error {
	var backendPID int
	if err := transaction.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&backendPID); err != nil {
		return err
	}
	transaction.starter.ready <- backendPID
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-transaction.starter.release:
		return transaction.Tx.Commit(ctx)
	}
}

type decisionDeliveryTransaction struct {
	name string
	pid  int
}

type decisionDeliveryScheduleStarter struct {
	txStarter
	name       string
	started    chan<- decisionDeliveryTransaction
	committing chan<- decisionDeliveryTransaction
	release    <-chan struct{}
}

type decisionDeliveryScheduleTx struct {
	pgx.Tx
	starter decisionDeliveryScheduleStarter
	pid     int
}

func (starter decisionDeliveryScheduleStarter) Begin(ctx context.Context) (pgx.Tx, error) {
	transaction, err := starter.txStarter.Begin(ctx)
	if err != nil {
		return nil, err
	}
	var pid int
	if err := transaction.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		_ = transaction.Rollback(ctx)
		return nil, err
	}
	if starter.started != nil {
		select {
		case starter.started <- decisionDeliveryTransaction{name: starter.name, pid: pid}:
		default:
		}
	}
	return decisionDeliveryScheduleTx{Tx: transaction, starter: starter, pid: pid}, nil
}

func (transaction decisionDeliveryScheduleTx) Commit(ctx context.Context) error {
	if transaction.starter.committing != nil {
		select {
		case transaction.starter.committing <- decisionDeliveryTransaction{name: transaction.starter.name, pid: transaction.pid}:
		default:
		}
	}
	if transaction.starter.release != nil {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-transaction.starter.release:
		}
	}
	return transaction.Tx.Commit(ctx)
}

func (transaction decisionDeliveryScheduleTx) Rollback(context.Context) error {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return transaction.Tx.Rollback(ctx)
}

func awaitDecisionDeliveryEvent(test *testing.T, events <-chan decisionDeliveryTransaction, description string) decisionDeliveryTransaction {
	test.Helper()
	timer := time.NewTimer(8 * time.Second)
	defer timer.Stop()
	select {
	case event := <-events:
		return event
	case <-timer.C:
		test.Fatalf("timed out waiting for %s", description)
		return decisionDeliveryTransaction{}
	}
}

// waitForDecisionWaiterBlockedBy polls until waiterPID waits on a lock held by
// blockerPID. waiterPID 0 matches any backend: use it when the handler under
// test opens other transactions through the same TxStarter first (the started
// event then names one of those, not the one that takes the lock).
func waitForDecisionWaiterBlockedBy(test *testing.T, waiterPID, blockerPID int, timeout time.Duration) bool {
	test.Helper()
	deadline := time.Now().Add(timeout)
	for {
		var blocked bool
		if err := testPool.QueryRow(context.Background(), `
			SELECT EXISTS (
				SELECT 1 FROM pg_stat_activity
				WHERE datname = current_database()
				  AND ($1::int = 0 OR pid = $1)
				  AND state = 'active'
				  AND wait_event_type = 'Lock'
				  AND wait_event IN ('transactionid', 'tuple', 'advisory')
				  AND $2::int = ANY(pg_blocking_pids(pid))
			)
		`, waiterPID, blockerPID).Scan(&blocked); err != nil {
			test.Fatalf("probe decision delivery wait from pid %d to pid %d: %v", waiterPID, blockerPID, err)
		}
		if blocked {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestDecisionDeliveryGroupSourceFenceConcurrent(test *testing.T) {
	for _, order := range []string{"delete_first", "delivery_first"} {
		test.Run(order, func(test *testing.T) {
			group := readyDecisionDeliveryGroup(test)
			handler, _ := newDecisionRunnerFixture(test)
			requestIDs := []pgtype.UUID{group.records[0].Request.ID, group.records[1].Request.ID}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if order == "delete_first" {
				deleteTx, err := testPool.Begin(ctx)
				if err != nil {
					test.Fatal(err)
				}
				defer deleteTx.Rollback(context.Background())
				if _, err := deleteTx.Exec(ctx, `DELETE FROM chat_message WHERE id = $1`, group.members[0].messageID); err != nil {
					test.Fatal(err)
				}
				blockedDelivery := make(chan error, 1)
				go func() {
					blockedDelivery <- handler.DeliverDecisionWake(ctx, group.core.issue.WorkspaceID, group.members[0].decision.ID)
				}()
				select {
				case err := <-blockedDelivery:
					var postgresError *pgconn.PgError
					if !errors.As(err, &postgresError) || postgresError.Code != "55P03" {
						test.Fatalf("delivery while source delete was uncommitted returned %v, want NOWAIT 55P03", err)
					}
				case <-ctx.Done():
					test.Fatal("delivery did not reach the held source fence before deadline")
				}
				if err := deleteTx.Commit(ctx); err != nil {
					test.Fatal(err)
				}
				if err := handler.DeliverDecisionWake(ctx, group.core.issue.WorkspaceID, group.members[0].decision.ID); err != nil {
					test.Fatal(err)
				}
				if count := dbfx.Count(test, `SELECT count(*) FROM decision_answer WHERE decision_request_id = ANY($1::uuid[]) AND woken_at IS NOT NULL`, requestIDs); count != 0 {
					test.Fatalf("delete-first partially acknowledged %d answers", count)
				}
				if count := dbfx.Count(test, `SELECT count(*) FROM decision_intent WHERE id = $1 AND group_woken_at IS NOT NULL`, group.core.intentID); count != 0 {
					test.Fatal("delete-first recorded a group wake")
				}
				if count := dbfx.Count(test, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND trigger_comment_id = ANY($2::uuid[])`, group.core.issueID, []pgtype.UUID{group.records[0].Comment.ID, group.records[1].Comment.ID}); count != 0 {
					test.Fatalf("delete-first delivered %d stale group sources", count)
				}
				return
			}

			release := make(chan struct{})
			var releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			defer unblock()
			committing := make(chan decisionDeliveryTransaction, 1)
			handler.TxStarter = decisionDeliveryScheduleStarter{txStarter: testPool, name: "delivery", committing: committing, release: release}
			deliveryDone := make(chan error, 1)
			go func() {
				deliveryDone <- handler.DeliverDecisionWake(ctx, group.core.issue.WorkspaceID, group.members[0].decision.ID)
			}()
			delivery := awaitDecisionDeliveryEvent(test, committing, "group delivery commit barrier")
			deleteDone := make(chan error, 1)
			go func() {
				_, err := testPool.Exec(ctx, `DELETE FROM chat_message WHERE id = $1`, group.members[0].messageID)
				deleteDone <- err
			}()
			if !waitForWaiterBlockedBy(test, delivery.pid, 3*time.Second) {
				test.Fatal("source delete did not wait behind the two-answer group delivery")
			}
			unblock()
			if err := <-deliveryDone; err != nil {
				test.Fatal(err)
			}
			if err := <-deleteDone; err != nil {
				test.Fatal(err)
			}
			if count := dbfx.Count(test, `SELECT count(*) FROM decision_answer WHERE decision_request_id = ANY($1::uuid[]) AND wake_outcome = 'run_enqueued' AND woken_at IS NOT NULL`, requestIDs); count != 2 {
				test.Fatalf("delivery-first acknowledged %d of 2 answers", count)
			}
			if count := dbfx.Count(test, `SELECT count(*) FROM decision_intent WHERE id = $1 AND group_woken_at IS NOT NULL`, group.core.intentID); count != 1 {
				test.Fatalf("delivery-first group wake receipts=%d, want 1", count)
			}
			for _, record := range group.records {
				if count := dbfx.Count(test, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND status = 'queued' AND (trigger_comment_id = $2 OR $2 = ANY(coalesced_comment_ids)) AND handoff_note LIKE $3`, group.core.issueID, record.Comment.ID, "%"+uuidToString(record.Comment.ID)+"%"); count != 1 {
					test.Fatalf("delivered group contains source %s %d times", uuidToString(record.Comment.ID), count)
				}
			}
			if err := handler.DeliverDecisionWake(ctx, group.core.issue.WorkspaceID, group.members[1].decision.ID); err != nil {
				test.Fatal(err)
			}
			if count := dbfx.Count(test, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND status = 'queued'`, group.core.issueID); count != 1 {
				test.Fatalf("replayed group created %d wakes, want 1", count)
			}
			withdrawal, err := handler.CheckDecisionSource(ctx, group.core.issue.WorkspaceID, group.members[0].decision.ID)
			if err != nil {
				test.Fatal(err)
			}
			if len(withdrawal.Comments) != 1 || withdrawal.Answer.ID != group.records[0].Answer.ID {
				test.Fatalf("deleted delivered source was not retracted once: %+v", withdrawal)
			}
		})
	}
}

func TestDecisionDeliveryLeaseTakeoverConcurrentGroup(test *testing.T) {
	resetDecisionLease(test)
	group := readyDecisionDeliveryGroup(test)
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	started := make(chan decisionDeliveryTransaction, 2)
	committing := make(chan decisionDeliveryTransaction, 2)
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	firstHandler, first := newDecisionRunnerFixture(test)
	firstHandler.TxStarter = decisionDeliveryScheduleStarter{txStarter: testPool, name: "old", started: started, committing: committing, release: release}
	type passResult struct {
		acquired bool
		err      error
	}
	firstDone := make(chan passResult, 1)
	go func() {
		acquired, err := first.ProcessPass(ctx)
		firstDone <- passResult{acquired: acquired, err: err}
	}()
	oldStart := awaitDecisionDeliveryEvent(test, started, "old worker transaction start")
	if oldStart.name != "old" {
		test.Fatalf("unexpected transaction start %q, want old worker", oldStart.name)
	}
	oldTransaction := awaitDecisionDeliveryEvent(test, committing, "old worker delivery commit barrier")
	if _, err := testPool.Exec(ctx, `UPDATE decision_worker_lease SET expires_at = clock_timestamp() - interval '1 second'`); err != nil {
		test.Fatal(err)
	}
	secondHandler, second := newDecisionRunnerFixture(test)
	secondHandler.TxStarter = decisionDeliveryScheduleStarter{txStarter: testPool, name: "new", started: started}
	secondDone := make(chan passResult, 1)
	go func() {
		acquired, err := second.ProcessPass(ctx)
		secondDone <- passResult{acquired: acquired, err: err}
	}()
	var newTransaction decisionDeliveryTransaction
	select {
	case newTransaction = <-started:
	case result := <-secondDone:
		test.Fatalf("new worker pass ended before delivery transaction: acquired=%v err=%v", result.acquired, result.err)
	case <-ctx.Done():
		test.Fatalf("new worker did not start its delivery transaction: %v", ctx.Err())
	}
	if newTransaction.name != "new" {
		test.Fatalf("unexpected transaction start %q, want new worker", newTransaction.name)
	}
	if !waitForDecisionWaiterBlockedBy(test, newTransaction.pid, oldTransaction.pid, 4*time.Second) {
		test.Fatalf("new worker pid %d did not overlap and wait at the old worker transaction boundary pid %d", newTransaction.pid, oldTransaction.pid)
	}
	unblock()
	if result := <-firstDone; !result.acquired || result.err != nil {
		test.Fatalf("old worker pass: acquired=%v err=%v", result.acquired, result.err)
	}
	if result := <-secondDone; !result.acquired || result.err != nil {
		test.Fatalf("new worker pass after takeover: acquired=%v err=%v", result.acquired, result.err)
	}
	if count := dbfx.Count(test, `SELECT count(*) FROM decision_intent WHERE id = $1 AND group_woken_at IS NOT NULL`, group.core.intentID); count != 1 {
		test.Fatalf("takeover group wake receipts=%d, want 1", count)
	}
	if count := dbfx.Count(test, `SELECT count(*) FROM decision_answer WHERE decision_request_id = ANY($1::uuid[]) AND wake_outcome = 'run_enqueued' AND woken_at IS NOT NULL`, []pgtype.UUID{group.records[0].Request.ID, group.records[1].Request.ID}); count != 2 {
		test.Fatalf("takeover durably acknowledged %d of 2 answers", count)
	}
	if count := dbfx.Count(test, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND status = 'queued'`, group.core.issueID); count != 1 {
		test.Fatalf("takeover created %d group wakes, want 1", count)
	}
	for _, record := range group.records {
		if count := dbfx.Count(test, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND status = 'queued' AND (trigger_comment_id = $2 OR $2 = ANY(coalesced_comment_ids)) AND handoff_note LIKE $3`, group.core.issueID, record.Comment.ID, "%"+uuidToString(record.Comment.ID)+"%"); count != 1 {
			test.Fatalf("takeover lost or duplicated answer source %s: %d", uuidToString(record.Comment.ID), count)
		}
	}
}

func TestDecisionDeliveryCrossDMMemberLockOrderConcurrent(test *testing.T) {
	core := newRequestCoreFixture(test, false)
	firstRequest, err := testHandler.createDecisionRequest(core.request(), core.issue, core.input())
	if err != nil {
		test.Fatal(err)
	}
	dbfx.Exec(test, `UPDATE agent_task_queue SET status = 'completed' WHERE id = $1`, core.taskID)
	secondTaskID := dbfx.Task(test, core.agentID, testutil.Cols{"issue_id": core.issueID, "status": "running", "runtime_id": handlerTestRuntimeID(test)})
	secondTag := core.intentFixture.post(test, "task_token", secondTaskID, "[@Owner](mention://member/"+testUserID+")", nil)
	var secondTagComment CommentResponse
	secondTag.JSON(&secondTagComment)
	var secondIntentID string
	dbfx.QueryRow(test, `SELECT decision_intent_id FROM comment WHERE id = $1`, secondTagComment.ID).Scan(&secondIntentID)
	secondPerson := core
	secondPerson.taskID, secondPerson.intentID, secondPerson.tagID = secondTaskID, secondIntentID, secondTagComment.ID
	secondPerson.humanID = testUserID
	secondPerson.sessionID = dbfx.Insert(test, "chat_session", testutil.Cols{"workspace_id": testWorkspaceID, "agent_id": core.agentID, "creator_id": testUserID, "title": "Second decision DM"})
	secondRequestContext := laneRequest(core.intentFixture, "")
	secondRequestContext.Header.Set("X-Task-ID", secondTaskID)
	secondRequest, err := testHandler.createDecisionRequest(secondRequestContext, core.issue, secondPerson.input())
	if err != nil {
		test.Fatal(err)
	}
	dbfx.Exec(test, `UPDATE agent_task_queue SET status = 'completed' WHERE id = $1`, secondTaskID)
	makeAnswer := func(person requestCoreFixture, request db.DecisionRequest, askComment db.Comment) askedRecordFixture {
		test.Helper()
		messageID := dbfx.Insert(test, "chat_message", testutil.Cols{"chat_session_id": person.sessionID, "role": "user", "content": "One decision from " + person.humanID, "sent_via": "interactive_jwt"})
		chatTaskID := dbfx.Task(test, core.agentID, testutil.Cols{"chat_session_id": person.sessionID, "status": "running", "runtime_id": handlerTestRuntimeID(test)})
		dbfx.Exec(test, `UPDATE agent_task_queue SET started_at = clock_timestamp() WHERE id = $1`, chatTaskID)
		member := askedRecordFixture{requestCoreFixture: person, decision: request, parentID: uuidToString(askComment.ID), messageID: messageID, chatTaskID: chatTaskID}
		recorded, err := testHandler.createAskedDecisionRecord(member.recordRequest(), core.issue, member.recordInput())
		if err != nil {
			test.Fatal(err)
		}
		dbfx.Exec(test, `UPDATE agent_task_queue SET status = 'completed' WHERE id = $1`, chatTaskID)
		dbfx.Exec(test, `UPDATE decision_answer SET wake_at = clock_timestamp() WHERE id = $1`, recorded.Answer.ID)
		return member
	}
	first := makeAnswer(core, firstRequest.Request, firstRequest.Comment)
	second := makeAnswer(secondPerson, secondRequest.Request, secondRequest.Comment)
	dbfx.Exec(test, `UPDATE issue SET assignee_type = 'member', assignee_id = $1 WHERE id = $2`, testUserID, core.issueID)
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	started := make(chan decisionDeliveryTransaction, 2)
	committing := make(chan decisionDeliveryTransaction, 2)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	firstHandler, _ := newDecisionRunnerFixture(test)
	secondHandler, _ := newDecisionRunnerFixture(test)
	firstHandler.TxStarter = decisionDeliveryScheduleStarter{txStarter: testPool, name: "first-dm", started: started, committing: committing, release: release}
	secondHandler.TxStarter = decisionDeliveryScheduleStarter{txStarter: testPool, name: "second-dm", started: started, committing: committing, release: release}
	start := make(chan struct{})
	done := make(chan error, 2)
	for _, attempt := range []struct {
		handler *Handler
		member  askedRecordFixture
	}{{firstHandler, first}, {secondHandler, second}} {
		go func(handler *Handler, member askedRecordFixture) {
			<-start
			done <- handler.DeliverDecisionWake(ctx, member.issue.WorkspaceID, member.decision.ID)
		}(attempt.handler, attempt.member)
	}
	close(start)
	transactions := map[string]int{}
	for range 2 {
		transaction := awaitDecisionDeliveryEvent(test, started, "concurrent cross-DM transaction start")
		transactions[transaction.name] = transaction.pid
	}
	winner := awaitDecisionDeliveryEvent(test, committing, "first cross-DM commit barrier")
	var waiterName string
	for name, pid := range transactions {
		if pid != winner.pid {
			waiterName = name
			if !waitForDecisionWaiterBlockedBy(test, pid, winner.pid, 4*time.Second) {
				test.Fatalf("%s delivery pid %d did not overlap at the shared issue lock held by pid %d", name, pid, winner.pid)
			}
			break
		}
	}
	unblock()
	var failures []error
	for range 2 {
		if err := <-done; err != nil {
			failures = append(failures, err)
		}
	}
	for _, err := range failures {
		var postgresError *pgconn.PgError
		if errors.As(err, &postgresError) && postgresError.Code == "40P01" {
			test.Fatalf("cross-DM member delivery deadlocked with PostgreSQL 40P01: %v", err)
		}
		test.Errorf("cross-DM delivery failed: %v", err)
	}
	if waiterName == "" {
		test.Fatal("the cross-DM issue-lock waiter was not observed")
	}
	for _, member := range []askedRecordFixture{first, second} {
		if count := dbfx.Count(test, `SELECT count(*) FROM decision_answer WHERE id IN (SELECT id FROM decision_answer WHERE decision_request_id = $1) AND wake_outcome = 'comment_member' AND woken_at IS NOT NULL`, member.decision.ID); count != 1 {
			test.Fatalf("member delivery for request %s acknowledged %d times", uuidToString(member.decision.ID), count)
		}
		if count := dbfx.Count(test, `SELECT count(*) FROM comment WHERE decision_answer_id = (SELECT id FROM decision_answer WHERE decision_request_id = $1) AND decision_comment_kind = 'delivery'`, member.decision.ID); count != 1 {
			test.Fatalf("member delivery for request %s wrote %d comments", uuidToString(member.decision.ID), count)
		}
	}
}

func TestDecisionDeliveryBlocksSourceDeleteThroughCommit(test *testing.T) {
	fixture, recorded := readyDecisionDelivery(test)
	handler, _ := newDecisionRunnerFixture(test)
	release := make(chan struct{})
	defer close(release)
	starter := &decisionDeliveryBarrier{txStarter: testPool, ready: make(chan int, 1), release: release}
	handler.TxStarter = starter
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	completed := make(chan error, 1)
	go func() { completed <- handler.DeliverDecisionWake(ctx, fixture.issue.WorkspaceID, fixture.decision.ID) }()
	var backendPID int
	select {
	case backendPID = <-starter.ready:
	case err := <-completed:
		test.Fatalf("delivery ended before source fence: %v", err)
	case <-ctx.Done():
		test.Fatal(ctx.Err())
	}
	deleted := make(chan error, 1)
	go func() {
		_, err := testPool.Exec(ctx, `DELETE FROM chat_message WHERE id = $1`, fixture.messageID)
		deleted <- err
	}()
	if !waitForWaiterBlockedBy(test, backendPID, 3*time.Second) {
		test.Fatal("source delete bypassed the delivery transaction")
	}
	release <- struct{}{}
	if err := <-completed; err != nil {
		test.Fatal(err)
	}
	if err := <-deleted; err != nil {
		test.Fatal(err)
	}
	if count := dbfx.Count(test, `SELECT count(*) FROM decision_answer WHERE id = $1 AND wake_outcome = 'run_enqueued'`, recorded.Answer.ID); count != 1 {
		test.Fatal("source-fenced delivery did not commit its receipt")
	}
}

func TestDecisionDeliveryPreservesAttribution(test *testing.T) {
	for _, queued := range []bool{false, true} {
		test.Run(map[bool]string{false: "enqueue", true: "merge"}[queued], func(test *testing.T) {
			fixture, recorded := readyDecisionDelivery(test)
			dbfx.Exec(test, `UPDATE agent_task_queue SET originator_user_id = $1, accountable_user_id = $1, originator_source = 'direct_human' WHERE id = $2`, fixture.humanID, fixture.chatTaskID)
			if queued {
				dbfx.Task(test, fixture.agentID, testutil.Cols{"issue_id": fixture.issueID, "status": "queued", "runtime_id": handlerTestRuntimeID(test), "originator_user_id": testUserID, "accountable_user_id": testUserID})
			}
			handler, _ := newDecisionRunnerFixture(test)
			if err := deliverDecisionForTest(handler, fixture); err != nil {
				test.Fatal(err)
			}
			var originator, accountable, source string
			dbfx.QueryRow(test, `SELECT originator_user_id::text, accountable_user_id::text, originator_source FROM agent_task_queue WHERE issue_id = $1 AND trigger_comment_id = $2`, fixture.issueID, recorded.Comment.ID).Scan(&originator, &accountable, &source)
			if originator != fixture.humanID || accountable != fixture.humanID || source != "comment_source" {
				test.Fatalf("decision attribution=%s/%s/%s, want asking turn's human", originator, accountable, source)
			}
		})
	}
}

func TestDecisionDeliveryGroupReadiness(test *testing.T) {
	for _, hold := range []string{"unsealed", "held", "open", "reopened", "complete", "unasked_target", "cancelled"} {
		test.Run(hold, func(test *testing.T) {
			resetDecisionLease(test)
			fixture := newAskedRecordFixture(test)
			recorded, err := testHandler.createAskedDecisionRecord(fixture.recordRequest(), fixture.issue, fixture.recordInput())
			if err != nil {
				test.Fatal(err)
			}
			dbfx.Exec(test, `UPDATE agent_task_queue SET status = 'completed' WHERE id = $1`, fixture.chatTaskID)
			if hold != "unsealed" {
				dbfx.Exec(test, `UPDATE agent_task_queue SET status = 'completed' WHERE id = $1`, fixture.taskID)
			} else {
				dbfx.Exec(test, `INSERT INTO decision_intent_target (decision_intent_id, human_user_id) VALUES ($1, $2)`, fixture.intentID, testUserID)
				dbfx.Exec(test, `UPDATE decision_intent SET sealed_at = NULL, consumed_at = NULL WHERE id = $1`, fixture.intentID)
				dbfx.Exec(test, `UPDATE agent_task_queue SET comment_thread_id = $1 WHERE id = $2`, fixture.tagID, fixture.taskID)
			}
			if hold == "held" {
				dbfx.Exec(test, `UPDATE decision_answer SET effective_at = clock_timestamp() + interval '10 minutes' WHERE id = $1`, recorded.Answer.ID)
			}
			if hold == "open" || hold == "reopened" || hold == "cancelled" {
				state := "open"
				if hold == "cancelled" {
					state = "cancelled"
				}
				dbfx.Insert(test, "decision_request", testutil.Cols{"workspace_id": testWorkspaceID, "issue_id": fixture.issueID, "asking_agent_id": fixture.agentID, "human_user_id": testUserID, "chat_session_id": fixture.sessionID, "issue_comment_id": fixture.tagID, "ask": "Second person", "escalation_class": "reversible", "status": state, "origin": "asked", "decision_intent_id": fixture.intentID})
			}
			if hold == "unasked_target" {
				dbfx.Exec(test, `INSERT INTO decision_intent_target (decision_intent_id, human_user_id) VALUES ($1, $2)`, fixture.intentID, testUserID)
			}
			_, runner := newDecisionRunnerFixture(test)
			decisionPass(test, runner)
			want := 0
			if hold == "complete" || hold == "unasked_target" || hold == "cancelled" {
				want = 1
			}
			if count := dbfx.Count(test, `SELECT count(*) FROM decision_answer WHERE id = $1 AND woken_at IS NOT NULL`, recorded.Answer.ID); count != want {
				test.Fatalf("group acknowledged answers=%d, want %d", count, want)
			}
			if count := dbfx.Count(test, `SELECT count(*) FROM decision_intent WHERE id = $1 AND group_woken_at IS NOT NULL`, fixture.intentID); count != want {
				test.Fatalf("group receipts=%d, want %d", count, want)
			}
			if hold == "unasked_target" || hold == "cancelled" {
				label := "%Target not asked: user " + testUserID + "%"
				if hold == "cancelled" {
					label = "%Cancelled request %"
				}
				if count := dbfx.Count(test, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND status = 'queued' AND handoff_note LIKE $2`, fixture.issueID, label); count != 1 {
					test.Fatal("group prompt omitted an unasked target or cancelled request")
				}
			}
		})
	}
}

func TestDecisionDeliveryGroupIgnoresCancelledNonmember(test *testing.T) {
	group := readyDecisionDeliveryGroup(test)
	cancelled := group.members[0]
	remaining := group.members[1]
	dbfx.Exec(test, `DELETE FROM member WHERE workspace_id = $1 AND user_id = $2`, testWorkspaceID, group.core.humanID)
	handler, _ := newDecisionRunnerFixture(test)
	if err := cancelDecisionForTest(handler, cancelled); err != nil {
		test.Fatal(err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		if err := deliverDecisionForTest(handler, remaining); err != nil {
			test.Fatal(err)
		}
	}
	answer, err := handler.Queries.GetDecisionAnswer(context.Background(), db.GetDecisionAnswerParams{WorkspaceID: remaining.issue.WorkspaceID, ID: group.records[1].Answer.ID})
	if err != nil {
		test.Fatal(err)
	}
	if !answer.WokenAt.Valid || answer.WakeOutcome.String != "run_enqueued" {
		test.Fatalf("remaining answer woken=%v outcome=%q, want run_enqueued", answer.WokenAt.Valid, answer.WakeOutcome.String)
	}
	if count := dbfx.Count(test, `SELECT count(*) FROM decision_intent WHERE id = $1 AND group_woken_at IS NOT NULL`, group.core.intentID); count != 1 {
		test.Fatalf("group receipts=%d, want 1", count)
	}
	if count := dbfx.Count(test, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND status = 'queued'`, group.core.issueID); count != 1 {
		test.Fatalf("group wakes=%d, want 1", count)
	}
	if count := dbfx.Count(test, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND status = 'queued' AND handoff_note LIKE '%Cancelled request %'`, group.core.issueID); count != 1 {
		test.Fatal("group prompt omitted the cancelled request")
	}
	if count := dbfx.Count(test, `SELECT count(*) FROM decision_answer WHERE id = $1 AND wake_outcome = 'skipped_ineligible'`, group.records[0].Answer.ID); count != 1 {
		test.Fatal("cancelled person's answer lost its skipped outcome")
	}
}

func TestDecisionDeliveryUnprompted(test *testing.T) {
	fixture, recorded := readyDecisionDelivery(test)
	dbfx.Exec(test, `UPDATE decision_request SET origin = 'unprompted', decision_intent_id = NULL, ping_message_id = NULL, issue_comment_id = $1 WHERE id = $2`, recorded.Comment.ID, fixture.decision.ID)
	dbfx.Exec(test, `UPDATE decision_intent SET sealed_at = NULL WHERE id = $1`, fixture.intentID)
	handler, _ := newDecisionRunnerFixture(test)
	for attempt := 0; attempt < 2; attempt++ {
		if err := deliverDecisionForTest(handler, fixture); err != nil {
			test.Fatal(err)
		}
	}
	if count := dbfx.Count(test, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND trigger_comment_id = $2`, fixture.issueID, recorded.Comment.ID); count != 1 {
		test.Fatalf("unprompted runs=%d, want 1", count)
	}
	if count := dbfx.Count(test, `SELECT count(*) FROM decision_intent WHERE id = $1 AND group_woken_at IS NOT NULL`, fixture.intentID); count != 0 {
		test.Fatal("unprompted delivery acknowledged an unrelated intent")
	}
}

func TestDecisionDeliveryCandidatesDoNotStarveCompleteGroups(test *testing.T) {
	blocked, _ := readyDecisionDelivery(test)
	dbfx.Exec(test, `UPDATE agent SET name = $1 WHERE id = $2`, "blocked-"+blocked.agentID, blocked.agentID)
	dbfx.Insert(test, "decision_request", testutil.Cols{"workspace_id": testWorkspaceID, "issue_id": blocked.issueID, "asking_agent_id": blocked.agentID, "human_user_id": testUserID, "chat_session_id": blocked.sessionID, "issue_comment_id": blocked.tagID, "ask": "Still open", "escalation_class": "reversible", "status": "open", "origin": "asked", "decision_intent_id": blocked.intentID})
	ready, _ := readyDecisionDelivery(test)
	candidates, err := testHandler.Queries.ListDecisionWakeCandidates(context.Background(), 1)
	if err != nil {
		test.Fatal(err)
	}
	if len(candidates) != 1 || candidates[0].ID != ready.decision.ID {
		test.Fatalf("old incomplete group starved the ready group: %+v", candidates)
	}
}
