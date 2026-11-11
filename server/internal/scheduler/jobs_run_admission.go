package scheduler

import (
	"context"
	"time"
)

// AdmissionExpirer delivers the lease-expiry events that are due.
type AdmissionExpirer interface {
	ExpireDueReservations(ctx context.Context) (int, error)
}

// RunAdmissionExpiryJob delivers the durable lease-expiry event of a reserved
// run (CHE-5213): the reservation stores its own expiry time, and a reservation
// nobody claimed returns to held and the slot goes to the next held run. The
// query reads only the partial reserved-run index. Release is otherwise by
// event (a run ending, unpause, a WIP-limit or priority change), and the
// full pass runs once at startup, not on a timer.
func RunAdmissionExpiryJob(admission AdmissionExpirer) JobSpec {
	return JobSpec{
		Name: "run_admission_expiry", Cadence: 15 * time.Second, CatchUpMode: CatchUpLatestOnly, CatchUpWindow: time.Hour,
		RunTimeout: 30 * time.Second, StaleTimeout: time.Minute, HeartbeatInterval: 10 * time.Second,
		AllowStaleReentry: true, MaxAttempts: 1, Scopes: StaticScopes(ScopeGlobal),
		Handler: func(ctx context.Context, _ HandlerInput) (HandlerResult, error) {
			n, err := admission.ExpireDueReservations(ctx)
			return HandlerResult{RowsAffected: int64(n)}, err
		},
	}
}

// WriteEventPurger drops finished write events past their age.
type WriteEventPurger interface {
	PurgeWriteEvents(ctx context.Context) (int64, error)
}

// WriteEventRetentionJob purges stable-event-id write records (CHE-5213).
func WriteEventRetentionJob(purger WriteEventPurger) JobSpec {
	return JobSpec{
		Name: "write_event_retention", Cadence: time.Hour, ScheduleDelay: 5 * time.Minute,
		CatchUpMode: CatchUpLatestOnly, CatchUpWindow: 24 * time.Hour,
		RunTimeout: 5 * time.Minute, StaleTimeout: 10 * time.Minute, HeartbeatInterval: 30 * time.Second,
		AllowStaleReentry: true, MaxAttempts: 1, Scopes: StaticScopes(ScopeGlobal),
		Handler: func(ctx context.Context, _ HandlerInput) (HandlerResult, error) {
			n, err := purger.PurgeWriteEvents(ctx)
			return HandlerResult{RowsAffected: n}, err
		},
	}
}
