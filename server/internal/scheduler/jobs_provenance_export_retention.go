package scheduler

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// JobNameProvenanceExportRetention is the canonical name used in audit rows.
// Stable across releases — do not rename without a migration.
const JobNameProvenanceExportRetention = "provenance_export_retention"

// provenanceExportRetentionSweptActivity is the activity_log action name for
// one workspace's retention sweep receipt. Stable across releases.
const provenanceExportRetentionSweptActivity = "provenance_export_retention_swept"

// provenanceRetentionWorkspaceLister is the subset of *db.Queries this job
// needs to enumerate workspaces, narrowed for testability without a live
// pool.
type provenanceRetentionWorkspaceLister interface {
	ListWorkspaceExportRetentionSettings(ctx context.Context) ([]pgtype.UUID, error)
}

// provenanceRetentionSweeper performs one workspace's atomic delete+receipt.
// Narrowed to an interface so the handler's iteration/error-propagation
// logic is unit-testable without Postgres; ProvenanceExportRetentionJob wires
// the real pool-backed implementation.
type provenanceRetentionSweeper interface {
	SweepWorkspace(ctx context.Context, workspaceID pgtype.UUID) (deleted int64, err error)
}

// ProvenanceExportRetentionJob (CHE-766) deletes provenance_export_log rows
// (see migration 515 — ids, digests and exclusion reasons only, never
// exported content) once they are older than the owning workspace's own,
// live-read export_manifest_retention_days. Each workspace's admin-configured
// window is honored individually, read at deletion time, rather than a
// global cutoff or a value captured earlier in the tick (see
// DeleteExpiredProvenanceExportLogsForWorkspace's doc comment for why that
// matters under a concurrent retention change).
//
// Each workspace's delete + its activity_log receipt commit together in one
// transaction (poolSweeper.SweepWorkspace), so a later workspace's failure
// mid-sweep cannot erase the durable, workspace-scoped record of what was
// already removed for workspaces processed earlier in the same tick — only
// the scheduler's own global sys_cron_executions row would otherwise survive
// a partial failure, and that row carries no per-workspace detail.
func ProvenanceExportRetentionJob(queries provenanceRetentionWorkspaceLister, pool *pgxpool.Pool) JobSpec {
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
		Handler: makeProvenanceExportRetentionHandler(queries, &poolSweeper{pool: pool}),
	}
}

func makeProvenanceExportRetentionHandler(queries provenanceRetentionWorkspaceLister, sweeper provenanceRetentionSweeper) Handler {
	return func(ctx context.Context, in HandlerInput) (HandlerResult, error) {
		workspaces, err := queries.ListWorkspaceExportRetentionSettings(ctx)
		if err != nil {
			return HandlerResult{}, fmt.Errorf("list workspace export retention settings: %w", err)
		}

		var totalDeleted int64
		workspacesSwept := 0
		for i, workspaceID := range workspaces {
			deleted, err := sweeper.SweepWorkspace(ctx, workspaceID)
			if err != nil {
				// Fail closed on the tick, not on prior progress: every workspace
				// swept before this one already committed its delete AND its
				// activity_log receipt in its own transaction (see SweepWorkspace),
				// so returning here loses no history — only this and later
				// workspaces are retried on the scheduler's own backoff.
				return HandlerResult{}, fmt.Errorf("sweep workspace %s: %w", util.UUIDToString(workspaceID), err)
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

// poolSweeper is the real, pool-backed provenanceRetentionSweeper.
type poolSweeper struct {
	pool *pgxpool.Pool
}

// provenanceRetentionReceiptDetails is the activity_log.details payload for
// provenanceExportRetentionSweptActivity. Field names are stable.
type provenanceRetentionReceiptDetails struct {
	DeletedCount int `json:"deleted_count"`
}

// SweepWorkspace deletes one workspace's expired provenance_export_log rows
// and writes its activity_log receipt in a single transaction: either both
// happen or neither does. A sweep that finds nothing to delete (deleted == 0)
// still commits cleanly but skips the receipt — an empty sweep is not an
// event worth a durable row, and this keeps activity_log from accumulating a
// no-op entry for every workspace on every hourly tick forever.
func (s *poolSweeper) SweepWorkspace(ctx context.Context, workspaceID pgtype.UUID) (int64, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	q := db.New(tx)

	deletedIDs, err := q.DeleteExpiredProvenanceExportLogsForWorkspace(ctx, workspaceID)
	if err != nil {
		return 0, fmt.Errorf("delete expired logs: %w", err)
	}

	if len(deletedIDs) > 0 {
		details, err := json.Marshal(provenanceRetentionReceiptDetails{DeletedCount: len(deletedIDs)})
		if err != nil {
			return 0, fmt.Errorf("marshal receipt details: %w", err)
		}
		if _, err := q.CreateActivity(ctx, db.CreateActivityParams{
			WorkspaceID: workspaceID,
			ActorType:   pgtype.Text{String: "system", Valid: true},
			Action:      provenanceExportRetentionSweptActivity,
			Details:     details,
		}); err != nil {
			// Fails the whole transaction: a deletion with no durable receipt is
			// exactly the gap this job exists to close (review finding B3), so
			// the deletion itself must not commit if its receipt cannot.
			return 0, fmt.Errorf("insert retention receipt: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit: %w", err)
	}
	return int64(len(deletedIDs)), nil
}

// workspaceMapKey returns a cheap, stable map key for a workspace UUID. It is
// NOT for display, logs, or error text: pgtype.UUID.Bytes are raw bytes, not
// UTF-8, and Postgres rejects most of them as invalid text (SQLSTATE 22021)
// — which is exactly how a prior version of this file broke
// sys_cron_executions.error_msg writes on sweep failure (review finding N4).
// Use util.UUIDToString for anything a human or the database will read.
func workspaceMapKey(id pgtype.UUID) string {
	return string(id.Bytes[:])
}
