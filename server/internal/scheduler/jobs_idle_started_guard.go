package scheduler

import (
	"context"
	"time"
)

type IdleStartedGuard interface {
	SweepIdleStartedIssues(context.Context) error
}

// IdleStartedGuardJob runs the idle started-issue guard (CHE-2609). The idle
// threshold is hours, so a five-minute cadence bounds the detection error.
func IdleStartedGuardJob(guard IdleStartedGuard) JobSpec {
	return JobSpec{
		Name: "issue_idle_started_guard", Cadence: 5 * time.Minute, CatchUpMode: CatchUpLatestOnly, CatchUpWindow: time.Hour,
		RunTimeout: 2 * time.Minute, StaleTimeout: 3 * time.Minute, HeartbeatInterval: 20 * time.Second,
		AllowStaleReentry: true, MaxAttempts: 1, Scopes: StaticScopes(ScopeGlobal),
		Handler: func(ctx context.Context, _ HandlerInput) (HandlerResult, error) {
			return HandlerResult{}, guard.SweepIdleStartedIssues(ctx)
		},
	}
}
