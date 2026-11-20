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

type claimBody struct {
	MaxTasks      int  `json:"max_tasks"`
	MaxIssueTasks *int `json:"max_issue_tasks"`
}

// reservedSlotsHarness runs the batch poller against a fake server whose claim
// replies are scripted per call, and records every claim request body.
type reservedSlotsHarness struct {
	t       *testing.T
	mu      sync.Mutex
	bodies  []claimBody
	cancel  context.CancelFunc
	wg      *sync.WaitGroup
	started chan string
	release chan struct{}
}

func newReservedSlotsHarness(t *testing.T, maxConcurrent, reserved int, reply func(call int) string) *reservedSlotsHarness {
	t.Helper()
	h := &reservedSlotsHarness{t: t, started: make(chan string, 16), release: make(chan struct{})}
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/api/daemon/tasks/claim") {
			var b claimBody
			json.NewDecoder(r.Body).Decode(&b)
			h.mu.Lock()
			h.bodies = append(h.bodies, b)
			h.mu.Unlock()
			w.Write([]byte(reply(int(calls.Add(1)))))
			return
		}
		w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)

	d := New(Config{
		ServerBaseURL:      srv.URL,
		HeartbeatInterval:  time.Hour,
		PollInterval:       20 * time.Millisecond,
		MaxConcurrentTasks: maxConcurrent,
		ReservedChatSlots:  reserved,
	}, slog.New(slog.NewTextHandler(noopWriter{}, nil)))
	d.workspaces["ws-1"] = &workspaceState{workspaceID: "ws-1", runtimeIDs: []string{"rt-1"}}
	d.runtimeIndex["rt-1"] = Runtime{ID: "rt-1", Provider: "claude"}
	d.cancelPollInterval = time.Hour
	d.runner = taskRunnerFunc(func(ctx context.Context, task Task, provider string, slot int, log *slog.Logger) (TaskResult, error) {
		h.started <- task.ID
		select {
		case <-h.release:
		case <-ctx.Done():
		}
		return TaskResult{Status: "completed"}, nil
	})

	sem := newTaskSlotSemaphore(d.cfg.MaxConcurrentTasks)
	h.wg = &sync.WaitGroup{}
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	go d.runBatchPoller(ctx, ctx, sem, make(chan struct{}, 1), h.wg)
	t.Cleanup(func() {
		close(h.release)
		cancel()
		h.wg.Wait()
	})
	return h
}

func (h *reservedSlotsHarness) waitStarted(ids ...string) {
	h.t.Helper()
	for range ids {
		select {
		case <-h.started:
		case <-time.After(3 * time.Second):
			h.t.Fatalf("expected %v to start", ids)
		}
	}
}

func (h *reservedSlotsHarness) waitBody(match func(claimBody) bool, what string) {
	h.t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		h.mu.Lock()
		for _, b := range h.bodies {
			if match(b) {
				h.mu.Unlock()
				return
			}
		}
		h.mu.Unlock()
		select {
		case <-deadline:
			h.mu.Lock()
			defer h.mu.Unlock()
			shown := h.bodies
			if len(shown) > 5 {
				shown = shown[:5]
			}
			h.t.Fatalf("no claim request matched %q; first requests: %+v", what, shown)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

const (
	reservedIssueTaskJSON = `{"id":"%s","runtime_id":"rt-1","issue_id":"i-%s","agent":{"name":"a"}}`
	reservedChatTaskJSON  = `{"id":"%s","runtime_id":"rt-1","chat_session_id":"c-%s","agent":{"name":"a"}}`
)

func taskJSON(tmpl, id string) string { return strings.ReplaceAll(tmpl, "%s", id) }

// With reserved_chat_slots=2 and 4 slots, two running issue tasks leave an
// issue budget of 0: the next claim must tell the server so, and a chat task
// it returns still runs in a reserved slot.
func TestReservedChatSlotsLimitIssueClaims(t *testing.T) {
	t.Parallel()
	h := newReservedSlotsHarness(t, 4, 2, func(call int) string {
		switch call {
		case 1:
			return `{"tasks":[` + taskJSON(reservedIssueTaskJSON, "t1") + `,` + taskJSON(reservedIssueTaskJSON, "t2") + `]}`
		case 2:
			return `{"tasks":[` + taskJSON(reservedChatTaskJSON, "t3") + `]}`
		}
		return `{"tasks":[]}`
	})
	h.waitBody(func(b claimBody) bool { return b.MaxIssueTasks != nil && *b.MaxIssueTasks == 2 && b.MaxTasks == 4 }, "first claim: max_issue_tasks=2")
	h.waitStarted("t1", "t2")
	h.waitBody(func(b claimBody) bool { return b.MaxIssueTasks != nil && *b.MaxIssueTasks == 0 }, "claim with issue budget 0 once two issue tasks run")
	h.waitStarted("t3")
}

// reserved_chat_slots=0 sends no limit, so claim requests equal today's.
func TestReservedChatSlotsZeroSendsNoIssueLimit(t *testing.T) {
	t.Parallel()
	h := newReservedSlotsHarness(t, 4, 0, func(call int) string {
		if call == 1 {
			return `{"tasks":[` + taskJSON(reservedIssueTaskJSON, "t1") + `]}`
		}
		return `{"tasks":[]}`
	})
	h.waitStarted("t1")
	h.waitBody(func(b claimBody) bool { return b.MaxTasks == 3 }, "second claim after t1 started")
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, b := range h.bodies {
		if b.MaxIssueTasks != nil {
			t.Fatalf("max_issue_tasks sent with reserved_chat_slots=0: %+v", b)
		}
	}
}

func TestValidateReservedChatSlots(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		reserved, max int
		ok            bool
	}{{0, 30, true}, {3, 30, true}, {29, 30, true}, {30, 30, false}, {31, 30, false}, {-1, 30, false}} {
		err := ValidateReservedChatSlots(c.reserved, c.max)
		if (err == nil) != c.ok {
			t.Fatalf("ValidateReservedChatSlots(%d, %d) = %v, want ok=%v", c.reserved, c.max, err, c.ok)
		}
		if err != nil && (!strings.Contains(err.Error(), fmt.Sprint(c.reserved)) || !strings.Contains(err.Error(), fmt.Sprint(c.max))) {
			t.Fatalf("error %q must name both values", err)
		}
	}
}
