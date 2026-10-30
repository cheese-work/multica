package handler

import (
	"testing"
)

// CHE-2599: the legacy pr_checks_failed callback must not wake an issue for a
// merged or closed PR when a late failing snapshot applies. The capture guard
// of CHE-2529 stores no event for that input, so only the callback itself can
// refuse it.

func (f liveDeliveryFixture) legacyChecksFailedReceipts(t *testing.T) int {
	t.Helper()
	return dbfx.Count(t, `SELECT count(*) FROM issue_wakeup_receipt r JOIN issue_wakeup w ON w.id = r.wakeup_id WHERE w.issue_id = $1 AND w.system_rule = 'pr_checks_failed' AND r.event_type = 'pr.checks_failed'`, f.issueID)
}

// legacyChecksFailedTasks counts receipts that started a task, so a receipt for
// an unroutable issue cannot pass for a wake.
func (f liveDeliveryFixture) legacyChecksFailedTasks(t *testing.T) int {
	t.Helper()
	return dbfx.Count(t, `SELECT count(*) FROM issue_wakeup_receipt r JOIN issue_wakeup w ON w.id = r.wakeup_id WHERE w.issue_id = $1 AND w.system_rule = 'pr_checks_failed' AND r.event_type = 'pr.checks_failed' AND r.task_id IS NOT NULL`, f.issueID)
}

// newRoutableFixture makes the issue routable (assigned to the fixture's
// runnable agent) and turns the legacy rule on, so a wake that fires starts a
// task.
func newRoutableFixture(t *testing.T) liveDeliveryFixture {
	t.Helper()
	f := newLiveDeliveryFixture(t, "running", false)
	setIssueAssigneeDirect(t, f.issueID, "agent", f.agentID)
	setWorkspacePRWakeSetting(t, "github_wake_on_ci_failure", true)
	return f
}

// finishTask ends the live run so the scheduler tick below is the only thing
// that could still start work.
func (f liveDeliveryFixture) finishTask(t *testing.T) {
	t.Helper()
	dbfx.Exec(t, `UPDATE agent_task_queue SET status = 'completed', completed_at = now() WHERE id = $1`, f.taskID)
}

func TestLegacyChecksFailedAfterTerminalPRDoesNotWake(t *testing.T) {
	for _, state := range []string{"merged", "closed"} {
		t.Run(state, func(t *testing.T) {
			f := newRoutableFixture(t)
			f.terminate(t, state)
			tasks := f.taskCount(t)
			f.refresh(t, f.checksFailed())
			f.finishTask(t)
			tick(t)
			if got := f.legacyChecksFailedReceipts(t); got != 0 {
				t.Fatalf("late failing snapshot on a %s PR recorded %d legacy receipts, want 0", state, got)
			}
			if got := f.taskCount(t); got != tasks {
				t.Fatalf("tasks %d -> %d after a late failing snapshot on a %s PR, want unchanged", tasks, got, state)
			}
		})
	}
}

// Reopen: a failure observed once the PR is open again wakes as before.
func TestLegacyChecksFailedAfterReopenWakes(t *testing.T) {
	f := newRoutableFixture(t)
	f.terminate(t, "closed")
	f.refresh(t, f.checksFailed())
	if got := f.legacyChecksFailedReceipts(t); got != 0 {
		t.Fatalf("closed PR recorded %d legacy receipts, want 0", got)
	}
	f.finishTask(t)
	dbfx.Exec(t, `UPDATE github_pull_request SET state = 'open' WHERE id = $1`, f.prID)
	f.refresh(t, f.checksFailed())
	tick(t)
	if got := f.legacyChecksFailedTasks(t); got != 1 {
		t.Fatalf("failure after reopen started %d legacy tasks, want 1", got)
	}
}

// Positive control: on an open PR the failure wakes as it does today.
func TestLegacyChecksFailedOnOpenPRWakes(t *testing.T) {
	f := newRoutableFixture(t)
	f.refresh(t, f.checksFailed())
	f.finishTask(t)
	tick(t)
	if got := f.legacyChecksFailedTasks(t); got != 1 {
		t.Fatalf("failure on an open PR started %d legacy tasks, want 1", got)
	}
}
