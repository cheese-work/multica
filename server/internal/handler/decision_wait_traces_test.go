package handler

import (
	"testing"
	"time"
)

func (f episodeFixture) cancelByLifecycle(t *testing.T, requestID string) {
	t.Helper()
	if _, err := testHandler.CancelDecisionLifecycle(t.Context(), parseUUID(testWorkspaceID), parseUUID(requestID)); err != nil {
		t.Fatal(err)
	}
}

// Design §9.9 trace A: the lifecycle cancels the request while the agent is
// archived; the agent is restored before the episode acts. The episode closes
// system_cancelled with one continuation and the saved placement.
func TestDecisionEpisodeTraceAArchivedThenRestored(t *testing.T) {
	f := newEpisodeFixture(t, false)
	f.enter(t)
	dbfx.Exec(t, `UPDATE agent SET archived_at = now() WHERE id = $1`, f.agentID)
	t.Cleanup(func() { dbfx.Exec(t, `UPDATE agent SET archived_at = NULL WHERE id = $1`, f.agentID) })
	f.cancelByLifecycle(t, f.requestID)
	if status, reason := f.requestState(t, f.requestID); status != "cancelled" || reason != "agent_archived" {
		t.Fatalf("request = %s/%s, want cancelled/agent_archived", status, reason)
	}
	if len(f.closedEpisodes(t)) != 0 {
		t.Fatal("the lifecycle closed the episode")
	}

	dbfx.Exec(t, `UPDATE agent SET archived_at = NULL WHERE id = $1`, f.agentID)
	f.deliverEpisodeOnly(t)

	f.wantOneContinuation(t, "system_cancelled")
	if status, assigneeType, assigneeID := issueStatusAndAssignee(t, f.issueID); status != "in_progress" || assigneeType != "agent" || assigneeID != f.agentID {
		t.Fatalf("issue = %s %s/%s, want the restored placement", status, assigneeType, assigneeID)
	}
}

// Design §9.10 trace B: the issue closes, the lifecycle cancels the request, a
// person reopens the issue (the episode suspends, flag false), then the episode
// acts: one system_cancelled continuation and no status or assignee write.
func TestDecisionEpisodeTraceBClosedThenReopened(t *testing.T) {
	f := newEpisodeFixture(t, false)
	f.enter(t)
	f.closeIssue(t, "done")
	f.cancelByLifecycle(t, f.requestID)
	if status, reason := f.requestState(t, f.requestID); status != "cancelled" || reason != "issue_closed" {
		t.Fatalf("request = %s/%s, want cancelled/issue_closed", status, reason)
	}

	f.memberMove(t, "in_progress").Want(200)
	if s := f.suspension(t); s.phase != "suspended" || s.reason != "status_moved" || s.flag {
		t.Fatalf("after the reopen = %+v, want suspended status_moved with the flag clear", s)
	}
	before := dbfx.Count(t, `SELECT count(*) FROM activity_log WHERE issue_id = $1 AND action IN ('status_changed', 'assignee_changed')`, f.issueID)

	f.deliverEpisodeOnly(t)

	f.wantOneContinuation(t, "system_cancelled")
	if after := dbfx.Count(t, `SELECT count(*) FROM activity_log WHERE issue_id = $1 AND action IN ('status_changed', 'assignee_changed')`, f.issueID); after != before {
		t.Fatalf("the resume wrote %d status or assignee changes, want none", after-before)
	}
}

// A withdrawn answer reopens the request: the episode keeps waiting until the
// human answers again, then delivers once.
func TestDecisionEpisodeWithdrawnAnswerWaitsForTheNewAnswer(t *testing.T) {
	f := newEpisodeFixture(t, false)
	f.enter(t)
	answerID, _ := f.answerRequest(t, f.requestID, -time.Minute, false)
	dbfx.Exec(t, `UPDATE decision_answer SET disclaimed_at = now() WHERE id = $1`, answerID)
	dbfx.Exec(t, `UPDATE decision_request SET status = 'open' WHERE id = $1`, f.requestID)

	f.deliverEpisodeOnly(t)
	if len(f.closedEpisodes(t)) != 0 || f.continuations(t) != 0 {
		t.Fatal("the episode acted on a withdrawn answer")
	}

	newID, _ := f.answerRequest(t, f.requestID, -time.Minute, false)
	f.deliverEpisodeOnly(t)
	f.wantOneContinuation(t, "answered")
	if woken, _ := f.answerState(t, newID); !woken {
		t.Fatal("the new answer was not acknowledged")
	}
	if woken, _ := f.answerState(t, answerID); woken {
		t.Fatal("the withdrawn answer was acknowledged")
	}
}
