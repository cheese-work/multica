package handler

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/util"
)

// A workflow-squad stage handoff is an enqueue like any other: it takes the
// locked run gate and debits the issue's run budget. Terra's CHE-6842 P1: the
// upstream helper wrote the row with a bare CreateAgentTask and skipped both.

// handoffBudgetCase leaves `left` runs in the issue's budget and returns the
// issue plus a squad whose leader is the seeded ready agent.
func handoffBudgetCase(t *testing.T, installationID int64, max, left int) (edgeCase, squadCommentTriggerFixture) {
	t.Helper()
	c := newEdgeCase(t, installationID, mergeEdgeSteps)
	spendBudget(t, c, max, "in_progress") // spends all max runs
	// Hand back `left` of them: the window counts admitted runs, so drop rows.
	if left > 0 {
		if _, err := testPool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE id IN (SELECT id FROM agent_task_queue WHERE issue_id=$1 ORDER BY created_at DESC LIMIT $2)`, c.issueID, left); err != nil {
			t.Fatal(err)
		}
	}
	return c, newSquadCommentTriggerFixture(t)
}

func handoff(t *testing.T, c edgeCase, sq squadCommentTriggerFixture) error {
	t.Helper()
	issue, err := testHandler.Queries.GetIssue(context.Background(), util.MustParseUUID(c.issueID))
	if err != nil {
		t.Fatal(err)
	}
	_, err = testHandler.TaskService.EnqueueTaskForWorkflowSquadLeader(context.Background(), issue, util.MustParseUUID(sq.LeaderID), util.MustParseUUID(sq.SquadID), "stage handoff", util.MustParseUUID(testUserID))
	return err
}

func TestWorkflowSquadHandoffRefusedWhenRunBudgetSpent(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	c, sq := handoffBudgetCase(t, 6842010, 2, 0)
	if err := handoff(t, c, sq); !errors.Is(err, service.ErrRunBudgetExhausted) {
		t.Fatalf("handoff over the cap = %v, want ErrRunBudgetExhausted", err)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id=$1 AND status IN ('queued','dispatched','running','deferred')`, c.issueID); n != 0 {
		t.Fatalf("refused handoff left %d open task rows, want 0", n)
	}
}

func TestWorkflowSquadHandoffDebitsRunBudget(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	c, sq := handoffBudgetCase(t, 6842011, 2, 1)
	if err := handoff(t, c, sq); err != nil {
		t.Fatalf("handoff with one run left refused: %v", err)
	}
	issue, err := testHandler.Queries.GetIssue(context.Background(), util.MustParseUUID(c.issueID))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := testHandler.TaskService.EnqueueTaskForIssue(context.Background(), issue); !errors.Is(err, service.ErrRunBudgetExhausted) {
		t.Fatalf("run after the handoff took the last slot = %v, want ErrRunBudgetExhausted", err)
	}
}

func TestWorkflowSquadHandoffConcurrentLastSlotAdmitsOne(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	c, sq := handoffBudgetCase(t, 6842012, 2, 1)
	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = handoff(t, c, sq)
		}(i)
	}
	wg.Wait()
	admitted := 0
	for _, err := range errs {
		if err == nil {
			admitted++
		} else if !errors.Is(err, service.ErrRunBudgetExhausted) && !errors.Is(err, service.ErrDuplicatePendingTask) {
			t.Fatalf("concurrent handoff = %v, want nil, ErrRunBudgetExhausted or ErrDuplicatePendingTask", err)
		}
	}
	if admitted != 1 {
		t.Fatalf("%d concurrent handoffs on one free run admitted %d, want exactly 1", n, admitted)
	}
}
