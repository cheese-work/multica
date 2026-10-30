package handler

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// CHE-2297 L2b: a run reserves E tokens at admission and its usage report
// settles them. The workflow does not store A and E until L2e, so these tests
// register the token source L2e will provide.

type tokenFx struct {
	reserve atomic.Int64
	epoch   atomic.Int64 // unix nano; 0: the epoch is long past
	active  atomic.Bool
}

func withTokenAccounting(t *testing.T, reserve int64) *tokenFx {
	t.Helper()
	fx := &tokenFx{}
	fx.reserve.Store(reserve)
	fx.active.Store(true)
	service.SetRunBudgetTokens(func(_ context.Context, _ *db.Queries, _ db.Issue) (service.RunBudgetTokens, bool, error) {
		if !fx.active.Load() {
			return service.RunBudgetTokens{}, false, nil
		}
		epoch := time.Unix(0, fx.epoch.Load())
		if fx.epoch.Load() == 0 {
			epoch = time.Unix(0, 0)
		}
		return service.RunBudgetTokens{Reserve: fx.reserve.Load(), Epoch: epoch}, true, nil
	})
	t.Cleanup(func() { service.SetRunBudgetTokens(nil) })
	return fx
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

func tokenEntries(t *testing.T, issueID, taskID, kind string) int {
	t.Helper()
	return dbfx.Count(t, `SELECT count(*) FROM issue_run_budget_entry WHERE issue_id=$1 AND kind=$2 AND event_key LIKE $3`, issueID, kind, kind+":"+taskID+"%")
}

func tokenSum(t *testing.T, issueID string) int64 {
	t.Helper()
	return int64(dbfx.Count(t, `SELECT COALESCE(SUM(tokens_delta), 0) FROM issue_run_budget_entry WHERE issue_id=$1`, issueID))
}

func TestRunBudgetTokensReportReleasesTheStoredReservationOnce(t *testing.T) {
	if testHandler == nil {
		t.Fatal("isolated database required")
	}
	c, _ := budgetedAt(t, 40, 1)
	withTokenAccounting(t, 1000)
	task := admitRun(t, c)
	if got := tokenSum(t, c.issueID); got != -1000 {
		t.Fatalf("tokens after admission = %d, want -1000 (the reservation)", got)
	}
	// weighted = 300 + 200 + 50 + 0.1 x 1000
	reportUsage(t, task, usageRow{Model: "m1", In: 300, Out: 200, CacheWrite: 50, CacheRead: 1000})
	if got := tokenSum(t, c.issueID); got != -650 {
		t.Fatalf("tokens after the report = %d, want -650 (-1000 reserved, +1000 released, -650 used)", got)
	}
	reportUsage(t, task, usageRow{Model: "m1", In: 300, Out: 200, CacheWrite: 50, CacheRead: 1000})
	if n := tokenEntries(t, c.issueID, task, "release"); n != 1 {
		t.Fatalf("release entries after a retried report = %d, want 1", n)
	}
	if n := tokenEntries(t, c.issueID, task, "tokens"); n != 1 {
		t.Fatalf("token entries after a retried identical report = %d, want 1", n)
	}
	if got := tokenSum(t, c.issueID); got != -650 {
		t.Fatalf("tokens after a retried report = %d, want -650", got)
	}
}

func TestRunBudgetTokensTaskAdmittedBeforeActivationGetsADebitAndNoRelease(t *testing.T) {
	if testHandler == nil {
		t.Fatal("isolated database required")
	}
	c, _ := budgetedAt(t, 40, 1)
	fx := withTokenAccounting(t, 1000)
	fx.active.Store(false)
	task := admitRun(t, c)
	if got := tokenSum(t, c.issueID); got != 0 {
		t.Fatalf("tokens after an inactive admission = %d, want 0 (no reservation)", got)
	}
	fx.active.Store(true)
	reportUsage(t, task, usageRow{Model: "m1", In: 400, Out: 100})
	if got := tokenSum(t, c.issueID); got != -500 {
		t.Fatalf("tokens after the report = %d, want -500 (debit only)", got)
	}
	if n := tokenEntries(t, c.issueID, task, "release"); n != 0 {
		t.Fatalf("release entries for a task with no reservation = %d, want 0", n)
	}
}

func TestRunBudgetTokensChangedReservationReleasesTheStoredAmount(t *testing.T) {
	if testHandler == nil {
		t.Fatal("isolated database required")
	}
	c, _ := budgetedAt(t, 40, 1)
	fx := withTokenAccounting(t, 1000)
	task := admitRun(t, c)
	fx.reserve.Store(5000)
	reportUsage(t, task, usageRow{Model: "m1", In: 100})
	if got := dbfx.Count(t, `SELECT tokens_delta FROM issue_run_budget_entry WHERE issue_id=$1 AND kind='release'`, c.issueID); got != 1000 {
		t.Fatalf("released %d, want the stored 1000, not the new 5000", got)
	}
}

func TestRunBudgetTokensCorrectedReportAddsOneEntryForTheDifference(t *testing.T) {
	if testHandler == nil {
		t.Fatal("isolated database required")
	}
	c, _ := budgetedAt(t, 40, 1)
	withTokenAccounting(t, 1000)
	task := admitRun(t, c)
	reportUsage(t, task, usageRow{Model: "m1", In: 600})
	reportUsage(t, task, usageRow{Model: "m1", In: 900})
	if n := tokenEntries(t, c.issueID, task, "tokens"); n != 2 {
		t.Fatalf("token entries after a correction = %d, want 2", n)
	}
	if got := dbfx.Count(t, `SELECT tokens_delta FROM issue_run_budget_entry WHERE issue_id=$1 AND event_key=$2`, c.issueID, "tokens:"+task+":2"); got != -300 {
		t.Fatalf("correction entry = %d, want -300", got)
	}
	if got := tokenSum(t, c.issueID); got != -900 {
		t.Fatalf("tokens = %d, want -900 (the corrected usage; the reservation is released)", got)
	}
}

func TestRunBudgetTokensTwoModelsSettleInOneEntry(t *testing.T) {
	if testHandler == nil {
		t.Fatal("isolated database required")
	}
	c, _ := budgetedAt(t, 40, 1)
	withTokenAccounting(t, 1000)
	task := admitRun(t, c)
	reportUsage(t, task, usageRow{Model: "m1", In: 100, Out: 50}, usageRow{Model: "m2", In: 200, Out: 25})
	if n := tokenEntries(t, c.issueID, task, "tokens"); n != 1 {
		t.Fatalf("token entries for a two-model report = %d, want 1 (one transaction)", n)
	}
	if got := tokenSum(t, c.issueID); got != -375 {
		t.Fatalf("tokens = %d, want -375 (both models)", got)
	}
}

func TestRunBudgetTokensTerminalRunWithoutUsageKeepsItsReservation(t *testing.T) {
	if testHandler == nil {
		t.Fatal("isolated database required")
	}
	c, _ := budgetedAt(t, 40, 1)
	withTokenAccounting(t, 1000)
	task := admitRun(t, c)
	finishRuns(t, c)
	// A report with no rows is a terminal run without usage.
	reportUsage(t, task)
	if got := tokenSum(t, c.issueID); got != -1000 {
		t.Fatalf("tokens = %d, want -1000 (the reservation is the charge)", got)
	}
	if n := tokenEntries(t, c.issueID, task, "release"); n != 0 {
		t.Fatalf("release entries = %d, want 0", n)
	}
}

func TestRunBudgetTokensTaskCreatedBeforeTheEpochIsNeverSettled(t *testing.T) {
	if testHandler == nil {
		t.Fatal("isolated database required")
	}
	c, _ := budgetedAt(t, 40, 1)
	fx := withTokenAccounting(t, 1000)
	fx.active.Store(false)
	task := admitRun(t, c)
	fx.active.Store(true)
	fx.epoch.Store(time.Now().Add(time.Minute).UnixNano())
	reportUsage(t, task, usageRow{Model: "m1", In: 700})
	if got := tokenSum(t, c.issueID); got != 0 {
		t.Fatalf("tokens = %d, want 0: a task older than the epoch is outside token accounting", got)
	}
}

func TestRunBudgetTokensOffWritesNothing(t *testing.T) {
	if testHandler == nil {
		t.Fatal("isolated database required")
	}
	c, _ := budgetedAt(t, 40, 1)
	task := admitRun(t, c)
	reportUsage(t, task, usageRow{Model: "m1", In: 700})
	if got := dbfx.Count(t, `SELECT count(*) FROM issue_run_budget_entry WHERE issue_id=$1 AND (tokens_delta<>0 OR kind NOT IN ('window','run'))`, c.issueID); got != 0 {
		t.Fatalf("%d token entries with accounting off, want none", got)
	}
}

var _ pgtype.UUID
