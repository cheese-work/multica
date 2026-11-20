package handler

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// Failed chained run (CHE-7675, slice 10 of CHE-7658): a chained run that ends
// failed or cancelled leaves the issue on its step with nobody running. One
// retry for the step's handler, then `blocked`; a pending switch or a person's
// cancel wins over the retry.

// chainRetryFixture is an issue on step in_progress, held by an agent whose run
// carried it there (the step sets chain_same_owner when chained is true).
type chainRetryFixture struct {
	issueID, workflowID, agent, runtimeID string
}

func newChainRetryFixture(t *testing.T, chained bool) chainRetryFixture {
	t.Helper()
	seedTestCatalog(t)
	resetDefaultWorkflow(t)
	agent := seededReadyAgentID(t)
	wf := createTestWorkflow(t, "Chain retry "+workflowTestSuffix(), "in_progress")
	step := handToAgent(agent, "Implement it.")
	if chained {
		step["chain_same_owner"] = true
	}
	setWorkflowHandoff(t, wf.ID, "in_progress", step).Want(200)
	issueID := dbfx.Issue(t, "chain retry", testutil.Cols{
		"status": "in_progress", "assignee_type": "agent", "assignee_id": agent,
	})
	setIssueOverride(t, issueID, wf.ID)
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM issue_workflow_switch_pending WHERE issue_id = $1`, issueID)
	})
	return chainRetryFixture{
		issueID: issueID, workflowID: wf.ID, agent: agent,
		runtimeID: handlerTestRuntimeID(t),
	}
}

// endedRun is a run of the chained step that ended in the given state.
func (f chainRetryFixture) endedRun(t *testing.T, state string, extra testutil.Cols) string {
	t.Helper()
	cols := testutil.Cols{
		"issue_id": f.issueID, "runtime_id": f.runtimeID, "status": state, "workflow_step": "in_progress",
		"started_at": testutil.Raw("now()"), "originator_user_id": testUserID, "accountable_user_id": testUserID,
	}
	for k, v := range extra {
		cols[k] = v
	}
	return dbfx.Task(t, f.agent, cols)
}

func (f chainRetryFixture) apply(t *testing.T, taskID string) {
	t.Helper()
	testHandler.ApplyChainedRunRetry(context.Background(), util.MustParseUUID(taskID))
}

func (f chainRetryFixture) queued(t *testing.T) []string {
	t.Helper()
	return pendingStepRuns(t, f.issueID, f.agent)
}

func (f chainRetryFixture) queuedTaskID(t *testing.T) string {
	t.Helper()
	var id string
	dbfx.QueryRow(t, `SELECT id FROM agent_task_queue WHERE issue_id = $1 AND status = 'queued'`, f.issueID).Scan(&id)
	return id
}

func (f chainRetryFixture) retryRows(t *testing.T) int {
	t.Helper()
	var n int
	dbfx.QueryRow(t, `SELECT count(*) FROM activity_log WHERE issue_id = $1 AND action = 'chained_run_retry'`, f.issueID).Scan(&n)
	return n
}

func (f chainRetryFixture) blockedReason(t *testing.T) string {
	t.Helper()
	var reason string
	dbfx.QueryRow(t, `SELECT COALESCE(metadata->>'blocked_reason', '') FROM issue WHERE id = $1`, f.issueID).Scan(&reason)
	return reason
}

// A chained run that failed gets exactly one retry for the step's handler, and
// a duplicate event for the same failure starts no second one.
func TestChainedRunFailureStartsOneRetry(t *testing.T) {
	f := newChainRetryFixture(t, true)
	failed := f.endedRun(t, "failed", nil)

	f.apply(t, failed)

	steps := f.queued(t)
	if len(steps) != 1 || steps[0] != "in_progress" {
		t.Fatalf("queued runs = %v, want one retry for the in_progress step", steps)
	}
	wantIssueStatus(t, f.issueID, "in_progress")
	if f.retryRows(t) != 1 {
		t.Fatalf("chained_run_retry rows = %d, want 1", f.retryRows(t))
	}

	f.apply(t, failed)

	if got := len(f.queued(t)); got != 1 {
		t.Fatalf("queued runs = %d after a duplicate event, want still 1", got)
	}
}

// A run cancelled by the server (not a person) is retried the same way.
func TestChainedRunServerCancelStartsOneRetry(t *testing.T) {
	f := newChainRetryFixture(t, true)
	cancelled := f.endedRun(t, "cancelled", testutil.Cols{"cancelled_by_type": "system"})

	f.apply(t, cancelled)

	if got := len(f.queued(t)); got != 1 {
		t.Fatalf("queued runs = %d, want 1 retry", got)
	}
}

// The retry failing too blocks the issue with the reason and starts no run.
func TestChainedRunFailingTwiceBlocksTheIssue(t *testing.T) {
	f := newChainRetryFixture(t, true)
	f.apply(t, f.endedRun(t, "failed", nil))
	retry := f.queuedTaskID(t)
	if retry == "" {
		t.Fatal("the first failure started no retry")
	}
	setTaskState(t, retry, "failed")

	f.apply(t, retry)

	wantIssueStatus(t, f.issueID, "blocked")
	if got := len(f.queued(t)); got != 0 {
		t.Fatalf("queued runs = %d after the second failure, want 0", got)
	}
	if reason := f.blockedReason(t); !strings.HasPrefix(reason, "chained-run-failed") {
		t.Fatalf("blocked_reason = %q, want chained-run-failed", reason)
	}
	var comments int
	dbfx.QueryRow(t, `SELECT count(*) FROM comment WHERE issue_id = $1 AND author_type = 'system' AND content LIKE '%chained-run-failed%'`, f.issueID).Scan(&comments)
	if comments != 1 {
		t.Fatalf("system comments naming the failure = %d, want 1", comments)
	}

	f.apply(t, retry)

	if comments2 := f.retryRows(t); comments2 != 1 {
		t.Fatalf("chained_run_retry rows = %d after a duplicate event, want 1", comments2)
	}
}

// A person's cancel is a decision to stop: nothing retries.
func TestChainedRunCancelledByAPersonIsNotRetried(t *testing.T) {
	f := newChainRetryFixture(t, true)
	cancelled := f.endedRun(t, "cancelled", testutil.Cols{"cancelled_by_type": "member", "cancelled_by_id": testUserID})

	f.apply(t, cancelled)

	if got := len(f.queued(t)); got != 0 {
		t.Fatalf("queued runs = %d after a person's cancel, want 0", got)
	}
	wantIssueStatus(t, f.issueID, "in_progress")
	if f.retryRows(t) != 0 {
		t.Fatal("a person's cancel left a retry record")
	}
}

// A run that was not carrying the step on (flag off) is not this feature's.
func TestChainedRunRetryIgnoresAStepThatDoesNotChain(t *testing.T) {
	f := newChainRetryFixture(t, false)

	f.apply(t, f.endedRun(t, "failed", nil))

	if got := len(f.queued(t)); got != 0 {
		t.Fatalf("queued runs = %d for a step without chain_same_owner, want 0", got)
	}
}

// A task already live on the issue holds the slot: no second run.
func TestChainedRunRetryWaitsForALiveTask(t *testing.T) {
	f := newChainRetryFixture(t, true)
	dbfx.Task(t, f.agent, testutil.Cols{"issue_id": f.issueID, "runtime_id": f.runtimeID, "status": "running", "workflow_step": "in_progress"})

	f.apply(t, f.endedRun(t, "failed", nil))

	if got := len(f.queued(t)); got != 0 {
		t.Fatalf("queued runs = %d beside a live task, want 0", got)
	}
	if f.retryRows(t) != 0 {
		t.Fatal("a retry was recorded while a task was live")
	}
}

// A pending switch wins: the failed run starts no Quick retry, and the issue
// gets one run, in the full workflow.
func TestChainedRunFailureWithPendingSwitchStartsOnlyTheFullWorkflowRun(t *testing.T) {
	f := newChainRetryFixture(t, true)
	full := createTestWorkflow(t, "Chain retry full "+workflowTestSuffix(), "in_progress")
	other := dbfx.Agent(t, "chain retry full handler "+workflowTestSuffix(), f.runtimeID)
	setWorkflowHandoff(t, full.ID, "in_progress", handToAgent(other, "Implement it.")).Want(200)
	dbfx.Exec(t, `INSERT INTO issue_workflow_switch_pending (issue_id, workspace_id, workflow_id, reason)
		VALUES ($1, $2, $3, 'test')`, f.issueID, testWorkspaceID, full.ID)
	failed := f.endedRun(t, "failed", nil)
	bus := events.New()
	testHandler.RegisterDeferredWorkflowSwitch(bus)
	testHandler.RegisterChainedRunRetry(bus)

	bus.Publish(events.Event{Type: protocol.EventTaskFailed, WorkspaceID: testWorkspaceID, Payload: map[string]any{"issue_id": f.issueID, "task_id": failed}})

	deadline := time.Now().Add(5 * time.Second)
	for issueOverride(t, f.issueID) != full.ID && time.Now().Before(deadline) {
		time.Sleep(25 * time.Millisecond)
	}
	if got := issueOverride(t, f.issueID); got != full.ID {
		t.Fatalf("issue follows %q, want the full workflow %q", got, full.ID)
	}
	if got := len(pendingStepRuns(t, f.issueID, f.agent)); got != 0 {
		t.Fatalf("Quick-workflow runs = %d, want 0", got)
	}
	if got := len(pendingStepRuns(t, f.issueID, other)); got != 1 {
		t.Fatalf("full-workflow runs = %d, want 1", got)
	}
	if f.retryRows(t) != 0 {
		t.Fatal("a retry was recorded although the switch won")
	}
}

// The listener turns failed and cancelled task events into a retry attempt, and
// a completed run is not one.
func TestChainedRunRetryListenerReactsToFailedAndCancelledOnly(t *testing.T) {
	for _, tc := range []struct {
		event string
		want  int
	}{
		{protocol.EventTaskFailed, 1},
		{protocol.EventTaskCancelled, 1},
		{protocol.EventTaskCompleted, 0},
	} {
		t.Run(tc.event, func(t *testing.T) {
			f := newChainRetryFixture(t, true)
			state := "failed"
			if tc.event == protocol.EventTaskCompleted {
				state = "completed"
			}
			ended := f.endedRun(t, state, testutil.Cols{"cancelled_by_type": "system"})
			bus := events.New()
			testHandler.RegisterChainedRunRetry(bus)

			bus.Publish(events.Event{Type: tc.event, WorkspaceID: testWorkspaceID, Payload: map[string]any{"issue_id": f.issueID, "task_id": ended}})

			deadline := time.Now().Add(5 * time.Second)
			for len(f.queued(t)) != tc.want && time.Now().Before(deadline) {
				time.Sleep(25 * time.Millisecond)
			}
			if got := len(f.queued(t)); got != tc.want {
				t.Fatalf("queued runs = %d, want %d", got, tc.want)
			}
		})
	}
}

// An unknown task id is not an error and writes nothing.
func TestChainedRunRetryUnknownTaskIsIgnored(t *testing.T) {
	f := newChainRetryFixture(t, true)

	f.apply(t, uuid.NewString())

	if got := len(f.queued(t)); got != 0 {
		t.Fatalf("queued runs = %d for an unknown task, want 0", got)
	}
}
