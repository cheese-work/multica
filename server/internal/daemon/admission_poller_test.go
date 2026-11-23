package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type admissionPollerFixture struct {
	d      *Daemon
	claims atomic.Int64
	maxReq atomic.Int64 // max_tasks of the most recent claim
}

// newAdmissionPollerFixture runs runBatchPoller against a fake server whose
// claim replies come from reply(callNumber). The returned stop func ends the
// poller and waits for in-flight tasks.
func newAdmissionPollerFixture(t *testing.T, a *Admission, reply func(call int64, w http.ResponseWriter), runner taskRunnerFunc) (*admissionPollerFixture, func()) {
	t.Helper()
	f := &admissionPollerFixture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/api/daemon/tasks/claim") {
			var body struct {
				MaxTasks int `json:"max_tasks"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.maxReq.Store(int64(body.MaxTasks))
			reply(f.claims.Add(1), w)
			return
		}
		w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)

	d := New(Config{
		ServerBaseURL:      srv.URL,
		HeartbeatInterval:  time.Hour,
		PollInterval:       20 * time.Millisecond,
		MaxConcurrentTasks: 4,
	}, slog.New(slog.NewTextHandler(noopWriter{}, nil)))
	d.workspaces["ws-1"] = &workspaceState{workspaceID: "ws-1", runtimeIDs: []string{"rt-1"}}
	d.runtimeIndex["rt-1"] = Runtime{ID: "rt-1", Provider: "claude"}
	d.cancelPollInterval = time.Hour
	d.admission = a
	d.runner = runner
	f.d = d

	sem := newTaskSlotSemaphore(d.cfg.MaxConcurrentTasks)
	var wg sync.WaitGroup
	ctx, cancel := context.WithCancel(context.Background())
	go d.runBatchPoller(ctx, ctx, sem, make(chan struct{}, 1), &wg)
	return f, func() { cancel(); wg.Wait() }
}

func admissionWaitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func admissionTaskJSON(ids ...string) string {
	return admissionTaskGenJSON("gen-1", ids...)
}

func admissionTaskGenJSON(gen string, ids ...string) string {
	var parts []string
	for _, id := range ids {
		parts = append(parts, fmt.Sprintf(`{"id":%q,"runtime_id":"rt-1","issue_id":"i-%s","dispatched_at":%q,"agent":{"name":"a"}}`, id, id, gen))
	}
	return `{"tasks":[` + strings.Join(parts, ",") + `]}`
}

// The claim asks for n = min(free slots, budget room) and the accepted task
// holds its R_default charge from accept until its run ends.
func TestPollerChargesAtClaimAndReleasesWhenTaskEnds(t *testing.T) {
	a, _ := newTestAdmission(4, 2) // room for 2 of the 4 slots
	release := make(chan struct{})
	var started atomic.Int64
	f, stop := newAdmissionPollerFixture(t, a, func(call int64, w http.ResponseWriter) {
		if call == 1 {
			w.Write([]byte(admissionTaskJSON("t1")))
			return
		}
		w.Write([]byte(`{"tasks":[]}`))
	}, func(ctx context.Context, task Task, provider string, slot int, log *slog.Logger) (TaskResult, error) {
		started.Add(1)
		<-release
		return TaskResult{Status: "completed"}, nil
	})
	defer stop()
	// Registered after stop, so it runs first: a failing assertion must not
	// leave the runner blocked and hang stop().
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()

	admissionWaitFor(t, "task start", func() bool { return started.Load() == 1 })
	if got := f.maxReq.Load(); got != 2 {
		t.Fatalf("first claim max_tasks = %d, want 2 (budget room, not 4 free slots)", got)
	}
	if got := a.Charged(); got != 2 {
		t.Fatalf("charged while the task runs = %d, want 2 (1 task x R; n-k refunded)", got)
	}
	close(release)
	admissionWaitFor(t, "release at task end", func() bool { return a.Charged() == 0 })
}

// Budget fully charged: the poller sends no claim at all.
func TestPollerSendsNoClaimWhenBudgetExhausted(t *testing.T) {
	a := NewAdmission(AdmissionConfig{Budget: 2, RDefault: 2}, map[AttemptRef]int64{ref("old"): 2})
	f, stop := newAdmissionPollerFixture(t, a, func(call int64, w http.ResponseWriter) {
		w.Write([]byte(`{"tasks":[]}`))
	}, func(ctx context.Context, task Task, provider string, slot int, log *slog.Logger) (TaskResult, error) {
		return TaskResult{Status: "completed"}, nil
	})
	time.Sleep(300 * time.Millisecond)
	stop()
	if got := f.claims.Load(); got != 0 {
		t.Fatalf("claims sent = %d, want 0 with no budget", got)
	}
}

// A failed (ambiguous) claim refunds the whole charge and the poller keeps going.
func TestPollerRefundsAmbiguousClaim(t *testing.T) {
	a, _ := newTestAdmission(4, 2)
	f, stop := newAdmissionPollerFixture(t, a, func(call int64, w http.ResponseWriter) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}, func(ctx context.Context, task Task, provider string, slot int, log *slog.Logger) (TaskResult, error) {
		return TaskResult{Status: "completed"}, nil
	})
	admissionWaitFor(t, "two failed claims", func() bool { return f.claims.Load() >= 2 })
	stop()
	if got := a.Charged(); got != 0 {
		t.Fatalf("charged after failed claims = %d, want 0", got)
	}
}

// A task whose run is cancelled before the agent starts still releases once.
func TestPollerReleasesWhenRunnerFailsBeforeLaunch(t *testing.T) {
	a, al := newTestAdmission(4, 2)
	var ran atomic.Int64
	_, stop := newAdmissionPollerFixture(t, a, func(call int64, w http.ResponseWriter) {
		if call == 1 {
			w.Write([]byte(admissionTaskJSON("t1")))
			return
		}
		w.Write([]byte(`{"tasks":[]}`))
	}, func(ctx context.Context, task Task, provider string, slot int, log *slog.Logger) (TaskResult, error) {
		ran.Add(1)
		return TaskResult{}, context.Canceled
	})
	admissionWaitFor(t, "run", func() bool { return ran.Load() == 1 })
	admissionWaitFor(t, "release", func() bool { return a.Charged() == 0 })
	stop()
	if al.count("double_release") != 0 {
		t.Fatalf("double_release alerts = %d, want 0", al.count("double_release"))
	}
}

// Flag off (nil admission): today's behaviour; the claim asks for every free slot.
func TestPollerFlagOffClaimsAllFreeSlots(t *testing.T) {
	f, stop := newAdmissionPollerFixture(t, nil, func(call int64, w http.ResponseWriter) {
		w.Write([]byte(`{"tasks":[]}`))
	}, func(ctx context.Context, task Task, provider string, slot int, log *slog.Logger) (TaskResult, error) {
		return TaskResult{Status: "completed"}, nil
	})
	admissionWaitFor(t, "claim", func() bool { return f.claims.Load() >= 1 })
	stop()
	if got := f.maxReq.Load(); got != 4 {
		t.Fatalf("max_tasks = %d, want 4", got)
	}
}

// A re-delivery (new generation) of a task that is already running is ignored,
// so it must not keep a charge of its own, and the running attempt's charge is
// released exactly once when it ends.
func TestPollerIgnoredRedeliveryHoldsNoCharge(t *testing.T) {
	a, al := newTestAdmission(8, 2)
	release := make(chan struct{})
	var started atomic.Int64
	f, stop := newAdmissionPollerFixture(t, a, func(call int64, w http.ResponseWriter) {
		switch call {
		case 1:
			w.Write([]byte(admissionTaskGenJSON("gen-1", "t1")))
		case 2:
			w.Write([]byte(admissionTaskGenJSON("gen-2", "t1")))
		default:
			w.Write([]byte(`{"tasks":[]}`))
		}
	}, func(ctx context.Context, task Task, provider string, slot int, log *slog.Logger) (TaskResult, error) {
		started.Add(1)
		<-release
		return TaskResult{Status: "completed"}, nil
	})
	defer stop()
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()

	admissionWaitFor(t, "redelivery claim", func() bool { return f.claims.Load() >= 3 })
	if got := a.Charged(); got != 2 {
		t.Fatalf("charged = %d, want 2: only the running attempt holds a charge", got)
	}
	close(release)
	admissionWaitFor(t, "release", func() bool { return a.Charged() == 0 })
	if started.Load() != 1 || al.count("double_release") != 0 {
		t.Fatalf("started=%d double_release=%d", started.Load(), al.count("double_release"))
	}
}
