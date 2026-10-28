package handler

import (
	"context"
	"errors"
	"testing"

	"github.com/multica-ai/multica/server/internal/service"
)

// The live Default workflow reviews at agent_review, a custom started status
// (CHE-2226 review finding F7). The first review of a head is admitted there as
// it is at in_review; a status that is not a review step gets no allowance.

func TestRunBudgetFirstReviewIsAdmittedAtAgentReview(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	createTestCustomStatus(t, "agent_review", "started")
	c := newEdgeCase(t, 2226060, mergeEdgeSteps)
	issue := spendBudget(t, c, 2, "agent_review")
	linkPR(t, c.issueID, 2226, "head-one")

	if _, err := testHandler.TaskService.EnqueueTaskForIssue(ctx, issue); err != nil {
		t.Fatalf("first review run at agent_review refused: %v", err)
	}
	finishRuns(t, c)
	if _, err := testHandler.TaskService.EnqueueTaskForIssue(ctx, issue); !errors.Is(err, service.ErrRunBudgetExhausted) {
		t.Fatalf("second run on the same head = %v, want ErrRunBudgetExhausted", err)
	}
	settle()
	if got := c.status(t); got != "blocked" {
		t.Fatalf("repeat run over the cap left the issue at %s, want blocked", got)
	}
}

func TestRunBudgetNonReviewStatusesGetNoAllowance(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	createTestCustomStatus(t, "need_decision", "started")
	c := newEdgeCase(t, 2226061, mergeEdgeSteps)
	issue := spendBudget(t, c, 2, "need_decision")
	linkPR(t, c.issueID, 2226, "head-one")
	if _, err := testHandler.TaskService.EnqueueTaskForIssue(context.Background(), issue); !errors.Is(err, service.ErrRunBudgetExhausted) {
		t.Fatalf("need_decision run over the cap = %v, want ErrRunBudgetExhausted", err)
	}
}

// A refused run never overwrites a finish: merged is a started-category status
// the guard names itself, approved and agent_accepted are done-category.
func TestRunBudgetKeepsEveryFinishedWorkflowStatus(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	createTestCustomStatus(t, "approved", "done")
	createTestCustomStatus(t, "agent_accepted", "done")
	for i, status := range []string{"merged", "approved", "agent_accepted"} {
		c := newEdgeCase(t, int64(2226070+i), mergeEdgeSteps)
		issue := spendBudget(t, c, 2, status)
		if _, err := testHandler.TaskService.EnqueueTaskForIssue(context.Background(), issue); !errors.Is(err, service.ErrRunBudgetExhausted) {
			t.Fatalf("%s run over the cap = %v, want ErrRunBudgetExhausted", status, err)
		}
		settle()
		if got := c.status(t); got != status {
			t.Fatalf("a refused run moved %s to %s", status, got)
		}
	}
}
