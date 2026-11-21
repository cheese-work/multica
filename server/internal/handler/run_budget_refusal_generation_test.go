package handler

import (
	"context"
	"testing"
)

// An approval answers the occurrence its card was opened for (CHE-6198). A
// status move that opens no window supersedes the refusal; a later exhausted
// admission in the same window revives the row. The old approval must not
// link, and so must not resume, that later occurrence.
func TestRunBudgetOldApprovalDoesNotResumeARevivedRefusal(t *testing.T) {
	c, e := hookCase(t, 6198001)
	testHandler.onRunBudgetExhausted(context.Background(), e)
	finished(t, c)
	var requestID, actionID, humanID string
	dbfx.QueryRow(t, `SELECT r.id::text, a.id::text, r.human_user_id::text FROM decision_request r JOIN decision_action a ON a.decision_request_id = r.id WHERE r.issue_id = $1`, c.issueID).Scan(&requestID, &actionID, &humanID)
	dbfx.Exec(t, `UPDATE member SET role = 'owner' WHERE workspace_id = $1 AND user_id = $2`, testWorkspaceID, humanID)
	dbfx.Cleanup(t, `DELETE FROM decision_answer WHERE decision_request_id = $1`, requestID)
	ids := refusalIDs(t, c)
	if len(ids) != 1 {
		t.Fatalf("refusal rows = %d, want 1", len(ids))
	}
	id := ids[0]

	// A noninteractive move supersedes the refusal and opens no window; the
	// next exhausted admission of the same window revives the row.
	c.enter(t, "in_progress")
	if err := testHandler.Queries.SupersedeRunBudgetRefusalsOnStatusChange(context.Background(), parseUUID(c.issueID)); err != nil {
		t.Fatal(err)
	}
	crashAt(t, "after_intent")
	testHandler.onRunBudgetExhausted(context.Background(), e)
	if got := refusalIDs(t, c); len(got) != 1 {
		t.Fatalf("refusal rows = %d after the later admission, want the window's single row", len(got))
	}
	before := wantResume(t, id, "open", "")

	card := pendingAction{singleCallFixture{intentFixture: intentFixture{issueID: c.issueID, humanID: humanID, agentID: c.agentID}}, requestID, actionID}
	card.approve(t, withVerifier(acceptingVerifier), card.claim(t, nil), 200)

	after := resumeRowOf(t, id)
	if after.State != "open" || after.ActionID.Valid || after.Phase != before.Phase {
		t.Fatalf("revived refusal after the old approval = %+v, want it untouched (open, unlinked, phase %s)", after, before.Phase)
	}
	if n := resumeQueued(t, c); n != 0 {
		t.Fatalf("queued runs = %d, want the old approval to resume nothing", n)
	}
}
