package handler

import (
	"net/http"
	"strings"
	"testing"
)

// A raise or top-up request that names a window the issue has left is refused:
// its card could never show a preview, and the executor would end it with
// stale_window (CHE-6882).
func TestRequestRunBudgetRefusesAWindowThatIsNoLongerOpen(test *testing.T) {
	setWorkspacePRMergeStatusForTest(test, "none")
	fixture := newBudgetRequestFixture(test)
	moveWindow(test, fixture.issueID, fixture.humanID)
	if windowID(test, fixture.issueID) == fixture.window {
		test.Fatal("control: the status move did not open a new window")
	}
	current := windowID(test, fixture.issueID)
	for name, body := range map[string]map[string]any{
		"raise":  {"kind": "raise", "new_cap": 9, "window_id": fixture.window},
		"top_up": {"kind": "top_up", "window_id": fixture.window},
	} {
		test.Run(name, func(test *testing.T) {
			text := fixture.request(test, fixture.humanID, "interactive_jwt", body).Want(http.StatusConflict).Text()
			if !strings.Contains(text, "stale_window") || !strings.Contains(text, current) {
				test.Fatalf("refusal %q, want stale_window naming the open window %s", text, current)
			}
		})
	}
	if n := fixture.openRequests(test); n != 0 {
		test.Fatalf("a refused request opened %d cards, want 0", n)
	}
}

// Approving a top-up opens a new window, so a raise card left open for the old
// window can no longer preview or apply. It is withdrawn, not left showing
// "Details unavailable" (CHE-6882).
func TestApprovingATopUpWithdrawsRunBudgetCardsOfTheOldWindow(test *testing.T) {
	setWorkspacePRMergeStatusForTest(test, "none")
	action, window := newPendingTopUp(test, true)
	fixture := budgetRequestFixture{action.singleCallFixture, window}
	var raise budgetRequestResponse
	fixture.request(test, fixture.humanID, "interactive_jwt", map[string]any{"kind": "raise", "new_cap": 9, "window_id": window}).Want(http.StatusCreated).JSON(&raise)
	status := func() string {
		var got string
		dbfx.QueryRow(test, `SELECT status FROM decision_request WHERE id = $1`, raise.RequestID).Scan(&got)
		return got
	}
	if got := status(); got != "open" {
		test.Fatalf("control: raise card status = %s, want open", got)
	}

	action.approve(test, withVerifier(acceptingVerifier), action.claim(test, nil), http.StatusOK)

	if state := action.state(test); state != "executed" {
		test.Fatalf("top-up state = %s (%s), want executed", state, action.failureCode(test))
	}
	if got := status(); got != "cancelled" {
		test.Fatalf("raise card of the old window status = %s, want cancelled", got)
	}
	var state string
	dbfx.QueryRow(test, `SELECT state FROM decision_action WHERE id = $1`, raise.ActionID).Scan(&state)
	if state == "pending" {
		test.Fatal("the withdrawn card's action is still pending")
	}
}
