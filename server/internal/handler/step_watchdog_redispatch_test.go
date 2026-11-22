package handler

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// redispatchIssue is a stalled approved issue whose requested_by is a
// workspace member, so its notice can be delivered.
func (f watchdogFixture) redispatchIssue(t *testing.T) string {
	t.Helper()
	issueID := f.stalledIssue(t, 25*time.Hour, 0)
	dbfx.Exec(t, `UPDATE issue SET requested_by_id = $2 WHERE id = $1`, issueID, testUserID)
	dbfx.Cleanup(t, `DELETE FROM issue_step_watchdog_action WHERE issue_id = $1`, issueID)
	dbfx.Cleanup(t, `DELETE FROM agent_task_queue WHERE issue_id = $1`, issueID)
	dbfx.Cleanup(t, `DELETE FROM issue_handoff_dispatch WHERE issue_id = $1`, issueID)
	return issueID
}

func sweepOnce(t *testing.T) {
	t.Helper()
	if err := testHandler.sweepStepWatchdog(context.Background(), 1000); err != nil {
		t.Fatal(err)
	}
}

// wantOneRedispatchAndNotice asserts the whole S4 outcome for one issue: one
// ledger row, one intent tagged with it, one run linked to the intent, one
// notice. It returns the run id.
func wantOneRedispatchAndNotice(t *testing.T, issueID string) string {
	t.Helper()
	if n := ledgerCount(t, issueID, "redispatch"); n != 1 {
		t.Errorf("redispatch ledger rows = %d, want 1", n)
	}
	var ledgerID string
	dbfx.QueryRow(t, `SELECT id::text FROM issue_step_watchdog_action WHERE issue_id = $1 AND action = 'redispatch'`, issueID).Scan(&ledgerID)
	if n := dbfx.Count(t, `SELECT count(*) FROM issue_handoff_dispatch WHERE issue_id = $1 AND handoff_write = $2 AND state = 'dispatched' AND task_id IS NOT NULL`, issueID, "watchdog:"+ledgerID); n != 1 {
		t.Errorf("dispatched intents tagged watchdog:%s = %d, want 1", ledgerID, n)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM issue_handoff_dispatch WHERE issue_id = $1`, issueID); n != 1 {
		t.Errorf("intents = %d, want 1", n)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1`, issueID); n != 1 {
		t.Errorf("runs = %d, want 1", n)
	}
	var taskID string
	dbfx.QueryRow(t, `SELECT id::text FROM agent_task_queue WHERE issue_id = $1`, issueID).Scan(&taskID)
	if n := noticeRows(t, issueID, "delivered"); n != 1 {
		t.Errorf("delivered notices = %d, want 1", n)
	}
	if n := watchdogInbox(t, testUserID, issueID); n != 1 {
		t.Errorf("inbox items = %d, want 1", n)
	}
	var content string
	dbfx.QueryRow(t, `SELECT content FROM comment WHERE issue_id = $1 AND author_type = 'system' AND content LIKE '%Step watchdog%' LIMIT 1`, issueID).Scan(&content)
	if !strings.Contains(content, "redispatch") {
		t.Errorf("notice does not name the re-dispatch:\n%s", content)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM comment WHERE issue_id = $1 AND content LIKE '%Step watchdog%'`, issueID); n != 1 {
		t.Errorf("notice comments = %d, want 1", n)
	}
	return taskID
}

// Outcome 1 (CHE-6116 S4): the issue stays in its step after the re-dispatch.
func TestStepWatchdogRedispatchIssueStaysInStep(t *testing.T) {
	f := newWatchdogFixture(t)
	setWatchdogMode(t, "on")
	issueID := f.redispatchIssue(t)

	sweepOnce(t)

	taskID := wantOneRedispatchAndNotice(t, issueID)
	var status string
	dbfx.QueryRow(t, `SELECT status FROM issue WHERE id = $1`, issueID).Scan(&status)
	if status != "approved" {
		t.Errorf("status = %q, want approved: the watchdog only moves an issue to blocked", status)
	}

	// The run ends and the issue is still in the step: a second sweep sends
	// no second dispatch and no second notice.
	dbfx.Exec(t, `UPDATE agent_task_queue SET status = 'completed' WHERE id = $1`, taskID)
	sweepOnce(t)
	wantOneRedispatchAndNotice(t, issueID)
}

// Outcome 2 (CHE-6116 S4): the run moves the issue at once, before the notice
// goes out. The notice is still owed and still delivered, once.
func TestStepWatchdogRedispatchRunMovesIssueAtOnce(t *testing.T) {
	f := newWatchdogFixture(t)
	setWatchdogMode(t, "on")
	issueID := f.redispatchIssue(t)

	testHandler.handoffDispatchHook = func(point string) error {
		if point != "after_commit" {
			return nil
		}
		dbfx.Exec(t, `UPDATE issue SET status = 'in_review' WHERE id = $1`, issueID)
		dbfx.Insert(t, "activity_log", testutil.Cols{
			"workspace_id": testWorkspaceID, "issue_id": issueID, "actor_type": "system", "action": "status_changed",
			"details": `{"from":"approved","to":"in_review"}`,
		})
		dbfx.Exec(t, `UPDATE agent_task_queue SET status = 'completed' WHERE issue_id = $1`, issueID)
		return nil
	}
	t.Cleanup(func() { testHandler.handoffDispatchHook = nil })

	sweepOnce(t)
	testHandler.handoffDispatchHook = nil

	wantOneRedispatchAndNotice(t, issueID)
	var status string
	dbfx.QueryRow(t, `SELECT status FROM issue WHERE id = $1`, issueID).Scan(&status)
	if status != "in_review" {
		t.Errorf("status = %q, want in_review (the run moved it)", status)
	}

	sweepOnce(t)
	wantOneRedispatchAndNotice(t, issueID)
}

// A re-dispatch the sweep claimed but did not finish (a crash after the ledger
// row) is not dispatched twice, and its notice still goes out once.
func TestStepWatchdogRedispatchTwoConcurrentSweepsMakeOneRun(t *testing.T) {
	f := newWatchdogFixture(t)
	setWatchdogMode(t, "on")
	issueID := f.redispatchIssue(t)

	sweepConcurrently(t, 4)

	wantOneRedispatchAndNotice(t, issueID)
}

// Shadow mode records what it would do and starts no run.
func TestStepWatchdogShadowDoesNotRedispatch(t *testing.T) {
	f := newWatchdogFixture(t)
	setWatchdogMode(t, "shadow")
	issueID := f.redispatchIssue(t)

	sweepOnce(t)

	if n := dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1`, issueID); n != 0 {
		t.Errorf("runs = %d, want 0 in shadow mode", n)
	}
	if n := ledgerCount(t, issueID, "redispatch"); n != 0 {
		t.Errorf("redispatch ledger rows = %d, want 0 in shadow mode", n)
	}
}
