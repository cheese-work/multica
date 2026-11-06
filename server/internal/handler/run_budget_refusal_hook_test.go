package handler

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/issueworkflow"
	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// The exhaustion hook is three transactions: R records the refusal, B blocks,
// N writes the notice. A crash between them, a status move or a refusal of a
// later window must leave the issue where its newest occurrence put it.

type refusalRow struct {
	Window, Status, State, StateReason, Phase, HookReason string
	Blocked                                               bool
}

func refusalRows(t *testing.T, c budgetCase) []refusalRow {
	t.Helper()
	rows, err := testPool.Query(context.Background(), `SELECT window_id, COALESCE(status,''), state, COALESCE(state_reason,''), hook_phase, COALESCE(hook_reason,''), blocked
		FROM run_budget_refusal WHERE issue_id=$1 ORDER BY created_at, id`, c.issueID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []refusalRow
	for rows.Next() {
		var r refusalRow
		if err := rows.Scan(&r.Window, &r.Status, &r.State, &r.StateReason, &r.Phase, &r.HookReason, &r.Blocked); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

// onlyRefusal fails unless the issue has exactly one refusal row.
func onlyRefusal(t *testing.T, c budgetCase) refusalRow {
	t.Helper()
	rows := refusalRows(t, c)
	if len(rows) != 1 {
		t.Fatalf("refusal rows = %d (%+v), want 1", len(rows), rows)
	}
	return rows[0]
}

// atStage runs fn when the hook reaches stage; fn's result says whether to stop there.
func atStage(t *testing.T, stage string, fn func() bool) {
	t.Helper()
	runBudgetHookAt = func(s string) bool {
		if s != stage {
			return false
		}
		runBudgetHookAt = func(string) bool { return false }
		return fn()
	}
	t.Cleanup(func() { runBudgetHookAt = func(string) bool { return false } })
}

func crashAt(t *testing.T, stage string) { atStage(t, stage, func() bool { return true }) }

func sweepHooks(t *testing.T) {
	t.Helper()
	testHandler.sweepRunBudgetHooks(context.Background(), time.Now().Add(time.Minute))
}

// blockRows counts the history rows the budget's block wrote.
func blockRows(t *testing.T, c budgetCase) int {
	t.Helper()
	return dbfx.Count(t, `SELECT count(*) FROM activity_log WHERE issue_id=$1 AND action='status_changed' AND details->'trigger'->'cause'->>'kind'='run_budget'`, c.issueID)
}

func recordOccurrence(t *testing.T, c budgetCase, to string) {
	t.Helper()
	if _, err := testPool.Exec(context.Background(), `INSERT INTO activity_log (workspace_id, issue_id, actor_type, actor_id, action, details)
		VALUES ($1, $2, 'member', $3, 'status_changed', jsonb_build_object('to', $4::text))`, testWorkspaceID, c.issueID, testUserID, to); err != nil {
		t.Fatal(err)
	}
}

// humanMove moves the status the way a request does: the update and its history
// row in one transaction.
func humanMove(t *testing.T, c budgetCase, to string) {
	t.Helper()
	ctx := context.Background()
	tx, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	q := testHandler.Queries.WithTx(tx)
	issue, err := q.GetIssue(ctx, parseUUID(c.issueID))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE issue SET status=$2 WHERE id=$1`, c.issueID, to); err != nil {
		t.Fatal(err)
	}
	trigger := service.StatusTrigger{Cause: service.StatusCause{Kind: service.StatusCauseUIOrAPI, By: service.StatusCauseActor{Type: "member", ID: testUserID}}, Channel: "web"}
	if _, err := service.RecordStatusChange(ctx, q, issue, issue.Status, to, trigger, nil); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

// newWindowRefusal opens a new window the way a person's activity does, spends
// it and returns the event of the refusal that follows, without running its hook.
func newWindowRefusal(t *testing.T, c budgetCase, status string) service.RunBudgetExhausted {
	t.Helper()
	events := make(chan service.RunBudgetExhausted, 1)
	service.SetOnRunBudgetExhausted(func(_ context.Context, e service.RunBudgetExhausted) { events <- e })
	t.Cleanup(func() { service.SetOnRunBudgetExhausted(testHandler.onRunBudgetExhausted) })
	recordOccurrence(t, c, status)
	return spendUntilRefused(t, c, events)
}

// spendUntilRefused finishes the earlier hook's recovery run, so its spend
// counts, then admits runs until the budget refuses one.
func spendUntilRefused(t *testing.T, c budgetCase, events chan service.RunBudgetExhausted) service.RunBudgetExhausted {
	t.Helper()
	// The earlier hook may have queued a recovery run; finish it so the spend counts.
	if _, err := testPool.Exec(context.Background(), `UPDATE agent_task_queue SET status='completed',completed_at=clock_timestamp() WHERE issue_id=$1 AND status IN ('queued','dispatched','running')`, c.issueID); err != nil {
		t.Fatal(err)
	}
	// A window opened on a blocked issue may hold fewer runs than the cap: spend until refused.
	for i := 0; i < 5; i++ {
		if err := c.admit(t); errors.Is(err, service.ErrRunBudgetExhausted) {
			return <-events
		} else if err != nil {
			t.Fatalf("run %d: %v", i+1, err)
		}
	}
	t.Fatal("the new window never refused")
	return service.RunBudgetExhausted{}
}

func hookCase(t *testing.T, installationID int64) (budgetCase, service.RunBudgetExhausted) {
	t.Helper()
	if testHandler == nil {
		t.Skip("database not available")
	}
	c := newBudgetCase(t, installationID, recoverySteps(seededReadyAgentID(t)), 2)
	recordOccurrence(t, c, "in_progress")
	return c, refusedEvent(t, c)
}

// finished asserts the hook ran to its end once: one block, one notice, one
// recovery run, the refusal done, the status and target of the refusal kept.
func finished(t *testing.T, c budgetCase) {
	t.Helper()
	r := onlyRefusal(t, c)
	if r.Phase != "done" || r.State != "open" || r.Status != "in_progress" || !r.Blocked {
		t.Fatalf("refusal = %+v, want open, done, blocked, status in_progress", r)
	}
	if got := c.status(t); got != "blocked" {
		t.Fatalf("issue is %s, want blocked", got)
	}
	if n := blockRows(t, c); n != 1 {
		t.Fatalf("block history rows = %d, want 1", n)
	}
	if comments, _ := c.notices(t, service.RunBudgetKindExhausted); comments != 1 {
		t.Fatalf("exhaustion notices = %d, want 1", comments)
	}
	if n := recoveryRuns(t, c.edgeCase); n != 1 {
		t.Fatalf("recovery runs = %d, want 1", n)
	}
}

func TestRunBudgetHookCrashAfterIntentIsFinishedBySweep(t *testing.T) {
	c, e := hookCase(t, 2856001)
	crashAt(t, "after_intent")
	testHandler.onRunBudgetExhausted(context.Background(), e)
	if r := onlyRefusal(t, c); r.Phase != "intent" || r.Status != "in_progress" {
		t.Fatalf("refusal after the crash = %+v, want intent for in_progress", r)
	}
	if got := c.status(t); got != "in_progress" || blockRows(t, c) != 0 {
		t.Fatalf("issue is %s with %d block rows after R alone, want in_progress and none", got, blockRows(t, c))
	}
	sweepHooks(t)
	sweepHooks(t)
	finished(t, c)
}

func TestRunBudgetHookCrashAfterBlockIsFinishedBySweep(t *testing.T) {
	c, e := hookCase(t, 2856002)
	crashAt(t, "after_block")
	testHandler.onRunBudgetExhausted(context.Background(), e)
	if r := onlyRefusal(t, c); r.Phase != "blocked" || r.Status != "in_progress" {
		t.Fatalf("refusal after the crash = %+v, want blocked for in_progress", r)
	}
	if got := c.status(t); got != "blocked" {
		t.Fatalf("issue is %s, want blocked by B", got)
	}
	if comments, _ := c.notices(t, service.RunBudgetKindExhausted); comments != 0 || recoveryRuns(t, c.edgeCase) != 0 {
		t.Fatalf("notice or recovery run written before N")
	}
	sweepHooks(t)
	sweepHooks(t)
	// A late duplicate of the callback finds the hook done.
	testHandler.onRunBudgetExhausted(context.Background(), e)
	finished(t, c)
}

// retiredClean asserts a retired hook moved and dispatched nothing.
func retiredClean(t *testing.T, c budgetCase, wantStatus string, wantRows int) {
	t.Helper()
	if got := c.status(t); got != wantStatus {
		t.Fatalf("issue is %s, want %s", got, wantStatus)
	}
	if n := blockRows(t, c); n != 0 {
		t.Fatalf("stale block history rows = %d, want 0", n)
	}
	if n := recoveryRuns(t, c.edgeCase); n != 0 {
		t.Fatalf("recovery runs = %d, want 0", n)
	}
	if comments, _ := c.notices(t, service.RunBudgetKindExhausted); comments != 0 {
		t.Fatalf("exhaustion notices = %d, want 0", comments)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM run_budget_notice WHERE issue_id=$1 AND kind=$2`, c.issueID, service.RunBudgetKindBlocked); n != 0 {
		t.Fatalf("blocked claims = %d, want 0", n)
	}
	if rows := refusalRows(t, c); len(rows) != wantRows {
		t.Fatalf("refusal rows = %d, want %d", len(rows), wantRows)
	}
}

func TestRunBudgetHookStatusMoveAfterIntentRetiresTheHook(t *testing.T) {
	for _, mode := range []string{"in-process", "sweep"} {
		t.Run(mode, func(t *testing.T) {
			c, e := hookCase(t, map[string]int64{"in-process": 2856003, "sweep": 2856004}[mode])
			atStage(t, "after_intent", func() bool {
				humanMove(t, c, "todo")
				return mode == "sweep"
			})
			testHandler.onRunBudgetExhausted(context.Background(), e)
			if mode == "sweep" {
				sweepHooks(t)
			}
			retiredClean(t, c, "todo", 1)
			if r := onlyRefusal(t, c); r.State != "superseded" || r.StateReason != "status_moved" || r.Phase != "retired" || r.HookReason != "status_moved" {
				t.Fatalf("refusal = %+v, want superseded and retired by status_moved", r)
			}
		})
	}
}

func TestRunBudgetHookNewerWindowRefusalRetiresTheOlderHook(t *testing.T) {
	for _, mode := range []string{"in-process", "sweep"} {
		t.Run(mode, func(t *testing.T) {
			c, e := hookCase(t, map[string]int64{"in-process": 2856005, "sweep": 2856006}[mode])
			atStage(t, "after_intent", func() bool {
				newer := newWindowRefusal(t, c, "in_progress")
				testHandler.onRunBudgetExhausted(context.Background(), newer)
				return mode == "sweep"
			})
			testHandler.onRunBudgetExhausted(context.Background(), e)
			if mode == "sweep" {
				sweepHooks(t)
			}
			rows := refusalRows(t, c)
			if len(rows) != 2 {
				t.Fatalf("refusal rows = %d (%+v), want 2", len(rows), rows)
			}
			if r := rows[0]; r.State != "superseded" || r.StateReason != "later_window" || r.Phase != "retired" || r.HookReason != "later_window" {
				t.Fatalf("older refusal = %+v, want superseded and retired by later_window", r)
			}
			if r := rows[1]; r.Phase != "done" || r.State != "open" || r.Status != "in_progress" {
				t.Fatalf("newer refusal = %+v, want open and done for in_progress", r)
			}
			if got := c.status(t); got != "blocked" {
				t.Fatalf("issue is %s, want blocked once", got)
			}
			if n := blockRows(t, c); n != 1 {
				t.Fatalf("block history rows = %d, want 1", n)
			}
			if comments, _ := c.notices(t, service.RunBudgetKindExhausted); comments != 1 {
				t.Fatalf("exhaustion notices = %d, want 1", comments)
			}
			if n := recoveryRuns(t, c.edgeCase); n != 1 {
				t.Fatalf("recovery runs = %d, want 1", n)
			}
		})
	}
}

func TestRunBudgetHookStatusAwayAndBackRetiresByOccurrence(t *testing.T) {
	c, e := hookCase(t, 2856007)
	crashAt(t, "after_intent")
	testHandler.onRunBudgetExhausted(context.Background(), e)
	// The history rows are written without RecordStatusChange, so only the
	// occurrence identity can tell this in_progress from the refused one.
	for _, to := range []string{"todo", "in_progress"} {
		c.enter(t, to)
		recordOccurrence(t, c, to)
	}
	sweepHooks(t)
	retiredClean(t, c, "in_progress", 1)
	if r := onlyRefusal(t, c); r.Phase != "retired" || r.HookReason != "status_moved" {
		t.Fatalf("refusal = %+v, want retired by status_moved", r)
	}
}

func TestRunBudgetHookRefusalWhileBlockedCopiesTheOriginalStatusAndTarget(t *testing.T) {
	c, e := hookCase(t, 2856008)
	testHandler.onRunBudgetExhausted(context.Background(), e)
	if got := c.status(t); got != "blocked" {
		t.Fatalf("issue is %s, want blocked", got)
	}
	// A later window opened by something other than a person's move (the
	// workflow's budget start): the issue is blocked, the newest status row is the
	// budget's own block, and the refusal names the status it sees.
	events := make(chan service.RunBudgetExhausted, 1)
	service.SetOnRunBudgetExhausted(func(_ context.Context, e service.RunBudgetExhausted) { events <- e })
	t.Cleanup(func() { service.SetOnRunBudgetExhausted(testHandler.onRunBudgetExhausted) })
	dbfx.Exec(t, `UPDATE issue_workflow SET run_budget_since=clock_timestamp() WHERE id=$1`, c.workflowID)
	newer := spendUntilRefused(t, c, events)
	if newer.Status != "blocked" {
		t.Fatalf("the refusal names status %q, want blocked", newer.Status)
	}
	testHandler.onRunBudgetExhausted(context.Background(), newer)
	rows := refusalRows(t, c)
	if len(rows) != 2 {
		t.Fatalf("refusal rows = %d (%+v), want 2", len(rows), rows)
	}
	if r := rows[1]; r.Status != "in_progress" || !r.Blocked || r.Phase != "done" {
		t.Fatalf("copied refusal = %+v, want the original status in_progress, blocked, done", r)
	}
	if got := c.status(t); got != "blocked" {
		t.Fatalf("issue is %s, want blocked", got)
	}
	if n := blockRows(t, c); n != 1 {
		t.Fatalf("block history rows = %d, want the first block only", n)
	}
	var agentID string
	if err := testPool.QueryRow(context.Background(), `SELECT agent_id::text FROM run_budget_refusal WHERE issue_id=$1 ORDER BY created_at DESC LIMIT 1`, c.issueID).Scan(&agentID); err != nil {
		t.Fatal(err)
	}
	if agentID != c.agentID {
		t.Fatalf("copied target agent = %s, want %s", agentID, c.agentID)
	}
}

func TestRunBudgetHookLookupErrorLeavesTheHookForTheSweep(t *testing.T) {
	c, e := hookCase(t, 2856009)
	// The workflow cannot be read when B looks for the Blocked step: that is not
	// "no Blocked step", so the hook must stay where it is and be retried.
	lookupFails := true
	forIssueWorkflow = func(ctx context.Context, q issueworkflow.Querier, ws, wf, project pgtype.UUID) (*issueworkflow.Definition, db.Project, error) {
		if lookupFails {
			return nil, db.Project{}, errors.New("workflow lookup unavailable")
		}
		return issueworkflow.ForIssue(ctx, q, ws, wf, project)
	}
	t.Cleanup(func() { forIssueWorkflow = issueworkflow.ForIssue })
	testHandler.onRunBudgetExhausted(context.Background(), e)
	if r := onlyRefusal(t, c); r.Phase != "intent" {
		t.Fatalf("hook phase after a failed lookup = %q, want intent (kept for the sweep)", r.Phase)
	}
	if got := c.status(t); got != "in_progress" || blockRows(t, c) != 0 {
		t.Fatalf("issue is %s with %d block rows, want in_progress and none", got, blockRows(t, c))
	}
	lookupFails = false
	sweepHooks(t)
	sweepHooks(t)
	finished(t, c)
}

func TestRunBudgetHookSecondRefusalInTheWindowBlocksAgainAndQueuesNoSecondRun(t *testing.T) {
	c, e := hookCase(t, 2856010)
	testHandler.onRunBudgetExhausted(context.Background(), e)
	finished(t, c)
	// A system move puts the issue back in progress; the same window refuses again.
	c.enter(t, "in_progress")
	testHandler.onRunBudgetExhausted(context.Background(), e)
	if got := c.status(t); got != "blocked" {
		t.Fatalf("issue is %s after the second refusal, want blocked", got)
	}
	if n := blockRows(t, c); n != 2 {
		t.Fatalf("block history rows = %d, want 2", n)
	}
	if n := recoveryRuns(t, c.edgeCase); n != 1 {
		t.Fatalf("recovery runs = %d, want still 1", n)
	}
	if comments, _ := c.notices(t, service.RunBudgetKindExhausted); comments != 1 {
		t.Fatalf("exhaustion notices = %d, want still 1", comments)
	}
	if r := onlyRefusal(t, c); r.Phase != "retired" || r.HookReason != "notice_exists" || r.State != "open" || !r.Blocked {
		t.Fatalf("refusal = %+v, want the one row open, blocked, its hook retired as notice_exists", r)
	}
}

func TestRunBudgetHookBlockedBeforeTheReleaseEndsAsUnknownOrigin(t *testing.T) {
	c, e := hookCase(t, 2856011)
	// Blocked by something that left no refusal row (a block from before this release).
	c.enter(t, "blocked")
	testHandler.onRunBudgetExhausted(context.Background(), e)
	r := onlyRefusal(t, c)
	if r.State != "superseded" || r.StateReason != "unknown_origin" || r.Status != "" || r.Phase != "done" {
		t.Fatalf("refusal = %+v, want superseded:unknown_origin, no status, hook done", r)
	}
	if got := c.status(t); got != "blocked" || blockRows(t, c) != 0 {
		t.Fatalf("issue is %s with %d budget block rows, want blocked and none", got, blockRows(t, c))
	}
	if comments, _ := c.notices(t, service.RunBudgetKindExhausted); comments != 1 {
		t.Fatalf("exhaustion notices = %d, want 1", comments)
	}
}
