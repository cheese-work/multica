package scheduler

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// insertRetentionTestWorkspace creates a bare workspace row with the given
// retention_days and returns its pgtype.UUID.
func insertRetentionTestWorkspace(t *testing.T, pool *pgxpool.Pool, retentionDays int32) pgtype.UUID {
	t.Helper()
	ctx := context.Background()
	var id pgtype.UUID
	suffix := uuid.NewString()[:8]
	err := pool.QueryRow(ctx,
		`INSERT INTO workspace (name, slug, description, issue_prefix, export_manifest_retention_days)
		 VALUES ($1, $2, '', $3, $4) RETURNING id`,
		"Retention Pool Test "+suffix, "retention-pool-"+suffix, "RPT", retentionDays,
	).Scan(&id)
	if err != nil {
		t.Fatalf("insert test workspace: %v", err)
	}
	t.Cleanup(func() {
		pool.Exec(context.Background(), `DELETE FROM activity_log WHERE workspace_id = $1`, id)
		pool.Exec(context.Background(), `DELETE FROM provenance_export_log WHERE workspace_id = $1`, id)
		pool.Exec(context.Background(), `DELETE FROM workspace WHERE id = $1`, id)
	})
	return id
}

func insertRetentionTestLog(t *testing.T, pool *pgxpool.Pool, workspaceID pgtype.UUID, age time.Duration) {
	t.Helper()
	ctx := context.Background()
	createdAt := time.Now().Add(-age)
	_, err := pool.Exec(ctx,
		`INSERT INTO provenance_export_log
		   (workspace_id, actor_type, actor_id, request_digest, manifest_digest,
		    cutoff, source_count, included_count, excluded_count, manifest, created_at)
		 VALUES ($1, 'member', gen_random_uuid(), $2, $2, $3, 1, 0, 0, '{"records":[],"exclusions":[]}', $3)`,
		workspaceID, fmt.Sprintf("digest-%d", age), createdAt,
	)
	if err != nil {
		t.Fatalf("insert test provenance_export_log row: %v", err)
	}
}

func countRetentionTestLogs(t *testing.T, pool *pgxpool.Pool, workspaceID pgtype.UUID) int {
	t.Helper()
	var count int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM provenance_export_log WHERE workspace_id = $1`, workspaceID).Scan(&count); err != nil {
		t.Fatalf("count provenance_export_log rows: %v", err)
	}
	return count
}

func countRetentionReceipts(t *testing.T, pool *pgxpool.Pool, workspaceID pgtype.UUID) int {
	t.Helper()
	var count int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM activity_log WHERE workspace_id = $1 AND action = $2`,
		workspaceID, provenanceExportRetentionSweptActivity).Scan(&count); err != nil {
		t.Fatalf("count activity_log receipts: %v", err)
	}
	return count
}

// TestPoolSweeper_DeletesExpiredAndWritesReceipt is the atomicity-and-content
// happy path: an expired row is deleted, a fresh row survives, and exactly
// one durable receipt records how many rows were removed for that workspace.
func TestPoolSweeper_DeletesExpiredAndWritesReceipt(t *testing.T) {
	pool := integrationPool(t)
	workspaceID := insertRetentionTestWorkspace(t, pool, 90)
	insertRetentionTestLog(t, pool, workspaceID, 100*24*time.Hour) // expired
	insertRetentionTestLog(t, pool, workspaceID, 10*24*time.Hour)  // not expired

	sweeper := &poolSweeper{pool: pool}
	deleted, err := sweeper.SweepWorkspace(context.Background(), workspaceID)
	if err != nil {
		t.Fatalf("SweepWorkspace: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("deleted = %d, want 1", deleted)
	}
	if remaining := countRetentionTestLogs(t, pool, workspaceID); remaining != 1 {
		t.Fatalf("remaining logs = %d, want 1 (the row inside the retention window)", remaining)
	}
	if receipts := countRetentionReceipts(t, pool, workspaceID); receipts != 1 {
		t.Fatalf("receipts = %d, want 1", receipts)
	}

	var details provenanceRetentionReceiptDetails
	var raw []byte
	if err := pool.QueryRow(context.Background(),
		`SELECT details FROM activity_log WHERE workspace_id = $1 AND action = $2`,
		workspaceID, provenanceExportRetentionSweptActivity).Scan(&raw); err != nil {
		t.Fatalf("read receipt details: %v", err)
	}
	if err := json.Unmarshal(raw, &details); err != nil {
		t.Fatalf("unmarshal receipt details: %v", err)
	}
	if details.DeletedCount != 1 {
		t.Fatalf("receipt deleted_count = %d, want 1", details.DeletedCount)
	}
}

// TestPoolSweeper_NoExpiredRowsSkipsReceipt guards against a receipt row for
// every workspace on every tick forever: a sweep that finds nothing to delete
// must not write a no-op activity_log entry.
func TestPoolSweeper_NoExpiredRowsSkipsReceipt(t *testing.T) {
	pool := integrationPool(t)
	workspaceID := insertRetentionTestWorkspace(t, pool, 90)
	insertRetentionTestLog(t, pool, workspaceID, 10*24*time.Hour) // not expired

	sweeper := &poolSweeper{pool: pool}
	deleted, err := sweeper.SweepWorkspace(context.Background(), workspaceID)
	if err != nil {
		t.Fatalf("SweepWorkspace: %v", err)
	}
	if deleted != 0 {
		t.Fatalf("deleted = %d, want 0", deleted)
	}
	if receipts := countRetentionReceipts(t, pool, workspaceID); receipts != 0 {
		t.Fatalf("receipts = %d, want 0 for a no-op sweep", receipts)
	}
}

// TestPoolSweeper_UsesLiveRetentionAtDeletionTime is the direct regression
// test for the review's stale-policy finding: retention_days is raised from
// 1 to 365 AFTER a row was inserted that is 100 days old (so it would be
// expired under the OLD 1-day setting but protected under the new 365-day
// one), and the sweep is only run after the raise. Because
// DeleteExpiredProvenanceExportLogsForWorkspace reads export_manifest_retention_days
// via a live subquery rather than a value the caller captured earlier, the
// row must survive.
func TestPoolSweeper_UsesLiveRetentionAtDeletionTime(t *testing.T) {
	pool := integrationPool(t)
	workspaceID := insertRetentionTestWorkspace(t, pool, 1)
	insertRetentionTestLog(t, pool, workspaceID, 100*24*time.Hour)

	// Simulate an admin's concurrent policy change landing between when a
	// caller might have listed this workspace's old retention setting and
	// when the delete actually runs.
	if _, err := pool.Exec(context.Background(),
		`UPDATE workspace SET export_manifest_retention_days = 365 WHERE id = $1`, workspaceID); err != nil {
		t.Fatalf("raise retention: %v", err)
	}

	sweeper := &poolSweeper{pool: pool}
	deleted, err := sweeper.SweepWorkspace(context.Background(), workspaceID)
	if err != nil {
		t.Fatalf("SweepWorkspace: %v", err)
	}
	if deleted != 0 {
		t.Fatalf("deleted = %d, want 0 — the row is only 100 days old and retention was raised to 365 before the sweep ran", deleted)
	}
	if remaining := countRetentionTestLogs(t, pool, workspaceID); remaining != 1 {
		t.Fatalf("remaining logs = %d, want 1 (protected by the raised retention)", remaining)
	}
}

// TestPoolSweeper_CrossWorkspaceIsolation ensures a sweep for one workspace
// never touches another's rows or receipts, regardless of the other
// workspace's own age/retention combination.
func TestPoolSweeper_CrossWorkspaceIsolation(t *testing.T) {
	pool := integrationPool(t)
	target := insertRetentionTestWorkspace(t, pool, 90)
	other := insertRetentionTestWorkspace(t, pool, 1)
	insertRetentionTestLog(t, pool, target, 100*24*time.Hour)
	insertRetentionTestLog(t, pool, other, 200*24*time.Hour) // also expired, but a different workspace

	sweeper := &poolSweeper{pool: pool}
	if _, err := sweeper.SweepWorkspace(context.Background(), target); err != nil {
		t.Fatalf("SweepWorkspace(target): %v", err)
	}

	if remaining := countRetentionTestLogs(t, pool, other); remaining != 1 {
		t.Fatalf("other workspace's log was affected by a sweep scoped to target: remaining=%d, want 1", remaining)
	}
	if receipts := countRetentionReceipts(t, pool, other); receipts != 0 {
		t.Fatalf("other workspace got a receipt from a sweep scoped to target: receipts=%d, want 0", receipts)
	}
}

// TestProvenanceExportRetention_PartialSweepFailureKeepsPriorReceipts is the
// end-to-end regression for the review's B3 finding: with three workspaces,
// the third's sweep fails; the first two must have already committed their
// delete AND their durable receipt (not rely on the tick's own
// sys_cron_executions row, which only records the failure globally).
func TestProvenanceExportRetention_PartialSweepFailureKeepsPriorReceipts(t *testing.T) {
	pool := integrationPool(t)
	wsOK1 := insertRetentionTestWorkspace(t, pool, 90)
	wsOK2 := insertRetentionTestWorkspace(t, pool, 90)
	wsFail := insertRetentionTestWorkspace(t, pool, 90)
	insertRetentionTestLog(t, pool, wsOK1, 100*24*time.Hour)
	insertRetentionTestLog(t, pool, wsOK2, 100*24*time.Hour)
	insertRetentionTestLog(t, pool, wsFail, 100*24*time.Hour)

	lister := &fakeWorkspaceLister{workspaces: []pgtype.UUID{wsOK1, wsOK2, wsFail}}
	sweeper := &partialFailureSweeper{real: &poolSweeper{pool: pool}, failOn: wsFail}

	handler := makeProvenanceExportRetentionHandler(lister, sweeper)
	if _, err := handler(context.Background(), HandlerInput{}); err == nil {
		t.Fatal("expected the tick to fail once wsFail's sweep errors")
	}

	for _, ws := range []pgtype.UUID{wsOK1, wsOK2} {
		if remaining := countRetentionTestLogs(t, pool, ws); remaining != 0 {
			t.Fatalf("workspace %s: expected its expired row deleted before the later failure, remaining=%d", util.UUIDToString(ws), remaining)
		}
		if receipts := countRetentionReceipts(t, pool, ws); receipts != 1 {
			t.Fatalf("workspace %s: expected a durable receipt to survive the later failure, receipts=%d", util.UUIDToString(ws), receipts)
		}
	}
	// wsFail's own row must be untouched — its transaction never committed.
	if remaining := countRetentionTestLogs(t, pool, wsFail); remaining != 1 {
		t.Fatalf("wsFail: expected its row untouched since the injected failure aborts before commit, remaining=%d", remaining)
	}
}

// partialFailureSweeper wraps a real poolSweeper but injects a failure for
// one workspace, simulating (for example) a transient DB error mid-tick
// without needing to fault-inject inside Postgres itself.
type partialFailureSweeper struct {
	real   *poolSweeper
	failOn pgtype.UUID
}

func (s *partialFailureSweeper) SweepWorkspace(ctx context.Context, workspaceID pgtype.UUID) (int64, error) {
	if workspaceID == s.failOn {
		return 0, fmt.Errorf("injected failure for workspace %s", util.UUIDToString(workspaceID))
	}
	return s.real.SweepWorkspace(ctx, workspaceID)
}

// concurrentSweepBlockWindow is how long TestPoolSweeper_ConcurrentRetentionRaiseDuringSweep
// holds the PATCH transaction's lock open before committing. The sweep must
// not observe a result before this window elapses; that ordering, not
// pg_locks introspection, is the proof of genuine blocking. pg_locks was
// tried first and rejected: Postgres does not reliably surface an uncontended
// or short-window row-level lock wait as a `granted=false` pg_locks row (the
// tuple lock itself often does not appear in pg_locks at all until there is
// real contention to report), so polling it produced false negatives even
// while the block was verifiably happening (confirmed separately by wall-clock
// timing against a real two-transaction session).
const concurrentSweepBlockWindow = 1500 * time.Millisecond

// TestPoolSweeper_ConcurrentRetentionRaiseDuringSweep is a genuinely
// overlapping two-transaction regression for the same stale-policy finding
// covered synchronously by TestPoolSweeper_UsesLiveRetentionAtDeletionTime,
// and for the serialization Sol's review specifically asked for: the PATCH
// path's row lock (FOR UPDATE, via UpdateWorkspaceExportPrivacy's real
// transaction) is held open — retention raised but NOT yet committed — while
// a sweep is started concurrently in a second goroutine. The sweep's
// DeleteExpiredProvenanceExportLogsForWorkspace query takes FOR SHARE on the
// same workspace row, so it must BLOCK until the PATCH transaction commits,
// not read a stale value out from under it. The proof of genuine overlap is
// timing: the sweep goroutine must not report a result until AFTER
// concurrentSweepBlockWindow has elapsed and the PATCH has committed — if the
// FOR SHARE lock were not serializing against FOR UPDATE, the sweep would
// finish almost immediately instead.
func TestPoolSweeper_ConcurrentRetentionRaiseDuringSweep(t *testing.T) {
	pool := integrationPool(t)
	workspaceID := insertRetentionTestWorkspace(t, pool, 1)
	insertRetentionTestLog(t, pool, workspaceID, 100*24*time.Hour)

	ctx := context.Background()

	// Start the PATCH-equivalent transaction and hold its FOR UPDATE lock open
	// (raised to 365 days, NOT yet committed) using the exact same queries the
	// real handler uses.
	patchTx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin patch tx: %v", err)
	}
	patchQueries := db.New(patchTx)
	if _, err := patchQueries.GetWorkspaceExportPrivacyForUpdate(ctx, workspaceID); err != nil {
		t.Fatalf("lock workspace row FOR UPDATE: %v", err)
	}
	if _, err := patchQueries.UpdateWorkspaceExportPrivacy(ctx, db.UpdateWorkspaceExportPrivacyParams{
		ID:                          workspaceID,
		ExportRedactionMode:         "small",
		ExportManifestRetentionDays: 365,
	}); err != nil {
		t.Fatalf("update retention inside patch tx: %v", err)
	}

	// Start the sweep concurrently. It must block on the FOR SHARE subquery
	// until patchTx below commits.
	type sweepResult struct {
		deleted  int64
		err      error
		finished time.Time
	}
	sweepStart := time.Now()
	sweepDone := make(chan sweepResult, 1)
	go func() {
		sweeper := &poolSweeper{pool: pool}
		deleted, err := sweeper.SweepWorkspace(ctx, workspaceID)
		sweepDone <- sweepResult{deleted, err, time.Now()}
	}()

	// Give the sweep goroutine time to actually reach and block on the FOR
	// SHARE subquery before committing — otherwise a slow scheduler could let
	// the sweep run to completion before it ever contends for the lock, which
	// would pass for the wrong reason (no contention, not successful blocking).
	time.Sleep(concurrentSweepBlockWindow)
	commitTime := time.Now()
	if err := patchTx.Commit(ctx); err != nil {
		t.Fatalf("commit patch tx: %v", err)
	}

	result := <-sweepDone
	if result.err != nil {
		t.Fatalf("SweepWorkspace: %v", result.err)
	}
	if result.finished.Before(commitTime) {
		t.Fatalf("sweep finished at %s, before the PATCH committed at %s — the FOR SHARE subquery did not block on the FOR UPDATE holder",
			result.finished.Format(time.RFC3339Nano), commitTime.Format(time.RFC3339Nano))
	}
	if result.finished.Sub(sweepStart) < concurrentSweepBlockWindow {
		t.Fatalf("sweep completed after only %s, less than the %s the PATCH held its lock — it did not genuinely wait",
			result.finished.Sub(sweepStart), concurrentSweepBlockWindow)
	}
	if result.deleted != 0 {
		t.Fatalf("deleted = %d, want 0 — the sweep must observe the committed 365-day retention it waited on, not the stale 1-day value", result.deleted)
	}
}
