package handler

import (
	"context"
	"errors"
	"testing"

	"github.com/multica-ai/multica/server/internal/service"
)

// CHE-9026: settings.run_budget.mode turns the per-issue run cap from a block
// into a warning or nothing. Unset or block keeps today's behaviour, which the
// other run budget tests cover.

func TestRunBudgetModeWarnKeepsIssueRunningAndReportsOverage(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	c := newEdgeCase(t, 9026001, mergeEdgeSteps)
	issue := spendBudget(t, c, 2, "in_progress")
	setWorkspaceSettings(t, `{"run_budget":{"mode":"warn"}}`)

	if _, err := testHandler.TaskService.EnqueueTaskForIssue(context.Background(), issue); err != nil {
		t.Fatalf("run over the cap in warn mode = %v, want it admitted", err)
	}
	settle()
	if got := c.status(t); got != "in_progress" {
		t.Fatalf("issue is %s, want in_progress kept in warn mode", got)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM activity_log WHERE issue_id=$1 AND action='run_budget_exhausted'`, c.issueID); n != 0 {
		t.Fatalf("run_budget_exhausted activity rows = %d, want none in warn mode", n)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM run_budget_refusal WHERE issue_id=$1`, c.issueID); n != 0 {
		t.Fatalf("run_budget_refusal rows = %d, want no card in warn mode", n)
	}
	snap, err := service.LoadRunBudgetSnapshot(context.Background(), testHandler.Queries, issue)
	if err != nil {
		t.Fatal(err)
	}
	if snap.State != "warn" || snap.RunsLeft != -1 || len(snap.Blocking) != 0 {
		t.Fatalf("snapshot = state %q, runs_left %d, blocking %v; want state warn, runs_left -1, nothing blocking", snap.State, snap.RunsLeft, snap.Blocking)
	}
}

func TestRunBudgetModeOffNeverBlocksOrOpensACard(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	c := newEdgeCase(t, 9026002, mergeEdgeSteps)
	issue := spendBudget(t, c, 2, "in_progress")
	setWorkspaceSettings(t, `{"run_budget":{"mode":"off"}}`)

	if _, err := testHandler.TaskService.EnqueueTaskForIssue(context.Background(), issue); err != nil {
		t.Fatalf("run over the cap in off mode = %v, want it admitted", err)
	}
	settle()
	if got := c.status(t); got != "in_progress" {
		t.Fatalf("issue is %s, want in_progress kept in off mode", got)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM run_budget_refusal WHERE issue_id=$1`, c.issueID); n != 0 {
		t.Fatalf("run_budget_refusal rows = %d, want no card in off mode", n)
	}
	snap, err := service.LoadRunBudgetSnapshot(context.Background(), testHandler.Queries, issue)
	if err != nil {
		t.Fatal(err)
	}
	if snap.State == "warn" || len(snap.Blocking) != 0 {
		t.Fatalf("snapshot = state %q, blocking %v; want no warning and nothing blocking in off mode", snap.State, snap.Blocking)
	}
}

func TestRunBudgetModeBlockAndUnsetStillRefuse(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	for i, settings := range []string{`{}`, `{"run_budget":{"mode":"block"}}`, `{"run_budget":{"mode":"bogus"}}`} {
		c := newEdgeCase(t, int64(9026010+i), mergeEdgeSteps)
		issue := spendBudget(t, c, 2, "in_progress")
		setWorkspaceSettings(t, settings)
		if _, err := testHandler.TaskService.EnqueueTaskForIssue(context.Background(), issue); !errors.Is(err, service.ErrRunBudgetExhausted) {
			t.Fatalf("settings %s: run over the cap = %v, want ErrRunBudgetExhausted", settings, err)
		}
		waitForRunBudgetBlock(t, c)
	}
}
