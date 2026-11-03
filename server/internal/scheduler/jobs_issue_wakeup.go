package scheduler

import (
	"context"
	"time"
)

type IssueWakeupDispatcher interface{ Tick(context.Context) error }

// Receipts and due times persist independently of the scheduler's plan bucket.
// A missed bucket therefore cannot drop a one-shot wakeup.
func IssueWakeupJob(dispatcher IssueWakeupDispatcher) JobSpec {
	return JobSpec{
		Name: "issue_wakeup_dispatch", Cadence: 30 * time.Second, CatchUpMode: CatchUpLatestOnly, CatchUpWindow: time.Hour,
		RunTimeout: 45 * time.Second, StaleTimeout: time.Minute, HeartbeatInterval: 10 * time.Second,
		AllowStaleReentry: true, MaxAttempts: 1, Scopes: StaticScopes(ScopeGlobal),
		Handler: func(ctx context.Context, _ HandlerInput) (HandlerResult, error) {
			return HandlerResult{}, dispatcher.Tick(ctx)
		},
	}
}

type WakeupDefinitionActivator interface{ RunWakeupActivation(context.Context) error }

// WakeupDefinitionActivationJob advances the legacy backfill that opens scoped
// definition writes (CHE-1082). Progress is persisted per page, so a missed or
// failed tick loses nothing, and once the activation is complete a tick is one
// cheap read. A failure is returned so it is logged and retried next tick.
func WakeupDefinitionActivationJob(activator WakeupDefinitionActivator) JobSpec {
	return JobSpec{
		Name: "wakeup_definition_activation", Cadence: 30 * time.Second, CatchUpMode: CatchUpLatestOnly, CatchUpWindow: time.Hour,
		RunTimeout: 45 * time.Second, StaleTimeout: time.Minute, HeartbeatInterval: 10 * time.Second,
		AllowStaleReentry: true, MaxAttempts: 1, Scopes: StaticScopes(ScopeGlobal),
		Handler: func(ctx context.Context, _ HandlerInput) (HandlerResult, error) {
			return HandlerResult{}, activator.RunWakeupActivation(ctx)
		},
	}
}
