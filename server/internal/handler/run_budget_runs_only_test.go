package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// CHE-5501 (S1): the run budget counts runs only. The workflow token columns
// stay in the schema until S5 (CHE-5509); nothing reads or writes them.

// setLegacyTokenColumns sets both workflow token columns by SQL, as a workflow
// saved before S1 would have them.
func setLegacyTokenColumns(t *testing.T, c edgeCase) {
	t.Helper()
	if _, err := testPool.Exec(context.Background(), `UPDATE issue_workflow SET run_budget_token_allowance=10000, run_budget_token_reserve=100 WHERE id=$1`, c.workflowID); err != nil {
		t.Fatal(err)
	}
}

func TestRunBudgetSnapshotJSONHasNoTokenKey(t *testing.T) {
	if testHandler == nil {
		t.Fatal("isolated database required")
	}
	ctx := context.Background()
	c, _ := budgetedAt(t, 40, 1)
	setLegacyTokenColumns(t, c)
	issue, err := testHandler.Queries.GetIssue(ctx, parseUUID(c.issueID))
	if err != nil {
		t.Fatal(err)
	}
	snap, err := service.LoadRunBudgetSnapshot(ctx, testHandler.Queries, issue)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil {
		t.Fatal(err)
	}
	if !snap.Budgeted {
		t.Fatalf("snapshot is not budgeted: %s", raw)
	}
	for k := range keys {
		if strings.HasPrefix(k, "tokens_") {
			t.Errorf("snapshot JSON has key %q; the budget counts runs only: %s", k, raw)
		}
	}
}

// A workflow with both token columns set and the token balance spent is
// admitted on runs alone.
func TestRunBudgetSpentTokenBalanceDoesNotRefuseAdmission(t *testing.T) {
	if testHandler == nil {
		t.Fatal("isolated database required")
	}
	ctx := context.Background()
	bc := newBudgetCase(t, 2314003, mergeEdgeSteps, 40)
	setLegacyTokenColumns(t, bc.edgeCase)
	task := admitRun(t, bc.edgeCase)
	// 13000 weighted tokens against an allowance of 10000: the old token balance is negative.
	reportUsage(t, task, usageRow{Model: "m1", In: 12000, Out: 1000})
	finishRuns(t, bc.edgeCase)
	issue, err := testHandler.Queries.GetIssue(ctx, parseUUID(bc.issueID))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := testHandler.TaskService.EnqueueTaskForIssue(ctx, issue); err != nil {
		if errors.Is(err, service.ErrRunBudgetExhausted) {
			t.Fatalf("admission refused on a spent token balance with runs left: %v", err)
		}
		t.Fatal(err)
	}
}

// Grants credit runs and no token value, with the token columns set.
func TestRunBudgetGrantsAndRefundWriteNoTokenValue(t *testing.T) {
	if testHandler == nil {
		t.Fatal("isolated database required")
	}
	ctx := context.Background()
	c, _ := budgetedAt(t, 40, 2)
	setLegacyTokenColumns(t, c)
	admitRun(t, c)
	finishRuns(t, c)
	grantHead(t, c.issueID, "h1")
	issue, err := testHandler.Queries.GetIssue(ctx, parseUUID(c.issueID))
	if err != nil {
		t.Fatal(err)
	}
	task, err := testHandler.TaskService.EnqueueTaskForIssue(ctx, issue)
	if err != nil {
		t.Fatal(err)
	}
	cancelled, err := testHandler.TaskService.CancelTaskWithReason(ctx, task.ID, "cancelled", service.FailureReasonSupersededHead)
	if err != nil {
		t.Fatal(err)
	}
	if err := inGrantTx(t, func(q *db.Queries) error { return service.RefundSupersededRun(ctx, q, *cancelled) }); err != nil {
		t.Fatal(err)
	}
	if got := entryCount(t, c.issueID, "head"); got != 1 {
		t.Fatalf("head grants = %d, want 1", got)
	}
	if got := entryCount(t, c.issueID, "refund"); got != 1 {
		t.Fatalf("refunds = %d, want 1", got)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM issue_run_budget_entry WHERE issue_id=$1 AND tokens_delta<>0`, c.issueID); n != 0 {
		t.Fatalf("%d ledger entries carry a token value, want 0", n)
	}
}

// No hand-written query names the retired token ledger value or the two
// workflow token columns. The columns themselves go in S5 (CHE-5509).
func TestNoQueryNamesRetiredTokenColumns(t *testing.T) {
	files, err := filepath.Glob("../../pkg/db/queries/*.sql")
	if err != nil || len(files) == 0 {
		t.Fatalf("queries not found: %v (%d files)", err, len(files))
	}
	retired := regexp.MustCompile(`tokens_delta|run_budget_token_allowance|run_budget_token_reserve`)
	for _, f := range files {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(src), "\n") {
			if retired.MatchString(line) {
				t.Errorf("%s:%d names a retired token column: %s", filepath.Base(f), i+1, strings.TrimSpace(line))
			}
		}
	}
}

type usageRow struct {
	Model                          string
	In, Out, CacheRead, CacheWrite int64
}

func reportUsage(t *testing.T, taskID string, rows ...usageRow) {
	t.Helper()
	usage := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		usage = append(usage, map[string]any{
			"provider": "anthropic", "model": r.Model, "input_tokens": r.In, "output_tokens": r.Out,
			"cache_read_tokens": r.CacheRead, "cache_write_tokens": r.CacheWrite,
		})
	}
	req := withURLParam(newOwnerDaemonRequest("POST", "/api/daemon/tasks/"+taskID+"/usage", map[string]any{"usage": usage}), "taskId", taskID)
	testutil.Call(t, testHandler.ReportTaskUsage, req).Want(http.StatusOK)
}

// admitRun admits one run on the case's issue and returns its task id.
func admitRun(t *testing.T, c edgeCase) string {
	t.Helper()
	issue, err := testHandler.Queries.GetIssue(context.Background(), parseUUID(c.issueID))
	if err != nil {
		t.Fatal(err)
	}
	task, err := testHandler.TaskService.EnqueueTaskForIssue(context.Background(), issue)
	if err != nil {
		t.Fatalf("admit run: %v", err)
	}
	return uuidToString(task.ID)
}

// memberMove opens a new window: a person moved the issue's status.
func memberMove(t *testing.T, c edgeCase) {
	t.Helper()
	time.Sleep(5 * time.Millisecond)
	if _, err := testPool.Exec(context.Background(), `INSERT INTO activity_log (workspace_id, issue_id, actor_type, actor_id, action, details)
		VALUES ($1, $2, 'member', $3, 'status_changed', '{"trigger":{"cause":{"kind":"ui_or_api"},"auth_source":"interactive_jwt"}}')`, testWorkspaceID, c.issueID, testUserID); err != nil {
		t.Fatal(err)
	}
}

func entryCount(t *testing.T, issueID, kind string) int {
	t.Helper()
	return dbfx.Count(t, `SELECT count(*) FROM issue_run_budget_entry WHERE issue_id=$1 AND kind=$2`, issueID, kind)
}

// A usage report takes the task lock before it writes any usage row, so two
// reports for one task cannot each hold a row the other needs.
func TestRunBudgetUsageReportLocksTheTaskBeforeWritingUsageRows(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Fatal("isolated database required")
	}
	c, _ := budgetedAt(t, 40, 1)
	task := admitRun(t, c)
	lockedReportWaits(t, task)
}

// A chat task has no issue; its reports serialize on the same task lock.
func TestRunBudgetUsageReportOfATaskWithoutAnIssueLocksTheTaskBeforeWritingUsageRows(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Fatal("isolated database required")
	}
	ctx := context.Background()
	session := newBuilderSession(t)
	var task string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO agent_task_queue (agent_id, chat_session_id, status, priority, context, runtime_id)
		VALUES ($1, $2, 'running', 2, '{}'::jsonb, $3) RETURNING id
	`, session.BuilderAgentID, session.SessionID, testRuntimeID).Scan(&task); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE id = $1`, task)
		_, _ = testPool.Exec(context.Background(), `DELETE FROM task_usage WHERE task_id = $1`, task)
	})
	lockedReportWaits(t, task)
}

// lockedReportWaits holds the task's usage-report lock and shows that a second
// report waits on it without having touched a usage row.
func lockedReportWaits(t *testing.T, task string) {
	t.Helper()
	ctx := context.Background()
	reportUsage(t, task, usageRow{Model: "a", In: 1}, usageRow{Model: "b", In: 1})
	holder, err := testHandler.TxStarter.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback(ctx)
	if err := testHandler.Queries.WithTx(holder).LockTaskUsageReport(ctx, parseUUID(task)); err != nil {
		t.Fatal(err)
	}
	// Without a task lock the report would write row "b" and then wait here.
	if _, err := holder.Exec(ctx, `SELECT 1 FROM task_usage WHERE task_id=$1 AND model='a' FOR UPDATE`, task); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		reportUsage(t, task, usageRow{Model: "b", In: 2}, usageRow{Model: "a", In: 2})
	}()
	time.Sleep(500 * time.Millisecond)
	probe, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, err = probe.Exec(ctx, `SELECT 1 FROM task_usage WHERE task_id=$1 AND model='b' FOR UPDATE NOWAIT`, task)
	probe.Rollback(ctx)
	if err != nil {
		t.Fatalf("a usage row is locked while the report waits for the task lock: %v", err)
	}
	if err := holder.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	<-done
}
