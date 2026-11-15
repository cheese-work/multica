package handler

import (
	"bytes"
	"context"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// A5b (CHE-2216 plan 3.2; T16 Multica part): write routes outside
// /api/issues that still change an issue answer 403 when that issue is outside
// the run's scope. The route inventory test in cmd/server names every other
// write route and why it needs no issue check.

type indirectFixture struct {
	writeScopeFixture
	ownComment, foreignComment       string
	ownAttachment, foreignAttachment string
	commentAttachment                string
	pendingOwn, pendingOther         string
	foreignTask                      string
	mixedAttachment                  string
	ownDecision, foreignDecision     string
}

func newIndirectFixture(t *testing.T) indirectFixture {
	t.Helper()
	f := indirectFixture{writeScopeFixture: newWriteScopeFixture(t)}
	comment := func(issue string) string {
		return dbfx.Insert(t, "comment", testutil.Cols{
			"workspace_id": testWorkspaceID, "issue_id": issue, "author_type": "member", "author_id": testUserID,
			"content": "scope comment", "type": "comment",
		})
	}
	attachment := func(cols testutil.Cols) string {
		base := testutil.Cols{
			"workspace_id": testWorkspaceID, "uploader_type": "member", "uploader_id": testUserID,
			"filename": "a.txt", "url": "https://example.test/a.txt", "content_type": "text/plain", "size_bytes": 1,
		}
		for k, v := range cols {
			base[k] = v
		}
		return dbfx.Insert(t, "attachment", base)
	}
	f.ownComment, f.foreignComment = comment(f.own), comment(f.foreign)
	f.ownAttachment = attachment(testutil.Cols{"issue_id": f.own})
	f.foreignAttachment = attachment(testutil.Cols{"issue_id": f.foreign})
	f.commentAttachment = attachment(testutil.Cols{"comment_id": f.foreignComment})
	// UploadFile accepts both owners: an attachment on an in-scope issue and on
	// a comment of a foreign one.
	f.mixedAttachment = attachment(testutil.Cols{"issue_id": f.own, "comment_id": f.foreignComment})
	f.pendingOwn = attachment(testutil.Cols{"uploader_type": "agent", "uploader_id": f.agentID})
	f.pendingOther = attachment(nil)
	f.foreignTask = createHandlerTestTaskForAgentOnIssue(t, f.agentID, f.foreign)

	agentID, sessionID, _, _ := setupDirectChatSession(t, context.Background(), "Scope decision")
	var foreignIssue string
	f.ownDecision, _, foreignIssue = seedDMDecision(t, agentID, sessionID, "own ask", "open", "1 hour")
	dbfx.Exec(t, `UPDATE decision_request SET issue_id = $2 WHERE id = $1`, f.ownDecision, f.own)
	f.foreignDecision, _, _ = seedDMDecision(t, agentID, sessionID, "foreign ask", "open", "1 hour")
	_ = foreignIssue
	return f
}

func TestTaskTokenWriteScopeIndirectRoutes(t *testing.T) {
	f := newIndirectFixture(t)
	const on = "on"
	var anyBody = map[string]any{}

	type route struct{ method, path string }
	inScope := []route{
		{"PUT", "/api/comments/" + f.ownComment},
		{"DELETE", "/api/comments/" + f.ownComment},
		{"DELETE", "/api/comments/" + f.ownComment + "/keep-replies"},
		{"POST", "/api/comments/" + f.ownComment + "/resolve"},
		{"DELETE", "/api/comments/" + f.ownComment + "/resolve"},
		{"POST", "/api/comments/" + f.ownComment + "/reactions"},
		{"DELETE", "/api/comments/" + f.ownComment + "/reactions"},
		{"DELETE", "/api/attachments/" + f.ownAttachment},
		{"DELETE", "/api/attachments/" + f.pendingOwn},
		{"POST", "/api/decisions/" + f.ownDecision + "/withdraw"},
	}
	outOfScope := []route{
		{"PUT", "/api/comments/" + f.foreignComment},
		{"DELETE", "/api/comments/" + f.foreignComment},
		{"DELETE", "/api/comments/" + f.foreignComment + "/keep-replies"},
		{"POST", "/api/comments/" + f.foreignComment + "/resolve"},
		{"DELETE", "/api/comments/" + f.foreignComment + "/resolve"},
		{"POST", "/api/comments/" + f.foreignComment + "/reactions"},
		{"DELETE", "/api/comments/" + f.foreignComment + "/reactions"},
		{"POST", "/api/comments/00000000-0000-4000-8000-0000000000aa/resolve"},
		{"POST", "/api/comments/not-a-uuid/resolve"},
		{"DELETE", "/api/attachments/" + f.foreignAttachment},
		{"DELETE", "/api/attachments/" + f.commentAttachment},
		{"DELETE", "/api/attachments/" + f.mixedAttachment},
		{"DELETE", "/api/attachments/" + f.pendingOther},
		{"DELETE", "/api/attachments/00000000-0000-4000-8000-0000000000aa"},
		{"POST", "/api/tasks/" + f.foreignTask + "/cancel"},
		{"POST", "/api/tasks/" + f.foreignTask + "/rerun"},
		{"POST", "/api/tasks/" + f.chatTaskID + "/cancel"},
		{"POST", "/api/tasks/00000000-0000-4000-8000-0000000000aa/cancel"},
		{"POST", "/api/decisions/" + f.foreignDecision + "/withdraw"},
		{"POST", "/api/decisions/00000000-0000-4000-8000-0000000000aa/withdraw"},
		{"PATCH", "/api/plugin-bridge/v1/issues/" + f.own},
		{"POST", "/api/plugin-bridge/v1/issues/" + f.own + "/comments"},
		{"PATCH", "/api/plugin-bridge/v1/issues/" + f.foreign},
		{"POST", "/api/plugin-bridge/v1/issues/" + f.foreign + "/comments"},
		{"POST", "/api/plugin-bridge/v1/hooks/any-hook"},
		{"PUT", "/api/plugin-bridge/v1/storage/scope/key"},
		{"DELETE", "/api/plugin-bridge/v1/storage/scope/key"},
	}

	t.Run("in scope passes", func(t *testing.T) {
		for _, c := range inScope {
			if st, reached := scoped(t, on, f.taskID, f.agentID, c.method, c.path, anyBody, nil); st != 200 || !reached {
				t.Errorf("%s %s: status %d reached %v, want pass", c.method, c.path, st, reached)
			}
		}
		for _, p := range []string{"cancel", "rerun"} {
			if st, reached := scoped(t, on, f.taskID, f.agentID, "POST", "/api/tasks/"+f.taskID+"/"+p, anyBody, nil); st != 200 || !reached {
				t.Errorf("own task %s: status %d reached %v, want pass", p, st, reached)
			}
		}
		if st, reached := scoped(t, on, f.chatTaskID, f.agentID, "POST", "/api/tasks/"+f.chatTaskID+"/cancel", anyBody, nil); st != 200 || !reached {
			t.Errorf("a task with no issue, its own: status %d reached %v, want pass", st, reached)
		}
	})

	t.Run("out of scope answers 403 and never reaches the handler", func(t *testing.T) {
		for _, c := range outOfScope {
			if st, reached := scoped(t, on, f.taskID, f.agentID, c.method, c.path, anyBody, nil); st != 403 || reached {
				t.Errorf("%s %s: status %d reached %v, want 403", c.method, c.path, st, reached)
			}
		}
	})

	t.Run("audit logs and passes, off passes", func(t *testing.T) {
		for _, mode := range []string{"audit", "off"} {
			for _, c := range outOfScope {
				if st, reached := scoped(t, mode, f.taskID, f.agentID, c.method, c.path, anyBody, nil); st != 200 || !reached {
					t.Errorf("%s %s %s: status %d reached %v, want pass", mode, c.method, c.path, st, reached)
				}
			}
		}
	})

	t.Run("reads pass", func(t *testing.T) {
		for _, p := range []string{"/api/comments/" + f.foreignComment, "/api/attachments/" + f.foreignAttachment, "/api/decisions/" + f.foreignDecision} {
			if st, reached := scoped(t, on, f.taskID, f.agentID, "GET", p, nil, nil); st != 200 || !reached {
				t.Errorf("GET %s: status %d reached %v, want pass", p, st, reached)
			}
		}
	})

	t.Run("members are untouched", func(t *testing.T) {
		member := func(r *http.Request) {
			r.Header.Del("X-Actor-Source")
			r.Header.Del("X-Task-ID")
			r.Header.Del("X-Agent-ID")
		}
		if st, reached := scoped(t, on, f.taskID, f.agentID, "DELETE", "/api/comments/"+f.foreignComment, nil, member); st != 200 || !reached {
			t.Errorf("member write: status %d reached %v, want pass", st, reached)
		}
	})

	t.Run("a decision request names its issue in the body", func(t *testing.T) {
		for issue, want := range map[string]int{f.own: 200, f.foreign: 403, "not-an-issue": 403} {
			if st, _ := scoped(t, on, f.taskID, f.agentID, "POST", "/api/decisions", map[string]any{"issue_id": issue}, nil); st != want {
				t.Errorf("issue_id %s: status %d, want %d", issue, st, want)
			}
		}
		if st, _ := scoped(t, on, f.taskID, f.agentID, "POST", "/api/decisions", map[string]any{"ISSUE_ID": f.foreign}, nil); st != 403 {
			t.Errorf("upper-case key: status %d, want 403", st)
		}
	})

	t.Run("a cross-issue grant frozen at claim reaches the indirect routes too", func(t *testing.T) {
		insertGrant(t, f.own, testWorkspaceID, []string{"multica_cross_issue"}, "1 minute", "1 hour", false)
		testHandler.runProfileForTask(context.Background(), parseUUID(testWorkspaceID), parseUUID(f.own), parseUUID(f.taskID), nil)
		if st, reached := scoped(t, on, f.taskID, f.agentID, "POST", "/api/comments/"+f.foreignComment+"/resolve", anyBody, nil); st != 200 || !reached {
			t.Errorf("foreign comment under a live grant: status %d reached %v, want pass", st, reached)
		}
	})
}

// uploadRequest builds a multipart upload with the given form fields.
func uploadRequest(t *testing.T, fields map[string]string) *http.Request {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range fields {
		if err := mw.WriteField(k, v); err != nil {
			t.Fatal(err)
		}
	}
	part, err := mw.CreateFormFile("file", "a.txt")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = part.Write([]byte("hello"))
	_ = mw.Close()
	req := httptest.NewRequest("POST", "/api/upload-file", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	return req
}

func TestTaskTokenWriteScopeUploadFile(t *testing.T) {
	f := newIndirectFixture(t)
	send := func(mode string, fields map[string]string) (status int, sawFile bool) {
		var next http.HandlerFunc = func(w http.ResponseWriter, r *http.Request) {
			file, _, err := r.FormFile("file")
			sawFile = err == nil
			if file != nil {
				file.Close()
			}
			w.WriteHeader(http.StatusOK)
		}
		req := uploadRequest(t, fields)
		req.Header.Set("X-Workspace-ID", testWorkspaceID)
		req.Header.Set("X-Actor-Source", "task_token")
		req.Header.Set("X-Task-Token-Purpose", "agent_task")
		req.Header.Set("X-Agent-ID", f.agentID)
		req.Header.Set("X-Task-ID", f.taskID)
		w := httptest.NewRecorder()
		testHandler.TaskTokenWriteScope(mode)(next).ServeHTTP(w, req)
		return w.Code, sawFile
	}

	for _, c := range []struct {
		name   string
		fields map[string]string
		want   int
	}{
		{"own issue", map[string]string{"issue_id": f.own}, 200},
		{"foreign issue", map[string]string{"issue_id": f.foreign}, 403},
		{"own comment", map[string]string{"comment_id": f.ownComment}, 200},
		{"foreign comment", map[string]string{"comment_id": f.foreignComment}, 403},
		{"own issue, foreign comment", map[string]string{"issue_id": f.own, "comment_id": f.foreignComment}, 403},
		{"unknown comment", map[string]string{"comment_id": "00000000-0000-4000-8000-0000000000aa"}, 403},
		{"no target", map[string]string{}, 200},
		{"chat upload is bound by the handler", map[string]string{"task_id": f.chatTaskID}, 200},
	} {
		if st, saw := send("on", c.fields); st != c.want || (c.want == 200 && !saw) {
			t.Errorf("%s: status %d file readable %v, want %d", c.name, st, saw, c.want)
		}
	}
	// Audit forwards a refused upload with the form still readable by the handler.
	if st, saw := send("audit", map[string]string{"issue_id": f.foreign}); st != 200 || !saw {
		t.Errorf("audit: status %d file readable %v, want pass with the file", st, saw)
	}

	// A body that is not a readable multipart form fails closed.
	req := httptest.NewRequest("POST", "/api/upload-file", bytes.NewReader([]byte("not a form")))
	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set("X-Workspace-ID", testWorkspaceID)
	req.Header.Set("X-Actor-Source", "task_token")
	req.Header.Set("X-Task-Token-Purpose", "agent_task")
	req.Header.Set("X-Agent-ID", f.agentID)
	req.Header.Set("X-Task-ID", f.taskID)
	w := httptest.NewRecorder()
	testHandler.TaskTokenWriteScope("on")(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("handler reached") })).ServeHTTP(w, req)
	if w.Code != 403 {
		t.Errorf("not multipart: status %d, want 403", w.Code)
	}
}

// The bridge relay resolves its issue in the workspace of the plugin
// installation header, not the token's. A run token is refused whatever the
// identifier resolves to in the token's own workspace.
func TestTaskTokenWriteScopeRefusesBridgeAcrossWorkspaces(t *testing.T) {
	f := newIndirectFixture(t)
	identifier := f.identifier(t, f.own)
	prefix := testHandler.getIssuePrefix(t.Context(), parseUUID(testWorkspaceID))
	var number int
	dbfx.QueryRow(t, `SELECT number FROM issue WHERE id = $1`, f.own).Scan(&number)
	// A second workspace whose issue has the very same identifier.
	other := dbfx.Workspace(t, "Scope bridge other workspace", "scope-bridge-other", testutil.Cols{"issue_prefix": prefix})
	dbfx.Issue(t, "Scope bridge foreign twin", testutil.Cols{"workspace_id": other, "number": number})

	installation := func(r *http.Request) { r.Header.Set(pluginInstallationHeader, "00000000-0000-4000-8000-0000000000bb") }
	for _, path := range []string{"/api/plugin-bridge/v1/issues/" + identifier, "/api/plugin-bridge/v1/issues/" + identifier + "/comments"} {
		if st, reached := scoped(t, "on", f.taskID, f.agentID, "POST", path, map[string]any{}, installation); st != 403 || reached {
			t.Errorf("POST %s: status %d reached %v, want 403", path, st, reached)
		}
	}
}

// An attachment on an in-scope issue and on a foreign comment is refused, and
// the row stays.
func TestTaskTokenWriteScopeKeepsMixedOwnerAttachment(t *testing.T) {
	f := newIndirectFixture(t)
	router := chi.NewRouter()
	router.Use(testHandler.TaskTokenWriteScope("on"))
	router.Delete("/api/attachments/{id}", testHandler.DeleteAttachment)

	req := newRequest("DELETE", "/api/attachments/"+f.mixedAttachment, nil)
	req.Header.Set("X-Actor-Source", "task_token")
	req.Header.Set("X-Task-Token-Purpose", "agent_task")
	req.Header.Set("X-Agent-ID", f.agentID)
	req.Header.Set("X-Task-ID", f.taskID)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != 403 {
		t.Errorf("status %d, want 403", w.Code)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM attachment WHERE id = $1`, f.mixedAttachment); n != 1 {
		t.Errorf("attachment rows = %d, want it preserved", n)
	}
}
