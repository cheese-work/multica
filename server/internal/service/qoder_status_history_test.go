package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/util"
)

func seedQoderStartTask(t *testing.T) (*delegatedFailureFixture, *TaskService, string) {
	t.Helper()
	fixture, svc := seedDelegatedFailureFixture(t)
	ctx := context.Background()
	for _, stmt := range []string{
		`UPDATE agent_runtime SET provider = 'qoder_cloud' WHERE id = ` + "'" + fixture.runtimeID + "'",
		`UPDATE issue SET status = 'todo' WHERE id = ` + "'" + fixture.issueID + "'",
	} {
		if _, err := fixture.pool.Exec(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	var taskID string
	if err := fixture.pool.QueryRow(ctx, `
		INSERT INTO agent_task_queue (agent_id, runtime_id, issue_id, status, priority, context)
		VALUES ($1, $2, $3, 'running', 0, '{}'::jsonb) RETURNING id::text`,
		fixture.coordinator, fixture.runtimeID, fixture.issueID).Scan(&taskID); err != nil {
		t.Fatal(err)
	}
	return fixture, svc, taskID
}

func TestSyncQoderIssueRecordsStatusChangeInTransaction(t *testing.T) {
	fixture, svc, taskID := seedQoderStartTask(t)
	ctx := context.Background()
	task, err := svc.Queries.GetAgentTask(ctx, util.MustParseUUID(taskID))
	if err != nil {
		t.Fatal(err)
	}

	change, err := syncQoderIssue(ctx, svc.Queries, task, false)
	if err != nil || change == nil {
		t.Fatalf("syncQoderIssue = %v, %v", change, err)
	}

	var raw []byte
	var n int
	var rowID string
	if err := fixture.pool.QueryRow(ctx, `SELECT count(*), min(details::text)::bytea, min(id::text) FROM activity_log WHERE issue_id = $1 AND action = 'status_changed'`, fixture.issueID).Scan(&n, &raw, &rowID); err != nil || n != 1 {
		t.Fatalf("status_changed rows = %d, %v; want 1", n, err)
	}
	var details struct {
		From, To string
		Trigger  struct {
			V          int
			AuthSource *string `json:"auth_source"`
			Cause      struct {
				Kind string
				By   struct{ Type, ID string }
				Ref  map[string]string
			}
		}
	}
	if err := json.Unmarshal(raw, &details); err != nil {
		t.Fatal(err)
	}
	cause := details.Trigger.Cause
	if details.From != "todo" || details.To != "in_progress" || details.Trigger.V != 1 || details.Trigger.AuthSource != nil ||
		cause.Kind != "qoder" || cause.By.Type != "agent" || cause.By.ID != util.UUIDToString(task.AgentID) || cause.Ref["run_id"] != taskID {
		t.Fatalf("details = %s", raw)
	}
	if change.activity.row.ID != util.MustParseUUID(rowID) {
		t.Fatal("change must name the committed row")
	}
}

func TestSyncQoderIssueRollsBackWithActivityFailure(t *testing.T) {
	fixture, svc, taskID := seedQoderStartTask(t)
	ctx := context.Background()
	fn := "che2841_fail_" + strings.ReplaceAll(fixture.issueID, "-", "")
	for _, stmt := range []string{
		fmt.Sprintf(`CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected activity failure'; END $$`, fn),
		fmt.Sprintf(`CREATE TRIGGER %s BEFORE INSERT ON activity_log FOR EACH ROW WHEN (NEW.issue_id = '%s' AND NEW.action = 'status_changed') EXECUTE FUNCTION %s()`, fn, fixture.issueID, fn),
	} {
		if _, err := fixture.pool.Exec(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		fixture.pool.Exec(ctx, fmt.Sprintf(`DROP TRIGGER IF EXISTS %s ON activity_log`, fn))
		fixture.pool.Exec(ctx, fmt.Sprintf(`DROP FUNCTION IF EXISTS %s()`, fn))
	})
	task, err := svc.Queries.GetAgentTask(ctx, util.MustParseUUID(taskID))
	if err != nil {
		t.Fatal(err)
	}
	tx, err := fixture.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, err = syncQoderIssue(ctx, svc.Queries.WithTx(tx), task, false)
	tx.Rollback(ctx)
	if err == nil {
		t.Fatal("syncQoderIssue must fail when the activity write fails")
	}

	var status string
	if err := fixture.pool.QueryRow(ctx, `SELECT status FROM issue WHERE id = $1`, fixture.issueID).Scan(&status); err != nil || status != "todo" {
		t.Fatalf("status = %q, %v; want todo after rollback", status, err)
	}
}
