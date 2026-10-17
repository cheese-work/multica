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

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"
)

// lastPollStore is a claim store that answers "absent" and records when the
// acceptance poll last asked. The first poll is held until `holdUntil`, which
// parks the loop just short of its deadline: the position where the next step
// timer used to overshoot it.
type lastPollStore struct {
	*sharedDedupe
	holdUntil time.Time

	mu       sync.Mutex
	polls    int
	lastPoll time.Time
}

func (s *lastPollStore) ClaimState(ctx context.Context, key string) (claimState, error) {
	s.mu.Lock()
	s.polls++
	first := s.polls == 1
	s.lastPoll = time.Now()
	s.mu.Unlock()
	if first {
		time.Sleep(time.Until(s.holdUntil))
	}
	return s.sharedDedupe.ClaimState(ctx, key)
}

func (s *lastPollStore) lastPollAt() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastPoll
}

func TestConfirmInbox_LastWaitDoesNotRunPastTheDeadline(t *testing.T) {
	t.Parallel()
	const (
		window = 800 * time.Millisecond
		step   = window / 8
	)
	start := time.Now()
	store := &lastPollStore{sharedDedupe: newSharedDedupe(), holdUntil: start.Add(window - 2*time.Millisecond)}
	router := NewRelayOutbound(&fanoutRelay{}, store, RelayConfig{AcceptanceWindow: window}, slog.Default())

	if got := router.confirmInbox(context.Background(), "event-overshoot"); got != relayNoHolder {
		t.Fatalf("confirmInbox = %v, want relayNoHolder", got)
	}
	// The poll that follows the parked one belongs at the deadline. Running a
	// full step past it (about 100ms here) is the overshoot; half a step leaves
	// the assertion far from both outcomes.
	if over := store.lastPollAt().Sub(start.Add(window)); over > step/2 {
		t.Fatalf("the last acceptance poll ran %v past the %v deadline; the final wait must be cut to the time left", over, window)
	}
}
