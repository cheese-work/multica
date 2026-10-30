package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// A server that re-delivers a task row whose prepare lease lapsed hands the
// daemon the same task id again while the first execution is still preparing.
// The second delivery must not start a competing execution.
func TestRunBatchPollerIgnoresDuplicateDeliveryOfInflightTask(t *testing.T) {
	t.Parallel()

	var claimCalls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/api/daemon/tasks/claim") {
			switch claimCalls.Add(1) {
			case 1:
				w.Write([]byte(`{"tasks":[{"id":"t1","runtime_id":"rt-1","issue_id":"i1","dispatched_at":"gen-1","agent":{"name":"a"}}]}`))
			case 2:
				w.Write([]byte(`{"tasks":[{"id":"t1","runtime_id":"rt-1","issue_id":"i1","dispatched_at":"gen-2","agent":{"name":"a"}}]}`))
			default:
				w.Write([]byte(`{"tasks":[]}`))
			}
			return
		}
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	d := New(Config{
		ServerBaseURL:      srv.URL,
		HeartbeatInterval:  time.Hour,
		PollInterval:       20 * time.Millisecond,
		MaxConcurrentTasks: 4,
	}, slog.New(slog.NewTextHandler(noopWriter{}, nil)))
	d.workspaces["ws-1"] = &workspaceState{workspaceID: "ws-1", runtimeIDs: []string{"rt-1"}}
	d.runtimeIndex["rt-1"] = Runtime{ID: "rt-1", Provider: "claude"}
	d.cancelPollInterval = time.Hour

	var runs atomic.Int64
	release := make(chan struct{})
	var sawGeneration atomic.Value
	d.runner = taskRunnerFunc(func(ctx context.Context, task Task, provider string, slot int, log *slog.Logger) (TaskResult, error) {
		runs.Add(1)
		<-release // first execution is still preparing
		sawGeneration.Store(d.latestClaimGeneration(task))
		return TaskResult{Status: "completed"}, nil
	})

	sem := newTaskSlotSemaphore(d.cfg.MaxConcurrentTasks)
	var taskWG sync.WaitGroup
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.runBatchPoller(ctx, ctx, sem, make(chan struct{}, 1), &taskWG)

	deadline := time.After(3 * time.Second)
	for claimCalls.Load() < 3 {
		select {
		case <-deadline:
			t.Fatalf("poller made %d claims, want the duplicate delivery to be claimed", claimCalls.Load())
		case <-time.After(10 * time.Millisecond):
		}
	}
	close(release)
	cancel()
	taskWG.Wait()

	if got := runs.Load(); got != 1 {
		t.Fatalf("task executed %d times, want 1: duplicate delivery must not start a second execution", got)
	}
	if got := sawGeneration.Load(); got != "gen-2" {
		t.Fatalf("running execution sees claim generation %v, want the redelivered gen-2 so its start is not rejected as stale", got)
	}
}

// The server refreshes dispatched_at when it re-delivers, and rejects a start
// that presents the old value. A duplicate delivery landing while the start is
// in flight must make the execution retry with the refreshed claim.
func TestStartTaskRetriesWithClaimRefreshedDuringStart(t *testing.T) {
	t.Parallel()

	var d *Daemon
	var starts atomic.Int64
	var secondClaim atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			DispatchedAt string `json:"dispatched_at"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		if starts.Add(1) == 1 {
			d.inflight.begin("t1", "gen-2") // duplicate delivery arrives mid-start
			http.Error(w, `{"error":"task claim is stale"}`, http.StatusConflict)
			return
		}
		secondClaim.Store(body.DispatchedAt)
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	d = New(Config{ServerBaseURL: srv.URL, HeartbeatInterval: time.Hour},
		slog.New(slog.NewTextHandler(noopWriter{}, nil)))
	task := Task{ID: "t1", RuntimeID: "rt-1", DispatchedAt: "gen-1", StartClaimSupported: true}
	d.inflight.begin(task.ID, task.DispatchedAt)

	if _, err := d.startTaskWithCurrentClaim(context.Background(), task); err != nil {
		t.Fatalf("start failed: %v; want a retry with the refreshed claim", err)
	}
	if got := secondClaim.Load(); got != "gen-2" {
		t.Fatalf("retry presented claim %v, want gen-2", got)
	}
}

// A rejected start with no newer claim ends this execution. A duplicate that
// arrives afterwards must start a fresh one rather than be ignored, or the task
// would have no execution at all.
func TestRejectedStartReleasesTaskForRedelivery(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"task claim is stale"}`, http.StatusConflict)
	}))
	defer srv.Close()

	d := New(Config{ServerBaseURL: srv.URL, HeartbeatInterval: time.Hour},
		slog.New(slog.NewTextHandler(noopWriter{}, nil)))
	task := Task{ID: "t1", RuntimeID: "rt-1", DispatchedAt: "gen-1", StartClaimSupported: true}
	d.inflight.begin(task.ID, task.DispatchedAt)

	if _, err := d.startTaskWithCurrentClaim(context.Background(), task); !errors.Is(err, errStartClaimRejected) {
		t.Fatalf("err = %v, want errStartClaimRejected", err)
	}
	if d.inflight.begin(task.ID, "gen-2") == nil {
		t.Fatal("redelivery after a rejected start was ignored; nothing would execute the task")
	}
}
