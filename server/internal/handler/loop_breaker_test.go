package handler

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// loopCase is an in_progress agent-assigned issue whose run budget is out of
// reach, so only the loop breaker can refuse a run.
func loopCase(t *testing.T, installationID int64) budgetCase {
	t.Helper()
	t.Setenv("MULTICA_LOOP_BREAKER_MAX_RUNS", "6")
	return newBudgetCase(t, installationID, recoverySteps(seededReadyAgentID(t)), 1000)
}

// seedRuns inserts n started runs of agentID on the issue, each age old.
func seedRuns(t *testing.T, c budgetCase, agentID string, n int, age time.Duration) {
	t.Helper()
	for i := 0; i < n; i++ {
		at := time.Now().Add(-age).Add(time.Duration(i) * time.Second)
		dbfx.Task(t, agentID, testutil.Cols{
			"issue_id": c.issueID, "status": "completed", "created_at": at, "started_at": at, "completed_at": at,
			"trigger_evidence_kind": "comment",
		})
	}
}

// enqueue starts one run on the issue as the assigned agent.
func (c budgetCase) enqueue(t *testing.T) error {
	t.Helper()
	issue, err := testHandler.Queries.GetIssue(context.Background(), parseUUID(c.issueID))
	if err != nil {
		t.Fatal(err)
	}
	_, err = testHandler.TaskService.EnqueueTaskForIssue(context.Background(), issue)
	return err
}

// tripped refuses one run and returns the breaker's event, without running the
// handler hook (it runs on its own goroutine in production).
func (c budgetCase) tripped(t *testing.T) service.LoopBreakerTripped {
	t.Helper()
	events := make(chan service.LoopBreakerTripped, 1)
	service.SetOnLoopBreakerTripped(func(_ context.Context, e service.LoopBreakerTripped) { events <- e })
	t.Cleanup(func() { service.SetOnLoopBreakerTripped(testHandler.onLoopBreakerTripped) })
	if err := c.enqueue(t); !errors.Is(err, service.ErrLoopBreaker) {
		t.Fatalf("run = %v, want ErrLoopBreaker", err)
	}
	select {
	case e := <-events:
		return e
	case <-time.After(5 * time.Second):
		t.Fatal("no loop-breaker event")
		return service.LoopBreakerTripped{}
	}
}

func (c budgetCase) loopNotices(t *testing.T) int {
	return dbfx.Count(t, `SELECT count(*) FROM comment WHERE issue_id=$1 AND author_type='system' AND content LIKE '%Loop breaker%'`, c.issueID)
}

func TestLoopBreakerSixthRunInWindowIsRefusedAndBlocksIssue(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	c := loopCase(t, 5135001)
	seedRuns(t, c, c.agentID, 5, 10*time.Minute)
	e := c.tripped(t)
	testHandler.onLoopBreakerTripped(context.Background(), e)

	if got := c.status(t); got != "blocked" {
		t.Fatalf("issue is %s, want blocked", got)
	}
	var name, reason string
	if err := testPool.QueryRow(context.Background(), `SELECT name FROM agent WHERE id=$1`, c.agentID).Scan(&name); err != nil {
		t.Fatal(err)
	}
	if err := testPool.QueryRow(context.Background(), `SELECT COALESCE(metadata->>'blocked_reason','') FROM issue WHERE id=$1`, c.issueID).Scan(&reason); err != nil {
		t.Fatal(err)
	}
	if want := "loop-breaker: " + name + " started 5 runs in 30 min"; reason != want {
		t.Fatalf("blocked_reason = %q, want %q", reason, want)
	}
	var body string
	if err := testPool.QueryRow(context.Background(), `SELECT content FROM comment WHERE issue_id=$1 AND author_type='system' AND content LIKE '%Loop breaker%'`, c.issueID).Scan(&body); err != nil {
		t.Fatalf("notice comment: %v", err)
	}
	for _, want := range []string{name, "comment", "mention://agent/"} {
		if !strings.Contains(body, want) {
			t.Fatalf("notice %q lacks %q", body, want)
		}
	}
}

func TestLoopBreakerFifthRunInWindowStarts(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	c := loopCase(t, 5135002)
	seedRuns(t, c, c.agentID, 4, 10*time.Minute)
	if err := c.enqueue(t); err != nil {
		t.Fatalf("5th run refused: %v", err)
	}
	if got := c.status(t); got != "in_progress" {
		t.Fatalf("issue is %s, want in_progress", got)
	}
}

func TestLoopBreakerDoesNotCountAnotherAgent(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	c := loopCase(t, 5135003)
	var runtimeID string
	if err := testPool.QueryRow(context.Background(), `SELECT runtime_id FROM agent WHERE id=$1`, c.agentID).Scan(&runtimeID); err != nil {
		t.Fatal(err)
	}
	other := dbfx.Agent(t, "loop-other-"+workflowTestSuffix(), runtimeID)
	seedRuns(t, c, other, 5, 10*time.Minute)
	if err := c.enqueue(t); err != nil {
		t.Fatalf("run refused for runs of another agent: %v", err)
	}
}

func TestLoopBreakerDoesNotCountRunsOlderThanWindow(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	c := loopCase(t, 5135004)
	seedRuns(t, c, c.agentID, 5, 31*time.Minute)
	if err := c.enqueue(t); err != nil {
		t.Fatalf("run refused for runs older than the window: %v", err)
	}
}

func TestLoopBreakerSecondRefusalInAnHourPostsNoSecondComment(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	c := loopCase(t, 5135005)
	seedRuns(t, c, c.agentID, 5, 10*time.Minute)
	testHandler.onLoopBreakerTripped(context.Background(), c.tripped(t))
	testHandler.onLoopBreakerTripped(context.Background(), c.tripped(t))
	if n := c.loopNotices(t); n != 1 {
		t.Fatalf("loop-breaker comments = %d, want 1", n)
	}
}
