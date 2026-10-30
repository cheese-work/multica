package handler

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

// CHE-2297 L2a: the run budget is a ledger. These tests pin the balance an
// issue reads from it and who is refused at zero.

// uuidTail is the random end of a v7 id: the leading characters are a clock.
func uuidTail() string { id := uuidToString(dbid.NewV7()); return id[len(id)-8:] }

func ledgerBalance(t *testing.T, issueID string) int64 {
	t.Helper()
	return int64(dbfx.Count(t, `SELECT COALESCE(SUM(runs_delta), 0) FROM issue_run_budget_entry WHERE issue_id=$1`, issueID))
}

// budgetedAt returns an issue whose workflow allows max runs and that already
// used `used` of them before the ledger existed: the runs are real task rows,
// the ledger is empty.
func budgetedAt(t *testing.T, max, used int) (edgeCase, string) {
	t.Helper()
	c := newEdgeCase(t, 2226300, mergeEdgeSteps)
	issue := spendBudget(t, c, used, "in_progress")
	if _, err := testPool.Exec(context.Background(), `UPDATE issue_workflow SET max_runs_per_issue=$2, run_budget_since=now()-interval '1 hour' WHERE id=$1`, c.workflowID, max); err != nil {
		t.Fatal(err)
	}
	// Deploy day: the runs stay, the ledger starts empty.
	if _, err := testPool.Exec(context.Background(), `DELETE FROM issue_run_budget_entry WHERE issue_id=$1`, c.issueID); err != nil {
		t.Fatal(err)
	}
	return c, uuidToString(issue.AssigneeID)
}

func TestRunBudgetLedgerCreditsTheRunsLeftAtDeploy(t *testing.T) {
	if testHandler == nil {
		t.Fatal("isolated database required")
	}
	ctx := context.Background()
	c, _ := budgetedAt(t, 40, 38)
	issue, err := testHandler.Queries.GetIssue(ctx, parseUUID(c.issueID))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := testHandler.TaskService.EnqueueTaskForIssue(ctx, issue); err != nil {
		t.Fatalf("run 39 of 40 refused: %v", err)
	}
	if got := ledgerBalance(t, c.issueID); got != 1 {
		t.Fatalf("balance after run 39 of 40 = %d, want 1 (credit 2, one debit)", got)
	}
	finishRuns(t, c)
	if _, err := testHandler.TaskService.EnqueueTaskForIssue(ctx, issue); err != nil {
		t.Fatalf("run 40 of 40 refused: %v", err)
	}
	finishRuns(t, c)
	if _, err := testHandler.TaskService.EnqueueTaskForIssue(ctx, issue); !errors.Is(err, service.ErrRunBudgetExhausted) {
		t.Fatalf("run 41 of 40 = %v, want ErrRunBudgetExhausted", err)
	}
}

func TestRunBudgetLedgerWritesRunEntriesOnly(t *testing.T) {
	if testHandler == nil {
		t.Fatal("isolated database required")
	}
	ctx := context.Background()
	c, _ := budgetedAt(t, 40, 2)
	issue, err := testHandler.Queries.GetIssue(ctx, parseUUID(c.issueID))
	if err != nil {
		t.Fatal(err)
	}
	task, err := testHandler.TaskService.EnqueueTaskForIssue(ctx, issue)
	if err != nil {
		t.Fatal(err)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM issue_run_budget_entry WHERE issue_id=$1 AND kind='run' AND event_key=$2 AND runs_delta=-1`, c.issueID, "run:"+uuidToString(task.ID)); n != 1 {
		t.Fatalf("run debits for the admitted task = %d, want 1", n)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM issue_run_budget_entry WHERE issue_id=$1 AND (tokens_delta<>0 OR kind NOT IN ('window','run'))`, c.issueID); n != 0 {
		t.Fatalf("%d token or credit entries, want none: no A or E check is stored", n)
	}
}

func TestRunBudgetLedgerConcurrentAdmissionsAtOneRunLeftAdmitOne(t *testing.T) {
	if testHandler == nil {
		t.Fatal("isolated database required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c, _ := budgetedAt(t, 3, 2)
	issue, err := testHandler.Queries.GetIssue(ctx, parseUUID(c.issueID))
	if err != nil {
		t.Fatal(err)
	}
	const racers = 8
	// A different agent per racer: the pending-task unique index cannot decide
	// the winner, only the budget can.
	agents := make([]string, racers)
	comments := make([]string, racers)
	for i := range agents {
		agents[i] = createHandlerTestAgent(t, "ledger-racer-"+uuidTail(), nil)
		comments[i] = dbfx.Comment(t, c.issueID, "race", testutil.Cols{})
	}
	var wg sync.WaitGroup
	start := make(chan struct{})
	results := make(chan error, racers)
	for i := range agents {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, err := testHandler.TaskService.EnqueueTaskForMention(ctx, issue, parseUUID(agents[i]), parseUUID(comments[i]), service.OriginDerived)
			results <- err
		}(i)
	}
	close(start)
	wg.Wait()
	close(results)
	admitted := 0
	for err := range results {
		switch {
		case err == nil:
			admitted++
		case !errors.Is(err, service.ErrRunBudgetExhausted):
			t.Fatalf("racer failed with %v, want admitted or ErrRunBudgetExhausted", err)
		}
	}
	if admitted != 1 {
		t.Fatalf("admitted %d runs at a balance of 1, want exactly 1", admitted)
	}
	if got := ledgerBalance(t, c.issueID); got != 0 {
		t.Fatalf("balance after the race = %d, want 0", got)
	}
}

// atZero spends the whole budget and returns the issue, a mentioned agent and
// helpers to author a comment as a person or as that agent.
func atZero(t *testing.T) (c edgeCase, trigger commentAgentTrigger) {
	t.Helper()
	c = newEdgeCase(t, 2226301, mergeEdgeSteps)
	spendBudget(t, c, 2, "in_progress")
	agentID := createHandlerTestAgent(t, "ledger-zero-"+uuidTail(), nil)
	agent, err := testHandler.Queries.GetAgent(context.Background(), parseUUID(agentID))
	if err != nil {
		t.Fatal(err)
	}
	return c, commentAgentTrigger{Agent: agent, Source: commentTriggerSourceMentionAgent}
}

func TestRunBudgetLedgerMemberMentionAtZeroIsAdmittedAndDebited(t *testing.T) {
	if testHandler == nil {
		t.Fatal("isolated database required")
	}
	ctx := context.Background()
	c, trigger := atZero(t)
	issue, err := testHandler.Queries.GetIssue(ctx, parseUUID(c.issueID))
	if err != nil {
		t.Fatal(err)
	}
	comment := dbfx.Comment(t, c.issueID, "@agent please look", testutil.Cols{})
	if _, err := testHandler.enqueueSingleCommentTrigger(ctx, issue, parseUUID(comment), trigger); err != nil {
		t.Fatalf("member mention at zero refused: %v", err)
	}
	if got := ledgerBalance(t, c.issueID); got != -1 {
		t.Fatalf("balance after the member mention = %d, want -1 (a named run is debited past zero)", got)
	}
}

func TestRunBudgetLedgerAgentMentionAtZeroIsRefused(t *testing.T) {
	if testHandler == nil {
		t.Fatal("isolated database required")
	}
	ctx := context.Background()
	c, trigger := atZero(t)
	issue, err := testHandler.Queries.GetIssue(ctx, parseUUID(c.issueID))
	if err != nil {
		t.Fatal(err)
	}
	author := seededReadyAgentID(t)
	comment := dbfx.Comment(t, c.issueID, "@agent please look", testutil.Cols{"author_type": "agent", "author_id": author})
	if _, err := testHandler.enqueueSingleCommentTrigger(ctx, issue, parseUUID(comment), trigger); !errors.Is(err, service.ErrRunBudgetExhausted) {
		t.Fatalf("agent mention at zero = %v, want ErrRunBudgetExhausted", err)
	}
}

func TestRunBudgetLedgerWakeupsAtZero(t *testing.T) {
	if testHandler == nil {
		t.Fatal("isolated database required")
	}
	ctx := context.Background()
	for _, tc := range []struct {
		name      string
		agentMade bool
		wantRuns  int
	}{
		{"member wakeup is admitted", false, 1},
		{"agent-created wakeup is refused", true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newEdgeCase(t, 2226302, mergeEdgeSteps)
			spendBudget(t, c, 2, "in_progress")
			agent := seededReadyAgentID(t)
			svc := service.IssueWakeupService{Tasks: testHandler.TaskService}
			w, err := svc.Create(ctx, parseUUID(c.issueID), parseUUID(testUserID), pgtype.UUID{}, service.WakeupInput{AgentID: agent, Kind: "at", AfterSeconds: 600, Instruction: "check"})
			if err != nil {
				t.Fatal(err)
			}
			if tc.agentMade {
				if _, err := testPool.Exec(ctx, `UPDATE issue_wakeup SET source_task_id=$2 WHERE id=$1`, w.ID, dbid.NewV7()); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := testPool.Exec(ctx, `UPDATE issue_wakeup SET next_fire_at=now()-interval '1 second' WHERE id=$1`, w.ID); err != nil {
				t.Fatal(err)
			}
			runWakeupTick(t)
			if n := dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE context->>'wakeup_id'=$1`, uuidToString(w.ID)); n != tc.wantRuns {
				t.Fatalf("wakeup runs at zero = %d, want %d", n, tc.wantRuns)
			}
		})
	}
}

// An issue enqueue holds one connection from the budget lock to the debit, so
// everything it reads must go through that connection, not the pool.
func TestRunBudgetIssueEnqueueFinishesOnOneConnectionPool(t *testing.T) {
	if testHandler == nil {
		t.Fatal("isolated database required")
	}
	c, _ := budgetedAt(t, 40, 2)
	issue, err := testHandler.Queries.GetIssue(context.Background(), parseUUID(c.issueID))
	if err != nil {
		t.Fatal(err)
	}
	cfg := testPool.Config().Copy()
	cfg.MaxConns, cfg.MinConns = 1, 0
	small, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer small.Close()
	tasks := service.NewTaskService(db.New(small), small, testHandler.TaskService.Hub, testHandler.TaskService.Bus)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := tasks.EnqueueTaskForIssue(ctx, issue); err != nil {
		t.Fatalf("issue enqueue on a one-connection pool = %v, want it admitted", err)
	}
}

// waitForLockWaiters blocks until n backends of this test database wait on a lock.
func waitForLockWaiters(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var waiting int
		if err := testPool.QueryRow(context.Background(), `SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting >= n {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("fewer than %d backends waited on a lock", n)
}

// Two raises read the same cap, then run one after the other: the ledger must
// gain the final cap's difference, not the sum of two stale differences.
func TestRunBudgetOverlappingRaisesCreditTheFinalCapOnce(t *testing.T) {
	if testHandler == nil {
		t.Fatal("isolated database required")
	}
	ctx := context.Background()
	c, _ := budgetedAt(t, 40, 2)
	hold, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer hold.Rollback(ctx)
	if _, err := hold.Exec(ctx, `SELECT id FROM issue WHERE id=$1 FOR NO KEY UPDATE`, c.issueID); err != nil {
		t.Fatal(err)
	}
	codes := make(chan int, 2)
	for i, runs := range []int{50, 60} {
		go func() { codes <- raiseBudget(t, c.issueID, map[string]any{"runs": runs}, nil).Code }()
		waitForLockWaiters(t, i+1)
	}
	if err := hold.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if code := <-codes; code != http.StatusOK {
			t.Fatalf("raise = %d, want 200 (50 then 60 are both above the cap in force)", code)
		}
	}
	if got, want := ledgerBalance(t, c.issueID), int64(60-2); got != want {
		t.Fatalf("balance after raises to 50 and 60 = %d, want %d (cap 60, 2 runs used)", got, want)
	}
}

// The workflow's cap governs the open window in both directions, without the
// ledger being touched by hand.
func TestRunBudgetWorkflowCapEditsGovernTheOpenWindow(t *testing.T) {
	if testHandler == nil {
		t.Fatal("isolated database required")
	}
	ctx := context.Background()
	c, _ := budgetedAt(t, 40, 2)
	if _, err := service.OpenRunBudgetWindow(ctx, testHandler.Queries, parseUUID(c.issueID)); err != nil {
		t.Fatal(err)
	}
	issue, err := testHandler.Queries.GetIssue(ctx, parseUUID(c.issueID))
	if err != nil {
		t.Fatal(err)
	}
	setCap := func(max int) {
		t.Helper()
		if _, err := testPool.Exec(ctx, `UPDATE issue_workflow SET max_runs_per_issue=$2 WHERE id=$1`, c.workflowID, max); err != nil {
			t.Fatal(err)
		}
	}
	setCap(1)
	if _, err := testHandler.TaskService.EnqueueTaskForIssue(ctx, issue); !errors.Is(err, service.ErrRunBudgetExhausted) {
		t.Fatalf("run after the cap fell to 1 with 2 used = %v, want ErrRunBudgetExhausted", err)
	}
	settle()
	setCap(10)
	if _, err := testHandler.TaskService.EnqueueTaskForIssue(ctx, issue); err != nil {
		t.Fatalf("run after the cap rose to 10 with 2 used refused: %v", err)
	}
	if got := ledgerBalance(t, c.issueID); got != 7 {
		t.Fatalf("balance = %d, want 7 (cap 10, 3 runs)", got)
	}
}
