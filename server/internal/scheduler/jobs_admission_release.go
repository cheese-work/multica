package scheduler

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

type AdmissionDeferralReleaser interface {
	ReleaseAdmissionDeferrals(ctx context.Context, issueIDs []pgtype.UUID) error
}

// AdmissionDeferralReleaseJob is the fallback for a missed GitHub event
// (CHE-4651): every 15 minutes it judges all deferred reviewer runs again and
// starts the ones whose head is admissible or whose 24 h ran out.
func AdmissionDeferralReleaseJob(releaser AdmissionDeferralReleaser) JobSpec {
	return JobSpec{
		Name: "admission_deferral_release", Cadence: 15 * time.Minute, CatchUpMode: CatchUpLatestOnly, CatchUpWindow: time.Hour,
		RunTimeout: 5 * time.Minute, StaleTimeout: 6 * time.Minute, HeartbeatInterval: 30 * time.Second,
		AllowStaleReentry: true, MaxAttempts: 1, Scopes: StaticScopes(ScopeGlobal),
		Handler: func(ctx context.Context, _ HandlerInput) (HandlerResult, error) {
			return HandlerResult{}, releaser.ReleaseAdmissionDeferrals(ctx, nil)
		},
	}
}
