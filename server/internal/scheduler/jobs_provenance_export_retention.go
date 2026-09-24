package scheduler

import (
	"context"
	"fmt"
	"time"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// JobNameProvenanceExportRetention is the canonical name used in audit rows.
// Stable across releases — do not rename without a migration.
const JobNameProvenanceExportRetention = "provenance_export_retention"

// provenanceRetentionQueries is the subset of *db.Queries this job needs,
// narrowed for testability without a live pool.
type provenanceRetentionQueries interface {
	ListWorkspaceExportRetentionSettings(ctx context.Context) ([]db.ListWorkspaceExportRetentionSettingsRow, error)
	DeleteExpiredProvenanceExportLogsForWorkspace(ctx context.Context, arg db.DeleteExpiredProvenanceExportLogsForWorkspaceParams) (int64, error)
}

// ProvenanceExportRetentionJob (CHE-766) deletes provenance_export_log rows
// (see migration 515 — ids, digests and exclusion reasons only, never
// exported content) once they are older than the owning workspace's own
// export_manifest_retention_days. Each workspace's admin-configured window is
// honored individually rather than one global cutoff.
//
// This is a deletion job, not a silent one: RowsAffected and the per-tick
// result map land in the scheduler's own sys_cron_executions audit row, so
// "how many manifests were reaped and when" stays reconstructable even though
// the manifests themselves are gone.
func ProvenanceExportRetentionJob(queries provenanceRetentionQueries) JobSpec {
	return JobSpec{
		Name:              JobNameProvenanceExportRetention,
		Cadence:           1 * time.Hour,
		ScheduleDelay:     5 * time.Minute,
		CatchUpMode:       CatchUpLatestOnly,
		CatchUpWindow:     24 * time.Hour,
		RunTimeout:        10 * time.Minute,
		StaleTimeout:      15 * time.Minute,
		HeartbeatInterval: 30 * time.Second,
		AllowStaleReentry: true,
		MaxAttempts:       3,
		RetryBackoff: []time.Duration{
			1 * time.Minute,
			5 * time.Minute,
			15 * time.Minute,
		},
		Scopes:  StaticScopes(ScopeGlobal),
		Handler: makeProvenanceExportRetentionHandler(queries),
	}
}

func makeProvenanceExportRetentionHandler(queries provenanceRetentionQueries) Handler {
	return func(ctx context.Context, in HandlerInput) (HandlerResult, error) {
		workspaces, err := queries.ListWorkspaceExportRetentionSettings(ctx)
		if err != nil {
			return HandlerResult{}, fmt.Errorf("list workspace export retention settings: %w", err)
		}

		var totalDeleted int64
		workspacesSwept := 0
		for i, ws := range workspaces {
			deleted, err := queries.DeleteExpiredProvenanceExportLogsForWorkspace(ctx, db.DeleteExpiredProvenanceExportLogsForWorkspaceParams{
				WorkspaceID:   ws.WorkspaceID,
				RetentionDays: ws.ExportManifestRetentionDays,
			})
			if err != nil {
				return HandlerResult{}, fmt.Errorf("delete expired provenance export logs: %w", err)
			}
			totalDeleted += deleted
			workspacesSwept++

			// Long workspace lists shouldn't starve the lease's stale_after
			// window; a heartbeat every 200 workspaces is cheap and keeps a
			// large sweep from being stolen mid-run.
			if in.Heartbeat != nil && i%200 == 199 {
				if err := in.Heartbeat(ctx); err != nil {
					return HandlerResult{}, fmt.Errorf("heartbeat: %w", err)
				}
			}
		}

		return HandlerResult{
			RowsAffected: totalDeleted,
			Result: map[string]any{
				"workspaces_swept": workspacesSwept,
			},
		}, nil
	}
}
