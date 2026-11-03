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
	return refusedEventAfter(t, c, func() {})
}

// refusedEventAfter is refusedEvent with afterSpend run between the spend and
// the refusal, to put the issue in the state the refusal finds it in.
func refusedEventAfter(t *testing.T, c budgetCase, afterSpend func()) service.RunBudgetExhausted {
	t.Helper()
	events := make(chan service.RunBudgetExhausted, 1)
	service.SetOnRunBudgetExhausted(func(_ context.Context, e service.RunBudgetExhausted) { events <- e })
	t.Cleanup(func() { service.SetOnRunBudgetExhausted(testHandler.onRunBudgetExhausted) })
	c.spend(t, 2)
	afterSpend()
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

// landBeforeBlock runs write after the callback has read the issue and passed
// its window check, and before the block write.
func landBeforeBlock(t *testing.T, write func()) {
	t.Helper()
	landed := false
	runBudgetBeforeBlock = func() { runBudgetBeforeBlock = func() {}; landed = true; write() }
	t.Cleanup(func() {
		runBudgetBeforeBlock = func() {}
		if !landed {
			t.Error("the write never landed: the callback did not reach the block")
		}
	})
}

// A head grant writes a ledger row and no issue field. Landing one between
// the callback's reads and its block must still win, with the expected
// revision unchanged. The revision is pinned to the value the callback read,
// so the test does not depend on what else moves it.
func TestRunBudgetExhaustionCallbackLosesToAHeadGrantLandingBeforeTheBlock(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	c := newBudgetCase(t, 2807004, recoverySteps(seededReadyAgentID(t)), 2)
	e := refusedEvent(t, c)
	landBeforeBlock(t, func() {
		ctx := context.Background()
		var revision int64
		if err := testPool.QueryRow(ctx, `SELECT revision FROM issue WHERE id=$1`, c.issueID).Scan(&revision); err != nil {
			t.Fatal(err)
		}
		grantHead(t, c.issueID, "head-during-callback")
		if _, err := testPool.Exec(ctx, `UPDATE issue SET revision=$2 WHERE id=$1`, c.issueID, revision); err != nil {
			t.Fatal(err)
		}
	})
	testHandler.onRunBudgetExhausted(context.Background(), e)
	assertNotBlocked(t, c)
}

func TestRunBudgetExhaustionCallbackLosesToATopUpLandingBeforeTheBlock(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	c := newBudgetCase(t, 2807005, recoverySteps(seededReadyAgentID(t)), 2)
	e := refusedEvent(t, c)
	landBeforeBlock(t, func() {
		if w := topUpBudget(t, c.issueID, nil); w.Code != http.StatusOK {
			t.Errorf("top-up = %d: %s", w.Code, w.Body.String())
		}
	})
	testHandler.onRunBudgetExhausted(context.Background(), e)
	assertNotBlocked(t, c)
}

func landBeforeRecovery(t *testing.T, write func()) {
	t.Helper()
	landed := false
	runBudgetBeforeRecovery = func() { runBudgetBeforeRecovery = func() {}; landed = true; write() }
	t.Cleanup(func() {
		runBudgetBeforeRecovery = func() {}
		if !landed {
			t.Error("the write never landed: the callback did not reach the recovery run")
		}
	})
}

func landBeforeNotice(t *testing.T, write func()) {
	t.Helper()
	landed := false
	runBudgetBeforeNotice = func() { runBudgetBeforeNotice = func() {}; landed = true; write() }
	t.Cleanup(func() {
		runBudgetBeforeNotice = func() {}
		if !landed {
			t.Error("the write never landed: the callback did not reach the notice")
		}
	})
}

// The notice-only paths move no status, so only the notice's own transaction
// can stop a stale callback from opening the old window's decision request.
func TestRunBudgetExhaustionNoticeLosesToATopUpLandingBeforeTheNotice(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	cases := []struct {
		name   string
		seed   int64
		steps  []map[string]any
		status string
	}{
		{"already blocked", 2807006, mergeEdgeSteps, "blocked"},
		{"workflow without blocked", 2807007, noBlockedSteps, "in_progress"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newBudgetCase(t, tc.seed, tc.steps, 2)
			e := refusedEventAfter(t, c, func() {
				if tc.status == "blocked" {
					c.enter(t, "blocked")
				}
			})
			landBeforeNotice(t, func() {
				if w := topUpBudget(t, c.issueID, nil); w.Code != http.StatusOK {
					t.Errorf("top-up = %d: %s", w.Code, w.Body.String())
				}
			})
			testHandler.onRunBudgetExhausted(context.Background(), e)
			if got := c.status(t); got != tc.status {
				t.Fatalf("status = %s, want %s kept", got, tc.status)
			}
			if n := c.decisionRows(t); n != 0 {
				t.Fatalf("decision requests = %d, want 0", n)
			}
			if n := recoveryRuns(t, c.edgeCase); n != 0 {
				t.Fatalf("recovery runs = %d, want 0", n)
			}
		})
	}
}

// The block commits while the window is exhausted, then a top-up lands before
// the recovery run is claimed. The recovery run bypasses the exhaustion
// refusal, so it must not claim the old window, and the notice (which follows
// it) drops the stale refusal.
func TestRunBudgetExhaustionRecoveryLosesToATopUpLandingAfterTheBlock(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	c := newBudgetCase(t, 2807008, recoverySteps(seededReadyAgentID(t)), 2)
	e := refusedEvent(t, c)
	landBeforeRecovery(t, func() {
		if w := topUpBudget(t, c.issueID, nil); w.Code != http.StatusOK {
			t.Errorf("top-up = %d: %s", w.Code, w.Body.String())
		}
	})
	testHandler.onRunBudgetExhausted(context.Background(), e)
	if got := c.status(t); got != "blocked" {
		t.Fatalf("status = %s, want the committed block kept", got)
	}
	if n := c.decisionRows(t); n != 0 {
		t.Fatalf("decision requests = %d, want 0", n)
	}
	if n := recoveryRuns(t, c.edgeCase); n != 0 {
		t.Fatalf("recovery runs = %d, want 0", n)
	}
}
