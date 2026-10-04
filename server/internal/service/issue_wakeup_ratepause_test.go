package service

import (
	"context"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// joinedPRRunWithConcurrentPause reproduces the claim/start interleaving: a
// PR fact rides a claimed run, the rule is paused for pausedReason before that
// run starts, and the rule settles the joined fact once the run has started.
func joinedPRRunWithConcurrentPause(t *testing.T, pausedReason string, recentRuns int) db.IssueWakeup {
	t.Helper()
	f, s, issue, agent := wakeFixture(t)
	ctx := context.Background()
	assignPRWakeupIssue(t, f, issue, agent)
	waiting := wakeWaitingRun(t, f, issue, agent, f.UserID)
	if err := s.TriggerPullRequestWakeup(ctx, issue, PullRequestWakeupInput{
		Rule: SystemRulePRChecksFailed, RepoOwner: "acme", RepoName: "widget", Number: 157,
		URL: "https://github.com/acme/widget/pull/157", HeadSHA: "head-joined", Conclusion: "FAILURE",
	}); err != nil {
		t.Fatal(err)
	}
	wakeClaim(t, f, s, waiting)
	w, err := f.q.GetSystemWakeup(ctx, db.GetSystemWakeupParams{IssueID: issue, SystemRule: systemRuleText(SystemRulePRChecksFailed)})
	if err != nil {
		t.Fatal(err)
	}
	if pausedReason == "" {
		// A person turned the rule off: disabled, no platform pause reason.
		f.Exec(t, "UPDATE issue_wakeup SET enabled=false,next_fire_at=NULL,disabled_at=clock_timestamp() WHERE id=$1", w.ID)
	} else if err := f.q.PauseIssueWakeup(ctx, db.PauseIssueWakeupParams{ID: w.ID, PausedReason: pgtype.Text{String: pausedReason, Valid: true}, BlockRuns: pausedReason != wakeupPausedMaxFires}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < recentRuns; i++ {
		f.Task(t, agent, testutil.Cols{"issue_id": issue, "status": "completed", "context": fmt.Sprintf(`{"wakeup_id":%q}`, util.UUIDToString(w.ID)),
			"runtime_id": testutil.Raw("(SELECT runtime_id FROM agent WHERE id='" + agent + "')")})
	}
	wakeStart(t, f, waiting)
	if w, err = f.q.GetSystemWakeup(ctx, db.GetSystemWakeupParams{IssueID: issue, SystemRule: systemRuleText(SystemRulePRChecksFailed)}); err != nil {
		t.Fatal(err)
	}
	if err := s.dispatchSystem(ctx, w); err != nil {
		t.Fatal(err)
	}
	if w, err = f.q.GetSystemWakeup(ctx, db.GetSystemWakeupParams{IssueID: issue, SystemRule: systemRuleText(SystemRulePRChecksFailed)}); err != nil {
		t.Fatal(err)
	}
	return w
}

func TestJoinedSettlementClearsRatePauseWhenLimitAllows(t *testing.T) {
	w := joinedPRRunWithConcurrentPause(t, wakeupPausedRate, 0)
	if !w.Enabled || w.PausedReason.Valid || w.DisabledAt.Valid {
		t.Fatalf("after valid resume: enabled=%t reason=%q disabled_at=%v; want enabled with both cleared", w.Enabled, w.PausedReason.String, w.DisabledAt)
	}
}

func TestJoinedSettlementKeepsRatePauseWhileLimitHolds(t *testing.T) {
	w := joinedPRRunWithConcurrentPause(t, wakeupPausedRate, wakeupHourlyRunLimit)
	if w.Enabled || w.PausedReason.String != wakeupPausedRate || !w.DisabledAt.Valid {
		t.Fatalf("limit still reached: enabled=%t reason=%q disabled_at=%v; want rate-paused and unchanged", w.Enabled, w.PausedReason.String, w.DisabledAt)
	}
}

func TestJoinedSettlementPreservesNonRatePauses(t *testing.T) {
	for _, reason := range []string{"", wakeupPausedLoop, wakeupPausedMaxFires} {
		name := reason
		if name == "" {
			name = "manual"
		}
		t.Run(name, func(t *testing.T) {
			w := joinedPRRunWithConcurrentPause(t, reason, 0)
			if w.Enabled || w.PausedReason.String != reason || !w.DisabledAt.Valid == (reason == "" || reason == wakeupPausedLoop) {
				t.Fatalf("%q pause: enabled=%t reason=%q disabled_at=%v; want paused and preserved", reason, w.Enabled, w.PausedReason.String, w.DisabledAt)
			}
		})
	}
}

// A rate-paused rule keeps its pending facts; once the rolling window frees
// up, the scheduler tick alone resumes it and starts the run.
func TestSchedulerResumesRatePausedRuleWithoutAnotherPREvent(t *testing.T) {
	f, s, issue, agent := wakeFixture(t)
	ctx := context.Background()
	assignPRWakeupIssue(t, f, issue, agent)
	if err := s.TriggerPullRequestWakeup(ctx, issue, PullRequestWakeupInput{
		Rule: SystemRulePRChecksFailed, RepoOwner: "acme", RepoName: "widget", Number: 157,
		URL: "https://github.com/acme/widget/pull/157", HeadSHA: "head-recover", Conclusion: "FAILURE",
	}); err != nil {
		t.Fatal(err)
	}
	w, err := f.q.GetSystemWakeup(ctx, db.GetSystemWakeupParams{IssueID: issue, SystemRule: systemRuleText(SystemRulePRChecksFailed)})
	if err != nil {
		t.Fatal(err)
	}
	// The first firing queued a run; settle it, then rate-pause with the fact pending.
	f.Exec(t, "UPDATE agent_task_queue SET status='completed',completed_at=clock_timestamp() WHERE context->>'wakeup_id'=$1", util.UUIDToString(w.ID))
	if err := f.q.PauseIssueWakeup(ctx, db.PauseIssueWakeupParams{ID: w.ID, PausedReason: pgtype.Text{String: wakeupPausedRate, Valid: true}, BlockRuns: true}); err != nil {
		t.Fatal(err)
	}
	f.Exec(t, "UPDATE agent_task_queue SET created_at=now()-interval '2 hours' WHERE context->>'wakeup_id'=$1", util.UUIDToString(w.ID))
	f.Exec(t, `INSERT INTO issue_wakeup_receipt(id,wakeup_id,revision,event_key,event_type,payload) VALUES(gen_random_uuid(),$1,$2,'pending-after-pause','pr.checks_failed','{}')`, w.ID, w.Revision)
	if err := s.TickWorkspaces(ctx, parseTestUUID(t, f.WorkspaceID)); err != nil {
		t.Fatal(err)
	}
	if w, err = f.q.GetSystemWakeup(ctx, db.GetSystemWakeupParams{IssueID: issue, SystemRule: systemRuleText(SystemRulePRChecksFailed)}); err != nil {
		t.Fatal(err)
	}
	if !w.Enabled || w.PausedReason.Valid || w.DisabledAt.Valid {
		t.Fatalf("after tick: enabled=%t reason=%q disabled_at=%v; want resumed with both cleared", w.Enabled, w.PausedReason.String, w.DisabledAt)
	}
	if got := f.Count(t, "SELECT count(*) FROM agent_task_queue WHERE context->>'wakeup_id'=$1 AND status='queued'", util.UUIDToString(w.ID)); got != 1 {
		t.Fatalf("queued wake runs after tick = %d, want 1", got)
	}
}
