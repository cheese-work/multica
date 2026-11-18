package handler

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

// executedCard is a decided action with its card answer: what a resume's event is about.
// The refusal under test is pointed at it, so the worker finds a real card.
func executedCard(test *testing.T) pendingAction {
	test.Helper()
	action := newPendingRaise(test, 30).pendingAction
	action.approveAccepted(test)
	return action
}

func pointRefusalAt(test *testing.T, id pgtype.UUID, action pendingAction) {
	test.Helper()
	dbfx.Exec(test, `UPDATE run_budget_refusal SET action_id = $2 WHERE id = $1`, id, action.actionID)
}

// resumePaths are the two ways a resume starts: the approval's own notify and the sweep.
var resumePaths = []struct {
	name string
	run  func(test *testing.T, id pgtype.UUID)
}{
	{"direct", resume},
	{"sweep", func(test *testing.T, _ pgtype.UUID) {
		testHandler.sweepRunBudgetResumes(context.Background(), time.Now().Add(time.Minute))
	}},
}

// Every terminal outcome of a resume tells the open card once (CHE-6947 D5a).
func TestRunBudgetResumePublishesTheCardUpdateOnEveryTerminalOutcome(test *testing.T) {
	cases := []struct {
		name          string
		state, reason string
		setup         func(test *testing.T, installationID int64) (budgetCase, pgtype.UUID)
	}{
		{"run_queued", "run_queued", "", func(test *testing.T, installation int64) (budgetCase, pgtype.UUID) {
			c, id := intentCase(test, installation)
			linkRefusal(test, id, resumeRowOf(test, id).Window)
			raiseCap(test, c)
			return c, id
		}},
		{"status_restored", "status_restored", "", func(test *testing.T, installation int64) (budgetCase, pgtype.UUID) {
			c, id := blockedCase(test, installation)
			linkRefusal(test, id, resumeRowOf(test, id).Window)
			raiseCap(test, c)
			return c, id
		}},
		{"still_blocked", "still_blocked", "runs", func(test *testing.T, installation int64) (budgetCase, pgtype.UUID) {
			c, id := reviewBlockedCase(test, installation)
			linkRefusal(test, id, resumeRowOf(test, id).Window)
			return c, id // the budget is still spent
		}},
		{"superseded", "superseded", "window_changed", func(test *testing.T, installation int64) (budgetCase, pgtype.UUID) {
			c, id := blockedCase(test, installation)
			linkRefusal(test, id, resumeRowOf(test, id).Window)
			topUp(test, c)
			return c, id
		}},
	}
	for caseIndex, c := range cases {
		for pathIndex, path := range resumePaths {
			test.Run(c.name+"/"+path.name, func(test *testing.T) {
				_, id := c.setup(test, int64(6947100+caseIndex*10+pathIndex))
				card := executedCard(test) // after the case: its fixture numbers issues by hand
				pointRefusalAt(test, id, card)
				seen := watchCardUpdates(test, card.issueID)
				path.run(test, id)
				wantResume(test, id, c.state, c.reason)
				wantCardUpdate(test, seen(), card.requestID)
				if receipt := card.cardAction(test).Receipt; receipt == nil || receipt.Outcome != c.state+withColon(c.reason) {
					test.Fatalf("card receipt = %+v; want the worker's outcome %s", receipt, c.state+withColon(c.reason))
				}
				path.run(test, id) // a second resume finds the refusal done and says nothing more
				if updates := seen(); len(updates) != 1 {
					test.Fatalf("events after a second resume = %d, want still 1", len(updates))
				}
			})
		}
	}
}

func withColon(reason string) string {
	if reason == "" {
		return ""
	}
	return ":" + reason
}

// A resume that waits for the pending run changed nothing, so it says nothing.
func TestRunBudgetResumeHeldBySurvivorPublishesNothing(test *testing.T) {
	path := bindPaths[1] // mention
	b := newBindCase(test, 6947190, path, true)
	card := executedCard(test) // after the case: its fixture numbers issues by hand
	pointRefusalAt(test, b.id, card)
	b.survivor(test, path, "dispatched", b.own(path))
	seen := watchCardUpdates(test, card.issueID)
	resume(test, b.id)
	wantResume(test, b.id, "resume_pending", "")
	if updates := seen(); len(updates) != 0 {
		test.Fatalf("events = %+v; want none while the refusal waits", updates)
	}
}
