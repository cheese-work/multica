package scheduler

import (
	"context"
	"time"
)

type ChatWakeupDispatcher interface{ Tick(context.Context) error }

// ChatWakeupJob fires due DM wakeups. Due times persist on the rows, so a
// missed bucket cannot drop a one-shot wakeup.
func ChatWakeupJob(dispatcher ChatWakeupDispatcher) JobSpec {
	return JobSpec{
		Name: "chat_wakeup_dispatch", Cadence: 30 * time.Second, CatchUpMode: CatchUpLatestOnly, CatchUpWindow: time.Hour,
		RunTimeout: 45 * time.Second, StaleTimeout: time.Minute, HeartbeatInterval: 10 * time.Second,
		AllowStaleReentry: true, MaxAttempts: 1, Scopes: StaticScopes(ScopeGlobal),
		Handler: func(ctx context.Context, _ HandlerInput) (HandlerResult, error) {
			return HandlerResult{}, dispatcher.Tick(ctx)
		},
	}
}
