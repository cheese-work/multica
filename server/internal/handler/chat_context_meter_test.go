package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// CHE-1808: a DM's response carries the latest completed run's prompt size and
// the model's window, or nothing when that run did not report them.
func TestChatSessionReportsTheLatestRunContext(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	dm := newAutoCompactDM(t, `{"auto_compact":{"enabled":false}}`, `{}`)

	type contextFields struct {
		Tokens *int64 `json:"context_tokens"`
		Window *int64 `json:"context_window"`
	}
	readSingle := func() contextFields {
		t.Helper()
		request := withChatTestWorkspaceCtx(t, withURLParam(newRequest(http.MethodGet, "/api/chat/sessions/"+dm.sessionID, nil), "sessionId", dm.sessionID))
		w := httptest.NewRecorder()
		testHandler.GetChatSession(w, request)
		var fields contextFields
		if err := json.Unmarshal(w.Body.Bytes(), &fields); err != nil {
			t.Fatalf("decode %s: %v", w.Body.String(), err)
		}
		return fields
	}
	readList := func() contextFields {
		t.Helper()
		request := withChatTestWorkspaceCtx(t, newRequest(http.MethodGet, "/api/chat/sessions", nil))
		var sessions []struct {
			ID string `json:"id"`
			contextFields
		}
		testutil.Call(t, testHandler.ListChatSessions, request).Want(http.StatusOK).JSON(&sessions)
		for _, s := range sessions {
			if s.ID == dm.sessionID {
				return s.contextFields
			}
		}
		t.Fatalf("session %s missing from the list", dm.sessionID)
		return contextFields{}
	}
	want := func(label string, got contextFields, tokens, window int64) {
		t.Helper()
		if got.Tokens == nil || got.Window == nil || *got.Tokens != tokens || *got.Window != window {
			t.Fatalf("%s: context = %v of %v, want %d of %d", label, got.Tokens, got.Window, tokens, window)
		}
	}

	if got := readSingle(); got.Tokens != nil || got.Window != nil {
		t.Fatalf("a DM with no completed run reports context %+v", got)
	}
	dm.complete(t, dm.sendRunning(t, "one"), 124_000, 200_000)
	want("single after one run", readSingle(), 124_000, 200_000)
	want("list after one run", readList(), 124_000, 200_000)

	dm.complete(t, dm.sendRunning(t, "two"), 150_000, 200_000)
	want("single after two runs", readSingle(), 150_000, 200_000)

	// The latest completed run did not report: the meter must hide, not show the
	// previous run's stale numbers.
	dm.complete(t, dm.sendRunning(t, "three"), 0, 0)
	if got := readSingle(); got.Tokens != nil || got.Window != nil {
		t.Fatalf("a run without context left the stale %+v", got)
	}
	if got := readList(); got.Tokens != nil || got.Window != nil {
		t.Fatalf("list: a run without context left the stale %+v", got)
	}
}
