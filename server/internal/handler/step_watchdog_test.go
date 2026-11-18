package handler

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
)

type watchdogFixture struct{ agentID, projectID, workflowID string }

// newWatchdogFixture is a project whose workflow hands `approved` to an agent.
func newWatchdogFixture(t *testing.T) watchdogFixture {
	t.Helper()
	createTestCustomStatus(t, "approved", "started")
	agentID := createHandlerTestAgent(t, "watchdog-"+uuid.NewString()[:8], []byte("[]"))
	wf := createTestWorkflowSteps(t, "Watchdog "+workflowTestSuffix(), []map[string]any{
		{"status_key": "backlog"}, {"status_key": "todo"}, {"status_key": "in_progress"},
		{"status_key": "approved", "handler": map[string]any{"type": "agent", "id": agentID}},
		{"status_key": "in_review"}, {"status_key": "blocked"},
	})
	projectID := createWorkflowTestProject(t, "Watchdog project")
	setProjectWorkflow(t, projectID, map[string]any{"workflow_id": wf.ID})
	return watchdogFixture{agentID, projectID, wf.ID}
}

// stalledIssue is an issue in `approved` whose status occurrence is `age` old,
// with `ended` ended runs in the last 7 days.
func (f watchdogFixture) stalledIssue(t *testing.T, age time.Duration, ended int, over ...testutil.Cols) string {
	t.Helper()
	cols := testutil.Cols{"assignee_type": "agent", "assignee_id": f.agentID, "status": "approved", "project_id": f.projectID}
	for _, o := range over {
		for k, v := range o {
			cols[k] = v
		}
	}
	issueID := dbfx.Issue(t, "stalled approved issue", cols)
	dbfx.Insert(t, "activity_log", testutil.Cols{
		"workspace_id": testWorkspaceID, "issue_id": issueID, "actor_type": "system", "action": "status_changed",
		"details": `{"from":"in_review","to":"approved"}`, "created_at": time.Now().Add(-age),
	})
	for i := 0; i < ended; i++ {
		dbfx.Task(t, f.agentID, testutil.Cols{
			"issue_id": issueID, "status": "completed", "runtime_id": handlerTestRuntimeID(t),
			"created_at": time.Now().Add(-time.Duration(i+1) * time.Hour),
		})
	}
	return issueID
}

func candidateFor(t *testing.T, issueID string) (stepCandidate, bool) {
	t.Helper()
	cands, err := testHandler.stepWatchdogCandidates(context.Background(), 1000)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cands {
		if uuidToString(c.Issue.ID) == issueID {
			return c, true
		}
	}
	return stepCandidate{}, false
}

// TestStepWatchdogClassifiesBudgetRefusedApprovedIssue reproduces CHE-4960: an
// issue in `approved` for 25 h, 36 runs in 7 days against a cap of 25, a
// handoff the guard skipped, and no active run.
func TestStepWatchdogClassifiesBudgetRefusedApprovedIssue(t *testing.T) {
	f := newWatchdogFixture(t)

	t.Run("weekly run guard", func(t *testing.T) {
		t.Setenv("MULTICA_WEEKLY_RUN_GUARD_MAX_RUNS", "25")
		issueID := f.stalledIssue(t, 25*time.Hour, 36)
		dbfx.Insert(t, "issue_handoff_dispatch", testutil.Cols{
			"id": uuid.NewString(), "workspace_id": testWorkspaceID, "issue_id": issueID, "target_type": "agent", "target_id": f.agentID,
			"actor_type": "system", "handoff_write": "status", "state": "skipped", "skip_reason": service.ErrWeeklyRunGuard.Error(),
		})
		c, ok := candidateFor(t, issueID)
		if !ok {
			t.Fatal("approved issue in its step 25 h with no run is not a candidate")
		}
		got, err := testHandler.classifyStep(context.Background(), c)
		if err != nil {
			t.Fatal(err)
		}
		if got.Case != stepCaseBlockBudget || !errors.Is(got.Refusal, service.ErrWeeklyRunGuard) {
			t.Errorf("class = %+v, want block_budget with ErrWeeklyRunGuard", got)
		}
	})

	t.Run("run budget", func(t *testing.T) {
		issueID := f.stalledIssue(t, 25*time.Hour, 36, testutil.Cols{"workflow_id": f.workflowID})
		dbfx.Exec(t, `UPDATE issue_workflow SET max_runs_per_issue = 25, run_budget_since = now() - interval '7 days' WHERE id = $1`, f.workflowID)
		dbfx.Cleanup(t, `DELETE FROM issue_run_budget_entry WHERE issue_id = $1`, issueID)
		c, ok := candidateFor(t, issueID)
		if !ok {
			t.Fatal("approved issue in its step 25 h with no run is not a candidate")
		}
		got, err := testHandler.classifyStep(context.Background(), c)
		if err != nil {
			t.Fatal(err)
		}
		if got.Case != stepCaseBlockBudget || !errors.Is(got.Refusal, service.ErrRunBudgetExhausted) || errors.Is(got.Refusal, service.ErrWeeklyRunGuard) {
			t.Errorf("class = %+v, want block_budget with ErrRunBudgetExhausted only", got)
		}
	})
}

// TestStepWatchdogCandidateNegatives: each case below is not a candidate, or
// classifies differently, and a comment never resets the status clock.
func TestStepWatchdogCandidateNegatives(t *testing.T) {
	f := newWatchdogFixture(t)
	t.Setenv("MULTICA_WEEKLY_RUN_GUARD_MAX_RUNS", "25")

	notCandidate := map[string]func(t *testing.T) string{
		"status 23 h old": func(t *testing.T) string { return f.stalledIssue(t, 23*time.Hour, 0) },
		"active run": func(t *testing.T) string {
			id := f.stalledIssue(t, 25*time.Hour, 0)
			dbfx.Task(t, f.agentID, testutil.Cols{"issue_id": id, "status": "running", "runtime_id": handlerTestRuntimeID(t)})
			return id
		},
		"finished status": func(t *testing.T) string {
			return f.stalledIssue(t, 25*time.Hour, 0, testutil.Cols{"status": "done"})
		},
		"member assignee": func(t *testing.T) string {
			return f.stalledIssue(t, 25*time.Hour, 0, testutil.Cols{"assignee_type": "member", "assignee_id": testUserID})
		},
		"held by Cheese stop in the description": func(t *testing.T) string {
			return f.stalledIssue(t, 25*time.Hour, 0, testutil.Cols{"description": "Blockers: Cheese stop CHE-1\nbody"})
		},
		"parent delivery_stop stopped": func(t *testing.T) string {
			parent := dbfx.Issue(t, "parent", testutil.Cols{"metadata": `{"delivery_stop":"stopped"}`})
			return f.stalledIssue(t, 25*time.Hour, 0, testutil.Cols{"parent_issue_id": parent})
		},
	}
	for name, seed := range notCandidate {
		t.Run(name, func(t *testing.T) {
			if _, ok := candidateFor(t, seed(t)); ok {
				t.Error("is a candidate, want none")
			}
		})
	}

	t.Run("a recent comment does not reset the clock, open admission redispatches", func(t *testing.T) {
		id := f.stalledIssue(t, 25*time.Hour, 0)
		dbfx.Comment(t, id, "still waiting", testutil.Cols{"created_at": time.Now().Add(-time.Hour)})
		dbfx.Exec(t, `UPDATE issue SET updated_at = now() WHERE id = $1`, id)
		c, ok := candidateFor(t, id)
		if !ok {
			t.Fatal("a comment hid the issue")
		}
		got, err := testHandler.classifyStep(context.Background(), c)
		if err != nil {
			t.Fatal(err)
		}
		if got.Case != stepCaseRedispatch || got.Refusal != nil {
			t.Errorf("class = %+v, want redispatch with no refusal", got)
		}
	})

	t.Run("squad target resolves to its leader", func(t *testing.T) {
		squad := dbfx.Squad(t, "watchdog squad "+uuid.NewString()[:8], f.agentID)
		id := f.stalledIssue(t, 25*time.Hour, 36, testutil.Cols{"assignee_type": "squad", "assignee_id": squad})
		c, ok := candidateFor(t, id)
		if !ok {
			t.Fatal("squad-assigned issue is not a candidate")
		}
		got, err := testHandler.classifyStep(context.Background(), c)
		if err != nil {
			t.Fatal(err)
		}
		if got.Case != stepCaseBlockBudget || !errors.Is(got.Refusal, service.ErrWeeklyRunGuard) {
			t.Errorf("class = %+v, want block_budget with ErrWeeklyRunGuard", got)
		}
	})
}
