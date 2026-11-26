package handler

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/util"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

// A workflow-squad stage handoff is an enqueue like any other: it takes the
// locked run gate and debits the issue's run budget. Terra's CHE-6842 P1: the
// upstream helper wrote the row with a bare CreateAgentTask and skipped both.

// handoffBudgetCase leaves `left` runs in the issue's run-budget ledger and
// returns the issue plus a squad whose leader is the seeded ready agent.
func handoffBudgetCase(t *testing.T, installationID int64, max, left int) (edgeCase, squadCommentTriggerFixture) {
	t.Helper()
	c := newEdgeCase(t, installationID, mergeEdgeSteps)
	spendBudget(t, c, max, "in_progress") // spends all max runs: ledger balance 0
	if left > 0 {
		// The ledger, not the task rows, holds the balance once the window is
		// open, so deleting rows would not give a run back. Credit it the way
		// a raise does (ledger rows are never deleted).
		var windowID string
		if err := testPool.QueryRow(context.Background(), `SELECT window_id FROM issue_run_budget_entry WHERE issue_id=$1 AND kind='window'`, c.issueID).Scan(&windowID); err != nil {
			t.Fatal(err)
		}
		if _, err := testPool.Exec(context.Background(), `INSERT INTO issue_run_budget_entry (id, workspace_id, issue_id, window_id, kind, event_key, runs_delta) SELECT $3::uuid, workspace_id, id, $2::text, 'raise', 'raise:'||$3::uuid::text, $4::int FROM issue WHERE id=$1`, c.issueID, windowID, uuidToString(dbid.NewV7()), left); err != nil {
			t.Fatal(err)
		}
		if got := ledgerBalance(t, c.issueID); got != int64(left) {
			t.Fatalf("seeded ledger balance = %d, want %d", got, left)
		}
	}
	return c, newSquadCommentTriggerFixture(t)
}

// handoffSquad inserts a squad with its own leader agent: a handoff to it has
// a deduplication key (issue, leader, squad, revision) no other handoff shares.
func handoffSquad(t *testing.T) squadCommentTriggerFixture {
	t.Helper()
	leaderID := createHandlerTestAgent(t, "handoff-leader-"+uuidTail(), nil)
	var squadID string
	if err := testPool.QueryRow(context.Background(), `INSERT INTO squad (workspace_id, name, description, leader_id, creator_id) VALUES ($1, $2, '', $3, $4) RETURNING id`, testWorkspaceID, "handoff-squad-"+uuidTail(), leaderID, testUserID).Scan(&squadID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM squad WHERE id = $1`, squadID) })
	return squadCommentTriggerFixture{SquadID: squadID, LeaderID: leaderID}
}

// handoff returns the error instead of failing the test: the concurrent test
// calls it from goroutines.
func handoff(c edgeCase, sq squadCommentTriggerFixture) error {
	ctx := context.Background()
	issue, err := testHandler.Queries.GetIssue(ctx, util.MustParseUUID(c.issueID))
	if err != nil {
		return err
	}
	_, err = testHandler.TaskService.EnqueueTaskForWorkflowSquadLeader(ctx, issue, util.MustParseUUID(sq.LeaderID), util.MustParseUUID(sq.SquadID), "stage handoff", util.MustParseUUID(testUserID))
	return err
}

func TestWorkflowSquadHandoffRefusedWhenRunBudgetSpent(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	c, sq := handoffBudgetCase(t, 6842010, 2, 0)
	if err := handoff(c, sq); !errors.Is(err, service.ErrRunBudgetExhausted) {
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
	if err := handoff(c, sq); err != nil {
		t.Fatalf("handoff with one run left refused: %v", err)
	}
	issue, err := testHandler.Queries.GetIssue(context.Background(), util.MustParseUUID(c.issueID))
	if err != nil {
		t.Fatal(err)
	}
	if got := ledgerBalance(t, c.issueID); got != 0 {
		t.Fatalf("ledger balance after the handoff = %d, want 0 (one run left, one debit)", got)
	}
	if _, err := testHandler.TaskService.EnqueueTaskForIssue(context.Background(), issue); !errors.Is(err, service.ErrRunBudgetExhausted) {
		t.Fatalf("run after the handoff took the last slot = %v, want ErrRunBudgetExhausted", err)
	}
}

func TestWorkflowSquadHandoffConcurrentLastSlotAdmitsOne(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	c, _ := handoffBudgetCase(t, 6842012, 2, 1)
	const n = 8
	// One squad and leader per racer: every handoff has its own deduplication
	// key, so the duplicate guard cannot decide the winner. Only the budget
	// lock can.
	squads := make([]squadCommentTriggerFixture, n)
	for i := range squads {
		squads[i] = handoffSquad(t)
	}
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			errs[i] = handoff(c, squads[i])
		}(i)
	}
	close(start)
	wg.Wait()
	admitted := 0
	for _, err := range errs {
		switch {
		case err == nil:
			admitted++
		case !errors.Is(err, service.ErrRunBudgetExhausted):
			t.Fatalf("concurrent handoff = %v, want nil or ErrRunBudgetExhausted", err)
		}
	}
	if admitted != 1 {
		t.Fatalf("%d concurrent handoffs on one ledger run admitted %d, want exactly 1", n, admitted)
	}
	if got := ledgerBalance(t, c.issueID); got != 0 {
		t.Fatalf("ledger balance after the race = %d, want 0", got)
	}
}
