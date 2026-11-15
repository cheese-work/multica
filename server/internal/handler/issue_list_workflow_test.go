package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// CHE-5963: the list endpoint must return the same workflow_id and
// requested_by_id as the single-issue read, on the default page and under a
// project filter (the roster A/B report reads workflow_id from the list).
func TestListIssues_ReturnsWorkflowIDAndRequestedByID(t *testing.T) {
	ctx := context.Background()
	suffix := time.Now().UnixNano()

	var projectID, workflowID string
	if err := testPool.QueryRow(ctx, `INSERT INTO project (workspace_id, title) VALUES ($1, $2) RETURNING id`,
		testWorkspaceID, fmt.Sprintf("List Workflow %d", suffix)).Scan(&projectID); err != nil {
		t.Fatalf("create project: %v", err)
	}
	t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM project WHERE id = $1`, projectID) })
	if err := testPool.QueryRow(ctx, `INSERT INTO issue_workflow (workspace_id, name, initial_status_key, steps) VALUES ($1, 'list wf', 'todo', '[]'::jsonb) RETURNING id`,
		testWorkspaceID).Scan(&workflowID); err != nil {
		t.Fatalf("create workflow: %v", err)
	}
	t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM issue_workflow WHERE id = $1`, workflowID) })

	var number int
	if err := testPool.QueryRow(ctx, `
		UPDATE workspace
		SET issue_counter = GREATEST(issue_counter, (SELECT COALESCE(MAX(number), 0) FROM issue WHERE workspace_id = $1)) + 1
		WHERE id = $1 RETURNING issue_counter
	`, testWorkspaceID).Scan(&number); err != nil {
		t.Fatalf("next issue number: %v", err)
	}
	var issueID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO issue (workspace_id, title, status, priority, creator_type, creator_id, position, number, project_id, workflow_id, requested_by_id)
		VALUES ($1, $2, 'todo', 'none', 'member', $3, 0, $4, $5, $6, $3) RETURNING id
	`, testWorkspaceID, fmt.Sprintf("wf-override-%d", suffix), testUserID, number, projectID, workflowID).Scan(&issueID); err != nil {
		t.Fatalf("create issue: %v", err)
	}
	t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM issue WHERE id = $1`, issueID) })

	for name, path := range map[string]string{
		"default page":   fmt.Sprintf("/api/issues?workspace_id=%s&limit=1000", testWorkspaceID),
		"project filter": fmt.Sprintf("/api/issues?workspace_id=%s&project_id=%s", testWorkspaceID, projectID),
	} {
		t.Run(name, func(t *testing.T) {
			w := httptest.NewRecorder()
			testHandler.ListIssues(w, newRequest("GET", path, nil))
			if w.Code != http.StatusOK {
				t.Fatalf("ListIssues: expected 200, got %d: %s", w.Code, w.Body.String())
			}
			var resp struct {
				Issues []IssueResponse `json:"issues"`
			}
			if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
				t.Fatalf("decode: %v", err)
			}
			for _, iss := range resp.Issues {
				if iss.ID != issueID {
					continue
				}
				if iss.WorkflowID == nil || *iss.WorkflowID != workflowID {
					t.Errorf("workflow_id = %v; want %s", iss.WorkflowID, workflowID)
				}
				if iss.RequestedByID == nil || *iss.RequestedByID != testUserID {
					t.Errorf("requested_by_id = %v; want %s", iss.RequestedByID, testUserID)
				}
				return
			}
			t.Fatalf("issue %s not in list response", issueID)
		})
	}
}
