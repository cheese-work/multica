package handler

import (
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"net/http"
	"net/http/httptest"
	"testing"
)

// writeOnce sends the same request twice behind IdempotentWrite and returns
// both recorders.
func writeOnce(t *testing.T, next http.HandlerFunc, build func() *http.Request) (first, second *httptest.ResponseRecorder) {
	t.Helper()
	wrapped := testHandler.IdempotentWrite(next)
	first, second = httptest.NewRecorder(), httptest.NewRecorder()
	wrapped.ServeHTTP(first, build())
	wrapped.ServeHTTP(second, build())
	return first, second
}

func TestIdempotentCommentWritesOnce(t *testing.T) {
	issue := dbfx.Issue(t, "idem comment")
	build := func(event string) func() *http.Request {
		return func() *http.Request {
			req := withURLParam(newRequest("POST", "/api/issues/"+issue+"/comments", map[string]any{"content": "once"}), "id", issue)
			req.Header.Set(EventIDHeader, event)
			return req
		}
	}
	first, second := writeOnce(t, testHandler.CreateComment, build("evt-comment-1"))
	if first.Code != http.StatusCreated || second.Code != http.StatusCreated {
		t.Fatalf("codes %d, %d: %s", first.Code, second.Code, second.Body.String())
	}
	if first.Body.String() != second.Body.String() || second.Header().Get(IdempotentReplayHeader) != "true" {
		t.Fatalf("redelivery did not replay the first response: %s vs %s", first.Body.String(), second.Body.String())
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM comment WHERE issue_id = $1`, issue); n != 1 {
		t.Fatalf("%d comments after a redelivered write, want 1", n)
	}
	testHandler.IdempotentWrite(http.HandlerFunc(testHandler.CreateComment)).ServeHTTP(httptest.NewRecorder(), build("evt-comment-2")())
	if n := dbfx.Count(t, `SELECT count(*) FROM comment WHERE issue_id = $1`, issue); n != 2 {
		t.Fatalf("a new event id must write: %d comments, want 2", n)
	}
}

func TestIdempotentFailedWriteIsRetried(t *testing.T) {
	issue := dbfx.Issue(t, "idem retry")
	send := func(content string) *httptest.ResponseRecorder {
		req := withURLParam(newRequest("POST", "/api/issues/"+issue+"/comments", map[string]any{"content": content}), "id", issue)
		req.Header.Set(EventIDHeader, "evt-retry")
		rec := httptest.NewRecorder()
		testHandler.IdempotentWrite(http.HandlerFunc(testHandler.CreateComment)).ServeHTTP(rec, req)
		return rec
	}
	if rec := send(""); rec.Code < 400 {
		t.Fatalf("empty comment returned %d", rec.Code)
	}
	if rec := send("now valid"); rec.Code != http.StatusCreated {
		t.Fatalf("redelivery after a failed write returned %d: %s", rec.Code, rec.Body.String())
	}
}

func TestIdempotentStatusChangeWritesOnce(t *testing.T) {
	issue := dbfx.Issue(t, "idem status")
	build := func() *http.Request {
		req := withURLParam(newRequest("PUT", "/api/issues/"+issue, map[string]any{"status": "in_review"}), "id", issue)
		req.Header.Set(EventIDHeader, "evt-status-1")
		return req
	}
	wrapped := testHandler.IdempotentWrite(http.HandlerFunc(testHandler.UpdateIssue))
	first := httptest.NewRecorder()
	wrapped.ServeHTTP(first, build())
	if first.Code != http.StatusOK {
		t.Fatalf("first update %d: %s", first.Code, first.Body.String())
	}
	dbfx.Exec(t, `UPDATE issue SET status = 'todo' WHERE id = $1`, issue)
	second := httptest.NewRecorder()
	wrapped.ServeHTTP(second, build())
	if second.Code != http.StatusOK {
		t.Fatalf("redelivery %d: %s", second.Code, second.Body.String())
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM issue WHERE id = $1 AND status = 'todo'`, issue); n != 1 {
		t.Fatal("a redelivered status change wrote again")
	}
}

func TestIdempotentWakeupWritesOnce(t *testing.T) {
	issue := dbfx.Issue(t, "idem wakeup")
	agent := dbfx.Agent(t, "idem wakeup", testRuntimeID)
	dbfx.Cleanup(t, "DELETE FROM issue_wakeup_receipt WHERE wakeup_id IN(SELECT id FROM issue_wakeup WHERE issue_id=$1)", issue)
	dbfx.Cleanup(t, "DELETE FROM issue_wakeup WHERE issue_id=$1", issue)
	first, second := writeOnce(t, testHandler.CreateIssueWakeup, func() *http.Request {
		req := withURLParam(newRequest("POST", "/api/issues/"+issue+"/wakeups",
			map[string]any{"agent_id": agent, "kind": "at", "after_seconds": 600, "instruction": "check"}), "id", issue)
		req.Header.Set(EventIDHeader, "evt-wakeup-1")
		return req
	})
	if first.Code != http.StatusCreated || second.Code != http.StatusCreated {
		t.Fatalf("codes %d, %d: %s", first.Code, second.Code, second.Body.String())
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM issue_wakeup WHERE issue_id = $1`, issue); n != 1 {
		t.Fatalf("%d wakeups after a redelivered write, want 1", n)
	}
}

func TestIdempotentPendingClaimBlocksThenGoesStale(t *testing.T) {
	issue := dbfx.Issue(t, "idem pending")
	build := func() *http.Request {
		req := withURLParam(newRequest("POST", "/api/issues/"+issue+"/comments", map[string]any{"content": "x"}), "id", issue)
		req.Header.Set(EventIDHeader, "evt-pending")
		return req
	}
	// A claim another request holds, not yet finished.
	pending := build()
	ws, _ := util.ParseUUID(testWorkspaceID)
	if _, err := testHandler.Queries.ClaimWriteEvent(t.Context(), db.ClaimWriteEventParams{WorkspaceID: ws, EventKey: writeEventKey(pending, "evt-pending")}); err != nil {
		t.Fatal(err)
	}
	wrapped := testHandler.IdempotentWrite(http.HandlerFunc(testHandler.CreateComment))
	rec := httptest.NewRecorder()
	wrapped.ServeHTTP(rec, build())
	if rec.Code != http.StatusConflict {
		t.Fatalf("redelivery during an unfinished write returned %d, want 409", rec.Code)
	}
	dbfx.Exec(t, `UPDATE write_event SET created_at = now() - interval '10 minutes' WHERE workspace_id = $1`, testWorkspaceID)
	rec = httptest.NewRecorder()
	wrapped.ServeHTTP(rec, build())
	if rec.Code != http.StatusCreated {
		t.Fatalf("redelivery after the claim went stale returned %d: %s", rec.Code, rec.Body.String())
	}
}
