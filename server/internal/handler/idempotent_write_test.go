package handler

import (
	"github.com/google/uuid"
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
	if _, err := testHandler.Queries.ClaimWriteEvent(t.Context(), db.ClaimWriteEventParams{WorkspaceID: ws, EventKey: writeEventKey(pending, "evt-pending"), Owner: util.MustParseUUID(uuid.NewString())}); err != nil {
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

// idemClass is one write class behind IdempotentWrite: its handler, a request
// carrying the event id, whether the write has landed, and how to undo it so a
// second execution is visible.
type idemClass struct {
	name   string
	next   http.HandlerFunc
	build  func() *http.Request
	landed func() bool
	undo   func()
}

func idemClasses(t *testing.T) []idemClass {
	t.Helper()
	event := "evt-class-" + t.Name()
	commentIssue := dbfx.Issue(t, "idem class comment")
	statusIssue := dbfx.Issue(t, "idem class status")
	wakeupIssue := dbfx.Issue(t, "idem class wakeup")
	agent := dbfx.Agent(t, "idem class wakeup", testRuntimeID)
	dbfx.Cleanup(t, "DELETE FROM issue_wakeup_receipt WHERE wakeup_id IN(SELECT id FROM issue_wakeup WHERE issue_id=$1)", wakeupIssue)
	dbfx.Cleanup(t, "DELETE FROM issue_wakeup WHERE issue_id=$1", wakeupIssue)
	with := func(req *http.Request) *http.Request { req.Header.Set(EventIDHeader, event); return req }
	return []idemClass{
		{
			name: "comment",
			next: testHandler.CreateComment,
			build: func() *http.Request {
				return with(withURLParam(newRequest("POST", "/api/issues/"+commentIssue+"/comments", map[string]any{"content": "once"}), "id", commentIssue))
			},
			landed: func() bool {
				return dbfx.Count(t, `SELECT count(*) FROM comment WHERE issue_id = $1`, commentIssue) > 0
			},
			undo: func() { dbfx.Exec(t, `DELETE FROM comment WHERE issue_id = $1`, commentIssue) },
		},
		{
			name: "status change",
			next: testHandler.UpdateIssue,
			build: func() *http.Request {
				return with(withURLParam(newRequest("PUT", "/api/issues/"+statusIssue, map[string]any{"status": "in_review"}), "id", statusIssue))
			},
			landed: func() bool {
				return dbfx.Count(t, `SELECT count(*) FROM issue WHERE id = $1 AND status = 'in_review'`, statusIssue) > 0
			},
			undo: func() { dbfx.Exec(t, `UPDATE issue SET status = 'todo' WHERE id = $1`, statusIssue) },
		},
		{
			name: "wakeup",
			next: testHandler.CreateIssueWakeup,
			build: func() *http.Request {
				return with(withURLParam(newRequest("POST", "/api/issues/"+wakeupIssue+"/wakeups",
					map[string]any{"agent_id": agent, "kind": "at", "after_seconds": 600, "instruction": "check"}), "id", wakeupIssue))
			},
			landed: func() bool {
				return dbfx.Count(t, `SELECT count(*) FROM issue_wakeup WHERE issue_id = $1`, wakeupIssue) > 0
			},
			undo: func() {
				dbfx.Exec(t, `DELETE FROM issue_wakeup_receipt WHERE wakeup_id IN (SELECT id FROM issue_wakeup WHERE issue_id = $1)`, wakeupIssue)
				dbfx.Exec(t, `DELETE FROM issue_wakeup WHERE issue_id = $1`, wakeupIssue)
			},
		},
	}
}

// A write that committed but whose outcome was never stored (a crash between
// the commit and the completion) must not run again when its claim goes stale.
func TestIdempotentCommittedWriteDoesNotRunAgainAfterCrash(t *testing.T) {
	for _, c := range idemClasses(t) {
		t.Run(c.name, func(t *testing.T) {
			crashing := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				c.next(w, r)
				panic("crash after the commit")
			})
			func() {
				defer func() { _ = recover() }()
				testHandler.IdempotentWrite(crashing).ServeHTTP(httptest.NewRecorder(), c.build())
			}()
			if !c.landed() {
				t.Fatal("setup: the first execution did not write")
			}
			c.undo()
			dbfx.Exec(t, `UPDATE write_event SET created_at = now() - interval '10 minutes' WHERE workspace_id = $1`, testWorkspaceID)
			rec := httptest.NewRecorder()
			testHandler.IdempotentWrite(http.HandlerFunc(c.next)).ServeHTTP(rec, c.build())
			if c.landed() {
				t.Fatal("a stale claim re-ran a write that had already committed")
			}
			if rec.Code >= 300 {
				t.Fatalf("redelivery of a committed write returned %d: %s", rec.Code, rec.Body.String())
			}
		})
	}
}

// A request whose claim was taken from it must not write.
func TestIdempotentWriteWithoutClaimDoesNotRun(t *testing.T) {
	for _, c := range idemClasses(t) {
		t.Run(c.name, func(t *testing.T) {
			stolen := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				dbfx.Exec(t, `DELETE FROM write_event WHERE workspace_id = $1`, testWorkspaceID)
				c.next(w, r)
			})
			rec := httptest.NewRecorder()
			testHandler.IdempotentWrite(stolen).ServeHTTP(rec, c.build())
			if c.landed() {
				t.Fatalf("a request that no longer held the claim wrote (response %d)", rec.Code)
			}
			if rec.Code < 400 {
				t.Fatalf("a request that lost its claim returned %d", rec.Code)
			}
		})
	}
}

// Two requests with one event id: the one that retakes a stale claim writes;
// the one that held it earlier must not write afterwards.
func TestIdempotentConcurrentReclaimWritesOnce(t *testing.T) {
	for _, c := range idemClasses(t) {
		t.Run(c.name, func(t *testing.T) {
			slow := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				dbfx.Exec(t, `UPDATE write_event SET created_at = now() - interval '10 minutes' WHERE workspace_id = $1`, testWorkspaceID)
				testHandler.IdempotentWrite(http.HandlerFunc(c.next)).ServeHTTP(httptest.NewRecorder(), c.build())
				if !c.landed() {
					t.Fatal("setup: the retaking request did not write")
				}
				c.undo()
				c.next(w, r)
			})
			testHandler.IdempotentWrite(slow).ServeHTTP(httptest.NewRecorder(), c.build())
			if c.landed() {
				t.Fatal("the request that lost the claim wrote after the retaking request")
			}
		})
	}
}
