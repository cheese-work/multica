package daemon

import (
	"context"
	"errors"
	"sync"
	"time"
)

// runTracker counts the agent processes a run has live, so a fence can stop
// the run and know it exited. Every executeAndDrain of the run registers here
// through its context: the first launch, the fresh-session retry and the
// delivery repair.
type runTracker struct {
	mu      sync.Mutex
	n       int
	waiters []chan struct{}
	sid     string // the latest session id a turn of the run revealed
}

// noteRunSession records the session id a turn revealed, so a fence that
// cancels the turn before it reports a result can still resume it.
func noteRunSession(ctx context.Context, sid string) {
	if t, _ := ctx.Value(runTrackerKey{}).(*runTracker); t != nil && sid != "" {
		t.mu.Lock()
		t.sid = sid
		t.mu.Unlock()
	}
}

func (t *runTracker) session() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.sid
}

type runTrackerKey struct{}

func withRunTracker(ctx context.Context, t *runTracker) context.Context {
	return context.WithValue(ctx, runTrackerKey{}, t)
}

// enterRun registers one live agent process and returns the func that ends it.
// With no tracker in ctx it does nothing.
func enterRun(ctx context.Context) func() {
	t, _ := ctx.Value(runTrackerKey{}).(*runTracker)
	if t == nil {
		return func() {}
	}
	t.mu.Lock()
	t.n++
	t.mu.Unlock()
	return func() {
		t.mu.Lock()
		defer t.mu.Unlock()
		if t.n--; t.n == 0 {
			for _, w := range t.waiters {
				close(w)
			}
			t.waiters = nil
		}
	}
}

// idle waits until no agent process is live. An error means one may still be.
func (t *runTracker) idle(timeout time.Duration) error {
	t.mu.Lock()
	if t.n == 0 {
		t.mu.Unlock()
		return nil
	}
	w := make(chan struct{})
	t.waiters = append(t.waiters, w)
	t.mu.Unlock()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-w:
		return nil
	case <-timer.C:
		return errors.New("the run's agent process did not exit")
	}
}
