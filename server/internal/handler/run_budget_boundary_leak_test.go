package handler

import (
	"context"
	"crypto/rand"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/testutil"
)

func leakTestIssueID(t *testing.T) pgtype.UUID {
	t.Helper()
	id := pgtype.UUID{Valid: true}
	if _, err := rand.Read(id.Bytes[:]); err != nil {
		t.Fatal(err)
	}
	return id
}

// dbLockFree reports whether another connection can take the issue's boundary
// advisory lock within wait.
func dbLockFree(t *testing.T, issueID pgtype.UUID, wait time.Duration) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	tx, err := testPool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	return testHandler.Queries.WithTx(tx).LockIssueRunBudgetBoundary(ctx, issueID) == nil
}

func setBoundaryLimits(t *testing.T, idle, keepalive, hold time.Duration) {
	t.Helper()
	prevIdle, prevKeep, prevHold := runBudgetBoundaryIdleTimeout, runBudgetBoundaryKeepalive, runBudgetBoundaryHoldLimit
	runBudgetBoundaryIdleTimeout, runBudgetBoundaryKeepalive, runBudgetBoundaryHoldLimit = idle, keepalive, hold
	t.Cleanup(func() {
		runBudgetBoundaryIdleTimeout, runBudgetBoundaryKeepalive, runBudgetBoundaryHoldLimit = prevIdle, prevKeep, prevHold
	})
}

// A holder whose process stopped keeping its transaction alive must not keep
// the database lock: Postgres ends the idle transaction.
func TestRunBudgetBoundaryLeakedHolderLosesDatabaseLock(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Fatal("isolated database required")
	}
	setBoundaryLimits(t, 300*time.Millisecond, time.Hour, time.Hour)
	issueID := leakTestIssueID(t)
	release, err := testHandler.lockRunBudgetBoundary(context.Background(), issueID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)
	if dbLockFree(t, issueID, 200*time.Millisecond) {
		t.Fatal("lock was free while held")
	}
	time.Sleep(time.Second)
	if !dbLockFree(t, issueID, 2*time.Second) {
		t.Fatal("leaked holder still holds the advisory lock after the idle timeout")
	}
}

// A holder that never calls release must not keep the in-process turn either:
// the next status write on the issue proceeds.
func TestRunBudgetBoundaryLeakedHolderIsForceReleased(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Fatal("isolated database required")
	}
	setBoundaryLimits(t, time.Hour, time.Hour, 300*time.Millisecond)
	issueID := leakTestIssueID(t)
	leaked, err := testHandler.lockRunBudgetBoundary(context.Background(), issueID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(leaked)
	time.Sleep(time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	next, err := testHandler.lockRunBudgetBoundary(ctx, issueID)
	if err != nil {
		t.Fatalf("next writer stayed blocked behind a leaked holder: %v", err)
	}
	next()
}

// A request that is cancelled before its holder releases still frees the
// advisory lock and returns the connection to the pool.
func TestRunBudgetBoundaryReleaseAfterCancelledRequest(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Fatal("isolated database required")
	}
	issueID := leakTestIssueID(t)
	before := testPool.Stat().AcquiredConns()
	ctx, cancelRequest := context.WithCancel(context.Background())
	release, err := testHandler.lockRunBudgetBoundary(ctx, issueID)
	if err != nil {
		t.Fatal(err)
	}
	cancelRequest()
	release()
	if !dbLockFree(t, issueID, 2*time.Second) {
		t.Fatal("advisory lock still held after release on a cancelled request")
	}
	deadline := time.Now().Add(2 * time.Second)
	for testPool.Stat().AcquiredConns() > before && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if got := testPool.Stat().AcquiredConns(); got > before {
		t.Fatalf("acquired connections = %d, want at most %d", got, before)
	}
}

// A live holder works on other connections while its boundary transaction sits
// idle. The idle timeout must not take the lock from it: a competing move stays
// blocked well past the timeout, until the holder releases.
func TestRunBudgetBoundaryLiveHolderKeepsLockPastIdleTimeout(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Fatal("isolated database required")
	}
	setBoundaryLimits(t, 300*time.Millisecond, 100*time.Millisecond, time.Hour)
	issueID := leakTestIssueID(t)
	release, err := testHandler.lockRunBudgetBoundary(context.Background(), issueID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)
	time.Sleep(1500 * time.Millisecond) // five idle timeouts of live work
	if dbLockFree(t, issueID, 200*time.Millisecond) {
		t.Fatal("a competing move took the lock from a live holder past the idle timeout")
	}
	release()
	if !dbLockFree(t, issueID, 2*time.Second) {
		t.Fatal("lock still held after release")
	}
}

// A batch status move whose run cannot be stopped after a failed role write
// leaves the loop early. It must not keep the boundary lock or the in-process
// turn: the next move on the issue proceeds with no watchdog to rescue it.
func TestBatchUpdateRoleUnrecordedBranchReleasesBoundaryLock(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Fatal("isolated database required")
	}
	setBoundaryLimits(t, time.Hour, time.Hour, time.Hour)
	_, issueID, agentID, taskID := seedRoleIssue(t)
	dbfx.Exec(t, `UPDATE agent_task_queue SET status = 'running', started_at = now() WHERE id = $1`, taskID)
	// The run's role row cannot be written and the run cannot be cancelled, so
	// advanceRunStep returns an error and the batch takes its failure branch.
	dbfx.Exec(t, `CREATE OR REPLACE FUNCTION che8849_fail() RETURNS trigger AS $$
		BEGIN RAISE EXCEPTION 'injected che8849 failure'; END $$ LANGUAGE plpgsql`)
	dbfx.Exec(t, `CREATE TRIGGER che8849_fail_role BEFORE INSERT ON activity_log
		FOR EACH ROW WHEN (NEW.action = 'workflow_role' AND NEW.details->>'task_id' = '`+taskID+`') EXECUTE FUNCTION che8849_fail()`)
	dbfx.Exec(t, `CREATE TRIGGER che8849_fail_cancel BEFORE UPDATE ON agent_task_queue
		FOR EACH ROW WHEN (NEW.id = '`+taskID+`' AND NEW.status = 'cancelled') EXECUTE FUNCTION che8849_fail()`)
	t.Cleanup(func() {
		dbfx.Exec(t, `DROP TRIGGER IF EXISTS che8849_fail_role ON activity_log`)
		dbfx.Exec(t, `DROP TRIGGER IF EXISTS che8849_fail_cancel ON agent_task_queue`)
		dbfx.Exec(t, `DROP FUNCTION IF EXISTS che8849_fail()`)
	})

	req := newRequest("POST", "/api/issues/batch?workspace_id="+testWorkspaceID, map[string]any{
		"issue_ids": []string{issueID},
		"updates":   map[string]any{"status": "in_review"},
	})
	req.Header.Set("X-Actor-Source", "task_token")
	req.Header.Set("X-Agent-ID", agentID)
	req.Header.Set("X-Task-ID", taskID)
	var out struct {
		Failed []struct {
			ID   string `json:"id"`
			Code string `json:"code"`
		} `json:"failed"`
	}
	testutil.Call(t, testHandler.BatchUpdateIssues, req).Want(http.StatusOK).JSON(&out)
	if len(out.Failed) != 1 || out.Failed[0].Code != "workflow_role_unrecorded" {
		t.Fatalf("failed = %+v, want one workflow_role_unrecorded: the failure branch was not reached", out.Failed)
	}

	id := parseUUID(issueID)
	if !dbLockFree(t, id, 2*time.Second) {
		t.Fatal("batch failure branch kept the advisory lock")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	next, err := testHandler.lockRunBudgetBoundary(ctx, id)
	if err != nil {
		t.Fatalf("next move on the issue stayed blocked behind the batch failure branch: %v", err)
	}
	next()
}
