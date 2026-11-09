package handler

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

// An approval links the refusal it answers; the worker then resumes it once:
// a direct run when the hook never blocked the issue, the restored status
// (with its handoff run) when it did.

type resumeRow struct {
	State, Reason, Phase, HookReason, Window string
	TaskID, ActionID                         pgtype.UUID
}

func refusalIDs(t *testing.T, c budgetCase) []pgtype.UUID {
	t.Helper()
	rows, err := testPool.Query(context.Background(), `SELECT id FROM run_budget_refusal WHERE issue_id=$1 ORDER BY created_at, id`, c.issueID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var ids []pgtype.UUID
	for rows.Next() {
		var id pgtype.UUID
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	return ids
}

func resumeRowOf(t *testing.T, id pgtype.UUID) resumeRow {
	t.Helper()
	var r resumeRow
	if err := testPool.QueryRow(context.Background(), `SELECT state, COALESCE(state_reason,''), hook_phase, COALESCE(hook_reason,''), COALESCE(resume_window_id,''), task_id, action_id
		FROM run_budget_refusal WHERE id=$1`, id).Scan(&r.State, &r.Reason, &r.Phase, &r.HookReason, &r.Window, &r.TaskID, &r.ActionID); err != nil {
		t.Fatal(err)
	}
	return r
}

// wantResume asserts the refusal's state and reason.
func wantResume(t *testing.T, id pgtype.UUID, state, reason string) resumeRow {
	t.Helper()
	r := resumeRowOf(t, id)
	if r.State != state || r.Reason != reason {
		t.Fatalf("refusal = %s:%s, want %s:%s (%+v)", r.State, r.Reason, state, reason, r)
	}
	return r
}

// intentCase leaves the refusal at tx R: the issue is still in_progress.
func intentCase(t *testing.T, installationID int64) (budgetCase, pgtype.UUID) {
	t.Helper()
	c, e := hookCase(t, installationID)
	crashAt(t, "after_intent")
	testHandler.onRunBudgetExhausted(context.Background(), e)
	return c, refusalIDs(t, c)[0]
}

// blockedCase runs the whole hook: the issue is Blocked, the refusal remembers in_progress.
func blockedCase(t *testing.T, installationID int64) (budgetCase, pgtype.UUID) {
	t.Helper()
	c, e := hookCase(t, installationID)
	testHandler.onRunBudgetExhausted(context.Background(), e)
	finished(t, c)
	return c, refusalIDs(t, c)[0]
}

// reviewBlockedCase is blockedCase for an in_review issue, whose step has a handler.
func reviewBlockedCase(t *testing.T, installationID int64) (budgetCase, pgtype.UUID) {
	t.Helper()
	if testHandler == nil {
		t.Skip("database not available")
	}
	c := newBudgetCase(t, installationID, recoverySteps(seededReadyAgentID(t)), 2)
	c.enter(t, "in_review")
	recordOccurrence(t, c, "in_review")
	testHandler.onRunBudgetExhausted(context.Background(), refusedEvent(t, c))
	if got := c.status(t); got != "blocked" {
		t.Fatalf("issue is %s, want blocked", got)
	}
	return c, refusalIDs(t, c)[0]
}

func linkRefusal(t *testing.T, id pgtype.UUID, window string) pgtype.UUID {
	t.Helper()
	action := dbid.NewV7()
	row, err := testHandler.Queries.GetRunBudgetRefusal(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if window == "" { // no top-up since the refusal: the approval pays for its own window
		window = row.WindowID
	}
	if err := linkRunBudgetRefusal(context.Background(), testHandler.Queries, row.IssueID, uuidToString(id), action, window); err != nil {
		t.Fatal(err)
	}
	return action
}

func raiseCap(t *testing.T, c budgetCase) {
	t.Helper()
	dbfx.Exec(t, `UPDATE issue_workflow SET max_runs_per_issue=30 WHERE id=$1`, c.workflowID)
}

// topUp opens a new window and returns its id.
func topUp(t *testing.T, c budgetCase) string {
	t.Helper()
	dbfx.Exec(t, `UPDATE issue_workflow SET run_budget_since=clock_timestamp() WHERE id=$1`, c.workflowID)
	_, window, err := service.OpenRunBudgetWindowID(context.Background(), testHandler.Queries, parseUUID(c.issueID))
	if err != nil {
		t.Fatal(err)
	}
	return window
}

func resume(t *testing.T, id pgtype.UUID) {
	t.Helper()
	testHandler.resumeRefusal(context.Background(), id)
}

// queuedRuns counts the runs the resume queued; the hook's recovery run is not one.
func resumeQueued(t *testing.T, c budgetCase) int {
	t.Helper()
	return dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id=$1 AND status='queued' AND task_kind IS DISTINCT FROM 'budget_recovery'`, c.issueID)
}

func restoreRows(t *testing.T, c budgetCase, action pgtype.UUID) int {
	t.Helper()
	return dbfx.Count(t, `SELECT count(*) FROM activity_log WHERE issue_id=$1 AND action='status_changed' AND details->'trigger'->'cause'->'ref'->>'action_id'=$2`, c.issueID, uuidToString(action))
}

func TestRunBudgetResumeApprovalBeforeBlockAdmitsTheRunDirectly(t *testing.T) {
	c, id := intentCase(t, 2860101)
	row := resumeRowOf(t, id)
	linkRefusal(t, id, row.Window)
	raiseCap(t, c)
	if r := onlyRefusal(t, c); r.State != "resume_pending" || r.Phase != "retired" || r.HookReason != "resume_pending" {
		t.Fatalf("linked refusal = %+v, want resume_pending with the hook retired", r)
	}
	sweepHooks(t) // B finds the hook retired: no block, no notice
	if got := c.status(t); got != "in_progress" || blockRows(t, c) != 0 || recoveryRuns(t, c.edgeCase) != 0 {
		t.Fatalf("issue %s, block rows %d: the approval must cancel the block", got, blockRows(t, c))
	}
	resume(t, id)
	if r := wantResume(t, id, "run_queued", ""); !r.TaskID.Valid {
		t.Fatalf("run_queued without a task id: %+v", r)
	}
	resume(t, id) // a duplicate resume
	if got, n := c.status(t), resumeQueued(t, c); got != "in_progress" || n != 1 {
		t.Fatalf("issue %s with %d queued runs, want in_progress and 1", got, n)
	}
	if comments, _ := c.notices(t, service.RunBudgetKindExhausted); comments != 0 {
		t.Fatalf("exhaustion notices = %d, want 0 after an approval won the race", comments)
	}
}

func TestRunBudgetResumeApprovalInTheGapAfterIntentCancelsTheBlock(t *testing.T) {
	c, e := hookCase(t, 2860102)
	atStage(t, "after_intent", func() bool {
		id := refusalIDs(t, c)[0]
		linkRefusal(t, id, resumeRowOf(t, id).Window)
		return false
	})
	testHandler.onRunBudgetExhausted(context.Background(), e)
	if r := onlyRefusal(t, c); r.State != "resume_pending" || r.Phase != "retired" {
		t.Fatalf("refusal = %+v, want resume_pending with the hook retired", r)
	}
	if got := c.status(t); got != "in_progress" || blockRows(t, c) != 0 || recoveryRuns(t, c.edgeCase) != 0 {
		t.Fatalf("issue %s, block rows %d: B must retire instead of blocking", got, blockRows(t, c))
	}
}

func TestRunBudgetResumeApprovalAfterBlockRestoresTheStatusOnce(t *testing.T) {
	c, id := blockedCase(t, 2860103)
	action := linkRefusal(t, id, resumeRowOf(t, id).Window)
	raiseCap(t, c)
	resume(t, id)
	resume(t, id)
	wantResume(t, id, "status_restored", "")
	if got := c.status(t); got != "in_progress" {
		t.Fatalf("issue is %s, want the pre-block status in_progress", got)
	}
	if n := restoreRows(t, c, action); n != 1 {
		t.Fatalf("restore history rows = %d, want 1", n)
	}
	if n := resumeQueued(t, c); n != 0 {
		t.Fatalf("queued runs = %d, want 0: in_progress has no handler", n)
	}
	sweepHooks(t) // N finds the resume linked: no notice after the restore
	if comments, _ := c.notices(t, service.RunBudgetKindExhausted); comments != 1 {
		t.Fatalf("exhaustion notices = %d, want only the hook's own", comments)
	}
}

func TestRunBudgetResumeApprovalInTheGapAfterBlockSendsNoNotice(t *testing.T) {
	c, e := hookCase(t, 2860104)
	atStage(t, "after_block", func() bool {
		id := refusalIDs(t, c)[0]
		linkRefusal(t, id, resumeRowOf(t, id).Window)
		return false
	})
	testHandler.onRunBudgetExhausted(context.Background(), e)
	if comments, _ := c.notices(t, service.RunBudgetKindExhausted); comments != 0 || recoveryRuns(t, c.edgeCase) != 0 {
		t.Fatalf("N wrote a notice or recovery run for a linked refusal")
	}
	if r := onlyRefusal(t, c); r.State != "resume_pending" || !r.Blocked {
		t.Fatalf("refusal = %+v, want resume_pending and blocked", r)
	}
	raiseCap(t, c)
	resume(t, refusalIDs(t, c)[0])
	if got := c.status(t); got != "in_progress" {
		t.Fatalf("issue is %s, want in_progress restored", got)
	}
}

func TestRunBudgetResumeRestoredStatusQueuesTheHandlerRun(t *testing.T) {
	c, id := reviewBlockedCase(t, 2860105)
	action := linkRefusal(t, id, resumeRowOf(t, id).Window)
	raiseCap(t, c)
	resume(t, id)
	if r := wantResume(t, id, "run_queued", ""); !r.TaskID.Valid {
		t.Fatalf("run_queued without a task id: %+v", r)
	}
	if got, n := c.status(t), resumeQueued(t, c); got != "in_review" || n != 1 || restoreRows(t, c, action) != 1 {
		t.Fatalf("issue %s, queued %d, restore rows %d: want in_review, 1, 1", got, n, restoreRows(t, c, action))
	}
}

func TestRunBudgetResumeRefusedRestoreRollsBackThenAFreshWindowQueuesItOnce(t *testing.T) {
	c, id := reviewBlockedCase(t, 2860106)
	action := linkRefusal(t, id, resumeRowOf(t, id).Window)
	resume(t, id) // the budget is still spent: admission refuses
	wantResume(t, id, "still_blocked", "runs")
	if got, n := c.status(t), restoreRows(t, c, action); got != "blocked" || n != 0 || resumeQueued(t, c) != 0 {
		t.Fatalf("issue %s, restore rows %d, queued %d: the refused restore must roll back whole", got, n, resumeQueued(t, c))
	}
	if r := onlyRefusal(t, c); !r.Blocked {
		t.Fatalf("refusal lost its block: %+v", r)
	}

	window := topUp(t, c)
	if window == resumeRowOf(t, id).Window {
		t.Fatal("a top-up must open a new window")
	}
	second := linkRefusal(t, id, window)
	if r := resumeRowOf(t, id); r.State != "resume_pending" || r.ActionID != second || r.Window != window {
		t.Fatalf("relinked refusal = %+v, want resume_pending for the top-up's window", r)
	}
	resume(t, id)
	resume(t, id)
	wantResume(t, id, "run_queued", "")
	if got, n := c.status(t), resumeQueued(t, c); got != "in_review" || n != 1 || restoreRows(t, c, second) != 1 {
		t.Fatalf("issue %s, queued %d, restore rows %d: want in_review, 1, 1", got, n, restoreRows(t, c, second))
	}
}

func TestRunBudgetResumeDirectAdmissionRefusedIsStillBlockedThenRetried(t *testing.T) {
	c, id := intentCase(t, 2860107)
	linkRefusal(t, id, resumeRowOf(t, id).Window)
	resume(t, id)
	wantResume(t, id, "still_blocked", "runs")
	if n := resumeQueued(t, c); n != 0 {
		t.Fatalf("queued runs = %d, want 0", n)
	}
	raiseCap(t, c)
	linkRefusal(t, id, resumeRowOf(t, id).Window)
	resume(t, id)
	wantResume(t, id, "run_queued", "")
	if n := resumeQueued(t, c); n != 1 {
		t.Fatalf("queued runs = %d, want 1", n)
	}
}

func TestRunBudgetResumeSecondApprovalAndTwoOwnersQueueOneRun(t *testing.T) {
	c, id := intentCase(t, 2860108)
	window := resumeRowOf(t, id).Window
	first := linkRefusal(t, id, window)
	linkRefusal(t, id, window) // a second owner's approval of the same refused work
	if r := resumeRowOf(t, id); r.ActionID != first || r.State != "resume_pending" {
		t.Fatalf("refusal = %+v, want the first approval's link kept", r)
	}
	raiseCap(t, c)
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); resume(t, id) }()
	}
	wg.Wait()
	wantResume(t, id, "run_queued", "")
	linkRefusal(t, id, window) // an approval after the run was queued changes nothing
	wantResume(t, id, "run_queued", "")
	if n := resumeQueued(t, c); n != 1 {
		t.Fatalf("queued runs = %d, want 1", n)
	}
}

func TestRunBudgetResumeOnlyTheLatestOfTwoOldRefusalsLinks(t *testing.T) {
	c, e1 := hookCase(t, 2860109)
	crashAt(t, "after_intent")
	testHandler.onRunBudgetExhausted(context.Background(), e1)
	e2 := newWindowRefusal(t, c, "in_progress")
	crashAt(t, "after_intent")
	testHandler.onRunBudgetExhausted(context.Background(), e2)
	ids := refusalIDs(t, c)
	if len(ids) != 2 {
		t.Fatalf("refusal rows = %d, want 2", len(ids))
	}
	wantResume(t, ids[0], "superseded", "later_window")
	linkRefusal(t, ids[0], resumeRowOf(t, ids[0]).Window)
	wantResume(t, ids[0], "superseded", "later_window")
	linkRefusal(t, ids[1], resumeRowOf(t, ids[1]).Window)
	wantResume(t, ids[1], "resume_pending", "")
}

func TestRunBudgetResumeStatusAwayAndBackBeforeApprovalIsSuperseded(t *testing.T) {
	c, id := blockedCase(t, 2860110)
	humanMove(t, c, "todo")
	humanMove(t, c, "in_progress")
	wantResume(t, id, "superseded", "status_moved")
	action := linkRefusal(t, id, resumeRowOf(t, id).Window)
	raiseCap(t, c)
	resume(t, id)
	wantResume(t, id, "superseded", "status_moved")
	if n, q := restoreRows(t, c, action), resumeQueued(t, c); n != 0 || q != 0 {
		t.Fatalf("restore rows %d, queued %d, want none", n, q)
	}
}

func TestRunBudgetResumeStatusChangeBetweenApprovalAndEnqueueQueuesNothing(t *testing.T) {
	t.Run("a status write supersedes the linked refusal", func(t *testing.T) {
		c, id := intentCase(t, 2860111)
		linkRefusal(t, id, resumeRowOf(t, id).Window)
		raiseCap(t, c)
		humanMove(t, c, "todo")
		resume(t, id)
		wantResume(t, id, "superseded", "status_moved")
		if n := resumeQueued(t, c); n != 0 {
			t.Fatalf("queued runs = %d, want 0", n)
		}
	})
	t.Run("the worker checks the occurrence itself", func(t *testing.T) {
		c, id := intentCase(t, 2860112)
		linkRefusal(t, id, resumeRowOf(t, id).Window)
		raiseCap(t, c)
		recordOccurrence(t, c, "in_progress") // a status row that superseded nothing
		resume(t, id)
		wantResume(t, id, "superseded", "status_moved")
		if n := resumeQueued(t, c); n != 0 {
			t.Fatalf("queued runs = %d, want 0", n)
		}
	})
}

func TestRunBudgetResumeAWindowOpenedAfterTheLinkSupersedesIt(t *testing.T) {
	c, id := intentCase(t, 2860113)
	linkRefusal(t, id, resumeRowOf(t, id).Window)
	topUp(t, c)
	resume(t, id)
	wantResume(t, id, "superseded", "window_changed")
	if n := resumeQueued(t, c); n != 0 {
		t.Fatalf("queued runs = %d, want 0", n)
	}
}

func TestRunBudgetResumeIssueClosedBeforeTheResumeQueuesNothing(t *testing.T) {
	c, id := intentCase(t, 2860114)
	linkRefusal(t, id, resumeRowOf(t, id).Window)
	raiseCap(t, c)
	dbfx.Exec(t, `UPDATE issue SET status='done' WHERE id=$1`, c.issueID)
	resume(t, id)
	wantResume(t, id, "superseded", "issue_closed")
	if n := resumeQueued(t, c); n != 0 {
		t.Fatalf("queued runs = %d, want 0", n)
	}
}

func TestRunBudgetResumeCrashBeforeCommitIsRetriedByTheSweepOnce(t *testing.T) {
	c, id := intentCase(t, 2860115)
	linkRefusal(t, id, resumeRowOf(t, id).Window)
	raiseCap(t, c)
	runBudgetResumeAt = func(stage string) bool {
		runBudgetResumeAt = func(string) bool { return false }
		return stage == "before_commit"
	}
	t.Cleanup(func() { runBudgetResumeAt = func(string) bool { return false } })
	resume(t, id)
	wantResume(t, id, "resume_pending", "")
	if n := resumeQueued(t, c); n != 0 {
		t.Fatalf("queued runs = %d after the crash, want 0: the transaction rolled back", n)
	}
	testHandler.sweepRunBudgetResumes(context.Background(), time.Now().Add(time.Minute))
	testHandler.sweepRunBudgetResumes(context.Background(), time.Now().Add(time.Minute))
	wantResume(t, id, "run_queued", "")
	if n := resumeQueued(t, c); n != 1 {
		t.Fatalf("queued runs = %d, want 1", n)
	}
}

func TestRunBudgetResumeApprovalExecutorsLinkTheirRefusal(t *testing.T) {
	for _, typ := range []string{raiseAction, topUpAction} {
		t.Run(typ, func(t *testing.T) {
			var id, issueID, window string
			action := pendingBudgetAction(t, typ, func(i, w string) string {
				issueID, window = i, w
				id = insertTestRefusal(t, i, w)
				return id
			})
			// Creating the request moved the fixture issue to blocked, which superseded
			// the refusal; a real refusal is already blocked, so reopen it.
			dbfx.Exec(t, `UPDATE run_budget_refusal SET state = 'open', state_reason = NULL WHERE id = $1`, id)
			action.approve(t, withVerifier(acceptingVerifier), action.claim(t, map[string]string{}), http.StatusOK)
			var linked, resumeWindow string
			dbfx.QueryRow(t, `SELECT COALESCE(action_id::text,''), COALESCE(resume_window_id,'') FROM run_budget_refusal WHERE id=$1`, id).Scan(&linked, &resumeWindow)
			if linked != action.actionID {
				t.Fatalf("refusal action_id = %q, want %q", linked, action.actionID)
			}
			if typ == raiseAction && resumeWindow != window {
				t.Fatalf("a raise resumes in the same window: %q, want %q", resumeWindow, window)
			}
			if typ == topUpAction && (resumeWindow == "" || resumeWindow == window) {
				t.Fatalf("a top-up resumes in the window it opens: %q, source %q", resumeWindow, window)
			}
			_ = issueID
		})
	}
}
