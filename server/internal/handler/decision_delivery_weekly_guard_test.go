package handler

import (
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// CHE-6239: an answered decision card starts exactly one run for the assignee
// even when the issue is past the weekly run guard.
func TestDecisionAnswerWakesAssigneeOnWeeklyGuardExhaustedIssue(test *testing.T) {
	test.Setenv("MULTICA_WEEKLY_RUN_GUARD_MAX_RUNS", "30")
	resetDecisionLease(test)
	fixture, recorded := readyDecisionDelivery(test)
	for i := 0; i < 31; i++ {
		at := time.Now().Add(-2 * 24 * time.Hour).Add(time.Duration(i) * time.Second)
		dbfx.Task(test, fixture.agentID, testutil.Cols{"issue_id": fixture.issueID, "status": "completed", "created_at": at, "started_at": at, "completed_at": at, "trigger_evidence_kind": "comment"})
	}
	_, runner := newDecisionRunnerFixture(test)
	decisionPass(test, runner)
	want := func(n int, what string) {
		test.Helper()
		if got := dbfx.Count(test, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND agent_id = $2 AND status = 'queued' AND (trigger_comment_id = $3 OR $3 = ANY(coalesced_comment_ids))`, fixture.issueID, fixture.agentID, recorded.Comment.ID); got != n {
			test.Fatalf("%s: queued runs = %d, want %d", what, got, n)
		}
	}
	want(1, "first delivery")
	decisionPass(test, runner)
	want(1, "second pass")
}
