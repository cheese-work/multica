package handler

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// CHE-2297 L2c: window credit, capped progress grants, sticky exhaustion and
// the superseded_head refund. A grant is +6 runs / 15M tokens; a window takes
// at most 3.

const progressKinds = `('step', 'head', 'stale_pr')`

func inGrantTx(t *testing.T, fn func(q *db.Queries) error) error {
	t.Helper()
	ctx := context.Background()
	tx, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if err := fn(testHandler.Queries.WithTx(tx)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

const headPR = "00000000-0000-4000-8000-000000000001"

func grantHead(t *testing.T, issueID, sha string) {
	t.Helper()
	err := inGrantTx(t, func(q *db.Queries) error {
		return service.GrantRunBudgetHead(context.Background(), q, parseUUID(issueID), parseUUID(headPR), sha)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func grantCount(t *testing.T, issueID string) int {
	t.Helper()
	return dbfx.Count(t, `SELECT count(*) FROM issue_run_budget_entry WHERE issue_id=$1 AND kind IN `+progressKinds, issueID)
}

func grantRuns(t *testing.T, issueID string) int {
	t.Helper()
	return dbfx.Count(t, `SELECT COALESCE(SUM(runs_delta), 0) FROM issue_run_budget_entry WHERE issue_id=$1 AND kind IN `+progressKinds, issueID)
}

// agentStep moves the issue the way an agent's status write is recorded: the
// row, the activity the listener writes, then the grant.
func agentStep(t *testing.T, c edgeCase, to string, recovery bool) {
	t.Helper()
	ctx := context.Background()
	prev := c.status(t)
	c.enter(t, to)
	if _, err := testPool.Exec(ctx, `INSERT INTO activity_log (workspace_id, issue_id, actor_type, actor_id, action, details)
		VALUES ($1, $2, 'agent', $3, 'status_changed', jsonb_build_object('from', $4::text, 'to', $5::text))`,
		testWorkspaceID, c.issueID, seededReadyAgentID(t), prev, to); err != nil {
		t.Fatal(err)
	}
	issue, err := testHandler.Queries.GetIssue(ctx, parseUUID(c.issueID))
	if err != nil {
		t.Fatal(err)
	}
	if err := inGrantTx(t, func(q *db.Queries) error { return service.GrantRunBudgetStep(ctx, q, issue, prev, recovery) }); err != nil {
		t.Fatal(err)
	}
}

// exhaust plants the window's exhaustion claim, as the first refused derived
// enqueue does.
func exhaust(t *testing.T, c edgeCase) {
	t.Helper()
	ctx := context.Background()
	if _, err := service.OpenRunBudgetWindow(ctx, testHandler.Queries, parseUUID(c.issueID)); err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(context.Background(), `INSERT INTO run_budget_notice (id, workspace_id, issue_id, window_id, kind)
		SELECT gen_random_uuid(), workspace_id, issue_id, window_id, 'exhausted' FROM issue_run_budget_entry
		WHERE issue_id=$1 AND kind='window' ORDER BY created_at DESC LIMIT 1`, c.issueID); err != nil {
		t.Fatal(err)
	}
}

func TestRunBudgetGrantReplayedHeadGrantsOnce(t *testing.T) {
	if testHandler == nil {
		t.Fatal("isolated database required")
	}
	c, _ := budgetedAt(t, 40, 2)
	before := ledgerBalance(t, c.issueID)
	grantHead(t, c.issueID, "aaa")
	grantHead(t, c.issueID, "aaa")
	if got := grantCount(t, c.issueID); got != 1 {
		t.Fatalf("grants for one replayed head = %d, want 1", got)
	}
	if got := ledgerBalance(t, c.issueID); got-before < service.RunBudgetGrantRuns {
		t.Fatalf("balance moved by %d (from %d), want a grant of %d on top of the window credit", got-before, before, service.RunBudgetGrantRuns)
	}
}

func TestRunBudgetGrantFourDistinctHeadsGrantThreeTimes(t *testing.T) {
	if testHandler == nil {
		t.Fatal("isolated database required")
	}
	c, _ := budgetedAt(t, 40, 2)
	for i := 0; i < 4; i++ {
		grantHead(t, c.issueID, fmt.Sprintf("head%d", i))
	}
	if got := grantCount(t, c.issueID); got != 3 {
		t.Fatalf("grants for 4 distinct heads = %d, want 3", got)
	}
	if got := grantRuns(t, c.issueID); got != 3*service.RunBudgetGrantRuns {
		t.Fatalf("granted runs = %d, want %d", got, 3*service.RunBudgetGrantRuns)
	}
}

func TestRunBudgetGrantConcurrentGrantsKeepTheCap(t *testing.T) {
	if testHandler == nil {
		t.Fatal("isolated database required")
	}
	c, _ := budgetedAt(t, 40, 2)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			grantHead(t, c.issueID, fmt.Sprintf("race%d", i))
		}(i)
	}
	wg.Wait()
	if got := grantCount(t, c.issueID); got != 3 {
		t.Fatalf("concurrent grants = %d, want 3", got)
	}
}

func TestRunBudgetStepGrantPolicy(t *testing.T) {
	if testHandler == nil {
		t.Fatal("isolated database required")
	}
	ctx := context.Background()

	t.Run("an agent's first entry to an unvisited status grants once", func(t *testing.T) {
		c, _ := budgetedAt(t, 40, 2)
		agentStep(t, c, "in_review", false)
		if got := grantCount(t, c.issueID); got != 1 {
			t.Fatalf("grants after the first entry = %d, want 1", got)
		}
		agentStep(t, c, "blocked", false)
		agentStep(t, c, "in_review", false)
		if got := grantCount(t, c.issueID); got != 2 {
			t.Fatalf("grants after blocked and a return to in_review = %d, want 2 (blocked only)", got)
		}
	})

	t.Run("a workflow edge's first entry grants once", func(t *testing.T) {
		c, _ := budgetedAt(t, 40, 2)
		issue, err := testHandler.Queries.GetIssue(ctx, parseUUID(c.issueID))
		if err != nil {
			t.Fatal(err)
		}
		if moved, _ := testHandler.moveIssueOnEdge(ctx, issue, "in_review", "ci_green_advance", nil, false); !moved {
			t.Fatal("the edge move did not happen")
		}
		if got := grantCount(t, c.issueID); got != 1 {
			t.Fatalf("grants after the edge move = %d, want 1", got)
		}
	})

	t.Run("a plain flip back to a visited status grants nothing", func(t *testing.T) {
		c, _ := budgetedAt(t, 40, 2)
		agentStep(t, c, "in_review", false)
		agentStep(t, c, "in_progress", false) // the status the window opened in
		agentStep(t, c, "in_review", false)
		if got := grantCount(t, c.issueID); got != 1 {
			t.Fatalf("grants after in_progress → in_review → in_progress → in_review = %d, want 1", got)
		}
	})

	t.Run("a member move opens a window and gives no step grant", func(t *testing.T) {
		c, _ := budgetedAt(t, 40, 2)
		recordMemberMovesOf(t, c.issueID)
		if _, err := service.OpenRunBudgetWindow(ctx, testHandler.Queries, parseUUID(c.issueID)); err != nil {
			t.Fatal(err)
		}
		moveStatusAsMember(t, c.issueID, "in_review")
		if got := grantCount(t, c.issueID); got != 0 {
			t.Fatalf("grants after a member move = %d, want 0", got)
		}
		if got := entryCount(t, c.issueID, "window"); got != 2 {
			t.Fatalf("window credits after a member move = %d, want 2", got)
		}
		agentStep(t, c, "in_progress", false) // the status the member moved from is visited from the start
		if got := grantCount(t, c.issueID); got != 0 {
			t.Fatalf("grants for the status the member left = %d, want 0", got)
		}
		agentStep(t, c, "blocked", false)
		if got := grantCount(t, c.issueID); got != 1 {
			t.Fatalf("grants after an agent enters another status in the new window = %d, want 1", got)
		}
		agentStep(t, c, "in_review", false) // the member's target
		if got := grantCount(t, c.issueID); got != 1 {
			t.Fatalf("grants for the member's target status = %d, want 1 (no new grant)", got)
		}
	})

	t.Run("a member move to the same status opens no window", func(t *testing.T) {
		c, _ := budgetedAt(t, 40, 2)
		recordMemberMovesOf(t, c.issueID)
		moveStatusAsMember(t, c.issueID, "in_progress")
		if got := dbfx.Count(t, `SELECT count(*) FROM activity_log WHERE issue_id=$1 AND action='status_changed'`, c.issueID); got != 0 {
			t.Fatalf("status_changed rows after a same-status move = %d, want 0", got)
		}
		if got := entryCount(t, c.issueID, "window"); got != 0 {
			t.Fatalf("window credits after a same-status move = %d, want 0", got)
		}
		if got := grantCount(t, c.issueID); got != 0 {
			t.Fatalf("grants after a same-status move = %d, want 0", got)
		}
	})

	t.Run("a recovery-caused move grants nothing", func(t *testing.T) {
		c, _ := budgetedAt(t, 40, 2)
		agentStep(t, c, "in_review", true)
		if got := grantCount(t, c.issueID); got != 0 {
			t.Fatalf("grants for a recovery-caused move = %d, want 0", got)
		}
	})

	t.Run("a move after exhaustion grants nothing", func(t *testing.T) {
		c, _ := budgetedAt(t, 40, 2)
		exhaust(t, c)
		agentStep(t, c, "in_review", false)
		if got := grantCount(t, c.issueID); got != 0 {
			t.Fatalf("grants for a move after exhaustion = %d, want 0", got)
		}
	})
}

func TestRunBudgetGrantStaysShutAfterExhaustionUntilAMemberActs(t *testing.T) {
	if testHandler == nil {
		t.Fatal("isolated database required")
	}
	c, _ := budgetedAt(t, 40, 2)
	exhaust(t, c)
	grantHead(t, c.issueID, "late")
	agentStep(t, c, "in_review", false)
	if got := grantCount(t, c.issueID); got != 0 {
		t.Fatalf("grants in an exhausted window = %d, want 0", got)
	}
	memberMove(t, c) // opens the next window
	grantHead(t, c.issueID, "late")
	if got := grantCount(t, c.issueID); got != 1 {
		t.Fatalf("grants after a member opened a window = %d, want 1", got)
	}
}

func TestRunBudgetGrantCreditsTokensOnlyOnceActivated(t *testing.T) {
	if testHandler == nil {
		t.Fatal("isolated database required")
	}
	c, _ := budgetedAt(t, 40, 2)
	storeTokenValues(t, c)
	grantHead(t, c.issueID, "before")
	if got := entrySum(t, c.issueID, "head"); got != 0 {
		t.Fatalf("tokens granted before activation = %d, want 0", got)
	}
	admitRun(t, c) // activates
	finishRuns(t, c)
	grantHead(t, c.issueID, "after")
	if got := entrySum(t, c.issueID, "head"); got != service.RunBudgetGrantTokens {
		t.Fatalf("tokens granted after activation = %d, want %d", got, service.RunBudgetGrantTokens)
	}
}

func TestRunBudgetRefundOncePerTaskOnlyForSupersededHead(t *testing.T) {
	if testHandler == nil {
		t.Fatal("isolated database required")
	}
	ctx := context.Background()
	c, _ := budgetedAt(t, 40, 2)
	issue, err := testHandler.Queries.GetIssue(ctx, parseUUID(c.issueID))
	if err != nil {
		t.Fatal(err)
	}
	refund := func(reason string) {
		t.Helper()
		task, err := testHandler.TaskService.EnqueueTaskForIssue(ctx, issue)
		if err != nil {
			t.Fatal(err)
		}
		cancelled, err := testHandler.TaskService.CancelTaskWithReason(ctx, task.ID, "cancelled", reason)
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 2; i++ { // a replay changes nothing
			if err := inGrantTx(t, func(q *db.Queries) error { return service.RefundSupersededRun(ctx, q, *cancelled) }); err != nil {
				t.Fatal(err)
			}
		}
	}
	before := ledgerBalance(t, c.issueID)
	refund("runtime_offline")
	if got := entryCount(t, c.issueID, "refund"); got != 0 {
		t.Fatalf("refunds for another cancellation reason = %d, want 0", got)
	}
	afterDebit := ledgerBalance(t, c.issueID)
	refund(service.FailureReasonSupersededHead)
	if got := entryCount(t, c.issueID, "refund"); got != 1 {
		t.Fatalf("refunds for a superseded_head cancellation, replayed = %d, want 1", got)
	}
	if got, want := ledgerBalance(t, c.issueID), afterDebit-1+1; got != want {
		t.Fatalf("balance after a debit and its refund = %d, want %d (before: %d)", got, want, before)
	}
}

// A pushed head reaches the ledger through the production supersede path: the
// new head grants once and the review it superseded is refunded once.
func TestRunBudgetGrantSupersedeWiring(t *testing.T) {
	if testHandler == nil {
		t.Fatal("isolated database required")
	}
	c, _ := budgetedAt(t, 40, 2)
	agentID := seededReadyAgentID(t)
	var runtimeID string
	dbfx.QueryRow(t, `SELECT runtime_id::text FROM agent WHERE id = $1`, agentID).Scan(&runtimeID)
	prID := dbfx.Insert(t, "github_pull_request", testutil.Cols{
		"workspace_id": testWorkspaceID, "installation_id": 1, "repo_owner": "multica-ai", "repo_name": "multica",
		"pr_number": testutil.Raw("(floor(random() * 1000000000))::int"), "title": "grant PR", "state": "open", "html_url": "https://example.test/pr",
		"pr_created_at": testutil.Raw("now()"), "pr_updated_at": testutil.Raw("now()"), "head_sha": reviewHeadA,
	})
	dbfx.Exec(t, `INSERT INTO issue_pull_request (issue_id, pull_request_id) VALUES ($1, $2)`, c.issueID, prID)
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM issue_pull_request WHERE pull_request_id = $1`, prID)
	})
	c.enter(t, "in_review") // the step the review run belongs to
	review := reviewRun(t, agentID, runtimeID, c.issueID, prID, "in_review", reviewHeadA)
	// The review was admitted in this window, so its debit is on the ledger.
	if _, err := service.OpenRunBudgetWindow(context.Background(), testHandler.Queries, parseUUID(c.issueID)); err != nil {
		t.Fatal(err)
	}
	dbfx.Exec(t, `INSERT INTO issue_run_budget_entry (id, workspace_id, issue_id, window_id, kind, event_key, runs_delta)
		SELECT gen_random_uuid(), workspace_id, issue_id, window_id, 'run', 'run:'||$2::text, -1 FROM issue_run_budget_entry
		WHERE issue_id=$1 AND kind='window' LIMIT 1`, c.issueID, review)

	pushHead(t, prID, reviewHeadB)
	pushHead(t, prID, reviewHeadB) // a replayed webhook

	if got := entryCount(t, c.issueID, "head"); got != 1 {
		t.Fatalf("head grants after a push and its replay = %d, want 1", got)
	}
	if got := entryCount(t, c.issueID, "refund"); got != 1 {
		t.Fatalf("refunds after a superseded review = %d, want 1", got)
	}
}
