package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// Task-token write scoping (CHE-2216 plan 3.2, slice A5a; T16 Multica part).
// A restricted run writes to its own issue, that issue's descendants and the
// issues its task created. Anything else answers 403 from the server.

type writeScopeFixture struct {
	taskID, chatTaskID                          string
	agentID                                     string
	own, child, grandchild, created             string
	foreign, createdByOtherTask, otherWorkspace string
}

func newWriteScopeFixture(t *testing.T) writeScopeFixture {
	t.Helper()
	f := writeScopeFixture{agentID: seededReadyAgentID(t)}
	f.own = dbfx.Issue(t, "scope own")
	f.child = dbfx.Issue(t, "scope child", testutil.Cols{"parent_issue_id": f.own})
	f.grandchild = dbfx.Issue(t, "scope grandchild", testutil.Cols{"parent_issue_id": f.child})
	f.foreign = dbfx.Issue(t, "scope foreign")
	f.taskID = createHandlerTestTaskForAgentOnIssue(t, f.agentID, f.own)
	f.chatTaskID = createHandlerTestTaskForAgentOnIssue(t, f.agentID, "")
	otherTask := createHandlerTestTaskForAgentOnIssue(t, f.agentID, f.foreign)
	f.created = dbfx.Issue(t, "scope created", testutil.Cols{"origin_type": "agent_create", "origin_id": f.taskID})
	f.createdByOtherTask = dbfx.Issue(t, "scope created elsewhere", testutil.Cols{"origin_type": "agent_create", "origin_id": otherTask})
	ws := dbfx.Workspace(t, "Scope Foreign WS", "scope-foreign-ws")
	f.otherWorkspace = dbfx.Issue(t, "scope other workspace", testutil.Cols{"workspace_id": ws})
	return f
}

func (f writeScopeFixture) identifier(t *testing.T, issueID string) string {
	t.Helper()
	var n int
	dbfx.QueryRow(t, `SELECT number FROM issue WHERE id = $1`, issueID).Scan(&n)
	return fmt.Sprintf("%s-%d", testHandler.getIssuePrefix(t.Context(), parseUUID(testWorkspaceID)), n)
}

// scoped sends one request through the middleware as a task token of taskID and
// reports the status; reached tells whether the wrapped handler ran.
func scoped(t *testing.T, mode, taskID, agentID, method, path string, body any, tweak func(*http.Request)) (status int, reached bool) {
	t.Helper()
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached = true; w.WriteHeader(http.StatusOK) })
	req := newRequest(method, path, body)
	req.Header.Set("X-Actor-Source", "task_token")
	req.Header.Set("X-Task-Token-Purpose", "agent_task")
	req.Header.Set("X-Agent-ID", agentID)
	req.Header.Set("X-Task-ID", taskID)
	if tweak != nil {
		tweak(req)
	}
	w := httptest.NewRecorder()
	testHandler.TaskTokenWriteScope(mode)(next).ServeHTTP(w, req)
	return w.Code, reached
}

func TestTaskTokenWriteScope(t *testing.T) {
	f := newWriteScopeFixture(t)
	const on = "on"
	issueWrites := []struct{ method, suffix string }{
		{"PUT", ""}, {"DELETE", ""}, {"POST", "/comments"}, {"POST", "/subscribe"}, {"PUT", "/metadata/k"},
		{"POST", "/pull-requests"}, {"POST", "/rerun"}, {"POST", "/labels"},
	}

	t.Run("in scope passes", func(t *testing.T) {
		for _, issue := range []string{f.own, f.child, f.grandchild, f.created} {
			for _, w := range issueWrites {
				if st, reached := scoped(t, on, f.taskID, f.agentID, w.method, "/api/issues/"+issue+w.suffix, nil, nil); st != 200 || !reached {
					t.Errorf("%s %s%s: status %d reached %v, want pass", w.method, issue, w.suffix, st, reached)
				}
			}
		}
	})

	t.Run("identifier of an in-scope issue passes, of a foreign one is refused", func(t *testing.T) {
		if st, _ := scoped(t, on, f.taskID, f.agentID, "POST", "/api/issues/"+f.identifier(t, f.child)+"/comments", nil, nil); st != 200 {
			t.Errorf("child identifier: status %d, want 200", st)
		}
		if st, reached := scoped(t, on, f.taskID, f.agentID, "POST", "/api/issues/"+f.identifier(t, f.foreign)+"/comments", nil, nil); st != 403 || reached {
			t.Errorf("foreign identifier: status %d reached %v, want 403", st, reached)
		}
	})

	t.Run("out of scope answers 403 and never reaches the handler", func(t *testing.T) {
		for label, issue := range map[string]string{
			"foreign": f.foreign, "created by another task": f.createdByOtherTask, "other workspace": f.otherWorkspace,
			"unresolvable": "not-an-issue", "unknown uuid": "00000000-0000-4000-8000-0000000000aa",
		} {
			for _, w := range issueWrites {
				if st, reached := scoped(t, on, f.taskID, f.agentID, w.method, "/api/issues/"+issue+w.suffix, nil, nil); st != 403 || reached {
					t.Errorf("%s %s %s%s: status %d reached %v, want 403", label, w.method, issue, w.suffix, st, reached)
				}
			}
		}
	})

	t.Run("reads and read-only previews pass", func(t *testing.T) {
		for _, c := range []struct{ method, path string }{
			{"GET", "/api/issues/" + f.foreign}, {"GET", "/api/issues/" + f.foreign + "/comments"},
			{"POST", "/api/issues/" + f.foreign + "/comments/trigger-preview"},
			{"POST", "/api/issues/" + f.foreign + "/quick-actions/x/render"},
			{"POST", "/api/issues/query"}, {"POST", "/api/issues/table/rows"}, {"POST", "/api/issues/preview-trigger"},
		} {
			if st, reached := scoped(t, on, f.taskID, f.agentID, c.method, c.path, nil, nil); st != 200 || !reached {
				t.Errorf("%s %s: status %d reached %v, want pass", c.method, c.path, st, reached)
			}
		}
	})

	t.Run("batch writes need every id in scope", func(t *testing.T) {
		for path, ids := range map[string][]string{
			"/api/issues/batch-update": {f.own, f.child}, "/api/issues/batch-delete": {f.created},
		} {
			if st, _ := scoped(t, on, f.taskID, f.agentID, "POST", path, map[string]any{"issue_ids": ids}, nil); st != 200 {
				t.Errorf("%s %v: status %d, want 200", path, ids, st)
			}
		}
		for path, ids := range map[string][]string{
			"/api/issues/batch-update": {f.own, f.foreign}, "/api/issues/batch-delete": {f.foreign},
		} {
			if st, reached := scoped(t, on, f.taskID, f.agentID, "POST", path, map[string]any{"issue_ids": ids}, nil); st != 403 || reached {
				t.Errorf("%s %v: status %d reached %v, want 403", path, ids, st, reached)
			}
		}
		if st, reached := scoped(t, on, f.taskID, f.agentID, "POST", "/api/issues/batch-update", nil, func(r *http.Request) { r.Body = io.NopCloser(strings.NewReader("{")) }); st != 403 || reached {
			t.Errorf("malformed batch body: status %d reached %v, want 403", st, reached)
		}
	})

	t.Run("a parent outside scope is a write to that issue", func(t *testing.T) {
		cases := []struct {
			method, path string
			parent       any
			want         int
		}{
			{"POST", "/api/issues", f.own, 200}, {"POST", "/api/issues", nil, 200}, {"POST", "/api/issues", f.foreign, 403},
			{"PUT", "/api/issues/" + f.own, f.foreign, 403}, {"PUT", "/api/issues/" + f.own, f.child, 200},
		}
		for _, c := range cases {
			if st, _ := scoped(t, on, f.taskID, f.agentID, c.method, c.path, map[string]any{"title": "x", "parent_issue_id": c.parent}, nil); st != c.want {
				t.Errorf("%s %s parent %v: status %d, want %d", c.method, c.path, c.parent, st, c.want)
			}
		}
	})

	t.Run("a chat task owns only what it created", func(t *testing.T) {
		if st, _ := scoped(t, on, f.chatTaskID, f.agentID, "PUT", "/api/issues/"+f.own, nil, nil); st != 403 {
			t.Errorf("chat task on a foreign issue: status %d, want 403", st)
		}
	})

	t.Run("fails closed when the task row is gone", func(t *testing.T) {
		if st, reached := scoped(t, on, "00000000-0000-4000-8000-0000000000bb", f.agentID, "PUT", "/api/issues/"+f.own, nil, nil); st != 403 || reached {
			t.Errorf("unknown task: status %d reached %v, want 403", st, reached)
		}
	})

	t.Run("members and other credentials are untouched", func(t *testing.T) {
		member := func(r *http.Request) {
			r.Header.Del("X-Actor-Source")
			r.Header.Del("X-Task-ID")
			r.Header.Del("X-Agent-ID")
		}
		if st, reached := scoped(t, on, f.taskID, f.agentID, "PUT", "/api/issues/"+f.foreign, nil, member); st != 200 || !reached {
			t.Errorf("member write: status %d reached %v, want pass", st, reached)
		}
	})

	t.Run("off and audit pass everything; audit does not refuse", func(t *testing.T) {
		for _, mode := range []string{"off", "audit", ""} {
			if st, reached := scoped(t, mode, f.taskID, f.agentID, "PUT", "/api/issues/"+f.foreign, nil, nil); st != 200 || !reached {
				t.Errorf("mode %q: status %d reached %v, want pass", mode, st, reached)
			}
		}
	})
}

// Decision 3a: a coordinator run also writes to its parent and sibling issues.
// The middleware honors the multica_cross_issue scope of a live grant on the
// run's own issue, read per request.
func TestTaskTokenWriteScopeHonorsCrossIssueGrant(t *testing.T) {
	f := newWriteScopeFixture(t)
	put := func() int {
		st, _ := scoped(t, "on", f.taskID, f.agentID, "PUT", "/api/issues/"+f.foreign, nil, nil)
		return st
	}
	if st := put(); st != 403 {
		t.Fatalf("no grant: status %d, want 403", st)
	}
	insertGrant(t, f.own, testWorkspaceID, []string{"docker", "device:a"}, "1 minute", "1 hour", false)
	insertGrant(t, f.foreign, testWorkspaceID, []string{"multica_cross_issue"}, "1 minute", "1 hour", false)
	insertGrant(t, f.child, testWorkspaceID, []string{"multica_cross_issue"}, "1 minute", "1 hour", false)
	if st := put(); st != 403 {
		t.Fatalf("grant without the scope, or on another issue: status %d, want 403", st)
	}
	dbfx.Exec(t, `DELETE FROM run_capability_grant WHERE issue_id = $1`, f.own)
	insertGrant(t, f.own, testWorkspaceID, []string{"multica_cross_issue"}, "2 hours", "-1 hour", false)
	insertGrant(t, f.own, testWorkspaceID, []string{"multica_cross_issue"}, "1 minute", "1 hour", true)
	if st := put(); st != 403 {
		t.Fatalf("expired and revoked grants: status %d, want 403", st)
	}
	insertGrant(t, f.own, testWorkspaceID, []string{"multica_cross_issue"}, "1 minute", "1 hour", false)
	if st := put(); st != 200 {
		t.Fatalf("live grant: status %d, want 200", st)
	}
	// Live state per request: revoking the grant takes effect on the next write.
	dbfx.Exec(t, `UPDATE run_capability_grant SET revoked_at = now() WHERE issue_id = $1`, f.own)
	if st := put(); st != 403 {
		t.Fatalf("after revocation: status %d, want 403", st)
	}
	// The grant never reaches another workspace.
	insertGrant(t, f.own, testWorkspaceID, []string{"multica_cross_issue"}, "1 minute", "1 hour", false)
	if st, _ := scoped(t, "on", f.taskID, f.agentID, "PUT", "/api/issues/"+f.otherWorkspace, nil, nil); st != 403 {
		t.Fatalf("other-workspace issue under a live grant: status %d, want 403", st)
	}
}

// scopedRouter mounts the middleware in front of the real issue handlers, so a
// refused request must also leave the database untouched.
func scopedRouter(mode string) http.Handler {
	r := chi.NewRouter()
	r.Use(testHandler.TaskTokenWriteScope(mode))
	r.Post("/api/issues", testHandler.CreateIssue)
	r.Post("/api/issues/quick-create", testHandler.QuickCreateIssue)
	r.Post("/api/issues/batch-update", testHandler.BatchUpdateIssues)
	r.Put("/api/issues/{id}", testHandler.UpdateIssue)
	r.Post("/api/issues/{id}/move", testHandler.MoveIssue)
	return r
}

func serveScoped(f writeScopeFixture, method, path, rawBody string) int {
	req := newRequest(method, path, nil)
	req.Body = io.NopCloser(strings.NewReader(rawBody))
	req.Header.Set("X-Actor-Source", "task_token")
	req.Header.Set("X-Task-Token-Purpose", "agent_task")
	req.Header.Set("X-Agent-ID", f.agentID)
	req.Header.Set("X-Task-ID", f.taskID)
	w := httptest.NewRecorder()
	scopedRouter("on").ServeHTTP(w, req)
	return w.Code
}

func parentOf(t *testing.T, issueID string) *string {
	t.Helper()
	var parent *string
	dbfx.QueryRow(t, `SELECT parent_issue_id::text FROM issue WHERE id = $1`, issueID).Scan(&parent)
	return parent
}

// The parent of a write is read the way the handler reads it: any key spelling
// the handler accepts (JSON field matching ignores case), every duplicate, on
// every route that can set a parent.
func TestTaskTokenWriteScopeParentBypass(t *testing.T) {
	f := newWriteScopeFixture(t)
	q := func(v string) string { return `"` + v + `"` }
	cases := []struct{ name, method, path, body string }{
		{"move with a parent", "POST", "/api/issues/" + f.own + "/move", `{"parent_issue_id":` + q(f.foreign) + `}`},
		{"quick-create with a parent", "POST", "/api/issues/quick-create", `{"prompt":"x","agent_id":` + q(f.agentID) + `,"parent_issue_id":` + q(f.foreign) + `}`},
		{"create, upper-case key", "POST", "/api/issues", `{"title":"x","PARENT_ISSUE_ID":` + q(f.foreign) + `}`},
		{"update, upper-case key", "PUT", "/api/issues/" + f.own, `{"PARENT_ISSUE_ID":` + q(f.foreign) + `}`},
		{"update, allowed key first then upper-case key", "PUT", "/api/issues/" + f.own, `{"parent_issue_id":` + q(f.child) + `,"PARENT_ISSUE_ID":` + q(f.foreign) + `}`},
		{"update, duplicate key", "PUT", "/api/issues/" + f.own, `{"parent_issue_id":` + q(f.child) + `,"parent_issue_id":` + q(f.foreign) + `}`},
		{"batch update, upper-case key", "POST", "/api/issues/batch-update", `{"issue_ids":[` + q(f.own) + `],"updates":{"Parent_Issue_Id":` + q(f.foreign) + `}}`},
		{"batch update, allowed key first then upper-case key", "POST", "/api/issues/batch-update", `{"issue_ids":[` + q(f.own) + `],"updates":{"parent_issue_id":` + q(f.child) + `,"PARENT_ISSUE_ID":` + q(f.foreign) + `}}`},
		{"batch update, upper-case updates", "POST", "/api/issues/batch-update", `{"issue_ids":[` + q(f.own) + `],"UPDATES":{"parent_issue_id":` + q(f.foreign) + `}}`},
	}
	for _, c := range cases {
		if st := serveScoped(f, c.method, c.path, c.body); st != 403 {
			t.Errorf("%s: status %d, want 403", c.name, st)
		}
	}
	if got := parentOf(t, f.own); got != nil {
		t.Errorf("a refused write moved the issue under %s", *got)
	}
	// A parent inside the run's scope still reaches the handler.
	if st := serveScoped(f, "POST", "/api/issues/"+f.child+"/move", `{"parent_issue_id":`+q(f.own)+`}`); st == 403 {
		t.Errorf("move under an in-scope parent: refused")
	}
}

// Audit mode must forward the request byte for byte, however large; enforce
// mode refuses a body it cannot read in full.
func TestTaskTokenWriteScopeOversizedBody(t *testing.T) {
	f := newWriteScopeFixture(t)
	big, _ := json.Marshal(map[string]string{"description": strings.Repeat("a", maxWriteScopeBody+100)})
	run := func(mode string) (status int, forwarded []byte) {
		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { forwarded, _ = io.ReadAll(r.Body) })
		req := newRequest("PUT", "/api/issues/"+f.own, nil)
		req.Body = io.NopCloser(bytes.NewReader(big))
		req.Header.Set("X-Actor-Source", "task_token")
		req.Header.Set("X-Task-Token-Purpose", "agent_task")
		req.Header.Set("X-Agent-ID", f.agentID)
		req.Header.Set("X-Task-ID", f.taskID)
		w := httptest.NewRecorder()
		testHandler.TaskTokenWriteScope(mode)(next).ServeHTTP(w, req.WithContext(context.Background()))
		return w.Code, forwarded
	}
	if st, got := run("audit"); st != 200 || !bytes.Equal(got, big) {
		t.Errorf("audit: status %d, forwarded %d of %d bytes", st, len(got), len(big))
	}
	if st, got := run("on"); st != 403 || got != nil {
		t.Errorf("on: status %d, handler reached %v, want 403 without the handler", st, got != nil)
	}
}
