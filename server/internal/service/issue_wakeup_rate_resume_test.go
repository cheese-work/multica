package service

import (
	"context"
	"fmt"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// ratePausedWithJoinedRun leaves a PR rule rate-paused while a run that joined
// its input starts: the claim and the pause interleave, then settlement runs.
// fillers is how many recent rule runs count toward the hourly limit.
func ratePausedWithJoinedRun(t *testing.T, fillers int) (principalFixture, *IssueWakeupService, db.IssueWakeup) {
	t.Helper()
	f, s, issue, agent := wakeFixture(t)
	assignPRWakeupIssue(t, f, issue, agent)
	ctx := context.Background()
	waiting := wakeWaitingRun(t, f, issue, agent, f.UserID)
	if err := s.TriggerPullRequestWakeup(ctx, issue, PullRequestWakeupInput{
		Rule: SystemRulePRChecksFailed, RepoOwner: "acme", RepoName: "widget", Number: 158,
		URL: "https://github.com/acme/widget/pull/158", HeadSHA: "head-joined", Conclusion: "FAILURE",
	}); err != nil {
		t.Fatal(err)
	}
	w, err := f.q.GetSystemWakeup(ctx, db.GetSystemWakeupParams{IssueID: issue, SystemRule: systemRuleText(SystemRulePRChecksFailed)})
	if err != nil {
		t.Fatal(err)
	}
	wakeClaim(t, f, s, waiting)
	for i := 0; i < fillers; i++ {
		f.Task(t, agent, testutil.Cols{"issue_id": issue, "status": "completed", "runtime_id": testutil.Raw("(SELECT runtime_id FROM agent WHERE id='" + agent + "')"),
			"context": testutil.Raw(fmt.Sprintf(`'{"wakeup_id":%q}'::jsonb`, util.UUIDToString(w.ID)))})
	}
	if err := f.q.PauseIssueWakeup(ctx, db.PauseIssueWakeupParams{ID: w.ID, PausedReason: systemRuleText(wakeupPausedRate), BlockRuns: true}); err != nil {
		t.Fatal(err)
	}
	wakeStart(t, f, waiting)
	if err := s.TickWorkspaces(ctx, parseTestUUID(t, f.WorkspaceID)); err != nil {
		t.Fatal(err)
	}
	got, err := f.q.LocklessWakeup(ctx, w.ID)
	if err != nil {
		t.Fatal(err)
	}
	return f, s, got
}

// A joined run settling must not revive a rate-paused rule that is still over
// its limit: enabled, reason and disabled_at move together or not at all.
func TestJoinedSettlementKeepsRateOverLimitRulePaused(t *testing.T) {
	_, _, w := ratePausedWithJoinedRun(t, wakeupHourlyRunLimit)
	if w.Enabled || w.PausedReason.String != wakeupPausedRate || !w.DisabledAt.Valid {
		t.Fatalf("over-limit rule after settlement = enabled %v, pause %q, disabled_at %v; want paused with rate metadata intact", w.Enabled, w.PausedReason.String, w.DisabledAt.Valid)
	}
}

// Under the limit the scheduler resumes the rule on its own, clearing the
// pause metadata in one step, with no further PR event.
func TestJoinedSettlementResumesRateRuleUnderLimitCleanly(t *testing.T) {
	_, _, w := ratePausedWithJoinedRun(t, 0)
	if !w.Enabled || w.PausedReason.Valid || w.DisabledAt.Valid {
		t.Fatalf("under-limit rule after settlement = enabled %v, pause %q, disabled_at %v; want enabled with metadata cleared", w.Enabled, w.PausedReason.String, w.DisabledAt.Valid)
	}
}

// Only a rate pause is ever lifted by the platform; loop and max_fires pauses
// survive both the resume statement and progress bookkeeping.
func TestRateResumeNeverClearsOtherPauses(t *testing.T) {
	ctx := context.Background()
	for _, reason := range []string{wakeupPausedLoop, wakeupPausedMaxFires} {
		f, s, issue, agent := wakeFixture(t)
		assignPRWakeupIssue(t, f, issue, agent)
		if err := s.TriggerPullRequestWakeup(ctx, issue, PullRequestWakeupInput{
			Rule: SystemRulePRChecksFailed, RepoOwner: "acme", RepoName: "widget", Number: 159,
			URL: "https://github.com/acme/widget/pull/159", HeadSHA: "head-" + reason, Conclusion: "FAILURE",
		}); err != nil {
			t.Fatal(err)
		}
		w, err := f.q.GetSystemWakeup(ctx, db.GetSystemWakeupParams{IssueID: issue, SystemRule: systemRuleText(SystemRulePRChecksFailed)})
		if err != nil {
			t.Fatal(err)
		}
		if err := f.q.PauseIssueWakeup(ctx, db.PauseIssueWakeupParams{ID: w.ID, PausedReason: systemRuleText(reason), BlockRuns: true}); err != nil {
			t.Fatal(err)
		}
		n, err := f.q.ResumeRateLimitedSystemWakeup(ctx, db.ResumeRateLimitedSystemWakeupParams{ID: w.ID, MaxRuns: wakeupHourlyRunLimit})
		if err != nil || n != 0 {
			t.Fatalf("%s pause resumed by rate recovery: rows %d, err %v", reason, n, err)
		}
		if err := f.q.AdvanceIssueWakeup(ctx, db.AdvanceIssueWakeupParams{ID: w.ID, Enabled: true}); err != nil {
			t.Fatal(err)
		}
		got, err := f.q.LocklessWakeup(ctx, w.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Enabled || got.PausedReason.String != reason || !got.DisabledAt.Valid {
			t.Fatalf("%s pause after advance = enabled %v, pause %q, disabled_at %v; want untouched", reason, got.Enabled, got.PausedReason.String, got.DisabledAt.Valid)
		}
	}
}
