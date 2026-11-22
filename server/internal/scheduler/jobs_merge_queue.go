package scheduler

import (
	"context"
	"time"
)

// MergeQueueSweeper binds pending dequeue intake rows to their queue builds.
type MergeQueueSweeper interface {
	Sweep(context.Context) (int, error)
}

// MergeQueueSweepJob settles pending merge-queue intake rows. The rows are the
// durable queue, so a crash or a deferred GitHub budget only delays a sweep.
func MergeQueueSweepJob(sweeper MergeQueueSweeper) JobSpec {
	return JobSpec{
		Name: "merge_queue_intake_sweep", Cadence: time.Minute, CatchUpMode: CatchUpLatestOnly, CatchUpWindow: time.Hour,
		RunTimeout: 50 * time.Second, StaleTimeout: 2 * time.Minute, HeartbeatInterval: 10 * time.Second,
		AllowStaleReentry: true, MaxAttempts: 1, Scopes: StaticScopes(ScopeGlobal),
		Handler: func(ctx context.Context, _ HandlerInput) (HandlerResult, error) {
			_, err := sweeper.Sweep(ctx)
			return HandlerResult{}, err
		},
	}
}
