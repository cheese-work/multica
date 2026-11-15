package daemon

import (
	"context"
	"io"
	"sync"
	"time"

	"github.com/multica-ai/multica/server/internal/cli"
)

const (
	updateProgressInterval = time.Second
	updateProgressTimeout  = 5 * time.Second
)

// updateProgressReporter posts the update's download state to the server, at
// most once per updateProgressInterval and never on the download path. A report
// that cannot go out yet (one is in flight, or the interval has not passed) is
// kept as pending, latest wins, and goes out on the trailing edge. flush sends
// the last pending report before the terminal status. Failures are ignored
// silently — an old server may not store progress, and the terminal report
// carries the outcome.
type updateProgressReporter struct {
	send  func(ctx context.Context, payload map[string]any) error
	now   func() time.Time
	sleep func(time.Duration)

	mu         sync.Mutex
	sent       bool
	last       time.Time
	inflight   bool
	closed     bool
	timer      *time.Timer
	pending    map[string]any
	done, size int64
	wg         sync.WaitGroup
}

func newUpdateProgressReporter(send func(context.Context, map[string]any) error) *updateProgressReporter {
	return &updateProgressReporter{send: send, now: time.Now, sleep: time.Sleep}
}

// progress returns a silent cli.Progress wired to this reporter.
func (r *updateProgressReporter) progress() *cli.Progress {
	p := cli.NewProgress(io.Discard)
	p.OnUpdate = func(done, total int64) {
		r.mu.Lock()
		r.done, r.size = done, total
		r.mu.Unlock()
		r.report("downloading", done, total)
	}
	p.OnPhase = func(name string) {
		r.mu.Lock()
		done, size := r.done, r.size
		r.mu.Unlock()
		r.report(name, done, size)
	}
	return p
}

func (r *updateProgressReporter) report(phase string, done, total int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	r.pending = map[string]any{
		"status":   "running",
		"progress": map[string]any{"phase": phase, "bytes_done": done, "bytes_total": total},
	}
	r.dispatchLocked()
}

// dispatchLocked sends the pending report if none is in flight and the interval
// has passed; otherwise it arms one timer for the trailing edge.
func (r *updateProgressReporter) dispatchLocked() {
	if r.closed || r.pending == nil || r.inflight {
		return
	}
	if r.sent {
		if wait := updateProgressInterval - r.now().Sub(r.last); wait > 0 {
			if r.timer == nil {
				r.timer = time.AfterFunc(wait, func() {
					r.mu.Lock()
					r.timer = nil
					r.dispatchLocked()
					r.mu.Unlock()
				})
			}
			return
		}
	}
	payload := r.pending
	r.pending = nil
	r.sent, r.last, r.inflight = true, r.now(), true
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), updateProgressTimeout)
		defer cancel()
		_ = r.send(ctx, payload)
		r.mu.Lock()
		r.inflight = false
		r.dispatchLocked() // trailing edge; Add happens before Done, so wait() covers it
		r.mu.Unlock()
	}()
}

// wait blocks until the in-flight report, if any, has finished.
func (r *updateProgressReporter) wait() { r.wg.Wait() }

// flush stops further reports and delivers the pending one, if any, after the
// throttle interval so the last bytes_done and phase reach the server before the
// terminal status. Each send is bounded by updateProgressTimeout.
func (r *updateProgressReporter) flush() {
	r.mu.Lock()
	r.closed = true
	if r.timer != nil {
		r.timer.Stop()
		r.timer = nil
	}
	r.mu.Unlock()
	r.wg.Wait()

	r.mu.Lock()
	payload, wait := r.pending, time.Duration(0)
	r.pending = nil
	if r.sent {
		wait = updateProgressInterval - r.now().Sub(r.last)
	}
	r.mu.Unlock()
	if payload == nil {
		return
	}
	if wait > 0 {
		r.sleep(wait)
	}
	ctx, cancel := context.WithTimeout(context.Background(), updateProgressTimeout)
	defer cancel()
	_ = r.send(ctx, payload)
}
