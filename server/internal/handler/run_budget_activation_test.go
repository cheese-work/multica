package handler

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// CHE-2297 L2e: token accounting turns on per workflow, once per issue, and a
// window gets at most one allowance. A = 10000 and E = 100 weighted tokens.

const (
	actAllowance = 10000
	actReserve   = 100
)

// storeTokenValues stores A and E on the case's workflow.
func storeTokenValues(t *testing.T, c edgeCase) {
	t.Helper()
	if _, err := testPool.Exec(context.Background(), `UPDATE issue_workflow SET run_budget_token_allowance=$2, run_budget_token_reserve=$3 WHERE id=$1`, c.workflowID, actAllowance, actReserve); err != nil {
		t.Fatal(err)
	}
}

// memberMove opens a new window: a person moved the issue's status.
func memberMove(t *testing.T, c edgeCase) {
	t.Helper()
	time.Sleep(5 * time.Millisecond)
	if _, err := testPool.Exec(context.Background(), `INSERT INTO activity_log (workspace_id, issue_id, actor_type, actor_id, action, details)
		VALUES ($1, $2, 'member', $3, 'status_changed', '{}')`, testWorkspaceID, c.issueID, testUserID); err != nil {
		t.Fatal(err)
	}
}

func entryCount(t *testing.T, issueID, kind string) int {
	t.Helper()
	return dbfx.Count(t, `SELECT count(*) FROM issue_run_budget_entry WHERE issue_id=$1 AND kind=$2`, issueID, kind)
}

func entrySum(t *testing.T, issueID, kind string) int64 {
	t.Helper()
	return int64(dbfx.Count(t, `SELECT COALESCE(SUM(tokens_delta), 0) FROM issue_run_budget_entry WHERE issue_id=$1 AND kind=$2`, issueID, kind))
}

// legacyTask is the one run budgetedAt left on the issue, admitted before token
// accounting existed.
func legacyTask(t *testing.T, c edgeCase) string {
	t.Helper()
	var id string
	if err := testPool.QueryRow(context.Background(), `SELECT id FROM agent_task_queue WHERE issue_id=$1 ORDER BY created_at LIMIT 1`, c.issueID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func wantTokens(t *testing.T, c edgeCase, want int64, why string) {
	t.Helper()
	if got := tokenSum(t, c.issueID); got != want {
		t.Fatalf("token balance %s = %d, want %d", why, got, want)
	}
}

func TestRunBudgetActivationByFirstAdmissionSettlesEarlierUsage(t *testing.T) {
	if testHandler == nil {
		t.Fatal("isolated database required")
	}
	c, _ := budgetedAt(t, 40, 1)
	legacy := legacyTask(t, c)
	reportUsage(t, legacy, usageRow{Model: "m1", In: 3000, Out: 1000})
	storeTokenValues(t, c)
	admitRun(t, c)
	wantTokens(t, c, actAllowance-4000-actReserve, "after the first admission (A - usage - E)")
	if n := entryCount(t, c.issueID, "allowance"); n != 1 {
		t.Fatalf("allowance entries = %d, want 1", n)
	}
	if n := entryCount(t, c.issueID, "token_activation"); n != 1 {
		t.Fatalf("activation entries = %d, want 1", n)
	}
	finishRuns(t, c)
	admitRun(t, c)
	if n := entryCount(t, c.issueID, "allowance"); n != 1 {
		t.Fatalf("allowance entries after a second admission = %d, want 1", n)
	}
	// A retried report writes nothing; a corrected one writes the difference.
	reportUsage(t, legacy, usageRow{Model: "m1", In: 3000, Out: 1000})
	if n := tokenEntries(t, c.issueID, legacy, "tokens"); n != 1 {
		t.Fatalf("token entries of the settled task after a retry = %d, want 1", n)
	}
	reportUsage(t, legacy, usageRow{Model: "m1", In: 3500, Out: 1000})
	if n := tokenEntries(t, c.issueID, legacy, "tokens"); n != 2 {
		t.Fatalf("token entries after a correction = %d, want 2", n)
	}
	wantTokens(t, c, actAllowance-4500-2*actReserve, "after the correction")
}

func TestRunBudgetActivationByUsageReportBeforeAnyAdmission(t *testing.T) {
	if testHandler == nil {
		t.Fatal("isolated database required")
	}
	c, _ := budgetedAt(t, 40, 1)
	storeTokenValues(t, c)
	reportUsage(t, legacyTask(t, c), usageRow{Model: "m1", In: 5000, Out: 1000})
	wantTokens(t, c, actAllowance-6000, "after the first event, a usage report (A - legacy usage)")
	admitRun(t, c)
	wantTokens(t, c, actAllowance-6000-actReserve, "after the next admission")
	if n := entryCount(t, c.issueID, "allowance"); n != 1 {
		t.Fatalf("allowance entries = %d, want 1", n)
	}
}

func TestRunBudgetMemberMoveBeforeActivationGivesRunsAndNoTokenEntry(t *testing.T) {
	if testHandler == nil {
		t.Fatal("isolated database required")
	}
	c, _ := budgetedAt(t, 40, 1)
	storeTokenValues(t, c)
	memberMove(t, c)
	if n, err := service.OpenRunBudgetWindow(context.Background(), testHandler.Queries, parseUUID(c.issueID)); err != nil || n != 40 {
		t.Fatalf("open window = %d, %v, want the cap 40", n, err)
	}
	if got := ledgerBalance(t, c.issueID); got != 40 {
		t.Fatalf("runs after the move = %d, want 40 at once", got)
	}
	if n := entryCount(t, c.issueID, "allowance") + entryCount(t, c.issueID, "token_activation"); n != 0 {
		t.Fatalf("token entries after a move before activation = %d, want 0", n)
	}
	admitRun(t, c)
	wantTokens(t, c, actAllowance-actReserve, "after the first admission (one allowance, not two)")
	if n := entryCount(t, c.issueID, "allowance"); n != 1 {
		t.Fatalf("allowance entries = %d, want 1", n)
	}
}

func TestRunBudgetTopUpBeforeActivationThenFirstReportGivesOneAllowance(t *testing.T) {
	if testHandler == nil {
		t.Fatal("isolated database required")
	}
	c, _ := budgetedAt(t, 40, 1)
	storeTokenValues(t, c)
	ctx := context.Background()
	// The top-up endpoint (L2d) opens its window through the shared claim.
	claim := func() {
		err := testHandler.withTx(ctx, func(q *db.Queries) error {
			if _, err := q.LockIssueRunBudget(ctx, parseUUID(c.issueID)); err != nil {
				return err
			}
			issue, err := q.GetIssue(ctx, parseUUID(c.issueID))
			if err != nil {
				return err
			}
			return service.ClaimRunBudgetAllowance(ctx, q, issue, "top-up-window")
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	claim()
	if n := entryCount(t, c.issueID, "allowance"); n != 0 {
		t.Fatalf("allowance entries from a top-up before activation = %d, want 0", n)
	}
	reportUsage(t, legacyTask(t, c), usageRow{Model: "m1", In: 1000})
	if n := entryCount(t, c.issueID, "allowance"); n != 1 {
		t.Fatalf("allowance entries after the first report = %d, want 1", n)
	}
	claim()
	if n := entryCount(t, c.issueID, "allowance"); n != 2 {
		t.Fatalf("allowance entries after a top-up window = %d, want 2 (one per window)", n)
	}
	claim()
	if n := entryCount(t, c.issueID, "allowance"); n != 2 {
		t.Fatalf("allowance entries after a repeated claim = %d, want 2", n)
	}
}

func TestRunBudgetMemberMoveAndFirstAdmissionTogetherGiveOneAllowance(t *testing.T) {
	if testHandler == nil {
		t.Fatal("isolated database required")
	}
	for _, moveFirst := range []bool{true, false} {
		c, _ := budgetedAt(t, 40, 1)
		storeTokenValues(t, c)
		memberMove(t, c)
		var wg sync.WaitGroup
		openWindow := func() {
			defer wg.Done()
			if _, err := service.OpenRunBudgetWindow(context.Background(), testHandler.Queries, parseUUID(c.issueID)); err != nil {
				t.Error(err)
			}
		}
		admit := func() {
			defer wg.Done()
			issue, err := testHandler.Queries.GetIssue(context.Background(), parseUUID(c.issueID))
			if err != nil {
				t.Error(err)
				return
			}
			if _, err := testHandler.TaskService.EnqueueTaskForIssue(context.Background(), issue); err != nil && !errors.Is(err, service.ErrDuplicatePendingTask) {
				t.Error(err)
			}
		}
		wg.Add(2)
		if moveFirst {
			go openWindow()
			time.Sleep(20 * time.Millisecond)
			go admit()
		} else {
			go admit()
			time.Sleep(20 * time.Millisecond)
			go openWindow()
		}
		wg.Wait()
		if n := entryCount(t, c.issueID, "allowance"); n != 1 {
			t.Fatalf("moveFirst=%v: allowance entries = %d, want 1", moveFirst, n)
		}
		wantTokens(t, c, actAllowance-actReserve, "after a move and a first admission")
	}
}

func TestRunBudgetEachNewWindowAddsExactlyOneAllowance(t *testing.T) {
	if testHandler == nil {
		t.Fatal("isolated database required")
	}
	c, _ := budgetedAt(t, 40, 1)
	storeTokenValues(t, c)
	admitRun(t, c)
	finishRuns(t, c)
	memberMove(t, c)
	admitRun(t, c)
	if got := entrySum(t, c.issueID, "allowance"); got != 2*actAllowance {
		t.Fatalf("allowances after one new window = %d, want %d", got, 2*actAllowance)
	}
	finishRuns(t, c)
	admitRun(t, c)
	if got := entrySum(t, c.issueID, "allowance"); got != 2*actAllowance {
		t.Fatalf("allowances after a third admission = %d, want %d", got, 2*actAllowance)
	}
}

func TestRunBudgetTaskCreatedBeforeTheEpochIsNeverSettledByActivation(t *testing.T) {
	if testHandler == nil {
		t.Fatal("isolated database required")
	}
	c, _ := budgetedAt(t, 40, 1)
	legacy := legacyTask(t, c)
	reportUsage(t, legacy, usageRow{Model: "m1", In: 3000})
	storeTokenValues(t, c)
	memberMove(t, c) // the epoch is this window's start, after the legacy task
	admitRun(t, c)
	reportUsage(t, legacy, usageRow{Model: "m1", In: 4000})
	if n := tokenEntries(t, c.issueID, legacy, "tokens"); n != 0 {
		t.Fatalf("token entries of a task created before the epoch = %d, want 0", n)
	}
	wantTokens(t, c, actAllowance-actReserve, "with the old task outside token accounting")
}

func TestRunBudgetConcurrentFirstEventsActivateOnce(t *testing.T) {
	if testHandler == nil {
		t.Fatal("isolated database required")
	}
	c, _ := budgetedAt(t, 40, 1)
	legacy := legacyTask(t, c)
	storeTokenValues(t, c)
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			issue, err := testHandler.Queries.GetIssue(context.Background(), parseUUID(c.issueID))
			if err != nil {
				t.Error(err)
				return
			}
			if _, err := testHandler.TaskService.EnqueueTaskForIssue(context.Background(), issue); err != nil && !errors.Is(err, service.ErrDuplicatePendingTask) {
				t.Error(err)
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		reportUsage(t, legacy, usageRow{Model: "m1", In: 2000})
	}()
	wg.Wait()
	if a, n := entryCount(t, c.issueID, "token_activation"), entryCount(t, c.issueID, "allowance"); a != 1 || n != 1 {
		t.Fatalf("activation entries = %d, allowance entries = %d, want 1 and 1", a, n)
	}
}

func TestRunBudgetTokenWarningAtEightyPercentComesFromTheUsageReport(t *testing.T) {
	if testHandler == nil {
		t.Fatal("isolated database required")
	}
	bc := newBudgetCase(t, 2314001, mergeEdgeSteps, 40)
	storeTokenValues(t, bc.edgeCase)
	task := admitRun(t, bc.edgeCase)
	reportUsage(t, task, usageRow{Model: "m1", In: 5000})
	if comments, _ := bc.waitNotices(t, service.RunBudgetKindWarn80Tokens, 0); comments != 0 {
		t.Fatalf("warning at 50%% used = %d comments, want 0", comments)
	}
	// 50% -> 85% used; no later enqueue.
	reportUsage(t, task, usageRow{Model: "m1", In: 8500})
	if comments, inbox := bc.waitNotices(t, service.RunBudgetKindWarn80Tokens, 1); comments != 1 || inbox != 1 {
		t.Fatalf("token warning = %d comments, %d inbox items, want 1 and 1", comments, inbox)
	}
	reportUsage(t, task, usageRow{Model: "m1", In: 9000})
	if comments, _ := bc.waitNotices(t, service.RunBudgetKindWarn80Tokens, 2); comments != 1 {
		t.Fatalf("token warnings after a further report = %d, want 1", comments)
	}
}

func TestRunBudgetNegativeTokenBalanceIsOvershootAndRefusesDerivedRuns(t *testing.T) {
	if testHandler == nil {
		t.Fatal("isolated database required")
	}
	bc := newBudgetCase(t, 2314002, mergeEdgeSteps, 40)
	storeTokenValues(t, bc.edgeCase)
	task := admitRun(t, bc.edgeCase)
	reportUsage(t, task, usageRow{Model: "m1", In: 12000, Out: 1000})
	wantTokens(t, bc.edgeCase, actAllowance-13000, "after an overshooting run")
	issue, err := testHandler.Queries.GetIssue(context.Background(), parseUUID(bc.issueID))
	if err != nil {
		t.Fatal(err)
	}
	snap, err := service.LoadRunBudgetSnapshot(context.Background(), testHandler.Queries, issue)
	if err != nil {
		t.Fatal(err)
	}
	if !snap.TokensActive || snap.TokensBalance != -3000 || snap.TokensOvershoot != 3000 {
		t.Fatalf("snapshot tokens = active %v, balance %d, overshoot %d, want true, -3000, 3000", snap.TokensActive, snap.TokensBalance, snap.TokensOvershoot)
	}
	bc.refuse(t)
	if _, err := testHandler.TaskService.EnqueueTaskForIssue(context.Background(), issue); !errors.Is(err, service.ErrRunBudgetExhausted) {
		t.Fatalf("admission at a negative token balance = %v, want ErrRunBudgetExhausted", err)
	}
	// A person moving the status opens a window with a new allowance.
	memberMove(t, bc.edgeCase)
	if err := bc.admit(t); err != nil {
		t.Fatalf("admission after a new window: %v", err)
	}
}

func TestRunBudgetWorkflowWithoutBothTokenValuesStaysRunsOnly(t *testing.T) {
	if testHandler == nil {
		t.Fatal("isolated database required")
	}
	c, _ := budgetedAt(t, 40, 1)
	if _, err := testPool.Exec(context.Background(), `UPDATE issue_workflow SET run_budget_token_allowance=$2 WHERE id=$1`, c.workflowID, actAllowance); err != nil {
		t.Fatal(err)
	}
	task := admitRun(t, c)
	reportUsage(t, task, usageRow{Model: "m1", In: 1000})
	if got := dbfx.Count(t, `SELECT count(*) FROM issue_run_budget_entry WHERE issue_id=$1 AND tokens_delta<>0 OR (issue_id=$1 AND kind IN ('allowance','token_activation'))`, c.issueID); got != 0 {
		t.Fatalf("token entries with only A stored = %d, want 0", got)
	}
}

// Member moves recorded with no admission in between (a no-start status move)
// still open one window each, and each window gets its allowance.
func TestRunBudgetEveryMemberWindowGetsItsAllowanceWithoutAdmissions(t *testing.T) {
	if testHandler == nil {
		t.Fatal("isolated database required")
	}
	c, _ := budgetedAt(t, 40, 1)
	legacy := legacyTask(t, c)
	storeTokenValues(t, c)
	admitRun(t, c) // activates
	finishRuns(t, c)
	memberMove(t, c)
	memberMove(t, c)
	reportUsage(t, legacy, usageRow{Model: "m1", In: 100})
	if got := entrySum(t, c.issueID, "allowance"); got != 3*actAllowance {
		t.Fatalf("allowances after two moves with no admission = %d, want %d", got, 3*actAllowance)
	}
	reportUsage(t, legacy, usageRow{Model: "m1", In: 200})
	if got := entrySum(t, c.issueID, "allowance"); got != 3*actAllowance {
		t.Fatalf("allowances after a second report = %d, want %d", got, 3*actAllowance)
	}
}

// The report that activates the issue settles earlier usage first, and the
// crossing it makes still writes the warning.
func TestRunBudgetActivatingReportThatCrossesEightyPercentWarns(t *testing.T) {
	if testHandler == nil {
		t.Fatal("isolated database required")
	}
	bc := newBudgetCase(t, 2314003, mergeEdgeSteps, 40)
	task := admitRun(t, bc.edgeCase) // before A and E are stored
	storeTokenValues(t, bc.edgeCase)
	reportUsage(t, task, usageRow{Model: "m1", In: 8500})
	wantTokens(t, bc.edgeCase, actAllowance-8500, "after the activating report")
	if comments, inbox := bc.waitNotices(t, service.RunBudgetKindWarn80Tokens, 1); comments != 1 || inbox != 1 {
		t.Fatalf("token warning = %d comments, %d inbox items, want 1 and 1", comments, inbox)
	}
}
