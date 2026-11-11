package service

import "testing"

// An agent's own change to its PR never wakes it (CHE-5184): a head it pushed
// or a PR it closed during or just after one of its runs on the issue.

// ownRun records a finished run of the woken agent on the issue, from
// startAgo to endAgo before now (a negative endAgo is not allowed; use "" for
// a run that is still running).
func (e coalesceEnv) ownRun(t *testing.T, agent, startedAgo, endedAgo string) {
	t.Helper()
	completed := "NULL"
	status := "running"
	if endedAgo != "" {
		completed = "now() - interval '" + endedAgo + "'"
		status = "completed"
	}
	var id string
	e.f.QueryRow(t, `INSERT INTO agent_task_queue (agent_id, runtime_id, issue_id, status, priority, started_at, completed_at)
		VALUES ($1, (SELECT runtime_id FROM agent WHERE id = $1), $2, '`+status+`', 0, now() - interval '`+startedAgo+`', `+completed+`) RETURNING id`,
		agent, e.issue).Scan(&id)
	e.f.Cleanup(t, `DELETE FROM agent_task_queue WHERE id=$1`, id)
}

func (e coalesceEnv) otherAgent(t *testing.T) string {
	t.Helper()
	var id string
	e.f.QueryRow(t, `INSERT INTO agent (workspace_id, name, runtime_mode, runtime_id, owner_id)
		SELECT workspace_id, 'other-author', runtime_mode, runtime_id, owner_id FROM agent WHERE id=$1 RETURNING id`, e.agent).Scan(&id)
	e.f.Cleanup(t, `DELETE FROM agent WHERE id=$1`, id)
	return id
}

func TestOwnPushDuringRunDoesNotWake(t *testing.T) {
	e := newCoalesceEnv(t)
	e.ownRun(t, e.agent, "10 minutes", "")
	event := e.captured(t, "head_changed")
	e.sweep(t)
	if got := e.runs(t); got != 0 {
		t.Fatalf("own push started %d runs, want 0", got)
	}
	if got := e.status(t, event); got != "superseded" {
		t.Fatalf("own push event is %q, want superseded", got)
	}
}

func TestOwnPushJustAfterRunEndedDoesNotWake(t *testing.T) {
	e := newCoalesceEnv(t)
	e.ownRun(t, e.agent, "10 minutes", "30 seconds")
	e.captured(t, "head_changed")
	e.sweep(t)
	if got := e.runs(t); got != 0 {
		t.Fatalf("own push after the run ended started %d runs, want 0", got)
	}
}

func TestOwnCloseDoesNotWake(t *testing.T) {
	e := newCoalesceEnv(t)
	e.ownRun(t, e.agent, "10 minutes", "20 seconds")
	e.f.Exec(t, `UPDATE github_pull_request SET state='open' WHERE id=$1`, e.pr)
	e.captured(t, "closed")
	e.sweep(t)
	if got := e.runs(t); got != 0 {
		t.Fatalf("own close started %d runs, want 0", got)
	}
}

func TestPushWithNoRecentRunStillWakes(t *testing.T) {
	e := newCoalesceEnv(t)
	e.ownRun(t, e.agent, "3 hours", "2 hours")
	e.captured(t, "head_changed")
	e.sweep(t)
	if got := e.runs(t); got != 1 {
		t.Fatalf("push by someone else started %d runs, want 1", got)
	}
}

func TestPushDuringRunOfAnotherAgentStillWakes(t *testing.T) {
	e := newCoalesceEnv(t)
	e.ownRun(t, e.otherAgent(t), "10 minutes", "")
	e.captured(t, "head_changed")
	e.sweep(t)
	if got := e.runs(t); got != 1 {
		t.Fatalf("push during another agent's run started %d runs, want 1", got)
	}
}

func TestChecksFailedDuringOwnRunStillWakes(t *testing.T) {
	e := newCoalesceEnv(t)
	e.ownRun(t, e.agent, "10 minutes", "")
	e.captured(t, "checks_failed")
	e.sweep(t)
	if got := e.runs(t); got != 1 {
		t.Fatalf("checks_failed started %d runs, want 1", got)
	}
}
