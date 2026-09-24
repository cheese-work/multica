package scheduler

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
)

// fakeWorkspaceLister is an in-memory double for
// provenanceRetentionWorkspaceLister so the handler's iteration and
// error-propagation logic is covered without a live Postgres pool.
type fakeWorkspaceLister struct {
	workspaces []pgtype.UUID
}

func (f *fakeWorkspaceLister) ListWorkspaceExportRetentionSettings(ctx context.Context) ([]pgtype.UUID, error) {
	return f.workspaces, nil
}

// fakeSweeper is an in-memory double for provenanceRetentionSweeper. Unlike
// the real poolSweeper it does not exercise the actual DELETE/receipt
// transaction — that atomicity, and the live-retention-at-deletion-time
// property, are covered by the real-DB tests in
// server/internal/handler/workspace_export_privacy_test.go and by
// TestPoolSweeper_* below.
type fakeSweeper struct {
	// deleted counts, keyed by workspace_id string form, how many rows a call
	// for that workspace should report as removed.
	deleted map[string]int64
	// failOn, if set, makes the sweep call for this workspace_id return an
	// error instead of a count.
	failOn string
	calls  []pgtype.UUID
}

func (f *fakeSweeper) SweepWorkspace(ctx context.Context, workspaceID pgtype.UUID) (int64, error) {
	f.calls = append(f.calls, workspaceID)
	key := workspaceMapKey(workspaceID)
	if f.failOn == key {
		return 0, errors.New("simulated sweep failure")
	}
	return f.deleted[key], nil
}

func fakeWorkspaceID(b byte) pgtype.UUID {
	var bytes [16]byte
	bytes[0] = b
	return pgtype.UUID{Bytes: bytes, Valid: true}
}

// TestProvenanceExportRetention_SweepsEveryWorkspace covers CHE-766's
// per-workspace fan-out: the job must call SweepWorkspace once per workspace
// and total the deleted counts into RowsAffected for the audit row (never a
// silent deletion — see the job's doc comment). Each workspace's own live
// retention_days is read inside SweepWorkspace's transaction (poolSweeper),
// not passed down from this loop — that is what closes the stale-policy race
// the review flagged, and is exercised against real Postgres in
// TestPoolSweeper_UsesLiveRetentionAtDeletionTime.
func TestProvenanceExportRetention_SweepsEveryWorkspace(t *testing.T) {
	wsA := fakeWorkspaceID(1)
	wsB := fakeWorkspaceID(2)
	lister := &fakeWorkspaceLister{workspaces: []pgtype.UUID{wsA, wsB}}
	sweeper := &fakeSweeper{
		deleted: map[string]int64{
			workspaceMapKey(wsA): 3,
			workspaceMapKey(wsB): 5,
		},
	}

	handler := makeProvenanceExportRetentionHandler(lister, sweeper)
	result, err := handler(context.Background(), HandlerInput{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RowsAffected != 8 {
		t.Fatalf("RowsAffected = %d, want 8 (3+5)", result.RowsAffected)
	}
	if got, want := result.Result["workspaces_swept"], 2; got != want {
		t.Fatalf("workspaces_swept = %v, want %v", got, want)
	}
	if len(sweeper.calls) != 2 {
		t.Fatalf("expected 2 sweep calls, got %d", len(sweeper.calls))
	}
}

// TestProvenanceExportRetention_FailsClosedOnSweepError ensures a sweep
// failure for one workspace surfaces as a job error (so the scheduler retries
// per its JobSpec backoff) instead of being swallowed and reported as a clean
// sweep. Workspaces already swept before the failure keep their committed
// delete + receipt (poolSweeper commits per workspace, not per tick) — this
// test covers the handler's propagation, not that per-workspace durability,
// which is a property of poolSweeper's own transaction boundary.
func TestProvenanceExportRetention_FailsClosedOnSweepError(t *testing.T) {
	wsA := fakeWorkspaceID(1)
	wsB := fakeWorkspaceID(2)
	lister := &fakeWorkspaceLister{workspaces: []pgtype.UUID{wsA, wsB}}
	sweeper := &fakeSweeper{failOn: workspaceMapKey(wsB)}

	handler := makeProvenanceExportRetentionHandler(lister, sweeper)
	if _, err := handler(context.Background(), HandlerInput{}); err == nil {
		t.Fatal("expected an error when a sweep call fails, got nil")
	}
	if len(sweeper.calls) != 2 {
		t.Fatalf("expected the handler to have attempted both workspaces before failing, got %d calls", len(sweeper.calls))
	}
}

// TestProvenanceExportRetention_NoWorkspacesIsANoop guards the empty-instance
// case: zero workspaces must not error and must report zero rows affected,
// not skip silently in a way that would look identical to a real sweep that
// found nothing to delete.
func TestProvenanceExportRetention_NoWorkspacesIsANoop(t *testing.T) {
	lister := &fakeWorkspaceLister{}
	sweeper := &fakeSweeper{}
	handler := makeProvenanceExportRetentionHandler(lister, sweeper)
	result, err := handler(context.Background(), HandlerInput{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RowsAffected != 0 {
		t.Fatalf("RowsAffected = %d, want 0", result.RowsAffected)
	}
	if got, want := result.Result["workspaces_swept"], 0; got != want {
		t.Fatalf("workspaces_swept = %v, want %v", got, want)
	}
}
