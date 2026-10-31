package scheduler

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Ops page jobs (CHE-2586 L1). Names are stable audit keys.
const (
	JobNameOpsSnapshotSampler = "ops_snapshot_sampler"
	JobNameOpsSnapshotRollup  = "ops_snapshot_rollup"
	JobNameOpsRetention       = "ops_retention"
)

const (
	opsSamplerCadence = 30 * time.Second
	opsRollupCadence  = 5 * time.Minute
	// opsRollupLookback is how far back each rollup run recomputes. The upsert
	// is idempotent, so a late sample or a run missed by downtime is folded in
	// on the next run while its raw rows still exist (raw retention is 7 d).
	opsRollupLookback = 48 * time.Hour

	opsRawRetention    = 7 * 24 * time.Hour
	opsRollupRetention = 35 * 24 * time.Hour
)

func ts(t time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: t, Valid: true} }

// OpsSnapshotSamplerJob writes one ops_snapshot row per machine key every
// 30 s. The scheduler's (job, scope, plan_time) lease plus the table's unique
// key make a second server instance a no-op. The manager must tick faster
// than the cadence (main.go sets 10 s) or a tick landing just past a boundary
// would skip that plan.
func OpsSnapshotSamplerJob(queries *db.Queries) JobSpec {
	return JobSpec{
		Name:              JobNameOpsSnapshotSampler,
		Cadence:           opsSamplerCadence,
		CatchUpMode:       CatchUpLatestOnly,
		CatchUpWindow:     2 * time.Minute,
		RunTimeout:        25 * time.Second,
		StaleTimeout:      time.Minute,
		HeartbeatInterval: 10 * time.Second,
		AllowStaleReentry: true,
		MaxAttempts:       1,
		Scopes:            StaticScopes(ScopeGlobal),
		Handler:           makeOpsSamplerHandler(queries),
	}
}

func makeOpsSamplerHandler(queries *db.Queries) Handler {
	return func(ctx context.Context, in HandlerInput) (HandlerResult, error) {
		rows, err := queries.InsertOpsSnapshot(ctx, ts(in.PlanTime))
		if err != nil {
			return HandlerResult{}, fmt.Errorf("insert ops snapshot: %w", err)
		}
		return HandlerResult{RowsAffected: rows}, nil
	}
}

// OpsSnapshotRollupJob folds raw snapshots into 5-minute intervals.
func OpsSnapshotRollupJob(queries *db.Queries) JobSpec {
	return JobSpec{
		Name:              JobNameOpsSnapshotRollup,
		Cadence:           opsRollupCadence,
		ScheduleDelay:     30 * time.Second,
		CatchUpMode:       CatchUpLatestOnly,
		CatchUpWindow:     time.Hour,
		RunTimeout:        5 * time.Minute,
		StaleTimeout:      10 * time.Minute,
		HeartbeatInterval: 30 * time.Second,
		AllowStaleReentry: true,
		MaxAttempts:       3,
		RetryBackoff:      []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute},
		Scopes:            StaticScopes(ScopeGlobal),
		Handler:           makeOpsRollupHandler(queries),
	}
}

func makeOpsRollupHandler(queries *db.Queries) Handler {
	return func(ctx context.Context, in HandlerInput) (HandlerResult, error) {
		to := in.PlanTime.UTC().Truncate(opsRollupCadence) // complete intervals only
		rows, err := queries.RollupOpsSnapshot5m(ctx, db.RollupOpsSnapshot5mParams{
			FromTs: ts(to.Add(-opsRollupLookback)),
			ToTs:   ts(to),
		})
		if err != nil {
			return HandlerResult{}, fmt.Errorf("rollup ops snapshots: %w", err)
		}
		return HandlerResult{RowsAffected: rows}, nil
	}
}

// OpsRetentionJob deletes raw snapshots after 7 d, rollups and requeue events
// after 35 d, and the sampler's own lease rows after 7 d.
func OpsRetentionJob(queries *db.Queries) JobSpec {
	return JobSpec{
		Name:              JobNameOpsRetention,
		Cadence:           time.Hour,
		ScheduleDelay:     5 * time.Minute,
		CatchUpMode:       CatchUpLatestOnly,
		CatchUpWindow:     24 * time.Hour,
		RunTimeout:        10 * time.Minute,
		StaleTimeout:      15 * time.Minute,
		HeartbeatInterval: 30 * time.Second,
		AllowStaleReentry: true,
		MaxAttempts:       3,
		RetryBackoff:      []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute},
		Scopes:            StaticScopes(ScopeGlobal),
		Handler:           makeOpsRetentionHandler(queries, time.Now),
	}
}

func makeOpsRetentionHandler(queries *db.Queries, now func() time.Time) Handler {
	return func(ctx context.Context, _ HandlerInput) (HandlerResult, error) {
		raw, rollup := ts(now().Add(-opsRawRetention)), ts(now().Add(-opsRollupRetention))
		deletes := []struct {
			what string
			run  func() (int64, error)
		}{
			{"raw snapshots", func() (int64, error) { return queries.DeleteOpsSnapshotBefore(ctx, raw) }},
			{"snapshot rollups", func() (int64, error) { return queries.DeleteOpsSnapshot5mBefore(ctx, rollup) }},
			{"task events", func() (int64, error) { return queries.DeleteAgentTaskEventBefore(ctx, rollup) }},
			{"sampler leases", func() (int64, error) { return queries.DeleteOpsSamplerCronExecutionsBefore(ctx, raw) }},
		}
		var total int64
		for _, d := range deletes {
			n, err := d.run()
			if err != nil {
				return HandlerResult{}, fmt.Errorf("delete ops %s: %w", d.what, err)
			}
			total += n
		}
		return HandlerResult{RowsAffected: total}, nil
	}
}
