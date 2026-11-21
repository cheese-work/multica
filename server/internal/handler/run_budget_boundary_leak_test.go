package handler

import (
	"context"
	"crypto/rand"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
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

func setBoundaryLimits(t *testing.T, idle, hold time.Duration) {
	t.Helper()
	prevIdle, prevHold := runBudgetBoundaryIdleTimeout, runBudgetBoundaryHoldLimit
	runBudgetBoundaryIdleTimeout, runBudgetBoundaryHoldLimit = idle, hold
	t.Cleanup(func() { runBudgetBoundaryIdleTimeout, runBudgetBoundaryHoldLimit = prevIdle, prevHold })
}

// A holder that never calls release must not keep the database lock: Postgres
// ends the idle transaction.
func TestRunBudgetBoundaryLeakedHolderLosesDatabaseLock(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Fatal("isolated database required")
	}
	setBoundaryLimits(t, 300*time.Millisecond, time.Hour)
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
	setBoundaryLimits(t, time.Hour, 300*time.Millisecond)
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
