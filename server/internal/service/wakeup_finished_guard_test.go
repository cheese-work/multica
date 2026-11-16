package service

import (
	"context"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// CHE-6299: the weekly run guard refuses a PR system wakeup on a finished
// issue. The refusal rolled the dispatch back and left the receipt pending, so
// the scheduler re-dispatched it every pass and each pass refused again. A
// finished issue consumes the receipt; an unfinished one keeps it.
func TestRefusedPRWakeupOnFinishedIssueConsumesItsReceipt(t *testing.T) {
	for _, tc := range []struct {
		status      string
		wantPending int
	}{
		{"done", 0},
		{"in_progress", 1},
	} {
		t.Run(tc.status, func(t *testing.T) {
			t.Setenv("MULTICA_WEEKLY_RUN_GUARD_MAX_RUNS", "1")
			e := newBuiltinEnv(t)
			e.f.Task(t, e.agent, testutil.Cols{"issue_id": e.issue, "status": "completed",
				"runtime_id": testutil.Raw("(SELECT runtime_id FROM agent WHERE id='" + e.agent + "')")})
			e.f.Exec(t, `UPDATE issue SET status=$2 WHERE id=$1`, e.issue, tc.status)

			_ = e.s.TriggerPullRequestWakeup(context.Background(), e.issue, PullRequestWakeupInput{Rule: SystemRulePRChecksFailed,
				RepoOwner: "acme", RepoName: "widget", Number: 300, URL: "https://github.com/acme/widget/pull/300", HeadSHA: "red-1", Conclusion: "FAILURE"})

			if got := e.tasks(t, SystemRulePRChecksFailed); got != 0 {
				t.Fatalf("runs started = %d, want 0 (the guard refuses)", got)
			}
			if got := e.unreserved(t, SystemRulePRChecksFailed); got != tc.wantPending {
				t.Fatalf("pending receipts = %d, want %d", got, tc.wantPending)
			}
		})
	}
}
