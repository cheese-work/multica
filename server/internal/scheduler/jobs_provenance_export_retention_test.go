package scheduler

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// fakeProvenanceRetentionQueries is an in-memory double for
// provenanceRetentionQueries so the handler's iteration and error-propagation
// logic is covered without a live Postgres pool. The DELETE semantics
// themselves (created_at cutoff math, workspace_id scoping) are covered by
// the SQL-level test in server/internal/handler/workspace_export_privacy_test.go
// and by exercising the real query through ExportProvenance's audit rows.
type fakeProvenanceRetentionQueries struct {
	settings []db.ListWorkspaceExportRetentionSettingsRow
	// deleted counts, keyed by workspace_id string form, how many rows a
	// call for that workspace should report as removed.
	deleted map[string]int64
	// failOn, if set, makes the delete call for this workspace_id return an
	// error instead of a count.
	failOn string
	calls  []db.DeleteExpiredProvenanceExportLogsForWorkspaceParams
}

func (f *fakeProvenanceRetentionQueries) ListWorkspaceExportRetentionSettings(ctx context.Context) ([]db.ListWorkspaceExportRetentionSettingsRow, error) {
	return f.settings, nil
}

func (f *fakeProvenanceRetentionQueries) DeleteExpiredProvenanceExportLogsForWorkspace(ctx context.Context, arg db.DeleteExpiredProvenanceExportLogsForWorkspaceParams) (int64, error) {
	f.calls = append(f.calls, arg)
	key := workspaceUUIDString(arg.WorkspaceID)
	if f.failOn == key {
		return 0, errors.New("simulated delete failure")
	}
	return f.deleted[key], nil
}

func workspaceUUIDString(id pgtype.UUID) string {
	return string(id.Bytes[:])
}

func fakeWorkspaceID(b byte) pgtype.UUID {
	var bytes [16]byte
	bytes[0] = b
	return pgtype.UUID{Bytes: bytes, Valid: true}
}

// TestProvenanceExportRetention_SweepsEachWorkspaceWithItsOwnRetention covers
// CHE-766's per-workspace retention requirement: the job must call the
// delete query once per workspace with THAT workspace's own
// export_manifest_retention_days, not one global value, and it must total
// the deleted counts into RowsAffected for the audit row (never a silent
// deletion — see the job's doc comment).
func TestProvenanceExportRetention_SweepsEachWorkspaceWithItsOwnRetention(t *testing.T) {
	wsA := fakeWorkspaceID(1)
	wsB := fakeWorkspaceID(2)
	fake := &fakeProvenanceRetentionQueries{
		settings: []db.ListWorkspaceExportRetentionSettingsRow{
			{WorkspaceID: wsA, ExportManifestRetentionDays: 90},
			{WorkspaceID: wsB, ExportManifestRetentionDays: 7},
		},
		deleted: map[string]int64{
			workspaceUUIDString(wsA): 3,
			workspaceUUIDString(wsB): 5,
		},
	}

	handler := makeProvenanceExportRetentionHandler(fake)
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
	if len(fake.calls) != 2 {
		t.Fatalf("expected 2 delete calls, got %d", len(fake.calls))
	}
	byWorkspace := map[string]int32{}
	for _, call := range fake.calls {
		byWorkspace[workspaceUUIDString(call.WorkspaceID)] = call.RetentionDays
	}
	if byWorkspace[workspaceUUIDString(wsA)] != 90 {
		t.Fatalf("workspace A retention_days = %d, want 90 (its own setting, not workspace B's)", byWorkspace[workspaceUUIDString(wsA)])
	}
	if byWorkspace[workspaceUUIDString(wsB)] != 7 {
		t.Fatalf("workspace B retention_days = %d, want 7 (its own setting, not workspace A's)", byWorkspace[workspaceUUIDString(wsB)])
	}
}

// TestProvenanceExportRetention_FailsClosedOnDeleteError ensures a delete
// failure for one workspace surfaces as a job error (so the scheduler retries
// per its JobSpec backoff) instead of being swallowed and reported as a
// clean sweep.
func TestProvenanceExportRetention_FailsClosedOnDeleteError(t *testing.T) {
	wsA := fakeWorkspaceID(1)
	fake := &fakeProvenanceRetentionQueries{
		settings: []db.ListWorkspaceExportRetentionSettingsRow{
			{WorkspaceID: wsA, ExportManifestRetentionDays: 90},
		},
		failOn: workspaceUUIDString(wsA),
	}

	handler := makeProvenanceExportRetentionHandler(fake)
	if _, err := handler(context.Background(), HandlerInput{}); err == nil {
		t.Fatal("expected an error when the delete call fails, got nil")
	}
}

// TestProvenanceExportRetention_NoWorkspacesIsANoop guards the empty-instance
// case: zero workspaces must not error and must report zero rows affected,
// not skip silently in a way that would look identical to a real sweep that
// found nothing to delete.
func TestProvenanceExportRetention_NoWorkspacesIsANoop(t *testing.T) {
	fake := &fakeProvenanceRetentionQueries{}
	handler := makeProvenanceExportRetentionHandler(fake)
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
