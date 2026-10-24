package service

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// A Qoder run moves its issue to in_progress on start and in_review on
// completion. Both keys are optional workflow columns, so the move must stay
// inside the issue's effective workflow: its own override, else its project's.
func TestSyncQoderIssueStaysInsideEffectiveWorkflow(t *testing.T) {
	cases := []struct {
		name       string
		scope      string // "issue" override or "project" workflow
		steps      string
		completing bool
		from, want string
	}{
		{"override without in_progress keeps start", "issue", `["todo","implement","done"]`, false, "todo", "todo"},
		{"project without in_progress keeps start", "project", `["todo","implement","done"]`, false, "todo", "todo"},
		{"override without in_review keeps completion", "issue", `["todo","in_progress","done"]`, true, "in_progress", "in_progress"},
		{"project without in_review keeps completion", "project", `["todo","in_progress","done"]`, true, "in_progress", "in_progress"},
		{"override with in_progress still starts", "issue", `["todo","in_progress","in_review","done"]`, false, "todo", "in_progress"},
		{"project with in_review still completes", "project", `["todo","in_progress","in_review","done"]`, true, "in_progress", "in_review"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixture, svc := seedDelegatedFailureFixture(t)
			ctx := context.Background()
			exec := func(sql string, args ...any) {
				t.Helper()
				if _, err := fixture.pool.Exec(ctx, sql, args...); err != nil {
					t.Fatalf("%s: %v", sql, err)
				}
			}
			exec(`UPDATE agent_runtime SET provider = 'qoder_cloud' WHERE id = $1`, fixture.runtimeID)

			steps := make([]map[string]any, 0, 4)
			var keys []string
			if err := json.Unmarshal([]byte(tc.steps), &keys); err != nil {
				t.Fatal(err)
			}
			for _, key := range keys {
				steps = append(steps, map[string]any{"status_key": key, "handler": map[string]any{"type": "none"}})
			}
			raw, _ := json.Marshal(steps)
			var workflowID string
			if err := fixture.pool.QueryRow(ctx, `
				INSERT INTO issue_workflow (workspace_id, name, initial_status_key, steps)
				VALUES ($1, $2, 'todo', $3::jsonb) RETURNING id::text`,
				fixture.workspaceID, "qoder-"+tc.scope, string(raw)).Scan(&workflowID); err != nil {
				t.Fatalf("seed workflow: %v", err)
			}
			if tc.scope == "issue" {
				exec(`UPDATE issue SET workflow_id = $2 WHERE id = $1`, fixture.issueID, workflowID)
			} else {
				var projectID string
				if err := fixture.pool.QueryRow(ctx, `
					INSERT INTO project (workspace_id, title, workflow_id) VALUES ($1, 'qoder-project', $2) RETURNING id::text`,
					fixture.workspaceID, workflowID).Scan(&projectID); err != nil {
					t.Fatalf("seed project: %v", err)
				}
				exec(`UPDATE issue SET project_id = $2 WHERE id = $1`, fixture.issueID, projectID)
			}
			exec(`UPDATE issue SET status = $2 WHERE id = $1`, fixture.issueID, tc.from)

			var issue db.Issue
			issue, err := svc.Queries.GetIssue(ctx, util.MustParseUUID(fixture.issueID))
			if err != nil {
				t.Fatal(err)
			}
			var taskID string
			taskContext := `{}`
			if tc.completing {
				taskContext = fmt.Sprintf(`{"qoder_issue_revision": %d}`, issue.Revision)
			}
			if err := fixture.pool.QueryRow(ctx, `
				INSERT INTO agent_task_queue (agent_id, runtime_id, issue_id, status, priority, context)
				VALUES ($1, $2, $3, 'running', 0, $4::jsonb) RETURNING id::text`,
				fixture.coordinator, fixture.runtimeID, fixture.issueID, taskContext).Scan(&taskID); err != nil {
				t.Fatalf("seed task: %v", err)
			}
			task, err := svc.Queries.GetAgentTask(ctx, util.MustParseUUID(taskID))
			if err != nil {
				t.Fatal(err)
			}

			if _, err := syncQoderIssue(ctx, svc.Queries, task, tc.completing); err != nil {
				t.Fatal(err)
			}
			after, err := svc.Queries.GetIssue(ctx, util.MustParseUUID(fixture.issueID))
			if err != nil {
				t.Fatal(err)
			}
			if after.Status != tc.want {
				t.Fatalf("status = %q, want %q (workflow %s)", after.Status, tc.want, tc.steps)
			}
		})
	}
}
