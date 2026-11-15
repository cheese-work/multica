package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Timings are scaled down by about 1000x from production
// (0/0.5s/2s/4s then 15s, 10s send timeout, 5 min bound).
func withOutboxTimings(t *testing.T, sendTimeout, maxAge, completedWait time.Duration) {
	t.Helper()
	pb, ps, pi, pm, pc := updateReportBackoffs, updateSendTimeout, updateRetryInterval, updateOutboxMaxAge, updateCompletedWait
	updateReportBackoffs = []time.Duration{0, 5 * time.Millisecond, 20 * time.Millisecond, 40 * time.Millisecond}
	updateSendTimeout, updateRetryInterval, updateOutboxMaxAge, updateCompletedWait = sendTimeout, 15*time.Millisecond, maxAge, completedWait
	t.Cleanup(func() {
		updateReportBackoffs, updateSendTimeout, updateRetryInterval, updateOutboxMaxAge, updateCompletedWait = pb, ps, pi, pm, pc
	})
}

type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}
func (l *lockedBuf) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

func waitDone(t *testing.T, done <-chan struct{}, within time.Duration, what string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(within):
		t.Fatalf("%s: not done within %s", what, within)
	}
}

// 1. A cancelled heartbeat context cannot lose a queued result.
func TestUpdateOutbox_SurvivesCancelledHeartbeatContext(t *testing.T) {
	withOutboxTimings(t, time.Second, 5*time.Second, time.Second)
	d, calls := updateReportDaemon(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	d.cfg.LaunchedBy = "desktop" // refusal: a terminal `failed` result
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	d.handleUpdate(ctx, "rt-1", &PendingUpdate{ID: "upd-1"})
	d.updateOutbox.Wait()

	if got := atomic.LoadInt32(calls); got != 1 {
		t.Fatalf("deliveries = %d, want 1", got)
	}
}

// 2a. 503 for a while, then 200: delivered after retries.
func TestUpdateOutbox_RetriesThroughServerErrors(t *testing.T) {
	withOutboxTimings(t, time.Second, 5*time.Second, time.Second)
	start := time.Now()
	var ok int32
	d, _ := updateReportDaemon(t, func(w http.ResponseWriter, _ *http.Request) {
		if time.Since(start) < 150*time.Millisecond {
			http.Error(w, "{}", http.StatusServiceUnavailable)
			return
		}
		atomic.AddInt32(&ok, 1)
		w.WriteHeader(http.StatusOK)
	})
	waitDone(t, d.enqueueUpdateResult("rt-1", "upd-1", map[string]any{"status": "failed"}), 3*time.Second, "delivery")
	if got := atomic.LoadInt32(&ok); got != 1 {
		t.Fatalf("delivered = %d, want 1", got)
	}
}

// 2b. 404: one attempt, no retry.
func TestUpdateOutbox_NoRetryOn4xx(t *testing.T) {
	withOutboxTimings(t, time.Second, 5*time.Second, time.Second)
	d, calls := updateReportDaemon(t, func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "{}", http.StatusNotFound) })
	waitDone(t, d.enqueueUpdateResult("rt-1", "upd-1", map[string]any{"status": "failed"}), 3*time.Second, "4xx stop")
	time.Sleep(100 * time.Millisecond)
	if got := atomic.LoadInt32(calls); got != 1 {
		t.Fatalf("attempts = %d, want 1", got)
	}
}

// 2c. Never answers: stops at the bound with exactly one error line.
func TestUpdateOutbox_StopsAtMaxAgeWithOneErrorLine(t *testing.T) {
	withOutboxTimings(t, time.Second, 300*time.Millisecond, time.Second)
	d, _ := updateReportDaemon(t, func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "{}", http.StatusServiceUnavailable) })
	var buf lockedBuf
	d.logger = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	start := time.Now()
	waitDone(t, d.enqueueUpdateResult("rt-1", "upd-1", map[string]any{"status": "failed"}), 3*time.Second, "max-age stop")
	if el := time.Since(start); el < 250*time.Millisecond || el > time.Second {
		t.Fatalf("stopped after %s, want about 300ms", el)
	}
	if n := strings.Count(buf.String(), "level=ERROR"); n != 1 {
		t.Fatalf("error lines = %d, want 1:\n%s", n, buf.String())
	}
}

// 3. A blocked request is cut by the per-send timeout, then the retry lands.
func TestUpdateOutbox_PerSendTimeout(t *testing.T) {
	withOutboxTimings(t, 100*time.Millisecond, 5*time.Second, time.Second)
	var n int32
	d, _ := updateReportDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&n, 1) == 1 {
			select {
			case <-r.Context().Done():
			case <-time.After(400 * time.Millisecond): // bound a hung sender in a red run
			}
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	start := time.Now()
	waitDone(t, d.enqueueUpdateResult("rt-1", "upd-1", map[string]any{"status": "failed"}), 3*time.Second, "delivery after timeout")
	if el := time.Since(start); el > time.Second {
		t.Fatalf("delivered after %s, want well under 1s", el)
	}
}

// 4. A blocked endpoint never holds the claim barrier.
func TestUpdateOutbox_BlockedEndpointDoesNotHoldBarrier(t *testing.T) {
	for _, name := range []string{"busy", "failed"} {
		t.Run(name, func(t *testing.T) {
			withOutboxTimings(t, time.Second, 5*time.Second, time.Second)
			release := make(chan struct{})
			// Only terminal results block: `running` keeps today's synchronous path.
			d, _ := updateReportDaemon(t, func(w http.ResponseWriter, r *http.Request) {
				var p map[string]any
				_ = json.NewDecoder(r.Body).Decode(&p)
				if p["status"] == "running" {
					return
				}
				select {
				case <-release:
				case <-r.Context().Done():
				}
			})
			t.Cleanup(func() { close(release); d.updateOutbox.Wait() })
			if name == "busy" {
				d.activeTasks.Store(1)
			} else {
				d.runUpdateFn = func(string) (string, error) { return "", context.DeadlineExceeded }
			}
			returned := make(chan struct{})
			go func() {
				d.handleUpdate(context.Background(), "rt-1", &PendingUpdate{ID: "upd-1"})
				close(returned)
			}()
			waitDone(t, returned, 500*time.Millisecond, "handleUpdate return")
			if !d.tryEnterClaim() {
				t.Fatal("claims blocked while the result is still queued")
			}
			d.exitClaim()
		})
	}
}

// 5. After a successful install the restart waits for `completed` at most updateCompletedWait.
func TestUpdateOutbox_RestartWaitsForCompletedAtMostBound(t *testing.T) {
	originalResolve, originalBrew := resolveSelfExecutable, isBrewInstall
	resolveSelfExecutable = func() (string, error) { return "/tmp/multica-next", nil }
	isBrewInstall = func() bool { return false }
	t.Cleanup(func() { resolveSelfExecutable, isBrewInstall = originalResolve, originalBrew })

	run := func(t *testing.T, block bool) time.Duration {
		withOutboxTimings(t, 5*time.Second, 5*time.Second, 200*time.Millisecond)
		release := make(chan struct{})
		d, _ := updateReportDaemon(t, func(w http.ResponseWriter, r *http.Request) {
			var p map[string]any
			_ = json.NewDecoder(r.Body).Decode(&p)
			if block && p["status"] == "completed" {
				select {
				case <-release:
				case <-r.Context().Done():
				}
			}
			w.WriteHeader(http.StatusOK)
		})
		t.Cleanup(func() { close(release) })
		d.runUpdateFn = func(string) (string, error) { return "ok", nil }
		var restarted atomic.Int32
		d.cancelFunc = func() { restarted.Add(1) }
		start := time.Now()
		d.handleUpdate(context.Background(), "rt-1", &PendingUpdate{ID: "upd-1", TargetVersion: "v1"})
		if restarted.Load() != 1 {
			t.Fatal("daemon did not restart")
		}
		return time.Since(start)
	}
	if el := run(t, true); el < 190*time.Millisecond || el > 600*time.Millisecond {
		t.Fatalf("blocked completed: restart after %s, want about 200ms", el)
	}
	if el := run(t, false); el > 150*time.Millisecond {
		t.Fatalf("answered completed: restart after %s, want no added wait", el)
	}
}
