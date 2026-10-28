package handler

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// The run budget must not catch the end of an issue's work or its review gate
// (CHE-2226). CHE-2164 was blocked after its PR merged; CHE-1192 was blocked
// the moment it entered in_review, so the independent review could not start.

// spendBudget gives the edge-case issue a workflow cap of max, an agent
// assignee, and max completed runs, so the next derived run is over the cap.
func spendBudget(t *testing.T, c edgeCase, max int, status string) db.Issue {
	t.Helper()
	ctx := context.Background()
	agentID := seededReadyAgentID(t)
	if _, err := testPool.Exec(ctx, `UPDATE issue_workflow SET max_runs_per_issue=$2, run_budget_since=now()-interval '1 hour' WHERE id=$1`, c.workflowID, max); err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(ctx, `UPDATE issue SET status='in_progress', assignee_type='agent', assignee_id=$2 WHERE id=$1`, c.issueID, agentID); err != nil {
		t.Fatal(err)
	}
	issue, err := testHandler.Queries.GetIssue(ctx, parseUUID(c.issueID))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < max; i++ {
		if _, err := testHandler.TaskService.EnqueueTaskForIssue(ctx, issue); err != nil {
			t.Fatalf("run %d inside the budget refused: %v", i+1, err)
		}
		finishRuns(t, c)
	}
	c.enter(t, status)
	issue, err = testHandler.Queries.GetIssue(ctx, parseUUID(c.issueID))
	if err != nil {
		t.Fatal(err)
	}
	return issue
}

func finishRuns(t *testing.T, c edgeCase) {
	t.Helper()
	if _, err := testPool.Exec(context.Background(), `UPDATE agent_task_queue SET status='completed',completed_at=clock_timestamp() WHERE issue_id=$1 AND status IN ('queued','dispatched','running')`, c.issueID); err != nil {
		t.Fatal(err)
	}
}

// linkPR links an open PR at head to the issue itself.
func linkPR(t *testing.T, issueID string, prNumber int32, head string) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	pr, err := testHandler.Queries.UpsertGitHubPullRequest(ctx, db.UpsertGitHubPullRequestParams{
		WorkspaceID: parseUUID(testWorkspaceID), InstallationID: 2226001,
		RepoOwner: "acme", RepoName: "widget", PrNumber: prNumber,
		Title: "review gate", State: "open", HtmlUrl: "https://github.com/acme/widget/pull/1",
		PrCreatedAt: pgtype.Timestamptz{Time: now, Valid: true},
		PrUpdatedAt: pgtype.Timestamptz{Time: now, Valid: true},
		HeadSha:     head, ClearMergeableState: pgtype.Bool{Bool: true, Valid: true},
	})
	if err != nil {
		t.Fatalf("UpsertGitHubPullRequest: %v", err)
	}
	if _, err := testHandler.Queries.LinkIssueToPullRequest(ctx, db.LinkIssueToPullRequestParams{IssueID: parseUUID(issueID), PullRequestID: pr.ID}); err != nil {
		t.Fatalf("LinkIssueToPullRequest: %v", err)
	}
}

// settle lets a delayed budget hook finish, so a test asserting "nothing
// changed" does not pass only because the hook has not run yet.
func settle() { time.Sleep(700 * time.Millisecond) }

// waitForRunBudgetBlock waits for the blocking hook of a refused run to finish
// its write, so a test that goes on to change the issue does not race it.
func waitForRunBudgetBlock(t *testing.T, c edgeCase) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if dbfx.Count(t, `SELECT count(*) FROM activity_log WHERE issue_id=$1 AND action='run_budget_exhausted'`, c.issueID) > 0 {
			return
		}
	}
	t.Fatal("the run budget block hook did not finish in 10s")
}

func TestRunBudgetKeepsMergedStatusAndAssignee(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	c := newEdgeCase(t, 2226001, mergeEdgeSteps)
	issue := spendBudget(t, c, 2, "merged")
	_, wasType, wasID := issueStatusAndAssignee(t, c.issueID)

	if _, err := testHandler.TaskService.EnqueueTaskForIssue(context.Background(), issue); !errors.Is(err, service.ErrRunBudgetExhausted) {
		t.Fatalf("run for a merged issue over the cap = %v, want it dropped with ErrRunBudgetExhausted", err)
	}
	settle()
	status, gotType, gotID := issueStatusAndAssignee(t, c.issueID)
	if status != "merged" {
		t.Fatalf("merged issue is now %s, want merged kept", status)
	}
	if gotType != wasType || gotID != wasID {
		t.Fatalf("assignee changed to %s/%s, want %s/%s", gotType, gotID, wasType, wasID)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM activity_log WHERE issue_id=$1 AND action='run_budget_exhausted'`, c.issueID); n != 0 {
		t.Fatalf("run_budget_exhausted activity rows = %d, want none for a finished issue", n)
	}
}

func TestRunBudgetKeepsDoneStatus(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	c := newEdgeCase(t, 2226002, mergeEdgeSteps)
	issue := spendBudget(t, c, 2, "done")
	if _, err := testHandler.TaskService.EnqueueTaskForIssue(context.Background(), issue); !errors.Is(err, service.ErrRunBudgetExhausted) {
		t.Fatalf("run for a done issue over the cap = %v, want ErrRunBudgetExhausted", err)
	}
	settle()
	if got := c.status(t); got != "done" {
		t.Fatalf("done issue is now %s, want done kept", got)
	}
}

// A hook that fires after the issue finished must not overwrite the finish.
func TestRunBudgetDelayedHookDoesNotOverwriteFinishedStatus(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	c := newEdgeCase(t, 2226003, mergeEdgeSteps)
	issue := spendBudget(t, c, 2, "in_progress")
	c.enter(t, "merged")
	testHandler.onRunBudgetExhausted(context.Background(), service.RunBudgetExhausted{IssueID: issue.ID, Runs: 2, Max: 2, Status: "in_progress", Target: service.RunTarget{AgentID: issue.AssigneeID, Trigger: "retry"}})
	if got := c.status(t); got != "merged" {
		t.Fatalf("delayed hook moved a merged issue to %s", got)
	}
}

func TestRunBudgetFirstReviewOfEachHeadIsAdmitted(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	c := newEdgeCase(t, 2226001, mergeEdgeSteps)
	issue := spendBudget(t, c, 2, "in_review")
	linkPR(t, c.issueID, 2226, "head-one")

	if _, err := testHandler.TaskService.EnqueueTaskForIssue(ctx, issue); err != nil {
		t.Fatalf("first review run on a new head refused: %v", err)
	}
	finishRuns(t, c)
	if _, err := testHandler.TaskService.EnqueueTaskForIssue(ctx, issue); !errors.Is(err, service.ErrRunBudgetExhausted) {
		t.Fatalf("second run on the same head = %v, want ErrRunBudgetExhausted", err)
	}
	// The same-head refusal schedules the blocking hook, which moves the issue
	// to blocked on its own connection. Wait for it, then put the issue back in
	// review, so the next head is judged on the review state and not on how
	// fast the hook ran. The spent budget and its window stay as they were.
	waitForRunBudgetBlock(t, c)
	c.enter(t, "in_review")

	if _, err := testPool.Exec(ctx, `UPDATE github_pull_request SET head_sha='head-two' WHERE id IN (SELECT pull_request_id FROM issue_pull_request WHERE issue_id=$1)`, c.issueID); err != nil {
		t.Fatal(err)
	}
	if _, err := testHandler.TaskService.EnqueueTaskForIssue(ctx, issue); err != nil {
		t.Fatalf("first review run on the next head refused: %v", err)
	}
}

func TestRunBudgetReviewWithoutPRIsNotAdmitted(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	c := newEdgeCase(t, 2226001, mergeEdgeSteps)
	issue := spendBudget(t, c, 2, "in_review")
	if _, err := testHandler.TaskService.EnqueueTaskForIssue(context.Background(), issue); !errors.Is(err, service.ErrRunBudgetExhausted) {
		t.Fatalf("in_review run with no linked PR = %v, want ErrRunBudgetExhausted", err)
	}
}

// A block says which run was refused and the exact command that lifts it.
func TestRunBudgetBlockNamesRefusedRunAndRaiseCommand(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	c := newEdgeCase(t, 2226001, mergeEdgeSteps)
	issue := spendBudget(t, c, 2, "in_progress")
	if _, err := testHandler.TaskService.EnqueueTaskForIssue(context.Background(), issue); !errors.Is(err, service.ErrRunBudgetExhausted) {
		t.Fatalf("run over the cap = %v, want ErrRunBudgetExhausted", err)
	}
	settle()
	if got := c.status(t); got != "blocked" {
		t.Fatalf("issue is %s, want blocked after a plain retry past the cap", got)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM activity_log WHERE issue_id=$1 AND action='run_budget_exhausted' AND details->>'raise_command' LIKE 'multica issue budget raise % --runs %' AND details->>'refused_run' <> ''`, c.issueID); n != 1 {
		t.Fatalf("run_budget_exhausted activity with refused_run and raise_command = %d, want 1", n)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM comment WHERE issue_id=$1 AND type='system' AND content LIKE '%a run for agent % (trigger: assignment or status handoff) while the issue was in_progress%' AND content LIKE '%was refused because the issue used 2 of 2 runs%' AND content LIKE '%multica issue budget raise %'`, c.issueID); n != 1 {
		t.Fatalf("block comment naming the run and the raise command = %d, want 1", n)
	}
}
