package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
)

func reportUpdate(t *testing.T, updateID string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	req := newOwnerDaemonRequest(
		http.MethodPost,
		"/api/daemon/runtimes/"+testRuntimeID+"/update/"+updateID+"/result",
		body,
	)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("runtimeId", testRuntimeID)
	rctx.URLParams.Add("updateId", updateID)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))

	w := httptest.NewRecorder()
	testHandler.ReportUpdateResult(w, req)
	return w
}

func runningUpdate(t *testing.T) *UpdateRequest {
	t.Helper()
	orig := testHandler.UpdateStore
	testHandler.UpdateStore = NewInMemoryUpdateStore()
	t.Cleanup(func() { testHandler.UpdateStore = orig })

	ctx := context.Background()
	update, err := testHandler.UpdateStore.Create(ctx, testRuntimeID, "v9.9.9", testUserID)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := testHandler.UpdateStore.PopPending(ctx, testRuntimeID); err != nil {
		t.Fatalf("pop: %v", err)
	}
	return update
}

func TestReportUpdateResult_RunningStoresProgress(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	update := runningUpdate(t)

	w := reportUpdate(t, update.ID, map[string]any{
		"status":   "running",
		"progress": map[string]any{"phase": "download", "bytes_done": 512, "bytes_total": 2048},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("report: %d %s", w.Code, w.Body.String())
	}

	got, err := testHandler.UpdateStore.Get(context.Background(), update.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	want := UpdateProgress{Phase: "download", BytesDone: 512, BytesTotal: 2048}
	if got.Progress == nil || *got.Progress != want {
		t.Fatalf("progress = %+v, want %+v", got.Progress, want)
	}
}

func TestReportUpdateResult_TerminalIgnoresLateProgress(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	update := runningUpdate(t)

	if w := reportUpdate(t, update.ID, map[string]any{"status": "completed", "output": "ok"}); w.Code != http.StatusOK {
		t.Fatalf("complete: %d %s", w.Code, w.Body.String())
	}
	w := reportUpdate(t, update.ID, map[string]any{
		"status":   "running",
		"progress": map[string]any{"phase": "download", "bytes_done": 1, "bytes_total": 2},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("late report: %d %s", w.Code, w.Body.String())
	}

	got, err := testHandler.UpdateStore.Get(context.Background(), update.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != UpdateCompleted || got.Progress != nil {
		t.Fatalf("status=%s progress=%+v, want completed with no progress", got.Status, got.Progress)
	}
}
