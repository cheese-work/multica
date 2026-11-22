package handler

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/pkg/protocol"
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

// A person's status move retires the refusal too. It publishes no card event of
// its own: the card re-reads on the issue:updated of the move (CHE-6947 AC7).
func TestRunBudgetStatusMoveRetiresTheRefusalAndTheCardReadsIt(test *testing.T) {
	c, id := reviewBlockedCase(test, 6947201)
	linkRefusal(test, id, resumeRowOf(test, id).Window)
	card := executedCard(test)
	pointRefusalAt(test, id, card)
	resume(test, id)
	wantResume(test, id, "still_blocked", "runs")

	var moved atomic.Bool
	testHandler.Bus.Subscribe(protocol.EventIssueUpdated, func(e events.Event) {
		payload, _ := e.Payload.(map[string]any)
		issue, _ := payload["issue"].(IssueResponse)
		if changed, _ := payload["status_changed"].(bool); changed && issue.ID == c.issueID {
			moved.Store(true)
		}
	})
	moveStatusAsMember(test, c.issueID, "todo")

	if !moved.Load() {
		test.Fatal("the move published no issue:updated with status_changed")
	}
	wantResume(test, id, "superseded", "status_moved")
	if receipt := card.cardAction(test).Receipt; receipt == nil || receipt.Outcome != "superseded:status_moved" {
		test.Fatalf("card receipt = %+v, want superseded:status_moved", receipt)
	}
}
