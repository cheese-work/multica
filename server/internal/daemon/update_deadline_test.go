package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/pkg/protocol"
)

// Timings are scaled down about 1000x: the 60 s margin is 0 and the 2-min
// and 5-min figures of the acceptance list are 120 ms and 300 ms.

func withDeadlineMargin(t *testing.T, m time.Duration) {
	t.Helper()
	prev := updateDeadlineMargin
	updateDeadlineMargin = m
	t.Cleanup(func() { updateDeadlineMargin = prev })
}

func msPtr(d time.Duration) *int64 { v := d.Milliseconds(); return &v }

// resultLog records every update report the fake server receives.
type resultLog struct {
	mu   sync.Mutex
	list []map[string]any
}

func (l *resultLog) add(p map[string]any) { l.mu.Lock(); l.list = append(l.list, p); l.mu.Unlock() }

// terminal returns the `status/reason` of every non-running report.
func (l *resultLog) terminal() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for _, p := range l.list {
		st, _ := p["status"].(string)
		if st == "running" {
			continue
		}
		r, _ := p["reason"].(string)
		out = append(out, st+"/"+r)
	}
	return out
}

func (l *resultLog) waitTerminal(t *testing.T, within time.Duration, want string) {
	t.Helper()
	end := time.Now().Add(within)
	for time.Now().Before(end) {
		for _, got := range l.terminal() {
			if got == want {
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("result %q not delivered within %s; got %v", want, within, l.terminal())
}

// deadlineDaemon is a daemon whose report endpoint records payloads and, via
// gate, may stall them. restarts counts triggerRestart's cancel.
func deadlineDaemon(t *testing.T, gate func(w http.ResponseWriter, r *http.Request)) (*Daemon, *resultLog, *atomic.Int32) {
	t.Helper()
	withDeadlineMargin(t, 0)
	withOutboxTimings(t, 50*time.Millisecond, 2*time.Second, time.Second)
	log := &resultLog{}
	d, _ := updateReportDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		if gate != nil {
			gate(w, r)
		}
		var p map[string]any
		if err := json.NewDecoder(r.Body).Decode(&p); err == nil {
			log.add(p)
		}
		w.WriteHeader(http.StatusOK)
	})
	var restarts atomic.Int32
	d.cancelFunc = func() { restarts.Add(1) }
	t.Cleanup(func() { d.updateOutbox.Wait() })
	return d, log, &restarts
}

func barrierHeld(d *Daemon) bool {
	d.claimMu.Lock()
	defer d.claimMu.Unlock()
	return d.pauseClaims
}

func runUpdateAsync(d *Daemon, anchor time.Time, u *PendingUpdate) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		d.handleUpdate(context.Background(), "rt-1", u, anchor)
	}()
	return done
}

// 1. A delayed ack: no barrier, a late_delivery result, claims never stop.
func TestUpdateDeadline_LateDelivery(t *testing.T) {
	d, log, _ := deadlineDaemon(t, nil)
	d.runUpdateFn = func(context.Context, string) (string, error) {
		t.Error("runUpdateFn must not run for a late delivery")
		return "", nil
	}
	anchor := time.Now().Add(-200 * time.Millisecond)
	d.handleUpdate(context.Background(), "rt-1",
		&PendingUpdate{ID: "u1", TargetVersion: "v1", StartWithinMs: msPtr(100 * time.Millisecond), FinishWithinMs: msPtr(time.Second)}, anchor)
	log.waitTerminal(t, time.Second, "failed/late_delivery")
	if barrierHeld(d) || d.updating.Load() || !d.tryEnterClaim() {
		t.Fatal("a late delivery must leave claims alone")
	}
}

// 2. A slow update: claims return at the deadline, no restart, a deadline result.
func TestUpdateDeadline_SlowUpdate(t *testing.T) {
	d, log, restarts := deadlineDaemon(t, nil)
	d.runUpdateFn = func(context.Context, string) (string, error) {
		time.Sleep(250 * time.Millisecond)
		return "ok", nil
	}
	done := runUpdateAsync(d, time.Now(), &PendingUpdate{ID: "u1", TargetVersion: "v1", FinishWithinMs: msPtr(100 * time.Millisecond)})
	time.Sleep(50 * time.Millisecond)
	if !barrierHeld(d) {
		t.Fatal("barrier must be held while the update runs")
	}
	time.Sleep(100 * time.Millisecond)
	if barrierHeld(d) {
		t.Fatal("barrier must be released at the deadline")
	}
	log.waitTerminal(t, time.Second, "failed/deadline")
	waitDone(t, done, time.Second, "handleUpdate")
	d.updateOutbox.Wait()
	if got := log.terminal(); len(got) != 1 {
		t.Fatalf("terminal results = %v, want only failed/deadline", got)
	}
	if restarts.Load() != 0 {
		t.Fatal("a late return must not restart")
	}
}

// 3. A stuck update that ignores its context: claims return, `updating` stays held.
func TestUpdateDeadline_StuckUpdate(t *testing.T) {
	d, log, _ := deadlineDaemon(t, nil)
	stuck := make(chan struct{})
	d.runUpdateFn = func(context.Context, string) (string, error) { <-stuck; return "ok", nil }
	done := runUpdateAsync(d, time.Now(), &PendingUpdate{ID: "u1", TargetVersion: "v1", FinishWithinMs: msPtr(100 * time.Millisecond)})
	time.Sleep(160 * time.Millisecond)
	if barrierHeld(d) {
		t.Fatal("barrier must be released at the deadline")
	}
	if !d.updating.Load() {
		t.Fatal("updating must stay held until the call returns")
	}
	log.waitTerminal(t, time.Second, "failed/deadline")
	close(stuck)
	waitDone(t, done, time.Second, "handleUpdate")
	if d.updating.Load() {
		t.Fatal("updating must be released when the call returns")
	}
}

// 4. A report endpoint that stalls after the deadline cannot hold the barrier,
// and the deadline result still lands.
func TestUpdateDeadline_SlowReportEndpoint(t *testing.T) {
	start := time.Now()
	const deadline, stall = 100 * time.Millisecond, 120 * time.Millisecond
	d, log, _ := deadlineDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		if el := time.Since(start); el >= deadline && el < deadline+stall {
			select {
			case <-r.Context().Done():
			case <-time.After(deadline + stall - el):
			}
		}
	})
	d.runUpdateFn = func(ctx context.Context, _ string) (string, error) { <-ctx.Done(); return "", ctx.Err() }
	done := runUpdateAsync(d, start, &PendingUpdate{ID: "u1", TargetVersion: "v1", FinishWithinMs: msPtr(deadline)})
	time.Sleep(deadline + 30*time.Millisecond)
	if barrierHeld(d) {
		t.Fatal("barrier must be released at the deadline")
	}
	log.waitTerminal(t, deadline+300*time.Millisecond-time.Since(start), "failed/deadline")
	waitDone(t, done, time.Second, "handleUpdate")
}

// 5. Both the update context and the heartbeat context end at the deadline:
// the deadline result is still delivered.
func TestUpdateDeadline_ContextAwareTransport(t *testing.T) {
	d, log, _ := deadlineDaemon(t, nil)
	d.runUpdateFn = func(ctx context.Context, _ string) (string, error) { <-ctx.Done(); return "", ctx.Err() }
	anchor := time.Now()
	hb, cancel := context.WithDeadline(context.Background(), anchor.Add(100*time.Millisecond))
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		d.handleUpdate(hb, "rt-1", &PendingUpdate{ID: "u1", TargetVersion: "v1", FinishWithinMs: msPtr(100 * time.Millisecond)}, anchor)
	}()
	log.waitTerminal(t, time.Second, "failed/deadline")
	waitDone(t, done, time.Second, "handleUpdate")
	if barrierHeld(d) {
		t.Fatal("barrier must be released")
	}
}

// 6. Install succeeds just before the deadline and `completed` never answers:
// the barrier is released at the deadline and no restart happens.
func TestUpdateDeadline_CompletedBlockedPastDeadline(t *testing.T) {
	var completedSeen atomic.Bool
	d, _, restarts := deadlineDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		var p map[string]any
		b := json.NewDecoder(r.Body)
		_ = b.Decode(&p)
		if p["status"] == "completed" {
			completedSeen.Store(true)
			<-r.Context().Done()
		}
	})
	updateCompletedWait = 2 * time.Second // the deadline, not this, must end the wait
	d.runUpdateFn = func(context.Context, string) (string, error) { time.Sleep(10 * time.Millisecond); return "ok", nil }
	done := runUpdateAsync(d, time.Now(), &PendingUpdate{ID: "u1", TargetVersion: "v1", FinishWithinMs: msPtr(100 * time.Millisecond)})
	time.Sleep(60 * time.Millisecond)
	if !barrierHeld(d) {
		t.Fatal("barrier is held while waiting for the completed delivery")
	}
	time.Sleep(80 * time.Millisecond)
	if barrierHeld(d) {
		t.Fatal("barrier must be released at the deadline")
	}
	waitDone(t, done, time.Second, "handleUpdate")
	if restarts.Load() != 0 {
		t.Fatal("no restart after the deadline")
	}
	if !completedSeen.Load() {
		t.Fatal("completed was never sent")
	}
}

// 7. A request without the fields keeps today's 3-min bound.
func TestUpdateDeadline_ManualKeepsThreeMinuteBound(t *testing.T) {
	d, _, _ := deadlineDaemon(t, nil)
	var got time.Duration
	d.runUpdateFn = func(ctx context.Context, _ string) (string, error) {
		dl, ok := ctx.Deadline()
		if !ok {
			t.Error("update context has no deadline")
		}
		got = time.Until(dl)
		return "", context.Canceled
	}
	d.handleUpdate(context.Background(), "rt-1", &PendingUpdate{ID: "u1", TargetVersion: "v1"}, time.Now())
	d.updateOutbox.Wait()
	if got < manualUpdateBound-5*time.Second || got > manualUpdateBound {
		t.Fatalf("deadline in %s, want about %s", got, manualUpdateBound)
	}
}

// 8. A daemon clock far behind the server's: the pause ends at anchor+expires_in.
func TestUpdateDeadline_PauseEndsAtAnchorPlusExpiresIn(t *testing.T) {
	d := &Daemon{}
	anchor := time.Now()
	d.applyHeartbeatPause(&protocol.DaemonHeartbeatPaused{
		Reason: "x", ExpiresAt: anchor.Add(5 * time.Minute).UTC().Format(time.RFC3339), ExpiresInMs: msPtr(100 * time.Millisecond),
	}, anchor)
	if d.heldPauseNow() == nil {
		t.Fatal("pause must be held before expires_in")
	}
	time.Sleep(150 * time.Millisecond)
	if d.heldPauseNow() != nil {
		t.Fatal("pause must end at anchor+expires_in_ms, not at the skewed expires_at")
	}
}

// 9. An update that finishes before the deadline restarts as today.
func TestUpdateDeadline_FinishesBeforeDeadlineRestarts(t *testing.T) {
	d, log, restarts := deadlineDaemon(t, nil)
	d.runUpdateFn = func(context.Context, string) (string, error) { return "ok", nil }
	d.handleUpdate(context.Background(), "rt-1", &PendingUpdate{ID: "u1", TargetVersion: "v1", FinishWithinMs: msPtr(time.Second)}, time.Now())
	d.updateOutbox.Wait()
	if restarts.Load() != 1 {
		t.Fatal("daemon did not restart")
	}
	if got := log.terminal(); len(got) != 1 || got[0] != "completed/" {
		t.Fatalf("terminal results = %v, want completed", got)
	}
}

// Typed reasons on the other failures.
func TestUpdateDeadline_TypedReasons(t *testing.T) {
	d, log, _ := deadlineDaemon(t, nil)
	d.runUpdateFn = func(context.Context, string) (string, error) { return "", context.Canceled }
	d.handleUpdate(context.Background(), "rt-1", &PendingUpdate{ID: "u1", TargetVersion: "v1"}, time.Now())
	d.updateOutbox.Wait()
	d.activeTasks.Store(1)
	d.handleUpdate(context.Background(), "rt-1", &PendingUpdate{ID: "u2", TargetVersion: "v1"}, time.Now())
	d.updateOutbox.Wait()
	got := log.terminal()
	if len(got) != 2 || got[0] != "failed/error" || got[1] != "failed/busy" {
		t.Fatalf("terminal results = %v, want [failed/error failed/busy]", got)
	}
}

// WS anchor: an echoed sequence picks its frame; without the echo the oldest
// unacked frame is used; an unknown sequence falls back to now.
func TestWSHeartbeatAnchor(t *testing.T) {
	d := &Daemon{}
	t0 := time.Now().Add(-3 * time.Second)
	d.noteWSHeartbeatSent("rt-1", 1, t0)
	d.noteWSHeartbeatSent("rt-1", 2, t0.Add(time.Second))
	d.noteWSHeartbeatSent("rt-1", 3, t0.Add(2*time.Second))

	if got := d.wsHeartbeatAnchor("rt-1", 2); !got.Equal(t0.Add(time.Second)) {
		t.Fatalf("echoed seq 2 anchored to %v", got)
	}
	if got := d.wsHeartbeatAnchor("rt-1", 0); !got.Equal(t0.Add(2 * time.Second)) {
		t.Fatalf("no echo must use the oldest unacked frame, got %v", got)
	}
	if got := d.wsHeartbeatAnchor("rt-1", 99); time.Since(got) > time.Second {
		t.Fatalf("unknown seq must fall back to now, got %v", got)
	}
}
