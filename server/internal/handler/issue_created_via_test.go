package handler

import (
	"context"
	"encoding/json"
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
