package service

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// CHE-6193: a wakeup a run guard refuses is disabled once, with one timeline
// line and a reason naming the guard, instead of failing on every pass.

// guardBlockEnv is an issue with one run, a derived time wakeup due now, and a
// way to tick the scheduler.
type guardBlockEnv struct {
	f      principalFixture
	s      *IssueWakeupService
	issue  pgtype.UUID
	agent  string
	wakeup db.IssueWakeup
}

func newGuardBlockEnv(t *testing.T) guardBlockEnv {
	t.Helper()
	f, s, issue, agent := conditionFixture(t)
	run := f.Task(t, agent, testutil.Cols{"issue_id": issue, "status": "completed",
		"runtime_id": testutil.Raw("(SELECT runtime_id FROM agent WHERE id='" + agent + "')")})
	// An agent-created wakeup is derived, so both guards judge it.
	w, err := s.Create(context.Background(), issue, parseTestUUID(t, f.UserID), parseTestUUID(t, run),
		WakeupInput{AgentID: agent, Kind: "every", IntervalSeconds: 3600, Instruction: "check again"})
	if err != nil {
		t.Fatal(err)
	}
	return guardBlockEnv{f: f, s: s, issue: issue, agent: agent, wakeup: w}
}

func (e guardBlockEnv) tick(t *testing.T) db.IssueWakeup {
	t.Helper()
	e.f.Exec(t, "UPDATE issue_wakeup SET next_fire_at=now()-interval '1 second' WHERE id=$1 AND enabled", e.wakeup.ID)
	if err := e.s.TickWorkspaces(context.Background(), parseTestUUID(t, e.f.WorkspaceID)); err != nil {
		t.Logf("tick: %v", err)
	}
	w, err := e.f.q.LocklessWakeup(context.Background(), e.wakeup.ID)
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func (e guardBlockEnv) lines(t *testing.T, reason string) int {
	t.Helper()
	return e.f.Count(t, `SELECT count(*) FROM activity_log WHERE issue_id=$1 AND action='wakeup_paused' AND details->>'reason'=$2`, e.issue, reason)
}

func (e guardBlockEnv) runs(t *testing.T) int {
	t.Helper()
	return wakeRuns(t, e.f, e.wakeup.ID)
}

// budget gives the issue a workflow whose budget the one run already spent.
func (e guardBlockEnv) budget(t *testing.T) db.Issue {
	t.Helper()
	wf := uuid.NewString()
	e.f.Exec(t, `INSERT INTO issue_workflow (id, workspace_id, name, initial_status_key, steps, max_runs_per_issue, run_budget_since)
		VALUES ($1, $2, 'guard block wf', 'todo', '[{"status_key":"todo","handler":{"type":"none"}}]'::jsonb, 1, '2026-01-01T00:00:00Z')`, wf, e.f.WorkspaceID)
	e.f.Cleanup(t, `DELETE FROM issue_workflow WHERE id=$1`, wf)
	e.f.Exec(t, `UPDATE issue SET workflow_id=$2 WHERE id=$1`, e.issue, wf)
	issue, err := e.f.q.GetIssue(context.Background(), e.issue)
	if err != nil {
		t.Fatal(err)
	}
	return issue
}

func (e guardBlockEnv) raise(t *testing.T, issue db.Issue, delta int) {
	t.Helper()
	ctx := context.Background()
	if _, err := OpenRunBudgetWindow(ctx, e.f.q, e.issue); err != nil {
		t.Fatal(err)
	}
	if err := RecordRunBudgetRaise(ctx, e.f.q, issue, parseTestUUID(t, uuid.NewString()), delta); err != nil {
		t.Fatal(err)
	}
}

func TestBudgetBlockedWakeupIsDisabledOnceAndARaiseReenablesIt(t *testing.T) {
	e := newGuardBlockEnv(t)
	issue := e.budget(t)

	w := e.tick(t)
	if w.Enabled || w.PausedReason.String != "run_budget" {
		t.Fatalf("after the refusal: enabled=%v reason=%q, want disabled with run_budget", w.Enabled, w.PausedReason.String)
	}
	e.tick(t)
	e.tick(t)
	if n := e.lines(t, "run_budget"); n != 1 {
		t.Fatalf("timeline lines = %d, want 1", n)
	}
	if n := e.runs(t); n != 0 {
		t.Fatalf("runs started = %d, want 0", n)
	}

	e.raise(t, issue, 5)
	w, _ = e.f.q.LocklessWakeup(context.Background(), e.wakeup.ID)
	if !w.Enabled || w.PausedReason.Valid {
		t.Fatalf("after a raise: enabled=%v reason=%v, want re-enabled", w.Enabled, w.PausedReason)
	}
}

func TestWeeklyGuardBlockedWakeupIgnoresARaiseAndReenablesBelowTheLimit(t *testing.T) {
	t.Setenv("MULTICA_WEEKLY_RUN_GUARD_MAX_RUNS", "1")
	e := newGuardBlockEnv(t)
	issue := e.budget(t)
	e.f.Exec(t, `UPDATE issue_workflow SET max_runs_per_issue=100 WHERE id=$1`, issue.WorkflowID)

	w := e.tick(t)
	if w.Enabled || w.PausedReason.String != "weekly_run_guard" {
		t.Fatalf("after the refusal: enabled=%v reason=%q, want disabled with weekly_run_guard", w.Enabled, w.PausedReason.String)
	}
	e.tick(t)
	if n := e.lines(t, "weekly_run_guard"); n != 1 {
		t.Fatalf("timeline lines = %d, want 1", n)
	}

	e.raise(t, issue, 5)
	if w, _ = e.f.q.LocklessWakeup(context.Background(), e.wakeup.ID); w.Enabled {
		t.Fatal("a budget raise re-enabled a weekly-guard wakeup")
	}

	// The run leaves the 7-day window: the count falls below the limit.
	e.f.Exec(t, `UPDATE agent_task_queue SET created_at=now()-interval '8 days' WHERE issue_id=$1`, e.issue)
	if err := e.s.TickWorkspaces(context.Background(), parseTestUUID(t, e.f.WorkspaceID)); err != nil {
		t.Logf("tick: %v", err)
	}
	if w, _ = e.f.q.LocklessWakeup(context.Background(), e.wakeup.ID); !w.Enabled || w.PausedReason.Valid {
		t.Fatalf("below the limit: enabled=%v reason=%v, want re-enabled", w.Enabled, w.PausedReason)
	}
}

// CHE-6166 FAIL, finding 1: a disabled wakeup must not block the progress run
// CHE-6175 admits.
func TestProgressEventAdmitsExactlyOneRunThroughADisabledWeeklyWakeup(t *testing.T) {
	t.Setenv("MULTICA_WEEKLY_RUN_GUARD_MAX_RUNS", "1")
	e := newGuardBlockEnv(t)

	if w := e.tick(t); w.Enabled || w.PausedReason.String != "weekly_run_guard" {
		t.Fatalf("after the refusal: enabled=%v reason=%q", w.Enabled, w.PausedReason.String)
	}

	// A new PR head arrives while the issue is still at the limit.
	e.f.Exec(t, `INSERT INTO issue_pr_event (workspace_id, issue_id, pull_request_id, kind, head_sha, observed_at, dedupe_key)
		VALUES ($1, $2, $3, 'head_changed', 'sha-new', now(), $4)`, e.f.WorkspaceID, e.issue, uuid.NewString(), uuid.NewString())
	e.f.Cleanup(t, `DELETE FROM issue_pr_event WHERE issue_id=$1`, e.issue)

	e.tick(t)
	if n := e.runs(t); n != 1 {
		t.Fatalf("runs after the progress event = %d, want exactly 1", n)
	}
	// The same event admits no second run: once the admitted run ends, the
	// wakeup is refused and disabled again.
	e.f.Exec(t, `UPDATE agent_task_queue SET status='completed' WHERE issue_id=$1 AND status IN ('queued','dispatched','running')`, e.issue)
	w := e.tick(t)
	e.tick(t)
	if n := e.runs(t); n != 1 {
		t.Fatalf("runs after a duplicate trigger = %d, want 1", n)
	}
	if w.Enabled || w.PausedReason.String != "weekly_run_guard" {
		t.Fatalf("after the duplicate: enabled=%v reason=%q, want disabled", w.Enabled, w.PausedReason.String)
	}
}

// CHE-6193 review P1: the resume sweep reads one bounded page per pass. Rules
// the guard still refuses must not pin the page, or an eligible rule behind
// them never resumes.
func TestWeeklyGuardResumeSweepReachesAnEligibleRuleBehindIneligibleOnes(t *testing.T) {
	t.Setenv("MULTICA_WEEKLY_RUN_GUARD_MAX_RUNS", "1")
	old := weeklyGuardResumePageSize
	weeklyGuardResumePageSize = 2
	t.Cleanup(func() { weeklyGuardResumePageSize = old })

	envs := []guardBlockEnv{newGuardBlockEnv(t), newGuardBlockEnv(t), newGuardBlockEnv(t)}
	var workspaces []pgtype.UUID
	for _, e := range envs {
		e.budget(t)
		if w := e.tick(t); w.Enabled || w.PausedReason.String != "weekly_run_guard" {
			t.Fatalf("setup: enabled=%v reason=%q, want weekly_run_guard pause", w.Enabled, w.PausedReason.String)
		}
		workspaces = append(workspaces, parseTestUUID(t, e.f.WorkspaceID))
	}
	// Only the newest pause is eligible: its run left the 7-day window.
	last := envs[2]
	last.f.Exec(t, `UPDATE agent_task_queue SET created_at=now()-interval '8 days' WHERE issue_id=$1`, last.issue)

	for pass := 1; pass <= 3; pass++ {
		if err := last.s.resumeWeeklyGuardWakeups(context.Background(), workspaces); err != nil {
			t.Fatal(err)
		}
	}
	if w, _ := last.f.q.LocklessWakeup(context.Background(), last.wakeup.ID); !w.Enabled || w.PausedReason.Valid {
		t.Fatalf("eligible rule behind ineligible ones: enabled=%v reason=%v, want re-enabled", w.Enabled, w.PausedReason)
	}
	for _, e := range envs[:2] {
		if w, _ := e.f.q.LocklessWakeup(context.Background(), e.wakeup.ID); w.Enabled {
			t.Fatal("an ineligible rule was re-enabled")
		}
	}
}
