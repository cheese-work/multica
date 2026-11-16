package service

import (
	"context"
	"testing"
)

// CHE-6188 (CHE-6165 fix 2a): the PR no-change snapshot holds the issue status
// and assignee. A merge and a merge-queue removal are events: a comparison
// cannot see them, so they always run.

func (e skipEnv) event(t *testing.T, in PullRequestWakeupInput) {
	t.Helper()
	in.RepoOwner, in.RepoName, in.Number, in.URL = "acme", "widget", 157, "https://github.com/acme/widget/pull/157"
	if err := e.s.TriggerPullRequestWakeup(context.Background(), e.issue, in); err != nil {
		t.Fatal(err)
	}
}

func (e skipEnv) checksPassed(t *testing.T, head string) {
	t.Helper()
	e.event(t, PullRequestWakeupInput{Rule: SystemRulePRChecksPassed, HeadSHA: head, Conclusion: "SUCCESS"})
}

func (e skipEnv) enablePassed(t *testing.T) {
	t.Helper()
	e.f.Exec(t, `UPDATE workspace SET settings='{"github_wake_on_ci_success":true}'::jsonb WHERE id=$1`, e.f.WorkspaceID)
}

// A green after a red on the same head is a new event (CHE-4938): the red in
// between is not in the snapshot, so a comparison cannot see it and the green
// is never skipped. A repeated green of one head is dropped by the per-head
// ledger before any comparison, so this rule is not in the skip list.
func TestChecksPassedAfterRedIsNeverSkipped(t *testing.T) {
	e := newSkipEnv(t, true)
	e.enablePassed(t)
	e.checksPassed(t, "h1")
	e.finishRuns(t, "completed")
	e.f.Exec(t, `INSERT INTO issue_wakeup_pr_event(wakeup_id,event_key) SELECT id, 'github:acme/widget#157:h1' FROM issue_wakeup WHERE issue_id=$1 AND system_rule='pr_checks_passed'`, e.issue)
	e.checksPassed(t, "h1")
	if got := e.systemRuns(t, SystemRulePRChecksPassed); got != 2 {
		t.Fatalf("checks-passed runs for a green after a red = %d, want 2", got)
	}
	if got := e.skipped(t); got != 0 {
		t.Fatalf("skip activities = %d, want 0", got)
	}
}

func TestPRWatchdogStillRunsWhenIssueStatusChanged(t *testing.T) {
	e := newSkipEnv(t, true)
	e.checksFailed(t, "e1")
	e.finishRuns(t, "completed")
	e.f.Exec(t, `UPDATE issue SET status='in_review' WHERE id=$1`, e.issue)
	e.checksFailed(t, "e2")
	if got := e.systemRuns(t, SystemRulePRChecksFailed); got != 2 {
		t.Fatalf("runs = %d, want 2", got)
	}
	if got := e.skipped(t); got != 0 {
		t.Fatalf("skip activities = %d, want 0", got)
	}
}

// A snapshot that differs only in PR lifecycle state is not unchanged.
func TestPRWatchdogStillRunsWhenOnlyPRLifecycleStateChanged(t *testing.T) {
	for _, state := range []string{"draft", "closed", "merged"} {
		t.Run(state, func(t *testing.T) {
			e := newSkipEnv(t, true)
			e.checksFailed(t, "e1")
			e.finishRuns(t, "completed")
			e.f.Exec(t, `UPDATE github_pull_request SET state=$2 WHERE id=$1`, e.prID, state)
			e.checksFailed(t, "e2")
			if got := e.systemRuns(t, SystemRulePRChecksFailed); got != 2 {
				t.Fatalf("runs after PR state %s = %d, want 2", state, got)
			}
		})
	}
}

// A merge with the same head, checks and mergeability as the last snapshot
// still starts a run for the merged handler; so does a merge-queue removal.
func TestMergedAndDequeuedEventsAreNeverSkipped(t *testing.T) {
	cases := []struct {
		name string
		in   PullRequestWakeupInput
	}{
		{"merged", PullRequestWakeupInput{Rule: SystemRulePRMerged, HeadSHA: "h1", MergeCommit: "m1"}},
		{"dequeued", PullRequestWakeupInput{Rule: SystemRulePRDequeued, HeadSHA: "h1", Reason: "checks_failed", EventAt: "2026-11-16T01:00:00Z"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newSkipEnv(t, true)
			e.checksFailed(t, "e1")
			e.finishRuns(t, "completed")
			// Seed the handler's own last run with the current snapshot, so a
			// comparison would call this fire unchanged.
			var fp string
			e.f.QueryRow(t, `SELECT context->>'wakeup_input_fingerprint' FROM agent_task_queue WHERE issue_id=$1`, e.issue).Scan(&fp)
			e.f.Exec(t, `INSERT INTO agent_task_queue (agent_id, issue_id, status, context, completed_at, runtime_id)
				SELECT agent_id, issue_id, 'completed', jsonb_build_object('wakeup_system', $2::text, 'wakeup_input_fingerprint', $3::text), now(), runtime_id
				FROM agent_task_queue WHERE issue_id=$1 LIMIT 1`, e.issue, tc.in.Rule, fp)
			e.event(t, tc.in)
			if got := e.systemRuns(t, tc.in.Rule); got != 2 {
				t.Fatalf("%s runs with an unchanged snapshot = %d, want 2 (seed + event)", tc.name, got)
			}
			if got := e.skipped(t); got != 0 {
				t.Fatalf("skip activities = %d, want 0", got)
			}
		})
	}
}
