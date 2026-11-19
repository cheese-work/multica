package scheduler

import (
	"context"
	"time"
)

type BlockedEntryLinkSweeper interface {
	SweepBlockedEntryLinks(context.Context) error
}

// BlockedEntryLinkSweepJob starts the blocked-step run for a blocked entry
// whose linked wakeup stopped being a live continuation (CHE-6973).
func BlockedEntryLinkSweepJob(sweeper BlockedEntryLinkSweeper) JobSpec {
	return JobSpec{
		Name: "blocked_entry_link_sweep", Cadence: time.Minute, CatchUpMode: CatchUpLatestOnly, CatchUpWindow: time.Minute,
		RunTimeout: time.Minute, StaleTimeout: 2 * time.Minute, HeartbeatInterval: 30 * time.Second,
		AllowStaleReentry: true, MaxAttempts: 1, Scopes: StaticScopes(ScopeGlobal),
		Handler: func(ctx context.Context, _ HandlerInput) (HandlerResult, error) {
			return HandlerResult{}, sweeper.SweepBlockedEntryLinks(ctx)
		},
	}
}
