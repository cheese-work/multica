package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// CHE-6098: the server stamps created_via on create; the caller cannot choose it.
func TestCreateIssue_CreatedVia(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()

	var agentID, runtimeID string
	if err := testPool.QueryRow(ctx,
		`SELECT id, runtime_id FROM agent WHERE workspace_id = $1 AND name = $2`,
		testWorkspaceID, "Handler Test Agent",
	).Scan(&agentID, &runtimeID); err != nil {
		t.Fatalf("find test agent: %v", err)
	}
	// A quick-create run, as the "create with agents" dialog enqueues it. The
	// service validates this task before it accepts the quick_create origin.
	var taskID string
	if err := testPool.QueryRow(ctx,
		`INSERT INTO agent_task_queue (agent_id, runtime_id, status, priority, originator_user_id, accountable_user_id, context)
		 VALUES ($1, $2, 'running', 0, $3, $3, jsonb_build_object('type', 'quick_create', 'workspace_id', $4::text)) RETURNING id`,
		agentID, runtimeID, testUserID, testWorkspaceID,
	).Scan(&taskID); err != nil {
		t.Fatalf("seed acting task: %v", err)
	}
	t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE id = $1`, taskID) })

	asAgent := func(r *http.Request) {
		r.Header.Set("X-Agent-ID", agentID)
		r.Header.Set("X-Task-ID", taskID)
	}
	cases := []struct {
		name   string
		body   map[string]any
		mutate func(*http.Request)
		want   string
	}{
		{"dialog: quick-create run", map[string]any{"origin_type": "quick_create", "origin_id": taskID}, asAgent, "dialog"},
		{"agent: plain agent run", nil, asAgent, "agent"},
		{"cli: human via CLI", nil, func(r *http.Request) { r.Header.Set("X-Client-Platform", "cli") }, "cli"},
		{"caller-supplied created_via is ignored", map[string]any{"created_via": "dialog"}, asAgent, "agent"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := map[string]any{"title": "created_via " + tc.name}
			for k, v := range tc.body {
				body[k] = v
			}
			req := newRequest("POST", "/api/issues?workspace_id="+testWorkspaceID, body)
			if tc.mutate != nil {
				tc.mutate(req)
			}
			w := httptest.NewRecorder()
			testHandler.CreateIssue(w, req)
			if w.Code != http.StatusCreated {
				t.Fatalf("CreateIssue: got %d: %s", w.Code, w.Body.String())
			}
			var created IssueResponse
			if err := json.NewDecoder(w.Body).Decode(&created); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				testHandler.DeleteIssue(httptest.NewRecorder(),
					withURLParam(newRequest("DELETE", "/api/issues/"+created.ID, nil), "id", created.ID))
			})
			if created.CreatedVia != tc.want {
				t.Fatalf("create response created_via = %q, want %q", created.CreatedVia, tc.want)
			}
			// The issue-get read path returns the stored value too.
			gw := httptest.NewRecorder()
			testHandler.GetIssue(gw, withURLParam(newRequest("GET", "/api/issues/"+created.ID, nil), "id", created.ID))
			var got IssueResponse
			if err := json.NewDecoder(gw.Body).Decode(&got); err != nil {
				t.Fatal(err)
			}
			if got.CreatedVia != tc.want {
				t.Fatalf("get created_via = %q, want %q", got.CreatedVia, tc.want)
			}
		})
	}
}

// CHE-6387: one classifier decides created_via from the origin task, its
// trigger comment and the parent issue (rule v1 of the CHE-6376 plan).
func TestCreateIssue_CreatedViaClassifiedFromOriginTask(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()

	var agentID, runtimeID string
	if err := testPool.QueryRow(ctx,
		`SELECT id, runtime_id FROM agent WHERE workspace_id = $1 AND name = $2`,
		testWorkspaceID, "Handler Test Agent",
	).Scan(&agentID, &runtimeID); err != nil {
		t.Fatalf("find test agent: %v", err)
	}

	seedSession := func() string {
		var id string
		if err := testPool.QueryRow(ctx,
			`INSERT INTO chat_session (workspace_id, agent_id, creator_id, title, runtime_id)
			 VALUES ($1, $2, $3, 'created_via dm', $4) RETURNING id`,
			testWorkspaceID, agentID, testUserID, runtimeID,
		).Scan(&id); err != nil {
			t.Fatalf("seed chat session: %v", err)
		}
		t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM chat_session WHERE id = $1`, id) })
		return id
	}

	seedIssue := func(title string) string {
		var id string
		if err := testPool.QueryRow(ctx,
			`INSERT INTO issue (workspace_id, title, status, priority, creator_type, creator_id, number)
			 VALUES ($1, $2, 'todo', 'none', 'member', $3,
			   (SELECT COALESCE(MAX(number),0)+1 FROM issue WHERE workspace_id = $1)) RETURNING id`,
			testWorkspaceID, title, testUserID,
		).Scan(&id); err != nil {
			t.Fatalf("seed issue: %v", err)
		}
		t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM issue WHERE id = $1`, id) })
		return id
	}
	seedComment := func(issueID, authorType, authorID string) string {
		var id string
		if err := testPool.QueryRow(ctx,
			`INSERT INTO comment (workspace_id, issue_id, author_type, author_id, content)
			 VALUES ($1, $2, $3, $4, 'do it') RETURNING id`,
			testWorkspaceID, issueID, authorType, authorID,
		).Scan(&id); err != nil {
			t.Fatalf("seed comment: %v", err)
		}
		return id
	}
	// seedTask inserts a running task of the test agent; chat/trigger columns are optional.
	seedTask := func(chatSessionID, issueID, commentID, evidenceKind any) string {
		var id string
		if err := testPool.QueryRow(ctx,
			`INSERT INTO agent_task_queue (agent_id, runtime_id, issue_id, status, priority, chat_session_id,
			    trigger_comment_id, trigger_evidence_kind, originator_user_id, accountable_user_id)
			 VALUES ($1, $2, $3, 'running', 0, $4, $5, $6, $7, $7) RETURNING id`,
			agentID, runtimeID, issueID, chatSessionID, commentID, evidenceKind, testUserID,
		).Scan(&id); err != nil {
			t.Fatalf("seed task: %v", err)
		}
		t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE id = $1`, id) })
		return id
	}
	seq := 0
	create := func(t *testing.T, taskID string, body map[string]any) IssueResponse {
		t.Helper()
		seq++
		body["title"] = fmt.Sprintf("created_via classified %d", seq)
		req := newRequest("POST", "/api/issues?workspace_id="+testWorkspaceID, body)
		req.Header.Set("X-Agent-ID", agentID)
		req.Header.Set("X-Task-ID", taskID)
		w := httptest.NewRecorder()
		testHandler.CreateIssue(w, req)
		if w.Code != http.StatusCreated {
			t.Fatalf("CreateIssue: got %d: %s", w.Code, w.Body.String())
		}
		var created IssueResponse
		if err := json.NewDecoder(w.Body).Decode(&created); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			testHandler.DeleteIssue(httptest.NewRecorder(),
				withURLParam(newRequest("DELETE", "/api/issues/"+created.ID, nil), "id", created.ID))
		})
		return created
	}

	hostIssue := seedIssue("created_via host")
	memberComment := seedComment(hostIssue, "member", testUserID)
	oldParent := seedIssue("created_via older parent")

	t.Run("dm: chat task makes a top-level issue", func(t *testing.T) {
		got := create(t, seedTask(seedSession(), nil, nil, nil), map[string]any{})
		if got.CreatedVia != "dm" {
			t.Fatalf("created_via = %q, want dm", got.CreatedVia)
		}
	})
	t.Run("comment: task started by a member comment", func(t *testing.T) {
		got := create(t, seedTask(nil, hostIssue, memberComment, "comment"), map[string]any{})
		if got.CreatedVia != "comment" {
			t.Fatalf("created_via = %q, want comment", got.CreatedVia)
		}
	})
	t.Run("agent: wakeup task copies a member parent comment", func(t *testing.T) {
		got := create(t, seedTask(nil, hostIssue, memberComment, "issue_wakeup"), map[string]any{})
		if got.CreatedVia != "agent" {
			t.Fatalf("created_via = %q, want agent", got.CreatedVia)
		}
	})
	t.Run("agent: comment task whose trigger comment is an agent's", func(t *testing.T) {
		agentComment := seedComment(hostIssue, "agent", agentID)
		got := create(t, seedTask(nil, hostIssue, agentComment, "comment"), map[string]any{})
		if got.CreatedVia != "agent" {
			t.Fatalf("created_via = %q, want agent", got.CreatedVia)
		}
	})
	t.Run("agent: DM child under a parent made in the same run", func(t *testing.T) {
		task := seedTask(seedSession(), nil, nil, nil)
		parent := create(t, task, map[string]any{})
		child := create(t, task, map[string]any{"parent_issue_id": parent.ID})
		if parent.CreatedVia != "dm" {
			t.Fatalf("parent created_via = %q, want dm", parent.CreatedVia)
		}
		if child.CreatedVia != "agent" {
			t.Fatalf("child created_via = %q, want agent", child.CreatedVia)
		}
	})
	t.Run("dm: DM child under an older parent", func(t *testing.T) {
		got := create(t, seedTask(seedSession(), nil, nil, nil), map[string]any{"parent_issue_id": oldParent})
		if got.CreatedVia != "dm" {
			t.Fatalf("created_via = %q, want dm", got.CreatedVia)
		}
	})
	t.Run("form: member create without a CLI channel", func(t *testing.T) {
		req := newRequest("POST", "/api/issues?workspace_id="+testWorkspaceID, map[string]any{"title": "created_via form 1"})
		w := httptest.NewRecorder()
		testHandler.CreateIssue(w, req)
		if w.Code != http.StatusCreated {
			t.Fatalf("CreateIssue: got %d: %s", w.Code, w.Body.String())
		}
		var created IssueResponse
		_ = json.NewDecoder(w.Body).Decode(&created)
		t.Cleanup(func() {
			testHandler.DeleteIssue(httptest.NewRecorder(),
				withURLParam(newRequest("DELETE", "/api/issues/"+created.ID, nil), "id", created.ID))
		})
		if created.CreatedVia != "form" {
			t.Fatalf("created_via = %q, want form", created.CreatedVia)
		}
	})
}
