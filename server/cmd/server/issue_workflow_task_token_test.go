package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// TestIssueWorkflowWritesRejectTaskToken drives the real Auth -> router chain:
// an agent's mat_ task token authenticates as its runtime owner (a workspace
// owner here), and must not be able to edit, delete, or re-hand-off a project
// workflow. A human credential on the same routes keeps working.
func TestIssueWorkflowWritesRejectTaskToken(t *testing.T) {
	if testPool == nil {
		t.Skip("no database connection")
	}
	ctx := context.Background()

	var agentID string
	if err := testPool.QueryRow(ctx, `
		SELECT id::text FROM agent WHERE workspace_id = $1 ORDER BY created_at LIMIT 1
	`, testWorkspaceID).Scan(&agentID); err != nil {
		t.Fatalf("load integration-test agent: %v", err)
	}
	matToken := mintAgentTaskToken(t, agentID, ensureAgentTask(t, agentID), testUserID)

	var workflowID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO issue_workflow (workspace_id, name, initial_status_key, owner_id)
		VALUES ($1, 'task-token-guard', 'backlog', $2) RETURNING id::text
	`, testWorkspaceID, testUserID).Scan(&workflowID); err != nil {
		t.Fatalf("insert workflow: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM issue_workflow WHERE id = $1`, workflowID)
	})

	do := func(t *testing.T, method, path, bearer string, body any) int {
		t.Helper()
		var buf bytes.Buffer
		if body != nil {
			if err := json.NewEncoder(&buf).Encode(body); err != nil {
				t.Fatalf("encode body: %v", err)
			}
		}
		req, err := http.NewRequest(method, testServer.URL+path, &buf)
		if err != nil {
			t.Fatalf("create request: %v", err)
		}
		req.Header.Set("Authorization", "Bearer "+bearer)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Workspace-ID", testWorkspaceID)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("perform request: %v", err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}

	base := "/api/issue-workflows/" + workflowID
	for _, tc := range []struct {
		name, method, path string
		body               any
	}{
		{"patch", http.MethodPatch, base, map[string]any{"name": "renamed-by-agent"}},
		{"delete", http.MethodDelete, base, nil},
		{"set handoff", http.MethodPut, base + "/handoffs/in_progress", map[string]any{"status_key": "in_progress", "instructions": "x"}},
	} {
		t.Run("mat_ "+tc.name, func(t *testing.T) {
			if got := do(t, tc.method, tc.path, matToken, tc.body); got != http.StatusForbidden {
				t.Fatalf("status = %d, want 403", got)
			}
		})
	}

	var name string
	if err := testPool.QueryRow(ctx, `SELECT name FROM issue_workflow WHERE id = $1`, workflowID).Scan(&name); err != nil {
		t.Fatalf("workflow must survive rejected writes: %v", err)
	}
	if name != "task-token-guard" {
		t.Fatalf("name = %q; rejected requests must have no side effect", name)
	}

	t.Run("mat_ get stays open", func(t *testing.T) {
		if got := do(t, http.MethodGet, base, matToken, nil); got != http.StatusOK {
			t.Fatalf("status = %d, want 200", got)
		}
	})
	t.Run("human patch", func(t *testing.T) {
		if got := do(t, http.MethodPatch, base, interactiveToken(t), map[string]any{"name": "renamed-by-owner"}); got != http.StatusOK {
			t.Fatalf("status = %d, want 200", got)
		}
	})
}

// A task token that names a decision gets past the router guard and is judged
// by the handler: a stale before hash is a 409, not the guard's 403. A cloud
// PAT never passes, and a task token without a decision id stays 403.
func TestIssueWorkflowWritesLetDecisionBoundTaskTokenReachHandler(t *testing.T) {
	if testPool == nil {
		t.Skip("no database connection")
	}
	ctx := context.Background()
	var agentID string
	if err := testPool.QueryRow(ctx, `SELECT id::text FROM agent WHERE workspace_id = $1 ORDER BY created_at LIMIT 1`, testWorkspaceID).Scan(&agentID); err != nil {
		t.Fatalf("load integration-test agent: %v", err)
	}
	matToken := mintAgentTaskToken(t, agentID, ensureAgentTask(t, agentID), testUserID)
	var workflowID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO issue_workflow (workspace_id, name, initial_status_key, owner_id)
		VALUES ($1, 'bound-write-route', 'backlog', $2) RETURNING id::text
	`, testWorkspaceID, testUserID).Scan(&workflowID); err != nil {
		t.Fatalf("insert workflow: %v", err)
	}
	t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM issue_workflow WHERE id = $1`, workflowID) })

	sha := strings.Repeat("a", 64)
	bound := map[string]any{"steps": []any{}, "decision_id": "00000000-0000-4000-8000-000000000001", "before_sha256": sha, "after_sha256": sha}
	handoff := map[string]any{"status_key": "in_progress", "decision_id": "00000000-0000-4000-8000-000000000001", "before_sha256": sha, "after_sha256": sha}
	base := "/api/issue-workflows/" + workflowID
	for _, tc := range []struct {
		name, method, path string
		bearer             string
		body               any
		want               int
	}{
		{"patch with decision", http.MethodPatch, base, matToken, bound, http.StatusConflict},
		{"handoff with decision", http.MethodPut, base + "/handoffs/backlog", matToken, handoff, http.StatusBadRequest},
		{"patch without decision", http.MethodPatch, base, matToken, map[string]any{"steps": []any{}}, http.StatusForbidden},
		{"delete stays human only", http.MethodDelete, base, matToken, nil, http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			if tc.body != nil {
				json.NewEncoder(&buf).Encode(tc.body)
			}
			req, _ := http.NewRequest(tc.method, testServer.URL+tc.path, &buf)
			req.Header.Set("Authorization", "Bearer "+tc.bearer)
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Workspace-ID", testWorkspaceID)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tc.want {
				b, _ := io.ReadAll(resp.Body)
				t.Fatalf("status = %d, want %d: %s", resp.StatusCode, tc.want, b)
			}
		})
	}
}
