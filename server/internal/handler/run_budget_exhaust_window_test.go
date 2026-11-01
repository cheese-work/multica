package handler

import (
	"context"
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/service"
)

// A refusal's hook runs on its own goroutine, after the caller is done. These
// tests take the refusal's event off the hook and deliver it by hand after the
// state change, so the callback's order against that change is fixed.

// refusedEvent spends the budget and returns the exhaustion event the next
// refusal fires, without running the block.
func refusedEvent(t *testing.T, c budgetCase) service.RunBudgetExhausted {
	t.Helper()
	events := make(chan service.RunBudgetExhausted, 1)
	service.SetOnRunBudgetExhausted(func(_ context.Context, e service.RunBudgetExhausted) { events <- e })
	t.Cleanup(func() { service.SetOnRunBudgetExhausted(testHandler.onRunBudgetExhausted) })
	c.spend(t, 2)
	c.refuse(t)
	return <-events
}

func assertNotBlocked(t *testing.T, c budgetCase) {
	t.Helper()
	if got := c.status(t); got != "in_progress" {
		t.Fatalf("delayed callback moved the issue to %s, want in_progress kept", got)
	}
	if n := c.decisionRows(t); n != 0 {
		t.Fatalf("decision requests = %d, want 0", n)
	}
	if n := recoveryRuns(t, c.edgeCase); n != 0 {
		t.Fatalf("recovery runs = %d, want 0", n)
	}
}

func TestRunBudgetDelayedExhaustionAfterTopUpDoesNothing(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	c := newBudgetCase(t, 2807001, recoverySteps(seededReadyAgentID(t)), 2)
	e := refusedEvent(t, c)
	if w := topUpBudget(t, c.issueID, nil); w.Code != http.StatusOK {
		t.Fatalf("top-up = %d: %s", w.Code, w.Body.String())
	}
	testHandler.onRunBudgetExhausted(context.Background(), e)
	assertNotBlocked(t, c)
}

func TestRunBudgetExhaustionCallbackWithoutTopUpStillBlocks(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	c := newBudgetCase(t, 2807002, recoverySteps(seededReadyAgentID(t)), 2)
	e := refusedEvent(t, c)
	testHandler.onRunBudgetExhausted(context.Background(), e)
	if got := c.status(t); got != "blocked" {
		t.Fatalf("issue is %s, want blocked", got)
	}
	if n := c.decisionRows(t); n != 1 {
		t.Fatalf("decision requests = %d, want 1", n)
	}
	if n := recoveryRuns(t, c.edgeCase); n != 1 {
		t.Fatalf("recovery runs = %d, want 1", n)
	}
}

func TestRunBudgetDelayedExhaustionAfterProgressGrantDoesNothing(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	c := newBudgetCase(t, 2807003, recoverySteps(seededReadyAgentID(t)), 2)
	e := refusedEvent(t, c)
	grantHead(t, c.issueID, "head-after-refusal")
	testHandler.onRunBudgetExhausted(context.Background(), e)
	assertNotBlocked(t, c)
}
