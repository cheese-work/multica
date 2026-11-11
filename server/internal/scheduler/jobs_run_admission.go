package scheduler

import (
	"context"
	"time"
)

// ReservationExpirer expires reservations whose lease lapsed unclaimed.
type ReservationExpirer interface {
	ExpireDueReservations(ctx context.Context) (int, error)
}

// RunAdmissionExpiryJob delivers the durable lease-expiry event for reserved
// runs (CHE-5213): a reservation nobody claimed returns to held and the slot
// goes to the next held run. It is a due-row query over reservation_expires_at,
// not a sweep of every run.
func RunAdmissionExpiryJob(expirer ReservationExpirer) JobSpec {
	return JobSpec{
		Name: "run_admission_expiry", Cadence: 15 * time.Second, CatchUpMode: CatchUpLatestOnly, CatchUpWindow: time.Hour,
		RunTimeout: 30 * time.Second, StaleTimeout: time.Minute, HeartbeatInterval: 10 * time.Second,
		AllowStaleReentry: true, MaxAttempts: 1, Scopes: StaticScopes(ScopeGlobal),
		Handler: func(ctx context.Context, _ HandlerInput) (HandlerResult, error) {
			_, err := expirer.ExpireDueReservations(ctx)
			return HandlerResult{}, err
		},
	}
}
