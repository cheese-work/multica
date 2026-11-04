package service

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// A 429 / concurrency-limit failure is transient: it gets exactly one retry,
// deferred so it does not fire back into the same saturated provider.
func TestProviderCapacityRetrySchedule(t *testing.T) {
	const reason = "agent_error.provider_capacity_or_rate_limit"

	task := func(attempt, max int32) db.AgentTaskQueue {
		return db.AgentTaskQueue{
			Attempt:     attempt,
			MaxAttempts: max,
			IssueID:     pgtype.UUID{Bytes: [16]byte{1}, Valid: true},
		}
	}
	eligible := []struct {
		name    string
		attempt int32
		max     int32
		want    bool
	}{
		{"first run retries once", 1, 2, true},
		{"retry is the ceiling", 2, 2, false},
		{"max_attempts=1 disables retry", 1, 1, false},
	}
	for _, tc := range eligible {
		if got := retryEligible(reason, task(tc.attempt, tc.max)); got != tc.want {
			t.Errorf("%s: retryEligible = %v, want %v", tc.name, got, tc.want)
		}
	}
	if got := retryAttemptCeiling(reason, 2); got != 2 {
		t.Errorf("ceiling = %d, want 2 (no widening)", got)
	}
	if got := retryDelayForAttempt(reason, 1); got != providerCapacityRetryWait {
		t.Errorf("delay = %s, want %s", got, providerCapacityRetryWait)
	}
	if providerCapacityRetryWait < 10*time.Minute || providerCapacityRetryWait > 15*time.Minute {
		t.Errorf("providerCapacityRetryWait = %s, want 10-15m", providerCapacityRetryWait)
	}
}
