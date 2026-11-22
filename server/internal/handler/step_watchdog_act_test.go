package handler

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// setWatchdogMode sets workspace setting step_watchdog.mode for the test.
func setWatchdogMode(t *testing.T, mode string) {
	t.Helper()
	var old string
	dbfx.QueryRow(t, `SELECT settings::text FROM workspace WHERE id = $1`, testWorkspaceID).Scan(&old)
	dbfx.Exec(t, `UPDATE workspace SET settings = settings || jsonb_build_object('step_watchdog', jsonb_build_object('mode', $2::text)) WHERE id = $1`, testWorkspaceID, mode)
	dbfx.Cleanup(t, `UPDATE workspace SET settings = $2::jsonb WHERE id = $1`, testWorkspaceID, old)
}

// staleNoHistoryIssue is an approved issue created `age` ago that never had a
// status_changed row, with 36 ended runs in 7 days.
func (f watchdogFixture) staleNoHistoryIssue(t *testing.T, age time.Duration) string {
	t.Helper()
	issueID := dbfx.Issue(t, "no status history", testutil.Cols{
		"assignee_type": "agent", "assignee_id": f.agentID, "status": "approved", "project_id": f.projectID,
		"created_at": time.Now().Add(-age),
	})
	for i := 0; i < 36; i++ {
		dbfx.Task(t, f.agentID, testutil.Cols{
			"issue_id": issueID, "status": "completed", "runtime_id": handlerTestRuntimeID(t),
			"created_at": time.Now().Add(-time.Duration(i+1) * time.Hour),
		})
	}
	return issueID
}

func ledgerCount(t *testing.T, issueID, action string) int {
	t.Helper()
	return dbfx.Count(t, `SELECT count(*) FROM issue_step_watchdog_action WHERE issue_id = $1 AND action = $2`, issueID, action)
}

func sweepConcurrently(t *testing.T, n int) {
	t.Helper()
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := testHandler.sweepStepWatchdog(context.Background(), 1000); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
}

func TestStepWatchdogOnBlocksBudgetRefusedOnce(t *testing.T) {
	f := newWatchdogFixture(t)
	setWatchdogMode(t, "on")

	t.Run("weekly run guard, duplicate sweeps", func(t *testing.T) {
		t.Setenv("MULTICA_WEEKLY_RUN_GUARD_MAX_RUNS", "25")
		issueID := f.stalledIssue(t, 25*time.Hour, 36)
		for i := 0; i < 2; i++ {
			if err := testHandler.sweepStepWatchdog(context.Background(), 1000); err != nil {
				t.Fatal(err)
			}
		}
		var status, reason string
		dbfx.QueryRow(t, `SELECT status, coalesce(metadata->>'blocked_reason', '') FROM issue WHERE id = $1`, issueID).Scan(&status, &reason)
		if status != "blocked" || reason != "weekly-run-guard" {
			t.Errorf("status = %q, blocked_reason = %q, want blocked and weekly-run-guard", status, reason)
		}
		if n := dbfx.Count(t, `SELECT count(*) FROM comment WHERE issue_id = $1 AND author_type = 'system' AND content LIKE '%weekly run guard%'`, issueID); n != 1 {
			t.Errorf("%d notices, want 1", n)
		}
		if n := dbfx.Count(t, `SELECT count(*) FROM comment WHERE issue_id = $1 AND author_type = 'system' AND content LIKE '%36 runs in the last 7 days%'`, issueID); n != 1 {
			t.Errorf("%d notices name the run count and window, want 1", n)
		}
		if n := ledgerCount(t, issueID, "block_budget"); n != 1 {
			t.Errorf("%d block_budget rows, want 1", n)
		}
		if n := dbfx.Count(t, `SELECT count(*) FROM issue i JOIN issue_status s ON s.workspace_id = i.workspace_id AND s.key = i.status WHERE i.id = $1 AND s.category IN ('done', 'closed')`, issueID); n != 0 {
			t.Error("the watchdog finished the issue")
		}
	})

	t.Run("run budget", func(t *testing.T) {
		issueID := f.stalledIssue(t, 25*time.Hour, 36, testutil.Cols{"workflow_id": f.workflowID})
		dbfx.Exec(t, `UPDATE issue_workflow SET max_runs_per_issue = 25, run_budget_since = now() - interval '7 days' WHERE id = $1`, f.workflowID)
		dbfx.Cleanup(t, `DELETE FROM issue_run_budget_entry WHERE issue_id = $1`, issueID)
		if err := testHandler.sweepStepWatchdog(context.Background(), 1000); err != nil {
			t.Fatal(err)
		}
		var status, reason string
		dbfx.QueryRow(t, `SELECT status, coalesce(metadata->>'blocked_reason', '') FROM issue WHERE id = $1`, issueID).Scan(&status, &reason)
		if status != "blocked" || reason != "run-budget" {
			t.Errorf("status = %q, blocked_reason = %q, want blocked and run-budget", status, reason)
		}
	})

	t.Run("open admission is not blocked", func(t *testing.T) {
		issueID := f.stalledIssue(t, 25*time.Hour, 0)
		dbfx.Cleanup(t, `DELETE FROM agent_task_queue WHERE issue_id = $1`, issueID)
		dbfx.Cleanup(t, `DELETE FROM issue_handoff_dispatch WHERE issue_id = $1`, issueID)
		if err := testHandler.sweepStepWatchdog(context.Background(), 1000); err != nil {
			t.Fatal(err)
		}
		var status string
		dbfx.QueryRow(t, `SELECT status FROM issue WHERE id = $1`, issueID).Scan(&status)
		if status != "approved" || ledgerCount(t, issueID, "block_budget") != 0 {
			t.Errorf("status = %q, want approved with no block_budget row", status)
		}
	})
}

func TestStepWatchdogConcurrentSweepsActOncePerOccurrence(t *testing.T) {
	f := newWatchdogFixture(t)
	setWatchdogMode(t, "on")
	t.Setenv("MULTICA_WEEKLY_RUN_GUARD_MAX_RUNS", "25")

	for name, seed := range map[string]func() string{
		"status history":    func() string { return f.stalledIssue(t, 25*time.Hour, 36) },
		"no status history": func() string { return f.staleNoHistoryIssue(t, 25*time.Hour) },
	} {
		t.Run(name, func(t *testing.T) {
			issueID := seed()
			sweepConcurrently(t, 4)
			if n := ledgerCount(t, issueID, "block_budget"); n != 1 {
				t.Errorf("%d block_budget rows, want 1", n)
			}
			if n := dbfx.Count(t, `SELECT count(*) FROM comment WHERE issue_id = $1 AND author_type = 'system'`, issueID); n != 1 {
				t.Errorf("%d system comments, want 1", n)
			}
			var status string
			dbfx.QueryRow(t, `SELECT status FROM issue WHERE id = $1`, issueID).Scan(&status)
			if status != "blocked" {
				t.Errorf("status = %q, want blocked", status)
			}
		})
	}

	t.Run("no status history keys on created:<issue id> in shadow", func(t *testing.T) {
		setWatchdogMode(t, "shadow")
		issueID := f.staleNoHistoryIssue(t, 25*time.Hour)
		sweepConcurrently(t, 4)
		var key string
		dbfx.QueryRow(t, `SELECT occurrence_key FROM issue_step_watchdog_action WHERE issue_id = $1 AND action = 'would_act'`, issueID).Scan(&key)
		if want := "created:" + issueID; key != want {
			t.Errorf("occurrence_key = %q, want %q", key, want)
		}
		if n := ledgerCount(t, issueID, "would_act"); n != 1 {
			t.Errorf("%d would_act rows, want 1", n)
		}
	})
}

func TestStepWatchdogShadowRecordsOnlyWouldAct(t *testing.T) {
	f := newWatchdogFixture(t)
	t.Setenv("MULTICA_WEEKLY_RUN_GUARD_MAX_RUNS", "25")

	for _, mode := range []string{"shadow", ""} { // unset is shadow
		t.Run("mode "+mode, func(t *testing.T) {
			if mode != "" {
				setWatchdogMode(t, mode)
			}
			issueID := f.stalledIssue(t, 25*time.Hour, 36)
			for i := 0; i < 2; i++ {
				if err := testHandler.sweepStepWatchdog(context.Background(), 1000); err != nil {
					t.Fatal(err)
				}
			}
			var status string
			dbfx.QueryRow(t, `SELECT status FROM issue WHERE id = $1`, issueID).Scan(&status)
			if status != "approved" {
				t.Errorf("status = %q, shadow must not move the issue", status)
			}
			if n := ledgerCount(t, issueID, "would_act"); n != 1 {
				t.Errorf("%d would_act rows, want 1", n)
			}
			if n := dbfx.Count(t, `SELECT count(*) FROM issue_step_watchdog_action WHERE issue_id = $1 AND action <> 'would_act'`, issueID); n != 0 {
				t.Errorf("%d other ledger rows, want 0", n)
			}
			if n := dbfx.Count(t, `SELECT count(*) FROM activity_log WHERE issue_id = $1 AND action = 'step_watchdog_would_act' AND details->>'case' = 'block_budget'`, issueID); n != 1 {
				t.Errorf("%d would-act activity rows, want 1", n)
			}
			if n := dbfx.Count(t, `SELECT count(*) FROM comment WHERE issue_id = $1`, issueID); n != 0 {
				t.Errorf("%d comments, want 0", n)
			}
		})
	}

	t.Run("mode off does nothing", func(t *testing.T) {
		setWatchdogMode(t, "off")
		issueID := f.stalledIssue(t, 25*time.Hour, 36)
		if err := testHandler.sweepStepWatchdog(context.Background(), 1000); err != nil {
			t.Fatal(err)
		}
		if n := dbfx.Count(t, `SELECT count(*) FROM issue_step_watchdog_action WHERE issue_id = $1`, issueID); n != 0 {
			t.Errorf("%d ledger rows, want 0", n)
		}
	})
}

// A sweep is bounded, so what it already handled must not fill the next batch.
func TestStepWatchdogSweepProgressesPastHandledCandidates(t *testing.T) {
	f := newWatchdogFixture(t)
	t.Setenv("MULTICA_WEEKLY_RUN_GUARD_MAX_RUNS", "25")
	const limit = 2

	t.Run("claimed shadow rows", func(t *testing.T) {
		setWatchdogMode(t, "shadow")
		var ids []string
		for _, age := range []time.Duration{30, 29, 28} {
			ids = append(ids, f.stalledIssue(t, age*time.Hour, 36))
		}
		for i := 0; i < 2; i++ {
			if err := testHandler.sweepStepWatchdog(context.Background(), limit); err != nil {
				t.Fatal(err)
			}
		}
		for _, id := range ids {
			if n := ledgerCount(t, id, "would_act"); n != 1 {
				t.Errorf("issue %s: %d would_act rows after two bounded sweeps, want 1", id, n)
			}
		}
	})

	t.Run("steps the mode does not act on yet", func(t *testing.T) {
		setWatchdogMode(t, "on")
		paused := createHandlerTestAgent(t, "paused-"+uuid.NewString()[:8], []byte("[]"))
		dbfx.Exec(t, `UPDATE agent SET scheduling_paused_at = now() WHERE id = $1`, paused)
		for _, age := range []time.Duration{30, 29, 28} {
			f.stalledIssue(t, age*time.Hour, 0, testutil.Cols{"assignee_id": paused}) // notify_only: a later slice's case
		}
		budget := f.stalledIssue(t, 25*time.Hour, 36)
		if err := testHandler.sweepStepWatchdog(context.Background(), limit); err != nil {
			t.Fatal(err)
		}
		var status string
		dbfx.QueryRow(t, `SELECT status FROM issue WHERE id = $1`, budget).Scan(&status)
		if status != "blocked" {
			t.Errorf("status = %q, want blocked: older issues the watchdog does not act on hid it", status)
		}
	})
}
