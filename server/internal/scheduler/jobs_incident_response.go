package scheduler

import (
	"context"
	"time"
)

type IncidentResponder interface {
	SweepIncidents(context.Context) error
}

// IncidentResponseJob reads recent failed runs and opens, updates or closes
// provider incidents (CHE-9892). The incident window is 30 minutes, so a
// ten-minute cadence bounds the detection delay.
func IncidentResponseJob(responder IncidentResponder) JobSpec {
	return JobSpec{
		Name: "incident_response", Cadence: 10 * time.Minute, CatchUpMode: CatchUpLatestOnly, CatchUpWindow: time.Hour,
		RunTimeout: 2 * time.Minute, StaleTimeout: 3 * time.Minute, HeartbeatInterval: 20 * time.Second,
		AllowStaleReentry: true, MaxAttempts: 1, Scopes: StaticScopes(ScopeGlobal),
		Handler: func(ctx context.Context, _ HandlerInput) (HandlerResult, error) {
			return HandlerResult{}, responder.SweepIncidents(ctx)
		},
	}
}
