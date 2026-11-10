package scheduler

import (
	"context"
	"time"
)

type DMProgressRunner interface {
	Run(context.Context, time.Time) error
}

// DMProgressDispatchJob sends the DM progress cards that are due (CHE-3369 C3).
// The pending rows are the durable queue, so a missed tick only delays a card.
func DMProgressDispatchJob(dispatcher DMProgressRunner) JobSpec {
	return JobSpec{
		Name: "dm_progress_dispatch", Cadence: 30 * time.Second, CatchUpMode: CatchUpLatestOnly, CatchUpWindow: time.Hour,
		RunTimeout: 45 * time.Second, StaleTimeout: time.Minute, HeartbeatInterval: 10 * time.Second,
		AllowStaleReentry: true, MaxAttempts: 1, Scopes: StaticScopes(ScopeGlobal),
		Handler: func(ctx context.Context, _ HandlerInput) (HandlerResult, error) {
			return HandlerResult{}, dispatcher.Run(ctx, time.Now())
		},
	}
}
