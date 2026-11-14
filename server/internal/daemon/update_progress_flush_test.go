package daemon

import (
	"context"
	"sync"
	"testing"
	"time"
)

// flushRig is a reporter on a fake clock; sleep advances the clock.
type flushRig struct {
	r    *updateProgressReporter
	mu   sync.Mutex
	now  time.Time
	sent []map[string]any
	at   []time.Time
}

func newFlushRig(send func(*flushRig)) *flushRig {
	g := &flushRig{now: time.Unix(1000, 0)}
	g.r = newUpdateProgressReporter(func(_ context.Context, p map[string]any) error {
		start := g.r.now()
		if send != nil {
			send(g)
		}
		g.mu.Lock()
		defer g.mu.Unlock()
		g.sent = append(g.sent, p)
		g.at = append(g.at, start)
		return nil
	})
	g.r.now = func() time.Time { g.mu.Lock(); defer g.mu.Unlock(); return g.now }
	g.r.sleep = func(d time.Duration) { g.advance(d) }
	return g
}

func (g *flushRig) advance(d time.Duration) { g.mu.Lock(); g.now = g.now.Add(d); g.mu.Unlock() }

func (g *flushRig) last(t *testing.T) map[string]any {
	t.Helper()
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.sent) == 0 {
		t.Fatal("no report sent")
	}
	return g.sent[len(g.sent)-1]["progress"].(map[string]any)
}

func TestFlush_DeliversFinalByteCountInsideWindow(t *testing.T) {
	g := newFlushRig(nil)
	for i := int64(10); i <= 100; i += 10 { // one 1 s window, last = bytes_total
		g.r.report("downloading", i, 100)
		g.advance(50 * time.Millisecond)
	}
	g.r.flush()

	if got := g.last(t)["bytes_done"]; got != int64(100) {
		t.Fatalf("final bytes_done = %v, want 100", got)
	}
}

func TestFlush_DeliversPhaseChangeBeforeTerminal(t *testing.T) {
	g := newFlushRig(nil)
	g.r.report("downloading", 100, 100)
	g.advance(10 * time.Millisecond)
	g.r.report("Installed", 100, 100)
	g.r.flush()

	if got := g.last(t)["phase"]; got != "Installed" {
		t.Fatalf("final phase = %v, want Installed", got)
	}
}

func TestFlush_NoReportAfterFlush(t *testing.T) {
	g := newFlushRig(nil)
	g.r.report("downloading", 1, 100)
	g.r.flush()
	n := len(g.sent)
	g.r.report("downloading", 2, 100)
	g.r.wait()
	if len(g.sent) != n {
		t.Fatalf("report after flush was sent")
	}
}

func TestReporter_NoTwoReportsCloserThanIntervalNorConcurrent(t *testing.T) {
	var cur, max int
	release := make(chan struct{})
	g := newFlushRig(func(g *flushRig) {
		g.mu.Lock()
		cur++
		if cur > max {
			max = cur
		}
		g.mu.Unlock()
		<-release
		g.mu.Lock()
		cur--
		g.mu.Unlock()
	})
	g.r.report("downloading", 1, 100)
	for i := int64(2); i <= 20; i++ { // dropped/pending while the first is in flight
		g.r.report("downloading", i, 100)
	}
	g.advance(300 * time.Millisecond)
	g.r.report("downloading", 99, 100)
	go func() {
		for range 3 {
			release <- struct{}{}
		}
	}()
	g.r.flush()

	g.mu.Lock()
	defer g.mu.Unlock()
	if max != 1 {
		t.Fatalf("max concurrent sends = %d, want 1", max)
	}
	for i := 1; i < len(g.at); i++ {
		if d := g.at[i].Sub(g.at[i-1]); d < updateProgressInterval {
			t.Fatalf("reports %d and %d are %v apart, want >= 1s", i-1, i, d)
		}
	}
	if len(g.sent) != 2 {
		t.Fatalf("sent %d reports, want 2 (first, then the latest pending)", len(g.sent))
	}
}

func TestReporter_TrailingEdgeSendsPendingWhenInflightFinishesAfterInterval(t *testing.T) {
	first := true
	release := make(chan struct{})
	g := newFlushRig(func(g *flushRig) {
		g.mu.Lock()
		blocked := first
		first = false
		g.mu.Unlock()
		if blocked {
			<-release
		}
	})
	g.r.report("downloading", 1, 100)
	g.r.report("downloading", 100, 100) // pending while the first is in flight
	g.advance(2 * updateProgressInterval)
	close(release)
	g.r.wait()

	if got := g.last(t)["bytes_done"]; got != int64(100) {
		t.Fatalf("trailing bytes_done = %v, want 100", got)
	}
}
