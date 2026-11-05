package scheduler

import (
	"context"
	"time"
)

type StageQueueReleaser interface{ ReleaseStageQueues(context.Context) error }

// StageQueueReleaseJob starts the PRs a full stage queue refused once a slot
// frees (CHE-3611). A waiting PR holds no slot, so no completion hook is needed.
func StageQueueReleaseJob(releaser StageQueueReleaser) JobSpec {
	return JobSpec{
		Name: "stage_queue_release", Cadence: 30 * time.Second, CatchUpMode: CatchUpLatestOnly, CatchUpWindow: time.Hour,
		RunTimeout: 45 * time.Second, StaleTimeout: time.Minute, HeartbeatInterval: 10 * time.Second,
		AllowStaleReentry: true, MaxAttempts: 1, Scopes: StaticScopes(ScopeGlobal),
		Handler: func(ctx context.Context, _ HandlerInput) (HandlerResult, error) {
			return HandlerResult{}, releaser.ReleaseStageQueues(ctx)
		},
	}
}
