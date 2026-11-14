package scheduler

import (
	"context"
	"time"
)

type ReleaseReceiptSweeper interface {
	SweepReleaseReceipts(context.Context) error
}

// ReleaseReceiptSweepJob settles release receipts that were stored but not
// acted on (CHE-4937): GitHub does not redeliver a failed webhook.
func ReleaseReceiptSweepJob(sweeper ReleaseReceiptSweeper) JobSpec {
	return JobSpec{
		Name: "github_release_receipt_sweep", Cadence: time.Minute, CatchUpMode: CatchUpLatestOnly, CatchUpWindow: time.Hour,
		RunTimeout: 2 * time.Minute, StaleTimeout: 3 * time.Minute, HeartbeatInterval: 20 * time.Second,
		AllowStaleReentry: true, MaxAttempts: 1, Scopes: StaticScopes(ScopeGlobal),
		Handler: func(ctx context.Context, _ HandlerInput) (HandlerResult, error) {
			return HandlerResult{}, sweeper.SweepReleaseReceipts(ctx)
		},
	}
}
