package handler

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/service"
)

// The review allowance belongs to the issue's reviewer and to one PR head
// (CHE-2226 review findings F1, F2, F3, F4).

// Other agents' runs on an in_review issue never spend the reviewer's first run.
func TestRunBudgetOtherAgentsDoNotSpendReviewAdmission(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	c := newEdgeCase(t, 2226020, mergeEdgeSteps)
	issue := spendBudget(t, c, 2, "in_review")
	linkPR(t, c.issueID, 2226, "head-one")
	leader := parseUUID(createHandlerTestAgent(t, "che2226-leader", nil))

	if _, err := testHandler.TaskService.EnqueueTaskForMention(ctx, issue, leader, pgtype.UUID{}, service.OriginDerived); !errors.Is(err, service.ErrRunBudgetExhausted) {
		t.Fatalf("leader run over the cap = %v, want ErrRunBudgetExhausted", err)
	}
	settle()
	if got := c.status(t); got != "in_review" {
		t.Fatalf("a refused leader run moved the issue to %s, want in_review kept", got)
	}
	if _, err := testHandler.TaskService.EnqueueTaskForIssue(ctx, issue); err != nil {
		t.Fatalf("reviewer's first run refused after a leader run: %v", err)
	}
}

// Of many concurrent enqueues for one head, across different agents, exactly
// one is admitted over the cap.
func TestRunBudgetPooledCallersAdmitOneRunPerHead(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	c := newEdgeCase(t, 2226021, mergeEdgeSteps)
	issue := spendBudget(t, c, 2, "in_review")
	linkPR(t, c.issueID, 2226, "head-one")
	others := []string{
		createHandlerTestAgent(t, "che2226-a", nil),
		createHandlerTestAgent(t, "che2226-b", nil),
	}

	var wg sync.WaitGroup
	var mu sync.Mutex
	admitted := 0
	start := make(chan struct{})
	enqueue := func(run func() error) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			err := run()
			if err != nil && !errors.Is(err, service.ErrRunBudgetExhausted) && !errors.Is(err, service.ErrDuplicatePendingTask) {
				t.Errorf("enqueue failed unexpectedly: %v", err)
			}
			if err == nil {
				mu.Lock()
				admitted++
				mu.Unlock()
			}
		}()
	}
	for i := 0; i < 4; i++ {
		enqueue(func() error { _, err := testHandler.TaskService.EnqueueTaskForIssue(ctx, issue); return err })
	}
	for _, id := range others {
		agent := parseUUID(id)
		enqueue(func() error {
			_, err := testHandler.TaskService.EnqueueTaskForMention(ctx, issue, agent, pgtype.UUID{}, service.OriginDerived)
			return err
		})
	}
	close(start)
	wg.Wait()
	if admitted != 1 {
		t.Fatalf("admitted %d runs for one head over the cap, want 1", admitted)
	}
}

// One reviewer's first run claims the one head it is pinned to, not every
// linked PR's head.
func TestRunBudgetReviewClaimsOnlyThePinnedHead(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	c := newEdgeCase(t, 2226022, mergeEdgeSteps)
	issue := spendBudget(t, c, 2, "in_review")
	linkPR(t, c.issueID, 2226, "head-one")
	linkPR(t, c.issueID, 2227, "head-two")

	if _, err := testHandler.TaskService.EnqueueTaskForIssue(context.Background(), issue); err != nil {
		t.Fatalf("first review run refused: %v", err)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM issue_review_admission WHERE issue_id=$1`, c.issueID); n != 1 {
		t.Fatalf("admissions after one run = %d, want 1", n)
	}
}

// The cap and the actor audit commit together.
func TestRaiseIssueRunBudgetRollsBackWhenAuditFails(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	c := newEdgeCase(t, 2226023, mergeEdgeSteps)
	spendBudget(t, c, 2, "in_progress")
	dbfx.Exec(t, `CREATE OR REPLACE FUNCTION che2226_refuse_audit() RETURNS trigger AS $$ BEGIN
		IF NEW.action = 'run_budget_raised' THEN RAISE EXCEPTION 'audit refused'; END IF; RETURN NEW; END; $$ LANGUAGE plpgsql`)
	dbfx.Exec(t, `CREATE TRIGGER che2226_refuse_audit BEFORE INSERT ON activity_log FOR EACH ROW EXECUTE FUNCTION che2226_refuse_audit()`)
	t.Cleanup(func() {
		testPool.Exec(ctx, `DROP TRIGGER IF EXISTS che2226_refuse_audit ON activity_log`)
		testPool.Exec(ctx, `DROP FUNCTION IF EXISTS che2226_refuse_audit()`)
	})

	if w := raiseBudget(t, c.issueID, map[string]any{"runs": 5}, nil); w.Code != http.StatusInternalServerError {
		t.Fatalf("raise with a failing audit = %d, want 500: %s", w.Code, w.Body.String())
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM issue WHERE id=$1 AND max_runs_override IS NOT NULL`, c.issueID); n != 0 {
		t.Fatal("the raise stayed in force without its audit")
	}
}

// The block names the agent and trigger of the run that was refused, not the
// issue's assignee.
func TestRunBudgetBlockNamesTheRefusedTarget(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	c := newEdgeCase(t, 2226024, mergeEdgeSteps)
	issue := spendBudget(t, c, 2, "in_progress")
	leader := createHandlerTestAgent(t, "che2226-target", nil)

	if _, err := testHandler.TaskService.EnqueueTaskForMention(context.Background(), issue, parseUUID(leader), pgtype.UUID{}, service.OriginDerived); !errors.Is(err, service.ErrRunBudgetExhausted) {
		t.Fatalf("mention run over the cap = %v, want ErrRunBudgetExhausted", err)
	}
	settle()
	if n := dbfx.Count(t, `SELECT count(*) FROM comment WHERE issue_id=$1 AND type='system' AND content LIKE '%a run for agent che2226-target (trigger: mention)%'`, c.issueID); n != 1 {
		t.Fatalf("block comment naming the mentioned agent and trigger = %d, want 1", n)
	}
}
