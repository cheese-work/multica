package handler

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/issueworkflow"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
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
		VALUES ($1, $2, 'member', $3, 'status_changed', jsonb_build_object('to', $4::text, 'trigger', jsonb_build_object('cause', jsonb_build_object('kind', 'ui_or_api'), 'auth_source', 'interactive_jwt')))`, testWorkspaceID, c.issueID, testUserID, to); err != nil {
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
	interactive := "interactive_jwt" // only an interactive move opens a window (S1d)
	trigger := service.StatusTrigger{Cause: service.StatusCause{Kind: service.StatusCauseUIOrAPI, By: service.StatusCauseActor{Type: "member", ID: testUserID}}, AuthSource: &interactive, Channel: "web"}
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

func TestRunBudgetHookOlderNoticeStepDoesNotRetireTheRevivedHook(t *testing.T) {
	c, e := hookCase(t, 2856012)
	testHandler.onRunBudgetExhausted(context.Background(), e)
	finished(t, c)
	// A second refusal of the window blocks again and pauses before its notice
	// step. A move supersedes it and a third refusal revives the row, then stops
	// after its intent. The paused notice step finds the window's notice and
	// must not retire the third refusal's hook.
	c.enter(t, "in_progress")
	atStage(t, "after_block", func() bool {
		c.enter(t, "in_progress")
		// What a status move's history row does to the refusals; the window stays.
		if err := testHandler.Queries.SupersedeRunBudgetRefusalsOnStatusChange(context.Background(), parseUUID(c.issueID)); err != nil {
			t.Fatal(err)
		}
		crashAt(t, "after_intent")
		testHandler.onRunBudgetExhausted(context.Background(), e)
		return false
	})
	testHandler.onRunBudgetExhausted(context.Background(), e)
	if r := onlyRefusal(t, c); r.Phase != "intent" {
		t.Fatalf("revived hook phase = %q (reason %q), want intent: an older step retired it", r.Phase, r.HookReason)
	}
	sweepHooks(t)
	if got := c.status(t); got != "blocked" {
		t.Fatalf("issue is %s after the sweep, want blocked by the revived hook", got)
	}
}

func TestRunBudgetHookDelayedStepOfAnOlderGenerationDoesNotAdvanceTheRevivedRow(t *testing.T) {
	c, e := hookCase(t, 2856013)
	testHandler.onRunBudgetExhausted(context.Background(), e)
	finished(t, c)
	var id pgtype.UUID
	var oldGeneration int32
	if err := testPool.QueryRow(context.Background(), `SELECT id, hook_generation FROM run_budget_refusal WHERE issue_id=$1`, c.issueID).Scan(&id, &oldGeneration); err != nil {
		t.Fatal(err)
	}
	// A move supersedes the refusal and a later refusal revives the row, then
	// stops after its intent. A callback that started in the older generation
	// and was delayed carries the older event: it must not run the new hook.
	c.enter(t, "in_progress")
	if err := testHandler.Queries.SupersedeRunBudgetRefusalsOnStatusChange(context.Background(), parseUUID(c.issueID)); err != nil {
		t.Fatal(err)
	}
	crashAt(t, "after_intent")
	testHandler.onRunBudgetExhausted(context.Background(), e)
	blocksBefore := blockRows(t, c)
	testHandler.advanceRefusalHook(context.Background(), id, oldGeneration, e)
	if r := onlyRefusal(t, c); r.Phase != "intent" {
		t.Fatalf("revived hook phase = %q, want intent: the older generation's event advanced it", r.Phase)
	}
	if got := blockRows(t, c); got != blocksBefore {
		t.Fatalf("block history rows = %d, want %d: the older event blocked the issue", got, blocksBefore)
	}
}

// taintedRefusal spends the budget, then makes a task that carries taint ask for
// one run too many. It returns the refusal's event and the context the callback
// would get, without running its hook.
func taintedRefusal(t *testing.T, c budgetCase, taint string) (context.Context, service.RunBudgetExhausted) {
	t.Helper()
	c.spend(t, 2)
	return refusalBy(t, c, taint)
}

// refusalBy asks for one run too many on a spent budget, as a task carrying
// taint, or as a request with no acting task when taint is empty.
func refusalBy(t *testing.T, c budgetCase, taint string) (context.Context, service.RunBudgetExhausted) {
	t.Helper()
	type refusal struct {
		ctx context.Context
		e   service.RunBudgetExhausted
	}
	events := make(chan refusal, 1)
	service.SetOnRunBudgetExhausted(func(ctx context.Context, e service.RunBudgetExhausted) { events <- refusal{ctx, e} })
	t.Cleanup(func() { service.SetOnRunBudgetExhausted(testHandler.onRunBudgetExhausted) })
	ctx := context.Background()
	if taint != "" {
		var actor pgtype.UUID
		if err := testPool.QueryRow(context.Background(), `UPDATE agent_task_queue SET taint_ids=ARRAY[$2]::text[]
			WHERE id=(SELECT id FROM agent_task_queue WHERE issue_id=$1 ORDER BY created_at LIMIT 1) RETURNING id`, c.issueID, taint).Scan(&actor); err != nil {
			t.Fatal(err)
		}
		ctx = service.WithActingTask(ctx, actor)
	}
	issue, err := testHandler.Queries.GetIssue(context.Background(), parseUUID(c.issueID))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := testHandler.TaskService.EnqueueTaskForIssue(ctx, issue); !errors.Is(err, service.ErrRunBudgetExhausted) {
		t.Fatalf("admission = %v, want ErrRunBudgetExhausted", err)
	}
	r := <-events
	return r.ctx, r.e
}

// recoveryTaint is the taint of the issue's recovery run.
func recoveryTaint(t *testing.T, c budgetCase) []string {
	t.Helper()
	var taint []string
	if err := testPool.QueryRow(context.Background(), `SELECT taint_ids FROM agent_task_queue WHERE issue_id=$1 AND task_kind='budget_recovery'`, c.issueID).Scan(&taint); err != nil {
		t.Fatal(err)
	}
	return taint
}

// A requester whose callback reaches the hook after its notice committed still
// answers the same hook: the recovery run already exists, queued or started, so
// the late taint goes onto that run too.
func TestRunBudgetHookLateRequesterTaintsTheExistingRecoveryRun(t *testing.T) {
	for i, runStatus := range []string{"queued", "running"} {
		t.Run(runStatus, func(t *testing.T) {
			if testHandler == nil {
				t.Skip("database not available")
			}
			c := newBudgetCase(t, int64(2856050+i), recoverySteps(seededReadyAgentID(t)), 2)
			recordOccurrence(t, c, "in_progress")
			c.spend(t, 2)
			ctxClean, eClean := refusalBy(t, c, "")
			ctxLate, eLate := refusalBy(t, c, "ext-1")
			id, generation, ok := testHandler.recordRefusalIntent(ctxClean, eClean)
			if !ok {
				t.Fatal("clean refusal not recorded")
			}
			testHandler.advanceRefusalHook(ctxClean, id, generation, eClean)
			finished(t, c)
			if got := recoveryTaint(t, c); len(got) != 0 {
				t.Fatalf("recovery run taint before the late requester = %v, want none", got)
			}
			if _, err := testPool.Exec(context.Background(), `UPDATE agent_task_queue SET status=$2 WHERE issue_id=$1 AND task_kind='budget_recovery'`, c.issueID, runStatus); err != nil {
				t.Fatal(err)
			}
			if id2, _, ok := testHandler.recordRefusalIntent(ctxLate, eLate); !ok || id2 != id {
				t.Fatalf("late refusal = %v, %v; want it to reuse the finished row", id2, ok)
			}
			if got := recoveryTaint(t, c); len(got) != 1 || got[0] != "ext-1" {
				t.Fatalf("recovery run taint = %v, want [ext-1]", got)
			}
			finished(t, c)
		})
	}
}

// A sweep rebuilds the event from the refusal row: the recovery run it creates
// must still carry the taint of the task whose request was refused, or an
// active grant could elevate a run that tainted input reached.
func TestRunBudgetHookSweepRecoveryKeepsTheRefusedTasksTaint(t *testing.T) {
	for i, stage := range []string{"after_intent", "after_block"} {
		t.Run(stage, func(t *testing.T) {
			if testHandler == nil {
				t.Skip("database not available")
			}
			c := newBudgetCase(t, int64(2856030+i), recoverySteps(seededReadyAgentID(t)), 2)
			recordOccurrence(t, c, "in_progress")
			ctx, e := taintedRefusal(t, c, "ext-1")
			crashAt(t, stage)
			testHandler.onRunBudgetExhausted(ctx, e)
			sweepHooks(t)
			sweepHooks(t)
			finished(t, c)
			if got := recoveryTaint(t, c); len(got) != 1 || got[0] != "ext-1" {
				t.Fatalf("recovery run taint = %v, want [ext-1]", got)
			}
		})
	}
}

// Two requesters can refuse in the same window and share one hook. Whoever
// finishes it, the recovery run carries the taint of every request it answers.
func TestRunBudgetHookRecoveryKeepsEveryRequestersTaint(t *testing.T) {
	// first and second name who refuses first; finisher is "sweep" or the
	// first requester's own callback.
	cases := []struct{ name, first, second, finisher string }{
		{"clean_then_tainted_sweep", "", "ext-1", "sweep"},
		{"tainted_then_clean_sweep", "ext-1", "", "sweep"},
		{"clean_callback_finishes_after_tainted_reuse", "", "ext-1", "first"},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if testHandler == nil {
				t.Skip("database not available")
			}
			c := newBudgetCase(t, int64(2856040+i), recoverySteps(seededReadyAgentID(t)), 2)
			recordOccurrence(t, c, "in_progress")
			c.spend(t, 2)
			ctx1, e1 := refusalBy(t, c, tc.first)
			ctx2, e2 := refusalBy(t, c, tc.second)
			id, generation, ok := testHandler.recordRefusalIntent(ctx1, e1)
			if !ok {
				t.Fatal("first refusal not recorded")
			}
			if id2, _, ok := testHandler.recordRefusalIntent(ctx2, e2); !ok || id2 != id {
				t.Fatalf("second refusal = %v, %v; want it to reuse the first's row", id2, ok)
			}
			if tc.finisher == "sweep" {
				sweepHooks(t)
			} else {
				testHandler.advanceRefusalHook(ctx1, id, generation, e1)
			}
			finished(t, c)
			if got := recoveryTaint(t, c); len(got) != 1 || got[0] != "ext-1" {
				t.Fatalf("recovery run taint = %v, want [ext-1]", got)
			}
		})
	}
}

// A tainted agent hands a spent issue over while a clean hook has already
// created and started the recovery run. The tainted request's callback is held,
// as it is when it has not run yet: the taint must already stop the recovery
// run from authorizing an elevated operation (it cannot be revoked afterwards).
func TestRunBudgetHookTaintedAssignmentFencesRecoveryRunBeforeItsCallback(t *testing.T) {
	taintedAssignmentFencesRecoveryRun(t, 2856060, false, "")
}

// A member cleared the taint ID for this issue: the issue stays clean, but the
// task that carries the ID is still tainted, and so is the run its assignment starts.
func TestRunBudgetHookClearedTaintAssignmentFencesRecoveryRunBeforeItsCallback(t *testing.T) {
	taintedAssignmentFencesRecoveryRun(t, 2856061, true, "")
}

// The recovery run is already claimed and running when the tainted assignment
// commits, and the ID was cleared for the issue: neither the pending-run update
// nor the issue taint reaches it, so the run itself takes the taint.
func TestRunBudgetHookClearedTaintAssignmentFencesARunningRecoveryRun(t *testing.T) {
	taintedAssignmentFencesRecoveryRun(t, 2856062, true, "running")
}

// The same, for a run the daemon holds in waiting_local_directory: it is claimed
// elevated, has not started, and starting it reads only its own taint.
func TestRunBudgetHookClearedTaintAssignmentFencesAWaitingRecoveryRun(t *testing.T) {
	taintedAssignmentFencesRecoveryRun(t, 2856065, true, "waiting_local_directory")
}

// taintedAssignment hands the spent issue to the recovery handler as a task that
// carries ext-1 on another issue, and returns once its refusal reached the held
// callback (which never runs, as after a crash).
func taintedAssignment(t *testing.T, c budgetCase) {
	t.Helper()
	ctx := context.Background()
	held := make(chan service.RunBudgetExhausted, 1)
	service.SetOnRunBudgetExhausted(func(_ context.Context, e service.RunBudgetExhausted) { held <- e })
	t.Cleanup(func() { service.SetOnRunBudgetExhausted(testHandler.onRunBudgetExhausted) })
	// The tainted agent works on another issue; its task alone carries the taint.
	actorRuntime := createClaimReclaimRuntime(t, ctx, "Budget previous runtime")
	other := dbfx.Agent(t, "Budget previous assignee", actorRuntime)
	actor := dbfx.Task(t, c.agentID, testutil.Cols{"status": "running", "runtime_id": actorRuntime, "issue_id": dbfx.Issue(t, "budget tainted actor"), "taint_ids": []string{"ext-1"}})
	// The issue sits with another agent, so handing it to the recovery handler is a change.
	dbfx.Exec(t, `UPDATE issue SET assignee_id=$2 WHERE id=$1`, c.issueID, other)
	testutil.Call(t, testHandler.UpdateIssue, testutil.WithURLParams(
		asRun(newRequest(http.MethodPatch, "/api/issues/"+c.issueID, map[string]any{
			"assignee_type": "agent", "assignee_id": c.agentID,
		}), c.agentID, actor), "id", c.issueID)).Want(http.StatusOK)
	select {
	case <-held:
	case <-time.After(5 * time.Second):
		t.Fatal("the tainted assignment's refusal never reached its callback")
	}
}

// memberClearsTaint is the member's clear of ext-1 for the issue.
func memberClearsTaint(t *testing.T, c budgetCase) {
	t.Helper()
	dbfx.Exec(t, `INSERT INTO issue_taint_clear (workspace_id, issue_id, taint_ids, cleared_by_member_id)
		SELECT $1, $2, ARRAY['ext-1'], id FROM member WHERE workspace_id = $1 LIMIT 1`, testWorkspaceID, c.issueID)
	dbfx.Exec(t, `UPDATE issue_taint SET taint_ids = '{}' WHERE issue_id = $1`, c.issueID)
}

// claimRecoveryRun hands the issue's recovery run to a fresh runtime with a live
// repo_write grant and claims it.
func claimRecoveryRun(t *testing.T, c budgetCase) (elevatedFixture, string, int64) {
	t.Helper()
	ctx := context.Background()
	runtimeID := createClaimReclaimRuntime(t, ctx, "Budget recovery runtime")
	agentID := dbfx.Agent(t, "Budget recovery agent", runtimeID)
	dbfx.Exec(t, `UPDATE agent_task_queue SET agent_id=$2, runtime_id=$3, status='dispatched', dispatched_at=now() - interval '120 seconds'
		WHERE issue_id=$1 AND task_kind='budget_recovery'`, c.issueID, agentID, runtimeID)
	insertGrant(t, c.issueID, testWorkspaceID, []string{"repo_write:1234"}, "1 minute", "1 hour", false)
	req := withURLParam(newOwnerDaemonRequest("POST", "/api/daemon/runtimes/"+runtimeID+"/tasks/claim", nil), "runtimeId", runtimeID)
	var claim struct {
		Task *struct {
			ID      string `json:"id"`
			Profile string `json:"profile"`
			Epoch   int64  `json:"epoch"`
		} `json:"task"`
	}
	testutil.Call(t, testHandler.ClaimTaskByRuntime, req).Want(http.StatusOK).JSON(&claim)
	if claim.Task == nil {
		t.Fatal("claim returned no recovery run")
	}
	return elevatedFixture{taskID: claim.Task.ID, issueID: c.issueID, runtimeID: runtimeID}, claim.Task.Profile, claim.Task.Epoch
}

// assertNoElevatedOp asserts the recovery run cannot authorize repo_write. A
// run claimed without elevation has no epoch to authorize against.
func assertNoElevatedOp(t *testing.T, f elevatedFixture, profile string, epoch int64) {
	t.Helper()
	if profile == "elevated" {
		// The refusal names the taint, not a stale epoch or an expired grant.
		if body := f.authorize(t, epoch, "repo_write:1234", hashA).Want(http.StatusForbidden).Body.String(); !strings.Contains(body, "tainted") {
			t.Fatalf("authorization refused for another reason: %s", body)
		}
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM elevated_op WHERE task_id = $1`, f.taskID); n != 0 {
		t.Fatalf("authorized operations = %d, want 0", n)
	}
}

// afterClaim is the status the recovery run has when the tainted assignment
// commits; empty means the assignment commits before the run is claimed.
func taintedAssignmentFencesRecoveryRun(t *testing.T, seed int64, cleared bool, afterClaim string) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	c := newBudgetCase(t, seed, recoverySteps(seededReadyAgentID(t)), 2)
	if cleared {
		memberClearsTaint(t, c)
	}
	recordOccurrence(t, c, "in_progress")
	c.spend(t, 2)
	ctxClean, eClean := refusalBy(t, c, "")
	if afterClaim == "" {
		taintedAssignment(t, c)
	}

	// The clean hook finishes and its recovery run starts before the held callback runs.
	id, generation, ok := testHandler.recordRefusalIntent(ctxClean, eClean)
	if !ok {
		t.Fatal("clean refusal not recorded")
	}
	testHandler.advanceRefusalHook(ctxClean, id, generation, eClean)
	finished(t, c)

	f, profile, epoch := claimRecoveryRun(t, c)
	if afterClaim != "" {
		if profile != "elevated" {
			t.Fatalf("clean recovery run profile = %q before the assignment, want elevated", profile)
		}
		// A claim leaves the run dispatched; the daemon's start makes it running
		// (after waiting_local_directory, if it waits), which the pending-run
		// taint update does not reach.
		dbfx.Exec(t, `UPDATE agent_task_queue SET status=$2, started_at=now() WHERE id=$1`, f.taskID, afterClaim)
		taintedAssignment(t, c)
	} else if profile == "elevated" {
		t.Fatalf("recovery run claimed elevated before the tainted request's callback ran")
	}
	assertNoElevatedOp(t, f, profile, epoch)
}

// A tainted agent hands the issue over, a member then clears the ID for the
// issue, and the server stops before the refusal callback ran. The clean hook is
// finished by the sweep; its recovery run still carries the assignment's taint.
func TestRunBudgetHookAssignClearRestartKeepsTheAssignmentsTaintOnTheRecoveryRun(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	c := newBudgetCase(t, 2856063, recoverySteps(seededReadyAgentID(t)), 2)
	recordOccurrence(t, c, "in_progress")
	c.spend(t, 2)
	ctxClean, eClean := refusalBy(t, c, "")
	taintedAssignment(t, c)
	memberClearsTaint(t, c)
	if got, err := testHandler.Queries.GetIssueTaintIDs(context.Background(), parseUUID(c.issueID)); err != nil || len(got) != 0 {
		t.Fatalf("issue taint after the clear = %v, %v, want none", got, err)
	}
	crashAt(t, "after_intent")
	testHandler.onRunBudgetExhausted(ctxClean, eClean)
	sweepHooks(t)
	sweepHooks(t)
	finished(t, c)
	if got := recoveryTaint(t, c); len(got) != 1 || got[0] != "ext-1" {
		t.Fatalf("recovery run taint = %v, want [ext-1]", got)
	}
}

// A tainted assignment and an elevated-op authorization of the issue's running
// recovery run overlap. The test holds a share lock on the run, as the
// authorization does first; the assignment then waits on the run, and the
// authorization asks for the issue. Locking the issue before the run (the
// assignment's old order) makes the two wait on each other and the database
// aborts one; locking the run first lets both finish.
func TestTaintedAssignmentAndAuthorizationOfARunningRecoveryRunDoNotDeadlock(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	c := newBudgetCase(t, 2856064, recoverySteps(seededReadyAgentID(t)), 2)
	recordOccurrence(t, c, "in_progress")
	c.spend(t, 2)
	ctxClean, eClean := refusalBy(t, c, "")
	id, generation, ok := testHandler.recordRefusalIntent(ctxClean, eClean)
	if !ok {
		t.Fatal("clean refusal not recorded")
	}
	testHandler.advanceRefusalHook(ctxClean, id, generation, eClean)
	finished(t, c)
	f, profile, epoch := claimRecoveryRun(t, c)
	if profile != "elevated" {
		t.Fatalf("clean recovery run profile = %q, want elevated", profile)
	}
	dbfx.Exec(t, `UPDATE agent_task_queue SET status='running', started_at=now() WHERE id=$1`, f.taskID)

	service.SetOnRunBudgetExhausted(func(context.Context, service.RunBudgetExhausted) {})
	t.Cleanup(func() { service.SetOnRunBudgetExhausted(testHandler.onRunBudgetExhausted) })
	actorRuntime := createClaimReclaimRuntime(t, ctx, "Budget previous runtime")
	other := dbfx.Agent(t, "Budget previous assignee", actorRuntime)
	actor := dbfx.Task(t, c.agentID, testutil.Cols{"status": "running", "runtime_id": actorRuntime, "issue_id": dbfx.Issue(t, "budget tainted actor"), "taint_ids": []string{"ext-1"}})
	dbfx.Exec(t, `UPDATE issue SET assignee_id=$2 WHERE id=$1`, c.issueID, other)

	hold, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer hold.Rollback(ctx)
	if _, err := hold.Exec(ctx, `SELECT 1 FROM agent_task_queue WHERE id = $1 FOR SHARE`, f.taskID); err != nil {
		t.Fatal(err)
	}
	lockWaiters := func() int {
		var n int
		dbfx.QueryRow(t, `SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&n)
		return n
	}
	until := func(what string, ok func() bool) {
		t.Helper()
		for deadline := time.Now().Add(5 * time.Second); !ok(); time.Sleep(20 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s", what)
			}
		}
	}

	assigned := make(chan int, 1)
	go func() {
		w := httptest.NewRecorder()
		testHandler.UpdateIssue(w, testutil.WithURLParams(
			asRun(newRequest(http.MethodPatch, "/api/issues/"+c.issueID, map[string]any{
				"assignee_type": "agent", "assignee_id": c.agentID,
			}), c.agentID, actor), "id", c.issueID))
		assigned <- w.Code
	}()
	until("the assignment to wait on the recovery run", func() bool { return lockWaiters() >= 1 })

	authorized := make(chan int, 1)
	go func() {
		w := httptest.NewRecorder()
		testHandler.AuthorizeElevatedOp(w, withURLParam(newOwnerDaemonRequest("POST", "/api/daemon/tasks/"+f.taskID+"/elevation/authorize",
			map[string]any{"epoch": epoch, "scope": "repo_write:1234", "input_hash": hashA}), "taskId", f.taskID))
		authorized <- w.Code
	}()
	// Either the authorization finished (run locked first) or it waits for the
	// issue (issue locked first); both mean it reached the issue lock.
	var authCode int
	authDone := false
	until("the authorization to reach the issue lock", func() bool {
		select {
		case authCode = <-authorized:
			authDone = true
			return true
		default:
			return lockWaiters() >= 2
		}
	})
	if err := hold.Rollback(ctx); err != nil {
		t.Fatal(err)
	}

	if !authDone {
		select {
		case authCode = <-authorized:
		case <-time.After(10 * time.Second):
			t.Fatal("authorization never finished")
		}
	}
	select {
	case code := <-assigned:
		if code != http.StatusOK {
			t.Fatalf("assignment status = %d, want 200", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("assignment never finished")
	}
	if authCode != http.StatusOK && authCode != http.StatusForbidden {
		t.Fatalf("authorization status = %d, want 200 or 403 (an aborted transaction is 500)", authCode)
	}
}

// A late tainted requester's callback and a tainted issue write both update the
// issue's recovery run. Both take the run lock before the issue lock, so neither
// waits on the other while holding what the other needs.
func TestLateRefusalCallbackAndTaintedWriteDoNotDeadlock(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	c := newBudgetCase(t, 2856066, recoverySteps(seededReadyAgentID(t)), 2)
	recordOccurrence(t, c, "in_progress")
	c.spend(t, 2)
	ctxClean, eClean := refusalBy(t, c, "")
	ctxLate, eLate := refusalBy(t, c, "ext-1")
	id, generation, ok := testHandler.recordRefusalIntent(ctxClean, eClean)
	if !ok {
		t.Fatal("clean refusal not recorded")
	}
	testHandler.advanceRefusalHook(ctxClean, id, generation, eClean)
	finished(t, c)
	var runID string
	dbfx.QueryRow(t, `SELECT id FROM agent_task_queue WHERE issue_id=$1 AND task_kind='budget_recovery'`, c.issueID).Scan(&runID)

	service.SetOnRunBudgetExhausted(func(context.Context, service.RunBudgetExhausted) {})
	t.Cleanup(func() { service.SetOnRunBudgetExhausted(testHandler.onRunBudgetExhausted) })
	actorRuntime := createClaimReclaimRuntime(t, ctx, "Budget previous runtime")
	other := dbfx.Agent(t, "Budget previous assignee", actorRuntime)
	actor := dbfx.Task(t, c.agentID, testutil.Cols{"status": "running", "runtime_id": actorRuntime, "issue_id": dbfx.Issue(t, "budget tainted actor"), "taint_ids": []string{"ext-1"}})
	dbfx.Exec(t, `UPDATE issue SET assignee_id=$2 WHERE id=$1`, c.issueID, other)

	hold, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer hold.Rollback(ctx)
	if _, err := hold.Exec(ctx, `SELECT 1 FROM agent_task_queue WHERE id = $1 FOR SHARE`, runID); err != nil {
		t.Fatal(err)
	}
	lockWaiters := func() int {
		var n int
		dbfx.QueryRow(t, `SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&n)
		return n
	}
	until := func(what string, ok func() bool) {
		t.Helper()
		for deadline := time.Now().Add(5 * time.Second); !ok(); time.Sleep(20 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s", what)
			}
		}
	}

	assigned := make(chan int, 1)
	go func() {
		w := httptest.NewRecorder()
		testHandler.UpdateIssue(w, testutil.WithURLParams(
			asRun(newRequest(http.MethodPatch, "/api/issues/"+c.issueID, map[string]any{
				"assignee_type": "agent", "assignee_id": c.agentID,
			}), c.agentID, actor), "id", c.issueID))
		assigned <- w.Code
	}()
	until("the tainted write to wait on the recovery run", func() bool { return lockWaiters() >= 1 })

	recorded := make(chan bool, 1)
	go func() {
		_, _, ok := testHandler.recordRefusalIntent(ctxLate, eLate)
		recorded <- ok
	}()
	// The callback waits on the recovery run, with or without the issue lock.
	until("the late callback to wait on the recovery run", func() bool { return lockWaiters() >= 2 })
	if err := hold.Rollback(ctx); err != nil {
		t.Fatal(err)
	}

	select {
	case code := <-assigned:
		if code != http.StatusOK {
			t.Fatalf("tainted write status = %d, want 200 (an aborted transaction is 500)", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("tainted write never finished")
	}
	select {
	case ok := <-recorded:
		if !ok {
			t.Fatal("late callback was not recorded (an aborted transaction records nothing)")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("late callback never finished")
	}
	if got := recoveryTaint(t, c); len(got) != 1 || got[0] != "ext-1" {
		t.Fatalf("recovery run taint = %v, want [ext-1]", got)
	}
}

// The notice transaction inserts the recovery run and stays uncommitted while a
// tainted late callback starts. The callback can lock nothing of the run ahead
// of the issue lock, because the run is not visible to it. After the commit the
// callback holds the issue lock; the run is claimed, and its authorization must
// not wait for the issue while holding the run's lock, which the callback's
// update of the run then waits for. One order covers every path: issue lock first.
func TestNoticeInsertThenTaintedCallbackAndClaimedRunAuthorizationDoNotDeadlock(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	c := newBudgetCase(t, 2856067, recoverySteps(seededReadyAgentID(t)), 2)
	recordOccurrence(t, c, "in_progress")
	c.spend(t, 2)
	ctxClean, eClean := refusalBy(t, c, "")
	ctxLate, eLate := refusalBy(t, c, "ext-1")
	id, generation, ok := testHandler.recordRefusalIntent(ctxClean, eClean)
	if !ok {
		t.Fatal("clean refusal not recorded")
	}
	testHandler.advanceRefusalHook(ctxClean, id, generation, eClean)
	finished(t, c)
	// The late requester's taint is on the issue's first run only; the member
	// cleared it for the issue, so the recovery run is claimed elevated.
	memberClearsTaint(t, c)
	var runID string
	dbfx.QueryRow(t, `SELECT id FROM agent_task_queue WHERE issue_id=$1 AND task_kind='budget_recovery'`, c.issueID).Scan(&runID)

	// Take the run back out and let a scripted notice transaction insert it again.
	dbfx.Exec(t, `DROP TABLE IF EXISTS che2856_run_copy`)
	dbfx.Exec(t, `CREATE TABLE che2856_run_copy AS SELECT * FROM agent_task_queue WHERE id = $1`, runID)
	t.Cleanup(func() { dbfx.Exec(t, `DROP TABLE IF EXISTS che2856_run_copy`) })
	dbfx.Exec(t, `DELETE FROM agent_task_queue WHERE id = $1`, runID)

	service.SetOnRunBudgetExhausted(func(context.Context, service.RunBudgetExhausted) {})
	t.Cleanup(func() { service.SetOnRunBudgetExhausted(testHandler.onRunBudgetExhausted) })

	// A test-held lock on the refusal row stops the callback after it took the
	// issue lock and before it updates the run.
	gate, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Rollback(ctx)
	if _, err := gate.Exec(ctx, `SELECT 1 FROM run_budget_refusal WHERE issue_id = $1 FOR UPDATE`, c.issueID); err != nil {
		t.Fatal(err)
	}
	notice, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer notice.Rollback(ctx)
	if _, err := notice.Exec(ctx, `SELECT id FROM issue WHERE id = $1 FOR NO KEY UPDATE`, c.issueID); err != nil {
		t.Fatal(err)
	}
	if _, err := notice.Exec(ctx, `INSERT INTO agent_task_queue SELECT * FROM che2856_run_copy`); err != nil {
		t.Fatal(err)
	}
	waiting := func(query string) bool {
		var n int
		dbfx.QueryRow(t, `SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock' AND query ILIKE '%' || $1 || '%'`, query).Scan(&n)
		return n > 0
	}
	until := func(what string, ok func() bool) {
		t.Helper()
		for deadline := time.Now().Add(5 * time.Second); !ok(); time.Sleep(20 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s", what)
			}
		}
	}

	recorded := make(chan bool, 1)
	go func() {
		_, _, ok := testHandler.recordRefusalIntent(ctxLate, eLate)
		recorded <- ok
	}()
	until("the late callback to wait for the notice transaction", func() bool { return waiting("FROM issue") })
	if err := notice.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	until("the late callback to reach the refusal row", func() bool { return waiting("MergeRunBudgetRefusalTaint") })

	f, _, epoch := claimRecoveryRun(t, c)
	authorized := make(chan int, 1)
	go func() {
		w := httptest.NewRecorder()
		testHandler.AuthorizeElevatedOp(w, withURLParam(newOwnerDaemonRequest("POST", "/api/daemon/tasks/"+f.taskID+"/elevation/authorize",
			map[string]any{"epoch": epoch, "scope": "repo_write:1234", "input_hash": hashA}), "taskId", f.taskID))
		authorized <- w.Code
	}()
	until("the authorization to wait for the issue lock", func() bool { return waiting("FOR SHARE") })
	if err := gate.Rollback(ctx); err != nil {
		t.Fatal(err)
	}

	select {
	case ok := <-recorded:
		if !ok {
			t.Fatal("late callback was not recorded (an aborted transaction records nothing)")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("late callback never finished")
	}
	select {
	case code := <-authorized:
		if code != http.StatusForbidden {
			t.Fatalf("authorization status = %d, want 403 (the run is tainted; an aborted transaction is 500)", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("authorization never finished")
	}
	if got := recoveryTaint(t, c); len(got) != 1 || got[0] != "ext-1" {
		t.Fatalf("recovery run taint = %v, want [ext-1]", got)
	}
}

// Workspace deletion removes the issue_run_taint rows of the workspace.
func TestIssueRunTaintWorkspaceDeletionRemovesItsRows(t *testing.T) {
	workspaceID := dbfx.Workspace(t, "issue run taint teardown", "run-taint-teardown-"+uuid.NewString())
	scoped := testutil.New(testPool, workspaceID, testUserID)
	scoped.Member(t, workspaceID, testUserID, "owner")
	issueID := scoped.Issue(t, "issue run taint teardown")
	dbfx.Exec(t, `INSERT INTO issue_run_taint (issue_id, workspace_id, taint_ids) VALUES ($1, $2, ARRAY['ext-1'])`, issueID, workspaceID)
	t.Cleanup(func() { dbfx.Exec(t, `DELETE FROM issue_run_taint WHERE issue_id = $1`, issueID) })

	w := httptest.NewRecorder()
	req := withURLParam(newRequest(http.MethodDelete, "/api/workspaces/"+workspaceID, nil), "id", workspaceID)
	testHandler.DeleteWorkspace(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("DeleteWorkspace = %d, want 204: %s", w.Code, w.Body.String())
	}
	if got := dbfx.Count(t, `SELECT count(*) FROM issue_run_taint WHERE workspace_id = $1`, workspaceID); got != 0 {
		t.Fatalf("issue_run_taint rows after workspace deletion = %d, want 0", got)
	}
}
