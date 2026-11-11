package scheduler

import (
	"context"
	"time"
)

// AdmissionReconciler is the admission pass: lapsed reservations expire, held
// inputs without a run are parked, and every project with held runs is released.
type AdmissionReconciler interface {
	ReconcileAdmission(ctx context.Context) error
}

// RunAdmissionExpiryJob delivers the durable lease-expiry event for reserved
// runs (CHE-5213): a reservation nobody claimed returns to held and the slot
// goes to the next held run. The same tick retries a release whose best-effort
// call after a run's end failed, and parks held inputs left without a run. Every
// query is over the partial admission indexes, not a sweep of all runs.
func RunAdmissionExpiryJob(admission AdmissionReconciler) JobSpec {
	return JobSpec{
		Name: "run_admission_expiry", Cadence: 15 * time.Second, CatchUpMode: CatchUpLatestOnly, CatchUpWindow: time.Hour,
		RunTimeout: 30 * time.Second, StaleTimeout: time.Minute, HeartbeatInterval: 10 * time.Second,
		AllowStaleReentry: true, MaxAttempts: 1, Scopes: StaticScopes(ScopeGlobal),
		Handler: func(ctx context.Context, _ HandlerInput) (HandlerResult, error) {
			return HandlerResult{}, admission.ReconcileAdmission(ctx)
		},
	}
}
