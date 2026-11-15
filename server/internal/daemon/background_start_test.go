package daemon

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
)

// startRecorder is a fake server that records StartTask calls and whether the
// provider had already been launched when each one arrived.
type startRecorder struct {
	starts          atomic.Int32
	backendRanFirst atomic.Bool
}

func (f *bgFixture) recordStarts(t *testing.T, status int) *startRecorder {
	t.Helper()
	rec := &startRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/start") {
			_, _ = w.Write([]byte(`{}`))
			return
		}
		rec.starts.Add(1)
		if _, err := os.Stat(f.argsOut); err == nil {
			rec.backendRanFirst.Store(true)
		}
		if status != http.StatusOK {
			http.Error(w, `{"error":"rejected"}`, status)
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)
	f.d.client = NewClient(srv.URL)
	return rec
}

func TestRunTaskBackgroundAcknowledgesStartBeforeBackend(t *testing.T) {
	f := newBackgroundFixture(t)
	rec := f.recordStarts(t, http.StatusOK)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	res, err := f.d.runTask(context.Background(), backgroundTask(), "claude", 0, logger)
	if err != nil || res.Status != "completed" {
		t.Fatalf("res = %+v, err = %v; want completed", res, err)
	}
	if got := rec.starts.Load(); got != 1 {
		t.Fatalf("StartTask calls = %d; want 1", got)
	}
	if rec.backendRanFirst.Load() {
		t.Fatal("StartTask must be called before the backend executes")
	}
	if _, err := os.Stat(f.argsOut); err != nil {
		t.Fatal("backend did not run after a successful start")
	}
}

func TestRunTaskBackgroundStartErrorStopsRun(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	for name, status := range map[string]int{"rejected claim": http.StatusConflict, "other error": http.StatusBadRequest} {
		t.Run(name, func(t *testing.T) {
			f := newBackgroundFixture(t)
			rec := f.recordStarts(t, status)
			task := backgroundTask()
			task.StartClaimSupported = true
			task.DispatchedAt = "gen-1"
			f.d.inflight.begin(task.ID, task.DispatchedAt)

			_, err := f.d.runTask(context.Background(), task, "claude", 0, logger)
			if err == nil {
				t.Fatal("a StartTask error must fail the run")
			}
			if status == http.StatusConflict {
				if !errors.Is(err, errStartClaimRejected) {
					t.Fatalf("err = %v; want errStartClaimRejected", err)
				}
			} else if !strings.Contains(err.Error(), "start task failed") {
				t.Fatalf("err = %v; want start task failed", err)
			}
			if rec.starts.Load() == 0 {
				t.Fatal("StartTask was never called")
			}
			if _, statErr := os.Stat(f.argsOut); statErr == nil {
				t.Fatal("backend must not execute after a StartTask error")
			}
		})
	}
}

func TestRunTaskBackgroundRefusalMakesNoStartCall(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	t.Run("ineligible provider", func(t *testing.T) {
		f := newBackgroundFixture(t)
		f.d.cfg.Agents["codex"] = f.d.cfg.Agents["claude"]
		rec := f.recordStarts(t, http.StatusOK)
		if _, err := f.d.runTask(context.Background(), backgroundTask(), "codex", 0, logger); err == nil {
			t.Fatal("want refusal")
		}
		if got := rec.starts.Load(); got != 0 {
			t.Fatalf("StartTask calls = %d; want 0 for a refused task", got)
		}
	})
	t.Run("custom runtime profile", func(t *testing.T) {
		f := newBackgroundFixture(t)
		rec := f.recordStarts(t, http.StatusOK)
		f.d.runtimeIndex["rt"] = Runtime{ID: "rt", Provider: "claude", ProfileID: "prof"}
		f.d.profileLaunchSpecs = map[string]profileLaunchSpec{"prof": {path: f.d.cfg.Agents["claude"].Path}}
		if _, err := f.d.runTask(context.Background(), backgroundTask(), "claude", 0, logger); err == nil {
			t.Fatal("want refusal")
		}
		if got := rec.starts.Load(); got != 0 {
			t.Fatalf("StartTask calls = %d; want 0 for a refused task", got)
		}
		if _, statErr := os.Stat(f.argsOut); statErr == nil {
			t.Fatal("custom profile must not launch a backend")
		}
	})
}
