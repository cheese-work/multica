package handler

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/service"
)

var noBlockedSteps = []map[string]any{
	{"status_key": "backlog"},
	{"status_key": "todo"},
	{"status_key": "in_progress"},
	{"status_key": "in_review"},
}

type budgetCase struct {
	edgeCase
	agentID string
}

// newBudgetCase builds an in_progress issue whose workflow allows max runs and
// whose requester is the test member.
func newBudgetCase(t *testing.T, installationID int64, steps []map[string]any, max int) budgetCase {
	t.Helper()
	c := newEdgeCase(t, installationID, steps)
	agentID := seededReadyAgentID(t)
	ctx := context.Background()
	if _, err := testPool.Exec(ctx, `UPDATE issue_workflow SET max_runs_per_issue=$2, run_budget_since=now()-interval '1 hour' WHERE id=$1`, c.workflowID, max); err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(ctx, `UPDATE issue SET status='in_progress', assignee_type='agent', assignee_id=$2, requested_by_id=$3 WHERE id=$1`, c.issueID, agentID, testUserID); err != nil {
		t.Fatal(err)
	}
	return budgetCase{edgeCase: c, agentID: agentID}
}

// admit enqueues one derived run and finishes every open run, so the next
// admission counts it.
func (c budgetCase) admit(t *testing.T) error {
	t.Helper()
	ctx := context.Background()
	issue, err := testHandler.Queries.GetIssue(ctx, parseUUID(c.issueID))
	if err != nil {
		t.Fatal(err)
	}
	_, err = testHandler.TaskService.EnqueueTaskForIssue(ctx, issue)
	testPool.Exec(ctx, `UPDATE agent_task_queue SET status='completed',completed_at=clock_timestamp() WHERE issue_id=$1 AND status IN ('queued','dispatched','running')`, c.issueID)
	return err
}

func (c budgetCase) spend(t *testing.T, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if err := c.admit(t); err != nil {
			t.Fatalf("run %d inside the budget refused: %v", i+1, err)
		}
	}
}

func (c budgetCase) refuse(t *testing.T) {
	t.Helper()
	if err := c.admit(t); !errors.Is(err, service.ErrRunBudgetExhausted) {
		t.Fatalf("admission = %v, want ErrRunBudgetExhausted", err)
	}
}

// notices counts the system comments and inbox items of one notice kind.
func (c budgetCase) notices(t *testing.T, kind string) (comments, inbox int) {
	t.Helper()
	return dbfx.Count(t, `SELECT count(*) FROM run_budget_notice n JOIN comment c ON c.id=n.comment_id WHERE n.issue_id=$1 AND n.kind=$2 AND c.author_type='system'`, c.issueID, kind),
		dbfx.Count(t, `SELECT count(*) FROM inbox_item WHERE issue_id=$1 AND type='run_budget' AND details->>'notice'=$2`, c.issueID, kind)
}

// waitNotices waits for the hook goroutine, then lets stray duplicates land
// before the caller asserts exact counts.
func (c budgetCase) waitNotices(t *testing.T, kind string, wantComments int) (comments, inbox int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if comments, _ = c.notices(t, kind); comments >= wantComments {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond)
	return c.notices(t, kind)
}

func TestRunBudgetExhaustionNoticeWithBlockedStatus(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	c := newBudgetCase(t, 2310001, mergeEdgeSteps, 2)
	c.spend(t, 2)
	c.refuse(t)
	if comments, inbox := c.waitNotices(t, service.RunBudgetKindExhausted, 1); comments != 1 || inbox != 1 {
		t.Fatalf("notices = %d comments, %d inbox items, want 1 and 1", comments, inbox)
	}
	if got := c.status(t); got != "blocked" {
		t.Fatalf("status = %s, want blocked", got)
	}
}

func TestRunBudgetExhaustionNoticeWithoutStatusMove(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	t.Run("already blocked", func(t *testing.T) {
		c := newBudgetCase(t, 2310002, mergeEdgeSteps, 2)
		c.spend(t, 2)
		c.enter(t, "blocked")
		c.refuse(t)
		if comments, inbox := c.waitNotices(t, service.RunBudgetKindExhausted, 1); comments != 1 || inbox != 1 {
			t.Fatalf("notices = %d comments, %d inbox items, want 1 and 1", comments, inbox)
		}
		if got := c.status(t); got != "blocked" {
			t.Fatalf("status = %s, want blocked", got)
		}
		if n := dbfx.Count(t, `SELECT count(*) FROM activity_log WHERE issue_id=$1 AND action='run_budget_exhausted'`, c.issueID); n != 0 {
			t.Fatalf("run_budget_exhausted activity rows = %d, want 0 for an issue already blocked", n)
		}
	})
	t.Run("workflow without blocked", func(t *testing.T) {
		c := newBudgetCase(t, 2310003, noBlockedSteps, 2)
		c.spend(t, 2)
		c.refuse(t)
		if comments, inbox := c.waitNotices(t, service.RunBudgetKindExhausted, 1); comments != 1 || inbox != 1 {
			t.Fatalf("notices = %d comments, %d inbox items, want 1 and 1", comments, inbox)
		}
		if got := c.status(t); got != "in_progress" {
			t.Fatalf("status = %s, want in_progress", got)
		}
	})
}

func TestRunBudgetConcurrentRefusalsGiveOneNotice(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	c := newBudgetCase(t, 2310004, mergeEdgeSteps, 2)
	c.spend(t, 2)
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.admit(t)
		}()
	}
	wg.Wait()
	if comments, inbox := c.waitNotices(t, service.RunBudgetKindExhausted, 1); comments != 1 || inbox != 1 {
		t.Fatalf("notices = %d comments, %d inbox items, want 1 and 1", comments, inbox)
	}
}

func TestRunBudgetSecondRefusalInWindowGivesNoNotice(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	c := newBudgetCase(t, 2310005, mergeEdgeSteps, 2)
	c.spend(t, 2)
	c.refuse(t)
	c.waitNotices(t, service.RunBudgetKindExhausted, 1)
	c.refuse(t)
	if comments, inbox := c.waitNotices(t, service.RunBudgetKindExhausted, 2); comments != 1 || inbox != 1 {
		t.Fatalf("notices = %d comments, %d inbox items, want 1 and 1", comments, inbox)
	}
}

func TestRunBudgetNewWindowRearms(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	c := newBudgetCase(t, 2310006, mergeEdgeSteps, 2)
	c.spend(t, 2)
	c.refuse(t)
	c.waitNotices(t, service.RunBudgetKindExhausted, 1)
	// A person moving the status opens a new window.
	if _, err := testPool.Exec(context.Background(), `INSERT INTO activity_log (workspace_id, issue_id, actor_type, actor_id, action, details)
		VALUES ($1, $2, 'member', $3, 'status_changed', '{}')`, testWorkspaceID, c.issueID, testUserID); err != nil {
		t.Fatal(err)
	}
	c.enter(t, "in_progress")
	c.spend(t, 2)
	c.refuse(t)
	if comments, inbox := c.waitNotices(t, service.RunBudgetKindExhausted, 2); comments != 2 || inbox != 2 {
		t.Fatalf("notices = %d comments, %d inbox items, want 2 and 2", comments, inbox)
	}
}

func TestRunBudgetInboxItemIgnoresMutedNotifications(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	c := newBudgetCase(t, 2310007, mergeEdgeSteps, 2)
	if _, err := testPool.Exec(context.Background(), `INSERT INTO notification_preference (workspace_id, user_id, preferences)
		VALUES ($1, $2, '{"unassigned_activity":"muted"}')
		ON CONFLICT (workspace_id, user_id) DO UPDATE SET preferences = EXCLUDED.preferences`, testWorkspaceID, testUserID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM notification_preference WHERE workspace_id=$1 AND user_id=$2`, testWorkspaceID, testUserID)
	})
	c.spend(t, 2)
	c.refuse(t)
	if _, inbox := c.waitNotices(t, service.RunBudgetKindExhausted, 1); inbox != 1 {
		t.Fatalf("inbox items = %d, want 1 even with notifications muted", inbox)
	}
}

func TestRunBudgetNoticeRecipientFallsBackToOwner(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	c := newBudgetCase(t, 2310008, mergeEdgeSteps, 2)
	if _, err := testPool.Exec(context.Background(), `UPDATE issue SET requested_by_id=NULL, creator_type='agent', creator_id=$2 WHERE id=$1`, c.issueID, c.agentID); err != nil {
		t.Fatal(err)
	}
	c.spend(t, 2)
	c.refuse(t)
	if _, inbox := c.waitNotices(t, service.RunBudgetKindExhausted, 1); inbox < 1 {
		t.Fatalf("inbox items = %d, want the workspace owners notified", inbox)
	}
}

func TestRunBudgetWarningAtEightyPercentOfAdmission(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	c := newBudgetCase(t, 2310009, mergeEdgeSteps, 5)
	c.spend(t, 3)
	if comments, inbox := c.waitNotices(t, service.RunBudgetKindWarn80, 1); comments != 0 || inbox != 0 {
		t.Fatalf("notices below 80%% = %d comments, %d inbox items, want 0 and 0", comments, inbox)
	}
	// The 4th admission makes 4 of 5 runs: it writes the warning itself.
	c.spend(t, 1)
	if comments, inbox := c.waitNotices(t, service.RunBudgetKindWarn80, 1); comments != 1 || inbox != 1 {
		t.Fatalf("notices at 80%% = %d comments, %d inbox items, want 1 and 1", comments, inbox)
	}
	if got := c.status(t); got != "in_progress" {
		t.Fatalf("status after the warning = %s, want in_progress", got)
	}
	c.spend(t, 1)
	if comments, _ := c.waitNotices(t, service.RunBudgetKindWarn80, 2); comments != 1 {
		t.Fatalf("comments after the 5th run = %d, want still 1", comments)
	}
}

// A deferred channel admission (a media-gated /issue task) is a run too: the
// one that makes 4 of 5 writes the warning.
func TestRunBudgetWarningCoversDeferredChannelAdmission(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	c := newBudgetCase(t, 2310012, mergeEdgeSteps, 5)
	c.spend(t, 3)
	issue, err := testHandler.Queries.GetIssue(context.Background(), parseUUID(c.issueID))
	if err != nil {
		t.Fatal(err)
	}
	task, err := testHandler.TaskService.EnqueueDeferredChannelIssueTask(context.Background(), issue, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("deferred admission refused: %v", err)
	}
	if err := testHandler.TaskService.PromoteDeferredChannelIssueTask(context.Background(), task.ID); err != nil {
		t.Fatal(err)
	}
	if comments, inbox := c.waitNotices(t, service.RunBudgetKindWarn80, 1); comments != 1 || inbox != 1 {
		t.Fatalf("notices after a deferred admission at 4 of 5 = %d comments, %d inbox items, want 1 and 1", comments, inbox)
	}
}

// warnEvent is what guardRunBudget fires when an admission would make the 4th
// of 5 runs, before that admission's insert has committed.
func (c budgetCase) warnEvent(t *testing.T) service.RunBudgetEvent {
	t.Helper()
	var since time.Time
	if err := testPool.QueryRow(context.Background(), `SELECT run_budget_since FROM issue_workflow WHERE id=$1`, c.workflowID).Scan(&since); err != nil {
		t.Fatal(err)
	}
	return service.RunBudgetEvent{IssueID: parseUUID(c.issueID), Kind: service.RunBudgetKindWarn80, Runs: 4, Max: 5, Since: since}
}

// A warning event for an admission that never committed (duplicate-pending
// rejection, rolled-back transaction) writes nothing and keeps the window's
// claim free for the real 80% admission.
func TestRunBudgetWarningWaitsForCommittedAdmission(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	old := runBudgetCommitWait
	runBudgetCommitWait = 300 * time.Millisecond
	t.Cleanup(func() { runBudgetCommitWait = old })

	c := newBudgetCase(t, 2310010, mergeEdgeSteps, 5)
	c.spend(t, 3)
	testHandler.onRunBudgetEvent(context.Background(), c.warnEvent(t))
	if comments, inbox := c.notices(t, service.RunBudgetKindWarn80); comments != 0 || inbox != 0 {
		t.Fatalf("notices for an uncommitted admission = %d comments, %d inbox items, want 0 and 0", comments, inbox)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM run_budget_notice WHERE issue_id=$1`, c.issueID); n != 0 {
		t.Fatalf("claim rows = %d, want 0: the false event must not consume the window's warn80 claim", n)
	}
	// The real 4th admission still writes its own warning.
	c.spend(t, 1)
	if comments, inbox := c.waitNotices(t, service.RunBudgetKindWarn80, 1); comments != 1 || inbox != 1 {
		t.Fatalf("notices after the real 4th run = %d comments, %d inbox items, want 1 and 1", comments, inbox)
	}
}

// The insert may commit a moment after the guard fires: the warning waits for it.
func TestRunBudgetWarningSeesLateCommit(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	c := newBudgetCase(t, 2310011, mergeEdgeSteps, 5)
	c.spend(t, 3)
	ev := c.warnEvent(t)
	go func() {
		time.Sleep(400 * time.Millisecond)
		c.admit(t)
	}()
	testHandler.onRunBudgetEvent(context.Background(), ev)
	if comments, inbox := c.notices(t, service.RunBudgetKindWarn80); comments != 1 || inbox != 1 {
		t.Fatalf("notices = %d comments, %d inbox items, want 1 and 1", comments, inbox)
	}
}

// slowAdmission opens the 4th admission's transaction and returns it unfinished.
func (c budgetCase) slowAdmission(t *testing.T) pgx.Tx {
	t.Helper()
	ctx := context.Background()
	issue, err := testHandler.Queries.GetIssue(ctx, parseUUID(c.issueID))
	if err != nil {
		t.Fatal(err)
	}
	tx, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tx.Rollback(ctx) })
	if _, err := testHandler.TaskService.EnqueueDecisionTaskInTx(ctx, testHandler.Queries.WithTx(tx), issue, issue.AssigneeID, pgtype.UUID{}, pgtype.UUID{}, nil, ""); err != nil {
		t.Fatal(err)
	}
	return tx
}

// A transaction that stays open past the commit wait still gets its warning
// once it commits: the handler follows the admission's transaction, not a timer.
func TestRunBudgetWarningSurvivesSlowCommit(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	old := runBudgetCommitWait
	runBudgetCommitWait = 300 * time.Millisecond
	t.Cleanup(func() { runBudgetCommitWait = old })

	c := newBudgetCase(t, 2310012, mergeEdgeSteps, 5)
	c.spend(t, 3)
	tx := c.slowAdmission(t)
	time.Sleep(3 * runBudgetCommitWait)
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	if comments, inbox := c.waitNotices(t, service.RunBudgetKindWarn80, 1); comments != 1 || inbox != 1 {
		t.Fatalf("notices after a slow commit = %d comments, %d inbox items, want 1 and 1", comments, inbox)
	}
}

// A transaction that rolls back after the wait leaves no warning and keeps the
// window's claim free.
func TestRunBudgetWarningDroppedOnSlowRollback(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	old := runBudgetCommitWait
	runBudgetCommitWait = 300 * time.Millisecond
	t.Cleanup(func() { runBudgetCommitWait = old })

	c := newBudgetCase(t, 2310013, mergeEdgeSteps, 5)
	c.spend(t, 3)
	tx := c.slowAdmission(t)
	time.Sleep(3 * runBudgetCommitWait)
	if err := tx.Rollback(context.Background()); err != nil {
		t.Fatal(err)
	}
	if comments, inbox := c.waitNotices(t, service.RunBudgetKindWarn80, 1); comments != 0 || inbox != 0 {
		t.Fatalf("notices after a rollback = %d comments, %d inbox items, want 0 and 0", comments, inbox)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM run_budget_notice WHERE issue_id=$1`, c.issueID); n != 0 {
		t.Fatalf("claim rows = %d, want 0", n)
	}
}

// A direct enqueue (no caller transaction) whose INSERT waits on a lock longer
// than the commit wait still gets its warning once the INSERT lands.
func TestRunBudgetWarningSurvivesDelayedDirectInsert(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	old := runBudgetCommitWait
	runBudgetCommitWait = 300 * time.Millisecond
	t.Cleanup(func() { runBudgetCommitWait = old })

	c := newBudgetCase(t, 2310014, mergeEdgeSteps, 5)
	c.spend(t, 3)
	ctx := context.Background()
	lock, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Rollback(ctx)
	if _, err := lock.Exec(ctx, `LOCK TABLE agent_task_queue IN SHARE ROW EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- c.admit(t) }()
	time.Sleep(3 * runBudgetCommitWait)
	if err := lock.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("delayed admission: %v", err)
	}
	if comments, inbox := c.waitNotices(t, service.RunBudgetKindWarn80, 1); comments != 1 || inbox != 1 {
		t.Fatalf("notices after a delayed direct insert = %d comments, %d inbox items, want 1 and 1", comments, inbox)
	}
}

// A named wakeup is a counted run too. The one that lifts the issue to 80% of
// the ceiling writes the warning, even though no direct enqueue follows it.
func TestRunBudgetWarningCoversNamedWakeupAdmission(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	c := newBudgetCase(t, 2310013, mergeEdgeSteps, 5)
	c.spend(t, 3)
	ctx := context.Background()
	svc := service.IssueWakeupService{Tasks: testHandler.TaskService}
	wakeup, err := svc.Create(ctx, parseUUID(c.issueID), parseUUID(testUserID), pgtype.UUID{}, service.WakeupInput{
		AgentID: c.agentID, Kind: "at", AfterSeconds: 600, Instruction: "check again",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		testPool.Exec(ctx, `DELETE FROM issue_wakeup_receipt WHERE wakeup_id = $1`, wakeup.ID)
		testPool.Exec(ctx, `DELETE FROM issue_wakeup WHERE issue_id = $1`, c.issueID)
	})
	dbfx.Exec(t, `UPDATE issue_wakeup SET next_fire_at = now() - interval '1 minute' WHERE id = $1`, wakeup.ID)
	if err := svc.TickWorkspaces(ctx, parseUUID(testWorkspaceID)); err != nil {
		t.Fatal(err)
	}
	runs := dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND context->>'wakeup_id' = $2`, c.issueID, uuidToString(wakeup.ID))
	if runs != 1 {
		t.Fatalf("runs started by the wakeup = %d, want 1", runs)
	}
	if comments, inbox := c.waitNotices(t, service.RunBudgetKindWarn80, 1); comments != 1 || inbox != 1 {
		t.Fatalf("notices after a named wakeup at 4 of 5 = %d comments, %d inbox items, want 1 and 1", comments, inbox)
	}
}
