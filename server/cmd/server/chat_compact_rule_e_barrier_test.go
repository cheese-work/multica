package main

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/auth"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Controlled lock barriers for Rule E (CHE-2819). A BEFORE trigger parks one
// real statement on a test-held advisory lock while its transaction holds the
// row locks under test, so the interleaving is forced, not left to scheduling.

// ruleEPause parks the first matching statement on `table` until the returned
// release func runs. when is a trigger WHEN condition.
func ruleEPause(t *testing.T, event, table, when string) (key int64, release func()) {
	t.Helper()
	key = time.Now().UnixNano()%1_000_000_000 + 1_000_000
	fn := fmt.Sprintf("rule_e_pause_%d", key)
	for _, ddl := range []string{
		fmt.Sprintf(`CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN PERFORM pg_advisory_xact_lock(%d); RETURN NEW; END $$`, fn, key),
		fmt.Sprintf(`CREATE TRIGGER %s BEFORE %s ON %s FOR EACH ROW WHEN (%s) EXECUTE FUNCTION %s()`, fn, event, table, when, fn),
	} {
		if _, err := testPool.Exec(ctx(), ddl); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		testPool.Exec(ctx(), fmt.Sprintf(`DROP TRIGGER IF EXISTS %s ON %s`, fn, table))
		testPool.Exec(ctx(), fmt.Sprintf(`DROP FUNCTION IF EXISTS %s()`, fn))
	})
	conn, err := testPool.Acquire(ctx())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx(), fmt.Sprintf(`SELECT pg_advisory_lock(%d)`, key)); err != nil {
		t.Fatal(err)
	}
	released := false
	release = func() {
		if released {
			return
		}
		released = true
		conn.Exec(ctx(), fmt.Sprintf(`SELECT pg_advisory_unlock(%d)`, key))
		conn.Release()
	}
	t.Cleanup(release)
	return key, release
}

func ruleEPollCount(t *testing.T, query string) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var n int
		if err := testPool.QueryRow(ctx(), query).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n > 0 || time.Now().After(deadline) {
			return n
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// ruleEWaitParked waits until a statement is blocked on the pause lock.
func ruleEWaitParked(t *testing.T, key int64) {
	t.Helper()
	q := fmt.Sprintf(`SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND NOT granted AND objid = %d::oid`, key)
	if ruleEPollCount(t, q) == 0 {
		t.Fatal("no statement parked on the pause lock")
	}
}

// ruleEWaitOnDM waits until a statement is blocked on a chat_session row lock.
func ruleEWaitOnDM(t *testing.T) {
	t.Helper()
	q := `SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock'
		AND wait_event IN ('transactionid', 'tuple') AND query ILIKE '%chat_session%'`
	if ruleEPollCount(t, q) == 0 {
		t.Fatal("no statement blocked on a chat_session row lock")
	}
}

func ruleENewService() *service.TaskService {
	return service.NewTaskService(db.New(testPool), testPool, nil, events.New())
}

func ruleEOnlineRuntime(t *testing.T, taskID string) string {
	t.Helper()
	var runtimeID string
	if err := testPool.QueryRow(ctx(), `SELECT runtime_id FROM agent_task_queue WHERE id = $1`, taskID).Scan(&runtimeID); err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(ctx(), `UPDATE agent_runtime SET status = 'online', last_seen_at = now() WHERE id = $1`, runtimeID); err != nil {
		t.Fatal(err)
	}
	return runtimeID
}

// ruleEClaimUntil claims for the runtime until the queued task is returned or the
// claim is empty. It fails when a claim takes longer than the limit: the claim
// must never wait on the DM row.
func ruleEClaimUntil(t *testing.T, svc *service.TaskService, runtimeID, queued string, limit time.Duration) bool {
	t.Helper()
	for k := 0; k < 10; k++ {
		c, cancel := context.WithTimeout(ctx(), limit)
		got, err := svc.ClaimTaskForRuntime(c, ruleEUUID(t, runtimeID))
		cancel()
		if err != nil {
			t.Errorf("claim: %v", err) // Errorf, not Fatalf: callers run this in a goroutine (CHE-4873)
			return false
		}
		if got == nil {
			return false
		}
		if util.UUIDToString(got.ID) == queued {
			return true
		}
	}
	return false
}

func ruleEQueuedState(t *testing.T, id string) (status string, stamp, dmEpoch int64) {
	t.Helper()
	var s *int64
	if err := testPool.QueryRow(ctx(), `
		SELECT atq.status, atq.chat_compaction_epoch, cs.compaction_epoch
		FROM agent_task_queue atq JOIN chat_session cs ON cs.id = atq.chat_session_id WHERE atq.id = $1`, id).Scan(&status, &s, &dmEpoch); err != nil {
		t.Fatal(err)
	}
	if stamp = -1; s != nil {
		stamp = *s
	}
	return
}

// Order 1: a compact parked while it holds the DM lock. The completion of its
// caller queues behind the DM (DM first), the claim never waits, and after the
// compact commits the completion finishes and the claim stamps the new epoch.
func TestRuleE_BarrierCompletionQueuesBehindCompactClaimDoesNot(t *testing.T) {
	svc := ruleENewService()
	fx := newRuleEFixture(t)
	queued := addRuleEChatTask(t, fx.sessionID, "queued")
	runtimeID := ruleEOnlineRuntime(t, queued)

	key, release := ruleEPause(t, "INSERT", "chat_message",
		fmt.Sprintf(`NEW.chat_session_id = '%s' AND NEW.message_kind = 'compaction_summary'`, fx.sessionID))
	compactDone := make(chan int, 1)
	go func() {
		code, _ := postCompact(t, fx.token, "x")
		compactDone <- code
	}()
	ruleEWaitParked(t, key)

	completeDone := make(chan error, 1)
	go func() {
		_, _, err := svc.CompleteTaskWithRunContext(ctx(), ruleEUUID(t, fx.taskID), []byte(`{"output":"done"}`), "", "", "", false, "", "", nil)
		completeDone <- err
	}()
	ruleEWaitOnDM(t)
	select {
	case err := <-completeDone:
		t.Fatalf("completion finished while the compact held the DM lock: %v", err)
	case <-time.After(300 * time.Millisecond):
	}

	if ruleEClaimUntil(t, svc, runtimeID, queued, 3*time.Second) {
		t.Fatal("claim returned the chat task while the compact held the DM")
	}
	if status, _, _ := ruleEQueuedState(t, queued); status != "queued" {
		t.Fatalf("queued task = %q while the compact held the DM, want queued", status)
	}

	release()
	if code := <-compactDone; code != http.StatusOK {
		t.Fatalf("compact = %d, want 200", code)
	}
	if err := <-completeDone; err != nil {
		t.Fatalf("completion after the compact: %v", err)
	}
	if !ruleEClaimUntil(t, svc, runtimeID, queued, 3*time.Second) {
		t.Fatal("claim after the compact did not return the queued task")
	}
	if status, stamp, dm := ruleEQueuedState(t, queued); status != "dispatched" || dm != 1 || stamp != 1 {
		t.Fatalf("queued task = %q at epoch %d, DM epoch %d, want dispatched at 1/1", status, stamp, dm)
	}
}

// Order 2: a claim parked mid-transaction (it holds the task and a shared DM
// lock). A compact that arrives meanwhile waits for the claim to finish.
// CHE-2910 allows one active task per DM, so the compact caller (running) and
// the parked claim (dispatching) cannot both commit: the claim loses to the
// unique index, the queued task stays queued, and the compact answers 200.
// CHE-4873: the claim goroutine used to Fatalf on that index error and never
// answered, so the test hung until the 10 minute package timeout.
func TestRuleE_BarrierCompactWaitsForInFlightClaim(t *testing.T) {
	svc := ruleENewService()
	fx := newRuleEFixture(t)
	queued := addRuleEChatTask(t, fx.sessionID, "queued")
	runtimeID := ruleEOnlineRuntime(t, queued)
	if _, err := testPool.Exec(ctx(), `UPDATE agent_task_queue SET status = 'completed' WHERE id = $1`, fx.taskID); err != nil {
		t.Fatal(err)
	}

	// The compact caller exists before the claim parks, as 'completed'; it turns
	// 'running' only once the claim holds the DM.
	caller := chatTaskToken(t, fx.sessionID, "completed")
	var callerTask string
	if err := testPool.QueryRow(ctx(), `SELECT task_id FROM task_token WHERE token_hash = $1`, auth.HashToken(caller)).Scan(&callerTask); err != nil {
		t.Fatal(err)
	}

	key, release := ruleEPause(t, "UPDATE", "agent_task_queue",
		fmt.Sprintf(`OLD.status = 'queued' AND NEW.status = 'dispatched' AND NEW.chat_session_id = '%s'`, fx.sessionID))
	claimed := make(chan error, 1)
	runtimeUUID := ruleEUUID(t, runtimeID)
	go func() {
		c, cancel := context.WithTimeout(ctx(), 30*time.Second)
		defer cancel()
		_, err := svc.ClaimTaskForRuntime(c, runtimeUUID)
		claimed <- err
	}()
	ruleEWaitParked(t, key)

	if _, err := testPool.Exec(ctx(), `UPDATE agent_task_queue SET status = 'running', started_at = now() WHERE id = $1`, callerTask); err != nil {
		t.Fatal(err)
	}
	compactDone := make(chan int, 1)
	go func() {
		code, _ := postCompact(t, caller, "x")
		compactDone <- code
	}()
	ruleEWaitOnDM(t)
	select {
	case code := <-compactDone:
		t.Fatalf("compact = %d while a claim held the DM share lock, want it to wait", code)
	case <-time.After(300 * time.Millisecond):
	}

	release()
	if err := <-claimed; err == nil || !strings.Contains(err.Error(), "idx_agent_task_queue_one_active_per_chat") {
		t.Fatalf("parked claim = %v, want the one-active-per-DM unique index to refuse it", err)
	}
	if code := <-compactDone; code != http.StatusOK {
		t.Fatalf("compact after the refused claim = %d, want 200", code)
	}
	if status, _, _ := ruleEQueuedState(t, queued); status != "queued" {
		t.Fatalf("queued task = %q, want queued after the refused claim", status)
	}
}

// Order 3: a DM row lock held by another transaction (a stand-in for a compact
// with no running caller, which L1a-2 adds) is skipped by the claim within the
// limit. A wait would run into the deadline and fail the test.
func TestRuleE_BarrierClaimSkipsHeldDMWithoutWaiting(t *testing.T) {
	svc := ruleENewService()
	fx := newRuleEFixture(t)
	queued := addRuleEChatTask(t, fx.sessionID, "queued")
	runtimeID := ruleEOnlineRuntime(t, queued)
	if _, err := testPool.Exec(ctx(), `UPDATE agent_task_queue SET status = 'completed' WHERE id = $1`, fx.taskID); err != nil {
		t.Fatal(err)
	}

	hold, err := testPool.Begin(ctx())
	if err != nil {
		t.Fatal(err)
	}
	defer hold.Rollback(ctx())
	if _, err := hold.Exec(ctx(), `SELECT id FROM chat_session WHERE id = $1 FOR NO KEY UPDATE`, fx.sessionID); err != nil {
		t.Fatal(err)
	}
	if ruleEClaimUntil(t, svc, runtimeID, queued, 3*time.Second) {
		t.Fatal("claim returned the chat task while the DM was locked")
	}
	if status, _, _ := ruleEQueuedState(t, queued); status != "queued" {
		t.Fatalf("queued task = %q, want queued", status)
	}
	if _, err := hold.Exec(ctx(), `UPDATE chat_session SET compaction_epoch = 1 WHERE id = $1`, fx.sessionID); err != nil {
		t.Fatal(err)
	}
	if err := hold.Commit(ctx()); err != nil {
		t.Fatal(err)
	}
	if !ruleEClaimUntil(t, svc, runtimeID, queued, 3*time.Second) {
		t.Fatal("claim after the lock was released did not return the task")
	}
	if status, stamp, dm := ruleEQueuedState(t, queued); status != "dispatched" || stamp != 1 || dm != 1 {
		t.Fatalf("queued task = %q at epoch %d, DM epoch %d, want dispatched at 1/1", status, stamp, dm)
	}
}
