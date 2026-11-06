package handler

import (
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/service"
)

func (f episodeFixture) raiseBudget(t *testing.T) {
	t.Helper()
	dbfx.Exec(t, `UPDATE issue_workflow SET run_budget_since = clock_timestamp()
		WHERE id = (SELECT workflow_id FROM project WHERE id = (SELECT project_id FROM issue WHERE id = $1))`, f.issueID)
}

// Design §9.7: run_budget -> dispatch_refused keeps the saved target and the
// flag, rewrites the block reason, posts one more comment, and the episode
// resumes once the conflicting task ends.
func TestDecisionEpisodeKindChangeBudgetToDispatchRefused(t *testing.T) {
	f := newEpisodeFixture(t, false)
	f.enter(t)
	answerID, _ := f.answerRequest(t, f.requestID, -time.Minute, false)
	f.exhaustBudget(t)
	f.deliverEpisodeOnly(t)
	service.WaitRunBudgetHooks()
	task := f.conflictingTask(t)
	f.raiseBudget(t)

	f.deliverEpisodeOnly(t)

	s := f.suspension(t)
	if s.reason != "dispatch_refused" || s.refusedTask != task || !s.flag || s.restoreStatus != "in_progress" || s.issueStatus != "blocked" || s.blockedReason != "decision-dispatch-refused" {
		t.Fatalf("episode = %+v, want dispatch_refused on %s with the original target, the flag and the new block reason", s, task)
	}
	if got := len(f.suspensionComments(t)); got != 2 {
		t.Fatalf("suspension comments = %d, want 2 (one per kind)", got)
	}

	// The ended task is removed so only the resume's own run is counted.
	dbfx.Exec(t, `DELETE FROM agent_task_queue WHERE id = $1`, task)
	f.deliverEpisodeOnly(t)
	f.wantOneContinuation(t, "answered")
	if status, assigneeType, assigneeID := issueStatusAndAssignee(t, f.issueID); status != "in_progress" || assigneeType != "agent" || assigneeID != f.agentID {
		t.Fatalf("issue = %s %s/%s, want the restored placement", status, assigneeType, assigneeID)
	}
	if woken, outcome := f.answerState(t, answerID); !woken || outcome != "run_enqueued" {
		t.Fatalf("answer woken=%v outcome=%q, want run_enqueued", woken, outcome)
	}
}

// dispatch_refused -> run_budget clears refused_task_id.
func TestDecisionEpisodeKindChangeDispatchRefusedToBudget(t *testing.T) {
	f := newEpisodeFixture(t, false)
	f.enter(t)
	f.answerRequest(t, f.requestID, -time.Minute, false)
	task := f.conflictingTask(t)
	dbfx.Exec(t, `UPDATE decision_wait SET dispatch_refused_since = now() - interval '16 minutes' WHERE issue_id = $1 AND closed_at IS NULL`, f.issueID)
	f.deliverEpisodeOnly(t)
	if s := f.suspension(t); s.reason != "dispatch_refused" || s.refusedTask != task {
		t.Fatalf("setup = %+v, want dispatch_refused on %s", s, task)
	}

	dbfx.Exec(t, `UPDATE agent_task_queue SET status = 'completed' WHERE id = $1`, task)
	f.exhaustBudget(t)
	f.deliverEpisodeOnly(t)
	service.WaitRunBudgetHooks()

	s := f.suspension(t)
	if s.reason != "run_budget" || s.refusedTask != "" || !s.flag || s.issueStatus != "blocked" || s.blockedReason != "run-budget" {
		t.Fatalf("episode = %+v, want run_budget without a refused task, still on the episode's block", s)
	}
}

// status_moved -> dispatch_refused: the person's move set the target; the
// suspension write blocks the issue and arms the flag for the saved target.
func TestDecisionEpisodeKindChangeStatusMovedToDispatchRefused(t *testing.T) {
	f := newEpisodeFixture(t, false)
	f.enter(t)
	f.answerRequest(t, f.requestID, -time.Minute, false)
	f.memberMove(t, "in_progress").Want(200)
	task := f.conflictingTask(t)

	f.deliverEpisodeOnly(t)

	s := f.suspension(t)
	if s.reason != "dispatch_refused" || s.refusedTask != task || !s.flag || s.issueStatus != "blocked" {
		t.Fatalf("episode = %+v, want dispatch_refused with the flag armed", s)
	}
}

// Migration 987: leaving blocked by a person's move clears the dispatch-refused
// reason, like the run-budget reason.
func TestDecisionDispatchRefusedReasonClearsWhenTheIssueLeavesBlocked(t *testing.T) {
	f := newEpisodeFixture(t, false)
	f.enter(t)
	f.answerRequest(t, f.requestID, -time.Minute, false)
	f.conflictingTask(t)
	dbfx.Exec(t, `UPDATE decision_wait SET dispatch_refused_since = now() - interval '16 minutes' WHERE issue_id = $1 AND closed_at IS NULL`, f.issueID)
	f.deliverEpisodeOnly(t)
	if s := f.suspension(t); s.blockedReason != "decision-dispatch-refused" {
		t.Fatalf("setup blocked_reason = %q", s.blockedReason)
	}

	f.memberMove(t, "todo").Want(200)

	if s := f.suspension(t); s.issueStatus != "todo" || s.blockedReason != "" {
		t.Fatalf("after the move = %s reason %q, want todo with the reason cleared", s.issueStatus, s.blockedReason)
	}
}

// Design §6 rule 3: a suspended episode whose resume condition is false waits,
// even when a bound request has already ended; the person's move resumes it and
// the episode closes with that request's reason.
func TestDecisionSuspendedEpisodeWaitsForItsResumeCondition(t *testing.T) {
	f := newEpisodeFixture(t, false)
	f.enter(t)
	f.answerRequest(t, f.requestID, -time.Minute, false)
	f.exhaustBudget(t)
	f.deliverEpisodeOnly(t)
	service.WaitRunBudgetHooks()
	f.cancelRequest(t, f.requestID, "expired")

	f.deliverEpisodeOnly(t)
	if len(f.closedEpisodes(t)) != 0 || f.continuations(t) != 0 {
		t.Fatal("a suspended episode acted before its resume condition held")
	}

	f.memberMove(t, "in_progress").Want(200)
	f.raiseBudget(t)
	f.deliverEpisodeOnly(t)
	f.wantOneContinuation(t, "expired")
}
