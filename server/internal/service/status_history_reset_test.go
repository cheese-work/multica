package service

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// A failed-task reset is a background status move: it writes its
// status_changed row with cause task_reset, the failed run as ref, and a null
// auth_source.
func TestHandleFailedTasksRecordsTaskReset(t *testing.T) {
	f, svc := seedDelegatedFailureFixture(t)
	ctx := context.Background()
	if _, err := f.pool.Exec(ctx, `UPDATE issue SET status = 'in_progress' WHERE id = $1`, f.issueID); err != nil {
		t.Fatal(err)
	}
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

	var raw []byte
	if err := f.pool.QueryRow(ctx, `SELECT details FROM activity_log WHERE issue_id = $1 AND action = 'status_changed'`, f.issueID).Scan(&raw); err != nil {
		t.Fatalf("want exactly one status_changed row: %v", err)
	}
	var details struct {
		From, To string
		Trigger  struct {
			V          int
			AuthSource *string `json:"auth_source"`
			Cause      struct {
				Kind string
				Ref  map[string]string
			}
		}
	}
	if err := json.Unmarshal(raw, &details); err != nil {
		t.Fatal(err)
	}
	if details.From != "in_progress" || details.To != "todo" || details.Trigger.V != 1 ||
		details.Trigger.Cause.Kind != "task_reset" || details.Trigger.Cause.Ref["run_id"] != taskID || details.Trigger.AuthSource != nil {
		t.Fatalf("details = %s", raw)
	}
}
