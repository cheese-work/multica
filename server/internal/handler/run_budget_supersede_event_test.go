package handler

import (
	"context"
	"testing"
)

// A later window's refusal retires an earlier refusal, which may be the one an
// approved card is waiting on. The card learns it once, after the commit (CHE-6947 D5b).
func TestRunBudgetLaterWindowRefusalTellsTheSupersededCard(test *testing.T) {
	c, id := reviewBlockedCase(test, 6947200)
	linkRefusal(test, id, resumeRowOf(test, id).Window)
	card := executedCard(test) // after the case: its fixture numbers issues by hand
	pointRefusalAt(test, id, card)
	resume(test, id)
	wantResume(test, id, "still_blocked", "runs")
	if receipt := card.cardAction(test).Receipt; receipt == nil || receipt.Outcome != "still_blocked:runs" {
		test.Fatalf("card receipt before the later window = %+v, want still_blocked:runs", receipt)
	}

	seen := watchCardUpdates(test, card.issueID)
	later := newWindowRefusal(test, c, "in_progress")
	testHandler.onRunBudgetExhausted(context.Background(), later)

	wantResume(test, id, "superseded", "later_window")
	wantCardUpdate(test, seen(), card.requestID)
	if receipt := card.cardAction(test).Receipt; receipt == nil || receipt.Outcome != "superseded:later_window" {
		test.Fatalf("card receipt = %+v, want superseded:later_window", receipt)
	}
}
