package service

import (
	"context"
	"testing"
)

func TestPRChecksPassedSettingIsOffUnlessEnabled(t *testing.T) {
	for _, tc := range []struct {
		name     string
		settings string
		want     bool
	}{
		{"unset", `{}`, false},
		{"empty", ``, false},
		{"enabled", `{"github_wake_on_ci_success":true}`, true},
		{"disabled", `{"github_wake_on_ci_success":false}`, false},
		{"master switch off", `{"github_enabled":false,"github_wake_on_ci_success":true}`, false},
		{"other rule's switch", `{"github_wake_on_ci_failure":true}`, false},
	} {
		got, err := PRWakeupEnabled([]byte(tc.settings), SystemRulePRChecksPassed)
		if err != nil || got != tc.want {
			t.Fatalf("%s: enabled = %v, %v; want %v", tc.name, got, err, tc.want)
		}
	}
	if enabled, err := PRWakeupEnabled([]byte(`{"github_wake_on_ci_success":true}`), SystemRulePRChecksFailed); err != nil || !enabled {
		t.Fatalf("CI failure rule must not read the success switch: %v, %v", enabled, err)
	}
}

func TestPRChecksPassedFiresOncePerHeadAndRearmsOnNewHead(t *testing.T) {
	f, s, issue, agent := wakeFixture(t)
	assignPRWakeupIssue(t, f, issue, agent)
	f.Exec(t, `UPDATE workspace SET settings='{"github_wake_on_ci_success":true}'::jsonb WHERE id=$1`, f.WorkspaceID)
	ctx := context.Background()
	input := PullRequestWakeupInput{
		Rule: SystemRulePRChecksPassed, RepoOwner: "acme", RepoName: "widget", Number: 201,
		URL: "https://github.com/acme/widget/pull/201", HeadSHA: "green-1", Conclusion: "SUCCESS",
	}
	runs := func() int {
		return f.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id=$1 AND context->>'wakeup_system'=$2`, issue, SystemRulePRChecksPassed)
	}
	for i := 0; i < 2; i++ {
		if err := s.TriggerPullRequestWakeup(ctx, issue, input); err != nil {
			t.Fatal(err)
		}
	}
	if got := runs(); got != 1 {
		t.Fatalf("green head delivered twice queued %d runs, want 1", got)
	}
	f.Exec(t, `UPDATE agent_task_queue SET status='completed',completed_at=clock_timestamp() WHERE issue_id=$1`, issue)
	input.HeadSHA = "green-2"
	if err := s.TriggerPullRequestWakeup(ctx, issue, input); err != nil {
		t.Fatal(err)
	}
	if got := runs(); got != 2 {
		t.Fatalf("new head queued %d runs in total, want 2", got)
	}
}

func TestPRChecksPassedIgnoresRedAndHonoursSetting(t *testing.T) {
	f, s, issue, agent := wakeFixture(t)
	assignPRWakeupIssue(t, f, issue, agent)
	ctx := context.Background()
	input := PullRequestWakeupInput{
		Rule: SystemRulePRChecksPassed, RepoOwner: "acme", RepoName: "widget", Number: 202,
		URL: "https://github.com/acme/widget/pull/202", HeadSHA: "head-a", Conclusion: "SUCCESS",
	}
	runs := func() int {
		return f.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id=$1 AND context->>'wakeup_system'=$2`, issue, SystemRulePRChecksPassed)
	}
	// Default off: nothing starts.
	if err := s.TriggerPullRequestWakeup(ctx, issue, input); err != nil {
		t.Fatal(err)
	}
	if got := runs(); got != 0 {
		t.Fatalf("rule off by default queued %d runs", got)
	}
	f.Exec(t, `UPDATE workspace SET settings='{"github_wake_on_ci_success":true}'::jsonb WHERE id=$1`, f.WorkspaceID)
	for _, red := range []string{"FAILURE", "ERROR", "PENDING", ""} {
		bad := input
		bad.Conclusion, bad.HeadSHA = red, "head-red"
		if err := s.TriggerPullRequestWakeup(ctx, issue, bad); err == nil {
			t.Fatalf("conclusion %q was accepted as a passing input", red)
		}
	}
	if got := runs(); got != 0 {
		t.Fatalf("red input queued %d runs", got)
	}
	f.Exec(t, `UPDATE workspace SET settings='{"github_wake_on_ci_success":false}'::jsonb WHERE id=$1`, f.WorkspaceID)
	input.HeadSHA = "head-b"
	if err := s.TriggerPullRequestWakeup(ctx, issue, input); err != nil {
		t.Fatal(err)
	}
	if got := runs(); got != 0 {
		t.Fatalf("setting off queued %d runs", got)
	}
}
