package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Daemon side of the revocation fence (CHE-2216 plan 3.8, slice B4b).

func TestElevationAuthorityBindsTaskAndEpoch(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		seen = append(seen, r.Method+" "+r.URL.Path)
		mu.Unlock()
		switch r.URL.Path {
		case "/api/daemon/tasks/t1/elevation/authorize":
			if body["epoch"] != float64(7) || body["scope"] != "repo_write:42" || body["input_hash"] != "abc" {
				http.Error(w, "bad body", http.StatusBadRequest)
				return
			}
			_, _ = w.Write([]byte(`{"op_id":"op-1","epoch":7}`))
		case "/api/daemon/tasks/t1/elevation/ops/op-1/complete":
			if body["result"] != "aborted" {
				http.Error(w, "bad body", http.StatusBadRequest)
				return
			}
			_, _ = w.Write([]byte(`{"op_id":"op-1","state":"aborted"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := NewClient(srv.URL)
	c.SetToken("tok")
	a := c.ElevationAuthority("t1", 7)
	id, err := a.Authorize(context.Background(), "repo_write:42", "abc")
	if err != nil || id != "op-1" {
		t.Fatalf("Authorize = %q, %v", id, err)
	}
	if err := a.Complete(context.Background(), id, "aborted"); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if len(seen) != 2 {
		t.Fatalf("requests %v, want authorize then complete", seen)
	}
}

func TestElevationAuthorityRefusalIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"stale epoch"}`, http.StatusForbidden)
	}))
	defer srv.Close()
	if id, err := NewClient(srv.URL).ElevationAuthority("t1", 1).Authorize(context.Background(), "repo_write:42", "abc"); err == nil {
		t.Fatalf("Authorize = %q on a 403, want an error", id)
	}
}

func TestElevationStateAndLaunchScopesEnded(t *testing.T) {
	var ended atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /api/daemon/tasks/t1/elevation":
			_, _ = w.Write([]byte(`{"state":"fencing","epoch":8}`))
		case "POST /api/daemon/tasks/t1/elevation/launch-scopes-ended":
			ended.Store(true)
			_, _ = w.Write([]byte(`{"ended":true}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := NewClient(srv.URL)
	if state, epoch, err := c.ElevationState(context.Background(), "t1"); err != nil || state != "fencing" || epoch != 8 {
		t.Fatalf("ElevationState = %q, %d, %v", state, epoch, err)
	}
	if err := c.EndLaunchScopes(context.Background(), "t1"); err != nil || !ended.Load() {
		t.Fatalf("EndLaunchScopes = %v, ended %v", err, ended.Load())
	}
}

type fakeBroker struct {
	log      *[]string
	drainErr error
}

func (b fakeBroker) Fence() { *b.log = append(*b.log, "fence") }
func (b fakeBroker) Drain(context.Context) error {
	*b.log = append(*b.log, "drain")
	return b.drainErr
}

// T28 (a)/(b): the local policy goes restricted first, whatever the server does.
func TestRunFenceOrderAndFailClosed(t *testing.T) {
	var log []string
	step := func(name string) { log = append(log, name) }
	f := RunFence{
		Broker:          fakeBroker{log: &log},
		Restrict:        func() { step("restrict") },
		StopRun:         func() error { step("stop"); return nil },
		EndLaunchScopes: func(context.Context) error { step("ended"); return nil },
	}
	if err := f.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	want := []string{"restrict", "fence", "stop", "ended", "drain"}
	if len(log) != len(want) {
		t.Fatalf("order %v, want %v", log, want)
	}
	for i := range want {
		if log[i] != want[i] {
			t.Fatalf("order %v, want %v", log, want)
		}
	}
}

func TestRunFenceKeepsFencingWhenRunWontStop(t *testing.T) {
	var log []string
	stopErr := errors.New("process group still active")
	f := RunFence{
		Broker:          fakeBroker{log: &log},
		Restrict:        func() { log = append(log, "restrict") },
		StopRun:         func() error { return stopErr },
		EndLaunchScopes: func(context.Context) error { log = append(log, "ended"); return nil },
	}
	if err := f.Run(context.Background()); !errors.Is(err, stopErr) {
		t.Fatalf("Run = %v, want the stop error", err)
	}
	for _, s := range log {
		if s == "ended" {
			t.Fatalf("launch scopes reported ended though the run is alive: %v", log)
		}
	}
}

func TestRunFenceReportsUnreportedOperations(t *testing.T) {
	var log []string
	drainErr := errors.New("unreported")
	f := RunFence{
		Broker:          fakeBroker{log: &log, drainErr: drainErr},
		Restrict:        func() {},
		StopRun:         func() error { return nil },
		EndLaunchScopes: func(context.Context) error { return nil },
	}
	if err := f.Run(context.Background()); !errors.Is(err, drainErr) {
		t.Fatalf("Run = %v, want the drain error", err)
	}
}

func TestWatchFenceFiresOnStateOrEpochChange(t *testing.T) {
	for name, tc := range map[string]struct {
		state string
		epoch int64
	}{
		"fencing":     {"fencing", 4},
		"final":       {"restricted_final", 5},
		"newer epoch": {"elevated", 5},
	} {
		t.Run(name, func(t *testing.T) {
			var calls atomic.Int32
			state := func(context.Context) (string, int64, error) {
				if calls.Add(1) == 1 {
					return "", 0, errors.New("server unreachable") // keeps polling
				}
				if calls.Load() == 2 {
					return "elevated", 4, nil
				}
				return tc.state, tc.epoch, nil
			}
			fired := make(chan struct{}, 2)
			done := make(chan struct{})
			go func() {
				WatchFence(context.Background(), state, 4, 5*time.Millisecond, func() { fired <- struct{}{} })
				close(done)
			}()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("WatchFence never returned")
			}
			if len(fired) != 1 {
				t.Fatalf("onFence fired %d times, want once", len(fired))
			}
		})
	}
}
