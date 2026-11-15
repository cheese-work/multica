package scheduler

import (
	"context"
	"time"
)

type MemberOwnedSweeper interface {
	SweepMemberOwnedIssues(context.Context) error
}

// MemberOwnedSweepJob returns open issues that sit with a member and have no
// open decision card to their last agent, once a day (CHE-5838).
func MemberOwnedSweepJob(sweeper MemberOwnedSweeper) JobSpec {
	return JobSpec{
		Name: "member_owned_issue_sweep", Cadence: 24 * time.Hour, CatchUpMode: CatchUpLatestOnly, CatchUpWindow: 24 * time.Hour,
		RunTimeout: 5 * time.Minute, StaleTimeout: 6 * time.Minute, HeartbeatInterval: 30 * time.Second,
		AllowStaleReentry: true, MaxAttempts: 1, Scopes: StaticScopes(ScopeGlobal),
		Handler: func(ctx context.Context, _ HandlerInput) (HandlerResult, error) {
			return HandlerResult{}, sweeper.SweepMemberOwnedIssues(ctx)
		},
	}
}
