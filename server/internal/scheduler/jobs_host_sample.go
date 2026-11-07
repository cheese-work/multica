package scheduler

import (
	"context"
	"fmt"
	"time"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Host sample jobs (CHE-2586 L2b). Names are stable audit keys.
const JobNameHostSampleRollup = "host_sample_rollup"

const (
	hostRollupCadence      = time.Hour
	hostRawRetention       = 7 * 24 * time.Hour
	hostRollupRetention    = 90 * 24 * time.Hour
	hostRollupLookback     = 48 * time.Hour
	hostRollupScheduleWait = 2 * time.Minute
)

// HostSampleRollupJob folds raw 1-minute host samples into hourly rows.
func HostSampleRollupJob(queries *db.Queries) JobSpec {
	return JobSpec{
		Name:              JobNameHostSampleRollup,
		Cadence:           hostRollupCadence,
		ScheduleDelay:     hostRollupScheduleWait,
		CatchUpMode:       CatchUpLatestOnly,
		CatchUpWindow:     24 * time.Hour,
		RunTimeout:        5 * time.Minute,
		StaleTimeout:      10 * time.Minute,
		HeartbeatInterval: 30 * time.Second,
		AllowStaleReentry: true,
		MaxAttempts:       3,
		RetryBackoff:      []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute},
		Scopes:            StaticScopes(ScopeGlobal),
		Handler:           makeHostRollupHandler(queries),
	}
}

func makeHostRollupHandler(queries *db.Queries) Handler {
	return func(ctx context.Context, in HandlerInput) (HandlerResult, error) {
		to := in.PlanTime.UTC().Truncate(hostRollupCadence) // complete hours only
		last, err := queries.MaxHostSampleHourlyBucket(ctx)
		if err != nil {
			return HandlerResult{}, fmt.Errorf("read newest host rollup: %w", err)
		}
		rows, err := queries.RollupHostSampleHourly(ctx, db.RollupHostSampleHourlyParams{
			FromTs: ts(rollupFrom(to, last, hostRollupLookback, hostRawRetention)),
			ToTs:   ts(to),
		})
		if err != nil {
			return HandlerResult{}, fmt.Errorf("rollup host samples: %w", err)
		}
		return HandlerResult{RowsAffected: rows}, nil
	}
}
