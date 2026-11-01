package handler

import (
	"net/http"
	"testing"
)

// A window nobody opened still holds credit or debt: the runs counted in it
// against the cap. A top-up or a person's status move starts the next window,
// and the old balance has to be written first, because a ledger sum cannot
// recover an entry that was never written. Each case admits a run with no
// pre-opening call, so the first ledger write is the one under test.

func TestRunBudgetTopUpKeepsTheCreditOfAnUnopenedWindow(t *testing.T) {
	if testHandler == nil {
		t.Fatal("isolated database required")
	}
	c, _ := budgetedAt(t, 40, 38)

	if w := topUpBudget(t, c.issueID, nil); w.Code != http.StatusOK {
		t.Fatalf("top-up = %d: %s", w.Code, w.Body.String())
	}
	admitRun(t, c)

	// 2 runs left in the old window + 40 in the new one, minus the admitted run.
	if got := ledgerBalance(t, c.issueID); got != 41 {
		t.Fatalf("balance after top-up and one admission = %d, want 41 (42 before the run)", got)
	}
}

func TestRunBudgetTopUpKeepsTheDebtOfAnUnopenedWindow(t *testing.T) {
	if testHandler == nil {
		t.Fatal("isolated database required")
	}
	c, _ := budgetedAt(t, 40, 43)

	if w := topUpBudget(t, c.issueID, nil); w.Code != http.StatusOK {
		t.Fatalf("top-up = %d: %s", w.Code, w.Body.String())
	}
	admitRun(t, c)

	// 3 runs overspent in the old window take 3 off the new window's 40.
	if got := ledgerBalance(t, c.issueID); got != 36 {
		t.Fatalf("balance after top-up and one admission = %d, want 36 (37 before the run)", got)
	}
}

func TestRunBudgetMemberStatusMoveKeepsTheCreditOfAnUnopenedWindow(t *testing.T) {
	if testHandler == nil {
		t.Fatal("isolated database required")
	}
	c, _ := budgetedAt(t, 40, 38)
	recordMemberMovesOf(t, c.issueID)

	moveStatusAsMember(t, c.issueID, "in_review")
	admitRun(t, c)

	if got := ledgerBalance(t, c.issueID); got != 41 {
		t.Fatalf("balance after a member move and one admission = %d, want 41 (42 before the run)", got)
	}
}

func TestRunBudgetMemberStatusMoveKeepsTheDebtOfAnUnopenedWindow(t *testing.T) {
	if testHandler == nil {
		t.Fatal("isolated database required")
	}
	c, _ := budgetedAt(t, 40, 43)
	recordMemberMovesOf(t, c.issueID)

	moveStatusAsMember(t, c.issueID, "in_review")
	admitRun(t, c)

	if got := ledgerBalance(t, c.issueID); got != 36 {
		t.Fatalf("balance after a member move and one admission = %d, want 36 (37 before the run)", got)
	}
}
