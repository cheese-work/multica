package service

import (
	"context"
	"encoding/json"
	"strconv"
	"testing"

	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// System writers move an issue with no person or run at the keyboard, so an
// entry gate never holds them (CHE-2531 fix round). Each test gates the step
// the writer enters behind a block gate no issue can pass, then runs the writer.

// gateIssueSteps puts the fixture issue under a workflow with the given step
// keys, each behind a failing block gate, and sets its status.
func gateIssueSteps(t *testing.T, f *delegatedFailureFixture, status string, keys ...string) {
	t.Helper()
	ctx := context.Background()
	steps := make([]map[string]any, 0, len(keys))
	for _, key := range keys {
		steps = append(steps, map[string]any{
			"status_key": key,
			"handler":    map[string]any{"type": "none"},
			"entry":      []map[string]any{{"guard": "pr.ready", "mode": "block", "member_override": true}},
		})
	}
	raw, _ := json.Marshal(steps)
	var workflowID string
	if err := f.pool.QueryRow(ctx, `
		INSERT INTO issue_workflow (workspace_id, name, initial_status_key, steps)
		VALUES ($1, 'gated-system-writer', $2, $3::jsonb) RETURNING id::text`,
		f.workspaceID, keys[0], string(raw)).Scan(&workflowID); err != nil {
		t.Fatalf("seed workflow: %v", err)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE issue SET workflow_id = $2, status = $3 WHERE id = $1`, f.issueID, workflowID, status); err != nil {
		t.Fatalf("gate issue: %v", err)
	}
}

func issueStatus(t *testing.T, f *delegatedFailureFixture) string {
	t.Helper()
	var status string
	if err := f.pool.QueryRow(context.Background(), `SELECT status FROM issue WHERE id = $1`, f.issueID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	return status
}

func TestEntryGateSkipsTaskReset(t *testing.T) {
	f, svc := seedDelegatedFailureFixture(t)
	ctx := context.Background()
	gateIssueSteps(t, f, "in_progress", "todo", "in_progress")
	var taskID string
	if err := f.pool.QueryRow(ctx, `
		INSERT INTO agent_task_queue (agent_id, runtime_id, issue_id, status, priority, error, failure_reason, completed_at)
		VALUES ($1, $2, $3, 'failed', 0, 'boom', 'agent_error.process_failure', now()) RETURNING id::text`,
		f.coordinator, f.runtimeID, f.issueID).Scan(&taskID); err != nil {
		t.Fatalf("seed failed task: %v", err)
	}
	failed, err := svc.Queries.GetAgentTask(ctx, util.MustParseUUID(taskID))
	if err != nil {
		t.Fatal(err)
	}

	svc.HandleFailedTasks(ctx, []db.AgentTaskQueue{failed})

	if got := issueStatus(t, f); got != "todo" {
		t.Fatalf("status = %q, want todo: the task reset must pass a gated todo step", got)
	}
}

func TestEntryGateSkipsQoderLifecycle(t *testing.T) {
	for _, completing := range []bool{false, true} {
		name, from, want := "start", "todo", "in_progress"
		if completing {
			name, from, want = "completion", "in_progress", "in_review"
		}
		t.Run(name, func(t *testing.T) {
			f, svc := seedDelegatedFailureFixture(t)
			ctx := context.Background()
			if _, err := f.pool.Exec(ctx, `UPDATE agent_runtime SET provider = 'qoder_cloud' WHERE id = $1`, f.runtimeID); err != nil {
				t.Fatal(err)
			}
			gateIssueSteps(t, f, from, "todo", "in_progress", "in_review")
			issue, err := svc.Queries.GetIssue(ctx, util.MustParseUUID(f.issueID))
			if err != nil {
				t.Fatal(err)
			}
			marker := `{}`
			if completing {
				marker = `{"qoder_issue_revision": ` + strconv.FormatInt(issue.Revision, 10) + `}`
			}
			var taskID string
			if err := f.pool.QueryRow(ctx, `
				INSERT INTO agent_task_queue (agent_id, runtime_id, issue_id, status, priority, context)
				VALUES ($1, $2, $3, 'running', 0, $4::jsonb) RETURNING id::text`,
				f.coordinator, f.runtimeID, f.issueID, marker).Scan(&taskID); err != nil {
				t.Fatal(err)
			}
			task, err := svc.Queries.GetAgentTask(ctx, util.MustParseUUID(taskID))
			if err != nil {
				t.Fatal(err)
			}

			if _, err := syncQoderIssue(ctx, svc.Queries, task, completing); err != nil {
				t.Fatal(err)
			}

			if got := issueStatus(t, f); got != want {
				t.Fatalf("status = %q, want %q: the Qoder move must pass a gated step", got, want)
			}
		})
	}
}
