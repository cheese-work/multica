package handler

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// forEachUpdateStore runs fn against the in-memory store and, when
// REDIS_TEST_URL is set, the Redis store.
func forEachUpdateStore(t *testing.T, fn func(t *testing.T, store UpdateStore)) {
	t.Helper()
	t.Run("memory", func(t *testing.T) { fn(t, NewInMemoryUpdateStore()) })
	t.Run("redis", func(t *testing.T) { fn(t, NewRedisUpdateStore(newRedisTestClient(t))) })
}

func TestUpdateStoreCancel(t *testing.T) {
	forEachUpdateStore(t, func(t *testing.T, store UpdateStore) {
		ctx := context.Background()
		rt := "rt-cancel-" + randomID()

		pending, err := store.CreateWithID(ctx, "cyc-1-1", "cyc-1", rt, "v1", "u")
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		ok, err := store.Cancel(ctx, pending.ID)
		if err != nil || !ok {
			t.Fatalf("cancel pending: ok=%v err=%v", ok, err)
		}
		got, _ := store.Get(ctx, pending.ID)
		if got == nil || got.Status != UpdateCancelled {
			t.Fatalf("status after cancel = %+v", got)
		}
		if p, err := store.PopPending(ctx, rt); err != nil || p != nil {
			t.Fatalf("PopPending after cancel = %+v err=%v", p, err)
		}
		// A cancelled request releases the runtime's active slot.
		if _, err := store.Create(ctx, rt, "v2", "u"); err != nil {
			t.Fatalf("create after cancel: %v", err)
		}
	})
}

func TestUpdateStoreCancelAfterPopIsFalse(t *testing.T) {
	forEachUpdateStore(t, func(t *testing.T, store UpdateStore) {
		ctx := context.Background()
		rt := "rt-cancel-run-" + randomID()
		req, err := store.CreateWithID(ctx, "cyc-2-1", "cyc-2", rt, "v1", "u")
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if p, err := store.PopPending(ctx, rt); err != nil || p == nil {
			t.Fatalf("pop: %+v %v", p, err)
		}
		ok, err := store.Cancel(ctx, req.ID)
		if err != nil || ok {
			t.Fatalf("cancel running: ok=%v err=%v", ok, err)
		}
		got, _ := store.Get(ctx, req.ID)
		if got == nil || got.Status != UpdateRunning {
			t.Fatalf("status after refused cancel = %+v", got)
		}
		if ok, _ := store.Cancel(ctx, "no-such-id"); ok {
			t.Fatal("cancel unknown id returned true")
		}
	})
}

func TestUpdateStoreCreateWithID(t *testing.T) {
	forEachUpdateStore(t, func(t *testing.T, store UpdateStore) {
		ctx := context.Background()
		rt := "rt-id-" + randomID()
		id := "cyc-3-" + randomID() + "-1"
		req, err := store.CreateWithID(ctx, id, "cyc-3", rt, "v1", "u")
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if req.ID != id || req.CycleID != "cyc-3" {
			t.Fatalf("request = %+v", req)
		}
		got, _ := store.Get(ctx, id)
		if got == nil || got.CycleID != "cyc-3" {
			t.Fatalf("stored = %+v", got)
		}
		if _, err := store.CreateWithID(ctx, id, "cyc-3", "rt-other-"+randomID(), "v1", "u"); !errors.Is(err, errUpdateDuplicateID) {
			t.Fatalf("duplicate id err = %v, want errUpdateDuplicateID", err)
		}
	})
}

// --- heartbeat fence ---

type fakeFence struct {
	mu    sync.Mutex
	calls []string
	adm   UpdateAdmission
	err   error
}

func (f *fakeFence) Admit(_ context.Context, cycleID string) (UpdateAdmission, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, cycleID)
	return f.adm, f.err
}

func heartbeatWith(t *testing.T, fence UpdateFence) UpdateStore {
	t.Helper()
	origStore, origFence := testHandler.UpdateStore, testHandler.UpdateFence
	store := NewInMemoryUpdateStore()
	testHandler.UpdateStore, testHandler.UpdateFence = store, fence
	t.Cleanup(func() { testHandler.UpdateStore, testHandler.UpdateFence = origStore, origFence })
	return store
}

func TestHeartbeatFenceRefusesCycleUpdate(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	fence := &fakeFence{adm: UpdateAdmission{Allowed: false, Reason: "cycle cancelled"}}
	store := heartbeatWith(t, fence)
	ctx := context.Background()
	req, err := store.CreateWithID(ctx, "cyc-4-1", "cyc-4", testRuntimeID, "v1", testUserID)
	if err != nil {
		t.Fatal(err)
	}

	ack, _, err := testHandler.processHeartbeat(ctx, testRuntimeID, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if ack.PendingUpdate != nil {
		t.Fatalf("refused request was delivered: %+v", ack.PendingUpdate)
	}
	got, _ := store.Get(ctx, req.ID)
	if got == nil || got.Status != UpdateFailed || !strings.HasPrefix(got.Error, "fenced:") {
		t.Fatalf("request = %+v, want failed with fenced: prefix", got)
	}
	if len(fence.calls) != 1 || fence.calls[0] != "cyc-4" {
		t.Fatalf("fence calls = %v", fence.calls)
	}
}

func TestHeartbeatFenceAllowsCycleUpdateWithBudget(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	now := time.Now()
	fence := &fakeFence{adm: UpdateAdmission{Allowed: true, StartCutoff: now.Add(30 * time.Second), DeadlineAt: now.Add(10 * time.Minute)}}
	store := heartbeatWith(t, fence)
	ctx := context.Background()
	if _, err := store.CreateWithID(ctx, "cyc-5-1", "cyc-5", testRuntimeID, "v1", testUserID); err != nil {
		t.Fatal(err)
	}

	ack, _, err := testHandler.processHeartbeat(ctx, testRuntimeID, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	pu := ack.PendingUpdate
	if pu == nil || pu.ID != "cyc-5-1" {
		t.Fatalf("pending update = %+v", pu)
	}
	if pu.StartWithinMs == nil || *pu.StartWithinMs <= 0 || *pu.StartWithinMs > 30_000 {
		t.Fatalf("start_within_ms = %v", pu.StartWithinMs)
	}
	if pu.FinishWithinMs == nil || *pu.FinishWithinMs <= 30_000 || *pu.FinishWithinMs > 600_000 {
		t.Fatalf("finish_within_ms = %v", pu.FinishWithinMs)
	}
}

func TestHeartbeatFenceErrorOrMissingFailsClosed(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	for name, fence := range map[string]UpdateFence{
		"error": &fakeFence{err: errors.New("db down")},
		"nil":   nil,
	} {
		t.Run(name, func(t *testing.T) {
			store := heartbeatWith(t, fence)
			ctx := context.Background()
			req, _ := store.CreateWithID(ctx, "cyc-6-1", "cyc-6", testRuntimeID, "v1", testUserID)
			ack, _, err := testHandler.processHeartbeat(ctx, testRuntimeID, false, nil)
			if err != nil || ack.PendingUpdate != nil {
				t.Fatalf("ack=%+v err=%v, want no delivery", ack.PendingUpdate, err)
			}
			got, _ := store.Get(ctx, req.ID)
			if got == nil || got.Status != UpdateFailed || !strings.HasPrefix(got.Error, "fenced:") {
				t.Fatalf("request = %+v", got)
			}
		})
	}
}

func TestHeartbeatManualUpdateSkipsFence(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	fence := &fakeFence{adm: UpdateAdmission{Allowed: false, Reason: "must not be asked"}}
	store := heartbeatWith(t, fence)
	ctx := context.Background()
	if _, err := store.Create(ctx, testRuntimeID, "v1", testUserID); err != nil {
		t.Fatal(err)
	}
	ack, _, err := testHandler.processHeartbeat(ctx, testRuntimeID, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	pu := ack.PendingUpdate
	if pu == nil || pu.StartWithinMs != nil || pu.FinishWithinMs != nil {
		t.Fatalf("manual update = %+v, want delivered with no budget", pu)
	}
	if len(fence.calls) != 0 {
		t.Fatalf("fence called for a manual update: %v", fence.calls)
	}
}

// --- report -> sink ---

type fakeSink struct {
	mu      sync.Mutex
	reports []ResultReport
	outcome RecordOutcome
	err     error
}

func (f *fakeSink) Record(_ context.Context, r ResultReport) (RecordOutcome, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reports = append(f.reports, r)
	if f.outcome == "" {
		return RecordRecorded, f.err
	}
	return f.outcome, f.err
}

func withSink(t *testing.T, sink UpdateResultSink) {
	t.Helper()
	orig := testHandler.UpdateResultSink
	testHandler.UpdateResultSink = sink
	t.Cleanup(func() { testHandler.UpdateResultSink = orig })
}

// cycleRunning swaps in a fresh store holding a running cycle request.
func cycleRunning(t *testing.T, id string) *UpdateRequest {
	t.Helper()
	orig := testHandler.UpdateStore
	testHandler.UpdateStore = NewInMemoryUpdateStore()
	t.Cleanup(func() { testHandler.UpdateStore = orig })
	ctx := context.Background()
	req, err := testHandler.UpdateStore.CreateWithID(ctx, id, "cyc-r", testRuntimeID, "v9", testUserID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := testHandler.UpdateStore.PopPending(ctx, testRuntimeID); err != nil {
		t.Fatal(err)
	}
	return req
}

func TestReportSinkReceivesURLRuntimeAndResolvedMachineNotBody(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	sink := &fakeSink{}
	withSink(t, sink)
	req := cycleRunning(t, "cyc-r-1")

	var ownerID, daemonID string
	if err := testPool.QueryRow(context.Background(),
		`SELECT owner_id::text, COALESCE(daemon_id, '') FROM agent_runtime WHERE id = $1`, testRuntimeID).Scan(&ownerID, &daemonID); err != nil {
		t.Fatal(err)
	}

	w := reportUpdate(t, req.ID, map[string]any{
		"status": "failed", "reason": "download_failed", "error": "boom", "output": "log",
		"runtime_id": "evil-rt", "owner_id": "evil-owner", "daemon_id": "evil-daemon", "cycle_id": "evil-cycle",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("report: %d %s", w.Code, w.Body.String())
	}
	if len(sink.reports) != 1 {
		t.Fatalf("sink reports = %d, want 1", len(sink.reports))
	}
	want := ResultReport{
		RequestID: req.ID, RuntimeID: testRuntimeID, OwnerID: ownerID, DaemonID: daemonID,
		Status: UpdateFailed, Reason: "download_failed", Output: "log", Error: "boom",
	}
	if sink.reports[0] != want {
		t.Fatalf("report = %+v\nwant     %+v", sink.reports[0], want)
	}
}

func TestReportOutcomeMapsToStatusCode(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	for outcome, wantCode := range map[RecordOutcome]int{
		RecordRecorded: http.StatusOK, RecordDuplicate: http.StatusOK,
		RecordConflict: http.StatusConflict, RecordNotFound: http.StatusNotFound,
	} {
		t.Run(string(outcome), func(t *testing.T) {
			withSink(t, &fakeSink{outcome: outcome})
			req := cycleRunning(t, "cyc-r-2")
			w := reportUpdate(t, req.ID, map[string]any{"status": "completed", "output": "ok"})
			if w.Code != wantCode {
				t.Fatalf("code = %d, want %d: %s", w.Code, wantCode, w.Body.String())
			}
			if outcome == RecordNotFound && !strings.Contains(w.Body.String(), "update not found") {
				t.Fatalf("body = %s", w.Body.String())
			}
		})
	}
}

func TestReportAfterStoreTimeoutStillReachesSink(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	sink := &fakeSink{}
	withSink(t, sink)
	req := cycleRunning(t, "cyc-r-3")
	store := testHandler.UpdateStore.(*InMemoryUpdateStore)
	store.mu.Lock()
	aged := time.Now().Add(-(updateRunningTimeout + time.Second))
	store.requests[req.ID].RunStartedAt = &aged
	store.mu.Unlock()
	if got, _ := store.Get(context.Background(), req.ID); got.Status != UpdateTimeout {
		t.Fatalf("precondition: status = %s", got.Status)
	}

	w := reportUpdate(t, req.ID, map[string]any{"status": "completed", "output": "ok"})
	if w.Code != http.StatusOK || len(sink.reports) != 1 {
		t.Fatalf("code=%d sink reports=%d", w.Code, len(sink.reports))
	}
}

func TestReportForLostCycleRequestReachesSinkOtherUnknownIs404(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	sink := &fakeSink{}
	withSink(t, sink)
	orig := testHandler.UpdateStore
	testHandler.UpdateStore = NewInMemoryUpdateStore() // restart: lost everything
	t.Cleanup(func() { testHandler.UpdateStore = orig })

	w := reportUpdate(t, "0198f3e2-aaaa-bbbb-cccc-ddddeeeeffff-2", map[string]any{"status": "completed"})
	if w.Code != http.StatusOK || len(sink.reports) != 1 {
		t.Fatalf("lost cycle id: code=%d sink reports=%d", w.Code, len(sink.reports))
	}
	sink.reports = nil

	for _, id := range []string{"0123456789abcdef0123456789abcdef", "not-a-cycle-x", "abc-"} {
		w = reportUpdate(t, id, map[string]any{"status": "completed"})
		if w.Code != http.StatusNotFound || len(sink.reports) != 0 {
			t.Fatalf("unknown id %q: code=%d sink reports=%d", id, w.Code, len(sink.reports))
		}
	}
}

func TestReportManualAndProgressNeverReachSink(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	sink := &fakeSink{}
	withSink(t, sink)

	// Manual request, terminal report.
	manual := runningUpdate(t)
	if w := reportUpdate(t, manual.ID, map[string]any{"status": "completed", "output": "ok"}); w.Code != http.StatusOK {
		t.Fatalf("manual report: %d", w.Code)
	}
	// Cycle request, running + progress.
	cyc := cycleRunning(t, "cyc-r-4")
	w := reportUpdate(t, cyc.ID, map[string]any{
		"status": "running", "progress": map[string]any{"phase": "download", "bytes_done": 1, "bytes_total": 2},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("progress report: %d", w.Code)
	}
	if len(sink.reports) != 0 {
		t.Fatalf("sink got %d reports, want 0", len(sink.reports))
	}
	got, _ := testHandler.UpdateStore.Get(context.Background(), cyc.ID)
	if got.Progress == nil || got.Progress.BytesDone != 1 {
		t.Fatalf("progress not stored: %+v", got)
	}
}
