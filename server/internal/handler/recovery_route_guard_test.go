package handler

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/multica-ai/multica/server/internal/dispatch"
)

// A recovery run records why the issue is blocked. Every other write is refused
// at the edge, so an enqueue path nobody listed (quick-create, chat, autopilot,
// wakeup trigger) cannot start work for it (CHE-2297 L3).
func TestRejectRecoveryWritesAllowsOnlyTheRecordingRoutes(t *testing.T) {
	cases := []struct {
		method, path string
		recovery     bool
		want         int
	}{
		{http.MethodPost, "/api/issues/quick-create", true, http.StatusForbidden},
		{http.MethodPost, "/api/issues/quick-create", false, http.StatusOK},
		{http.MethodPost, "/api/issues/i1/wakeups/w1/trigger", true, http.StatusForbidden},
		{http.MethodPost, "/api/chat/sessions/s1/messages", true, http.StatusForbidden},
		{http.MethodPost, "/api/autopilots/a1/trigger", true, http.StatusForbidden},
		{http.MethodPost, "/api/issues/i1/rerun", true, http.StatusForbidden},
		{http.MethodGet, "/api/issues/i1", true, http.StatusOK},
		{http.MethodPut, "/api/issues/i1", true, http.StatusOK},
		{http.MethodPost, "/api/issues/i1/move", true, http.StatusOK},
		{http.MethodPost, "/api/issues/i1/comments", true, http.StatusOK},
		{http.MethodPut, "/api/issues/i1/metadata/status_summary", true, http.StatusOK},
		{http.MethodPost, "/api/upload-file", true, http.StatusOK},
	}
	h := RejectRecoveryWrites(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	for _, c := range cases {
		req := httptest.NewRequest(c.method, c.path, nil)
		if c.recovery {
			req = req.WithContext(dispatch.WithBudgetRecoveryCaller(req.Context()))
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != c.want {
			t.Errorf("%s %s recovery=%v: got %d, want %d", c.method, c.path, c.recovery, rec.Code, c.want)
		}
	}
}
