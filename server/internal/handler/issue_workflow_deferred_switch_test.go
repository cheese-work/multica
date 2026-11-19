package handler

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// Deferred workflow switch core (CHE-7669, slice 4 of CHE-7658): one pending
// row per issue, consumed when the issue's last live task ends.

// deferredSwitchFixture is an issue that follows `from` (no handoffs), sits on
// in_progress, and has a pending switch to `to`, a workflow whose in_progress
// step hands the issue to `handler`.
type deferredSwitchFixture struct {
	issueID, from, to, handler string
}

func newDeferredSwitchFixture(t *testing.T) deferredSwitchFixture {
	t.Helper()
	seedTestCatalog(t)
	resetDefaultWorkflow(t)
	handler := seededReadyAgentID(t)
	from := createTestWorkflow(t, "Switch from "+workflowTestSuffix(), "in_progress")
	to := createTestWorkflow(t, "Switch to "+workflowTestSuffix(), "in_progress")
	setWorkflowHandoff(t, to.ID, "in_progress", handToAgent(handler, "Implement it.")).Want(http.StatusOK)
	issueID := dbfx.Issue(t, "deferred switch", testutil.Cols{"status": "in_progress"})
	setIssueOverride(t, issueID, from.ID)
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM issue_workflow_switch_pending WHERE issue_id = $1`, issueID)
	})
	return deferredSwitchFixture{issueID: issueID, from: from.ID, to: to.ID, handler: handler}
}

// pend records the pending switch the way the later steps (escalate, rollback)
// will: one row per issue.
func (f deferredSwitchFixture) pend(t *testing.T, workflowID string) {
	t.Helper()
	dbfx.Exec(t, `INSERT INTO issue_workflow_switch_pending (issue_id, workspace_id, workflow_id, reason)
		VALUES ($1, $2, $3, 'test')`, f.issueID, testWorkspaceID, workflowID)
}

func (f deferredSwitchFixture) pendingRows(t *testing.T) int {
	t.Helper()
	var n int
	dbfx.QueryRow(t, `SELECT count(*) FROM issue_workflow_switch_pending WHERE issue_id = $1`, f.issueID).Scan(&n)
	return n
}

func (f deferredSwitchFixture) tasksIn(t *testing.T, state string) int {
	t.Helper()
	var n int
	dbfx.QueryRow(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND status = $2`, f.issueID, state).Scan(&n)
	return n
}

func (f deferredSwitchFixture) revision(t *testing.T) int64 {
	t.Helper()
	var n int64
	dbfx.QueryRow(t, `SELECT revision FROM issue WHERE id = $1`, f.issueID).Scan(&n)
	return n
}

func (f deferredSwitchFixture) apply(t *testing.T) {
	t.Helper()
	testHandler.ApplyDeferredWorkflowSwitch(context.Background(), util.MustParseUUID(f.issueID))
}

func (f deferredSwitchFixture) wantFollows(t *testing.T, workflowID string) {
	t.Helper()
	if got := issueOverride(t, f.issueID); got != workflowID {
		t.Fatalf("issue follows workflow %q, want %q", got, workflowID)
	}
}

func setTaskState(t *testing.T, taskID, state string) {
	t.Helper()
	dbfx.Exec(t, `UPDATE agent_task_queue SET status = $2 WHERE id = $1`, taskID, state)
}

// The last live task ends: the switch applies, the issue enters the full
// workflow's step and its handler gets exactly one run.
func TestDeferredSwitchAppliesOneRunAfterTheLastLiveTaskEnds(t *testing.T) {
	f := newDeferredSwitchFixture(t)
	running := dbfx.Task(t, f.handler, testutil.Cols{
		"issue_id": f.issueID, "status": "running",
		"runtime_id": testutil.Raw("(SELECT runtime_id FROM agent WHERE id='" + f.handler + "')"),
	})
	f.pend(t, f.to)

	f.apply(t)
	f.wantFollows(t, f.from)
	if f.pendingRows(t) != 1 || f.tasksIn(t, "queued") != 0 {
		t.Fatalf("a live task must keep the row and start nothing: rows=%d queued=%d", f.pendingRows(t), f.tasksIn(t, "queued"))
	}

	setTaskState(t, running, "completed")
	f.apply(t)

	f.wantFollows(t, f.to)
	if f.pendingRows(t) != 0 {
		t.Fatal("the pending row survived its apply")
	}
	wantIssueStatus(t, f.issueID, "in_progress")
	if _, at, aid := issueStatusAndAssignee(t, f.issueID); at != "agent" || aid != f.handler {
		t.Fatalf("assignee = %s/%s, want the full workflow's handler %s", at, aid, f.handler)
	}
	if n := f.tasksIn(t, "queued"); n != 1 {
		t.Fatalf("queued runs = %d, want exactly one for the full workflow's step", n)
	}
}

// A queued task still lives between two terminal events: the row waits, and
// only the event that ends the last live task applies it, once.
func TestDeferredSwitchTwoTerminalEventsAroundALiveQueuedTaskApplyOnce(t *testing.T) {
	f := newDeferredSwitchFixture(t)
	runtime := testutil.Raw("(SELECT runtime_id FROM agent WHERE id='" + f.handler + "')")
	first := dbfx.Task(t, f.handler, testutil.Cols{"issue_id": f.issueID, "status": "running", "runtime_id": runtime})
	second := dbfx.Task(t, f.handler, testutil.Cols{"issue_id": f.issueID, "status": "queued", "runtime_id": runtime})
	f.pend(t, f.to)

	setTaskState(t, first, "completed")
	f.apply(t)
	f.wantFollows(t, f.from)
	if f.pendingRows(t) != 1 {
		t.Fatal("the first terminal event applied the switch while a queued task lived")
	}

	setTaskState(t, second, "cancelled")
	f.apply(t)
	f.wantFollows(t, f.to)
	if n := f.tasksIn(t, "queued"); n != 1 {
		t.Fatalf("queued runs = %d, want the one run of the applied switch", n)
	}

	// A late duplicate of the first event finds nothing to apply.
	f.apply(t)
	if n := f.tasksIn(t, "queued"); n != 1 {
		t.Fatalf("queued runs = %d after a duplicate event, want still one", n)
	}
}

// A completion event that arrives twice applies nothing the second time, and
// an issue with no pending row is not written at all.
func TestDeferredSwitchDuplicateCompletionAppliesNothing(t *testing.T) {
	f := newDeferredSwitchFixture(t)
	f.pend(t, f.to)
	f.apply(t)
	f.wantFollows(t, f.to)
	revision, queued := f.revision(t), f.tasksIn(t, "queued")

	f.apply(t)

	if got := f.revision(t); got != revision {
		t.Fatalf("revision %d -> %d: a duplicate event wrote the issue", revision, got)
	}
	if got := f.tasksIn(t, "queued"); got != queued {
		t.Fatalf("queued runs %d -> %d after a duplicate event", queued, got)
	}
	f.wantFollows(t, f.to)
}

// An apply the target refuses blocks the issue with the reason and drops the
// row, so the same refusal does not repeat on every later event.
func TestDeferredSwitchRefusedApplyBlocksWithTheReason(t *testing.T) {
	f := newDeferredSwitchFixture(t)
	missing := uuid.NewString()
	f.pend(t, missing)

	f.apply(t)

	wantIssueStatus(t, f.issueID, "blocked")
	f.wantFollows(t, f.from)
	if f.pendingRows(t) != 0 {
		t.Fatal("a refused apply left its row to refuse again")
	}
	if n := f.tasksIn(t, "queued"); n != 0 {
		t.Fatalf("queued runs = %d, a refused switch starts none", n)
	}
	var reason string
	dbfx.QueryRow(t, `SELECT COALESCE(metadata->>'blocked_reason', '') FROM issue WHERE id = $1`, f.issueID).Scan(&reason)
	if !strings.HasPrefix(reason, "workflow-switch-refused") || !strings.Contains(reason, missing) {
		t.Fatalf("blocked_reason = %q, want workflow-switch-refused naming the target %s", reason, missing)
	}
	var comments int
	dbfx.QueryRow(t, `SELECT count(*) FROM comment WHERE issue_id = $1 AND author_type = 'system' AND content LIKE '%workflow-switch-refused%'`, f.issueID).Scan(&comments)
	if comments != 1 {
		t.Fatalf("system comments naming the refusal = %d, want 1", comments)
	}
}

func (f deferredSwitchFixture) refusalComments(t *testing.T) int {
	t.Helper()
	var n int
	dbfx.QueryRow(t, `SELECT count(*) FROM comment WHERE issue_id = $1 AND author_type = 'system' AND content LIKE '%workflow-switch-refused%'`, f.issueID).Scan(&n)
	return n
}

func (f deferredSwitchFixture) blockedReason(t *testing.T) string {
	t.Helper()
	var reason string
	dbfx.QueryRow(t, `SELECT COALESCE(metadata->>'blocked_reason', '') FROM issue WHERE id = $1`, f.issueID).Scan(&reason)
	return reason
}

// A refusal whose block cannot be written keeps the request: the row, the
// issue and its comments stay as they were, and the next event refuses again.
// Once the block can be written, one event settles all three together.
func TestDeferredSwitchFailedBlockKeepsTheRowUntilTheBlockIsDurable(t *testing.T) {
	f := newDeferredSwitchFixture(t)
	f.pend(t, uuid.NewString())
	suffix := strings.ReplaceAll(f.issueID, "-", "")
	dbfx.Exec(t, `CREATE FUNCTION deferred_switch_no_block_`+suffix+`() RETURNS trigger LANGUAGE plpgsql AS
		$$ BEGIN IF NEW.id = '`+f.issueID+`' AND NEW.status = 'blocked' THEN RAISE EXCEPTION 'injected block failure'; END IF; RETURN NEW; END $$`)
	dbfx.Exec(t, `CREATE TRIGGER deferred_switch_no_block_`+suffix+` BEFORE UPDATE ON issue
		FOR EACH ROW EXECUTE FUNCTION deferred_switch_no_block_`+suffix+`()`)
	dropFailure := func() {
		testPool.Exec(context.Background(), `DROP TRIGGER IF EXISTS deferred_switch_no_block_`+suffix+` ON issue`)
		testPool.Exec(context.Background(), `DROP FUNCTION IF EXISTS deferred_switch_no_block_`+suffix+`()`)
	}
	t.Cleanup(dropFailure)

	f.apply(t)

	wantIssueStatus(t, f.issueID, "in_progress")
	if f.pendingRows(t) != 1 {
		t.Fatal("a refusal whose block failed dropped the row: the request is lost")
	}
	if n := f.refusalComments(t); n != 0 {
		t.Fatalf("refusal comments = %d with the block rolled back, want 0", n)
	}
	if reason := f.blockedReason(t); reason != "" {
		t.Fatalf("blocked_reason = %q with the block rolled back", reason)
	}

	dropFailure()
	f.apply(t)

	wantIssueStatus(t, f.issueID, "blocked")
	if f.pendingRows(t) != 0 || f.refusalComments(t) != 1 || f.blockedReason(t) == "" {
		t.Fatalf("retry must settle together: rows=%d comments=%d reason=%q", f.pendingRows(t), f.refusalComments(t), f.blockedReason(t))
	}
}

// A workflow with no blocked step cannot block the issue: the refusal is still
// recorded once, in a comment and in blocked_reason, so nothing repeats.
func TestDeferredSwitchRefusalOnAWorkflowWithoutBlockedStillRecordsTheReason(t *testing.T) {
	f := newDeferredSwitchFixture(t)
	noBlocked := createTestWorkflowSteps(t, "No blocked "+workflowTestSuffix(), statusSteps("backlog", "in_progress"))
	setIssueOverride(t, f.issueID, noBlocked.ID)
	missing := uuid.NewString()
	f.pend(t, missing)

	f.apply(t)

	wantIssueStatus(t, f.issueID, "in_progress")
	f.wantFollows(t, noBlocked.ID)
	if f.pendingRows(t) != 0 {
		t.Fatal("a refusal that cannot block left its row to refuse again")
	}
	if n := f.refusalComments(t); n != 1 {
		t.Fatalf("refusal comments = %d, want 1", n)
	}
	if reason := f.blockedReason(t); !strings.HasPrefix(reason, "workflow-switch-refused") || !strings.Contains(reason, missing) {
		t.Fatalf("blocked_reason = %q, want workflow-switch-refused naming %s", reason, missing)
	}
}

// The listener turns each terminal task event into an apply attempt.
func TestDeferredSwitchListenerAppliesOnTaskTerminalEvents(t *testing.T) {
	for _, eventType := range []string{protocol.EventTaskCompleted, protocol.EventTaskFailed, protocol.EventTaskCancelled} {
		t.Run(eventType, func(t *testing.T) {
			f := newDeferredSwitchFixture(t)
			f.pend(t, f.to)
			bus := events.New()
			testHandler.RegisterDeferredWorkflowSwitch(bus)

			bus.Publish(events.Event{Type: eventType, WorkspaceID: testWorkspaceID, Payload: map[string]any{"issue_id": f.issueID, "task_id": uuid.NewString()}})

			deadline := time.Now().Add(5 * time.Second)
			for f.pendingRows(t) != 0 && time.Now().Before(deadline) {
				time.Sleep(25 * time.Millisecond)
			}
			f.wantFollows(t, f.to)
		})
	}
}
