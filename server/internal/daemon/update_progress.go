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
// most once per updateProgressInterval and never on the download path: a report
// that is still in flight, or too soon after the last, is dropped. Failures are
// ignored silently — an old server may not store progress, and the terminal
// report carries the outcome.
type updateProgressReporter struct {
	send func(ctx context.Context, payload map[string]any) error
	now  func() time.Time

	mu         sync.Mutex
	sent       bool
	last       time.Time
	inflight   bool
	done, size int64
	wg         sync.WaitGroup
}

func newUpdateProgressReporter(send func(context.Context, map[string]any) error) *updateProgressReporter {
	return &updateProgressReporter{send: send, now: time.Now}
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
	now := r.now()
	if r.inflight || (r.sent && now.Sub(r.last) < updateProgressInterval) {
		r.mu.Unlock()
		return
	}
	r.sent, r.last, r.inflight = true, now, true
	r.wg.Add(1)
	r.mu.Unlock()

	payload := map[string]any{
		"status":   "running",
		"progress": map[string]any{"phase": phase, "bytes_done": done, "bytes_total": total},
	}
	go func() {
		defer r.wg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), updateProgressTimeout)
		defer cancel()
		_ = r.send(ctx, payload)
		r.mu.Lock()
		r.inflight = false
		r.mu.Unlock()
	}()
}

// wait blocks until the in-flight report, if any, has finished.
func (r *updateProgressReporter) wait() { r.wg.Wait() }
