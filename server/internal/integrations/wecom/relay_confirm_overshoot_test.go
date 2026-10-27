package wecom

// relay_confirm_overshoot_test.go — the acceptance wait ends at its deadline.
//
// confirmInbox polls on a step timer and keeps the caller's context as a
// backstop a little past the deadline (window/8 + 1ms). A step timer started
// just before the deadline used to run a whole step past it, so the margin
// between that timer and the backstop shrank to the 1ms left by the last poll's
// distance from the deadline: any timer lateness (a loaded CI runner) let the
// backstop win the select, the verdict became relayUncertain, and the fallback
// bot was never offered the card. This pins the cause, not the symptom: the
// last wait is cut to the time left, so the final poll lands at the deadline.
//
// It runs in a synctest bubble: time is fake and moves only when every
// goroutine is blocked, so the poll instants are exact and no wall-clock sleep,
// scheduler delay or runner load can change the outcome.

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

// pollTimesStore is a claim store that answers "absent" and records when each
// acceptance poll asked. The first poll is held until `holdUntil`, which parks
// the loop just short of its deadline: the position where the next step timer
// used to overshoot it.
type pollTimesStore struct {
	*sharedDedupe
	holdUntil time.Time

	mu    sync.Mutex
	polls []time.Time
}

func (s *pollTimesStore) ClaimState(ctx context.Context, key string) (claimState, error) {
	s.mu.Lock()
	s.polls = append(s.polls, time.Now())
	first := len(s.polls) == 1
	s.mu.Unlock()
	if first {
		time.Sleep(time.Until(s.holdUntil))
	}
	return s.sharedDedupe.ClaimState(ctx, key)
}

func (s *pollTimesStore) pollTimes() []time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]time.Time(nil), s.polls...)
}

func TestConfirmInbox_LastWaitDoesNotRunPastTheDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const window = 800 * time.Millisecond
		start := time.Now()
		deadline := start.Add(window)
		// The first poll returns 2ms before the deadline, so the step timer that
		// follows is the one that used to run a whole step (100ms) past it.
		store := &pollTimesStore{sharedDedupe: newSharedDedupe(), holdUntil: deadline.Add(-2 * time.Millisecond)}
		router := NewRelayOutbound(&fanoutRelay{}, store, RelayConfig{AcceptanceWindow: window}, slog.Default())

		if got := router.confirmInbox(context.Background(), "event-overshoot"); got != relayNoHolder {
			t.Fatalf("confirmInbox = %v, want relayNoHolder", got)
		}

		polls := store.pollTimes()
		if len(polls) != 2 {
			t.Fatalf("the claim was polled %d times, want 2: the parked poll and the one at the deadline", len(polls))
		}
		if !polls[0].Equal(start) {
			t.Fatalf("first poll at %v, want the start of the wait", polls[0].Sub(start))
		}
		if last := polls[len(polls)-1]; !last.Equal(deadline) {
			t.Fatalf("the last acceptance poll ran at %v, want exactly the %v deadline; the final wait must be cut to the time left", last.Sub(start), window)
		}
		if elapsed := time.Since(start); elapsed != window {
			t.Fatalf("confirmInbox returned after %v, want exactly the %v window", elapsed, window)
		}
	})
}
