package service

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
)

// Workspace wakeup pause (CHE-1727): while workspace.settings.wakeups_paused is
// true nothing fires; what arrives meanwhile is dropped, not replayed.

func setWakeupsPaused(t *testing.T, f principalFixture, paused bool) {
	t.Helper()
	if paused {
		f.Exec(t, `UPDATE workspace SET settings=COALESCE(settings,'{}'::jsonb)||'{"wakeups_paused":true}'::jsonb WHERE id=$1`, f.WorkspaceID)
		return
	}
	f.Exec(t, `UPDATE workspace SET settings=COALESCE(settings,'{}'::jsonb)-'wakeups_paused' WHERE id=$1`, f.WorkspaceID)
}

func tickWorkspace(t *testing.T, f principalFixture, s *IssueWakeupService) {
	t.Helper()
	if err := s.TickWorkspaces(context.Background(), parseTestUUID(t, f.WorkspaceID)); err != nil {
		t.Fatal(err)
	}
}

func TestPausedWorkspaceDropsEventInputsWithoutReplay(t *testing.T) {
	f, s, issue, agent := wakeFixture(t)
	w := wakeCreate(t, f, s, issue, WakeupInput{AgentID: agent, Kind: "event", Mode: "continuous", EventTypes: []string{"comment.created"}, Instruction: "Look at it"})
	setWakeupsPaused(t, f, true)
	f.Comment(t, util.UUIDToString(issue), "arrives while paused")
	wakeDispatch(t, s, w)
	if n := wakeRuns(t, f, w.ID); n != 0 {
		t.Fatalf("paused workspace started %d runs", n)
	}
	if n := f.Count(t, `SELECT count(*) FROM issue_wakeup_receipt WHERE wakeup_id=$1 AND processed_at IS NOT NULL AND payload->>'outcome'='paused'`, w.ID); n != 1 {
		t.Fatalf("receipts consumed with outcome paused = %d, want 1", n)
	}
	setWakeupsPaused(t, f, false)
	wakeDispatch(t, s, w)
	if n := wakeRuns(t, f, w.ID); n != 0 {
		t.Fatalf("resume replayed %d dropped inputs", n)
	}
	f.Comment(t, util.UUIDToString(issue), "arrives after resume")
	wakeDispatch(t, s, w)
	if n := wakeRuns(t, f, w.ID); n != 1 {
		t.Fatalf("after resume the rule started %d runs, want 1", n)
	}
}

func TestPausedWorkspaceFiresOverdueOneShotOnceAfterResume(t *testing.T) {
	f, s, issue, agent := wakeFixture(t)
	w := wakeCreate(t, f, s, issue, WakeupInput{AgentID: agent, Kind: "at", AfterSeconds: 3600, Instruction: "once"})
	f.Exec(t, "UPDATE issue_wakeup SET next_fire_at=now()-interval '1 hour' WHERE id=$1", w.ID)
	setWakeupsPaused(t, f, true)
	tickWorkspace(t, f, s)
	if n := wakeRuns(t, f, w.ID); n != 0 {
		t.Fatalf("paused workspace started %d runs", n)
	}
	if n := f.Count(t, "SELECT count(*) FROM issue_wakeup WHERE id=$1 AND enabled AND next_fire_at<now()", w.ID); n != 1 {
		t.Fatal("pause must keep the overdue one-shot armed")
	}
	setWakeupsPaused(t, f, false)
	tickWorkspace(t, f, s)
	tickWorkspace(t, f, s)
	if n := wakeRuns(t, f, w.ID); n != 1 {
		t.Fatalf("overdue one-shot fired %d times after resume, want 1", n)
	}
}

func TestPausedWorkspaceSkipsRecurringSlots(t *testing.T) {
	f, s, issue, agent := wakeFixture(t)
	w := wakeCreate(t, f, s, issue, WakeupInput{AgentID: agent, Kind: "every", IntervalSeconds: 3600, Instruction: "tick"})
	f.Exec(t, "UPDATE issue_wakeup SET next_fire_at=now()-interval '3 hours' WHERE id=$1", w.ID)
	setWakeupsPaused(t, f, true)
	tickWorkspace(t, f, s)
	if n := wakeRuns(t, f, w.ID); n != 0 {
		t.Fatalf("paused workspace started %d runs", n)
	}
	if n := f.Count(t, "SELECT count(*) FROM issue_wakeup WHERE id=$1 AND enabled AND next_fire_at>now()", w.ID); n != 1 {
		t.Fatal("missed slots must be skipped: next fire belongs in the future")
	}
	setWakeupsPaused(t, f, false)
	tickWorkspace(t, f, s)
	if n := wakeRuns(t, f, w.ID); n != 0 {
		t.Fatalf("resume replayed %d skipped slots", n)
	}
}

func TestPausedWorkspaceHoldsConditionUntilResume(t *testing.T) {
	f, s, issue, agent := conditionFixture(t)
	w := wakeCreate(t, f, s, issue, WakeupInput{AgentID: agent, Kind: "event", Instruction: "Review when ready",
		Condition: condition(t, map[string]any{"type": "issue_field", "field": "status", "value": "in_review"})})
	setWakeupsPaused(t, f, true)
	wakeSetStatus(t, f, issue, "in_review")
	f.Exec(t, "UPDATE issue_wakeup SET next_fire_at=now()-interval '1 second' WHERE id=$1", w.ID)
	tickWorkspace(t, f, s)
	if n := wakeRuns(t, f, w.ID); n != 0 {
		t.Fatalf("paused workspace started %d runs", n)
	}
	setWakeupsPaused(t, f, false)
	f.Exec(t, "UPDATE issue_wakeup SET next_fire_at=now()-interval '1 second' WHERE id=$1", w.ID)
	tickWorkspace(t, f, s)
	if n := wakeRuns(t, f, w.ID); n != 1 {
		t.Fatalf("condition that still holds fired %d times after resume, want 1", n)
	}
}

func TestPausedWorkspaceDoesNotAffectOtherWorkspaces(t *testing.T) {
	pf, ps, pIssue, pAgent := wakeFixture(t)
	of, os, oIssue, oAgent := wakeFixture(t)
	paused := wakeCreate(t, pf, ps, pIssue, WakeupInput{AgentID: pAgent, Kind: "at", AfterSeconds: 3600, Instruction: "paused"})
	other := wakeCreate(t, of, os, oIssue, WakeupInput{AgentID: oAgent, Kind: "at", AfterSeconds: 3600, Instruction: "other"})
	pf.Exec(t, "UPDATE issue_wakeup SET next_fire_at=now()-interval '1 hour' WHERE id=$1", paused.ID)
	of.Exec(t, "UPDATE issue_wakeup SET next_fire_at=now()-interval '1 hour' WHERE id=$1", other.ID)
	setWakeupsPaused(t, pf, true)
	both := []pgtype.UUID{parseTestUUID(t, pf.WorkspaceID), parseTestUUID(t, of.WorkspaceID)}
	if err := ps.TickWorkspaces(context.Background(), both...); err != nil {
		t.Fatal(err)
	}
	if n := wakeRuns(t, pf, paused.ID); n != 0 {
		t.Fatalf("paused workspace started %d runs", n)
	}
	if n := wakeRuns(t, of, other.ID); n != 1 {
		t.Fatalf("unpaused workspace started %d runs, want 1", n)
	}
}

func TestPausedWorkspaceStillAllowsManualTrigger(t *testing.T) {
	f, s, issue, agent := wakeFixture(t)
	w := wakeCreate(t, f, s, issue, WakeupInput{AgentID: agent, Kind: "event", Mode: "continuous", EventTypes: []string{"comment.created"}, Instruction: "Look at it"})
	setWakeupsPaused(t, f, true)
	if err := s.Trigger(context.Background(), issue, w.ID, parseTestUUID(t, f.UserID)); err != nil {
		t.Fatal(err)
	}
	if n := wakeRuns(t, f, w.ID); n != 1 {
		t.Fatalf("explicit human trigger started %d runs while paused, want 1", n)
	}
}

func TestPausedWorkspaceHoldsSystemPRRules(t *testing.T) {
	e := newBuiltinEnv(t)
	setWakeupsPaused(t, e.f, true)
	e.mergedPR(t, 41)
	if n := e.tasks(t, SystemRulePRMerged); n != 0 {
		t.Fatalf("paused workspace woke %d times for a merged PR", n)
	}
	setWakeupsPaused(t, e.f, false)
	if err := e.s.TickWorkspaces(context.Background(), parseTestUUID(t, e.f.WorkspaceID)); err != nil {
		t.Fatal(err)
	}
	if n := e.tasks(t, SystemRulePRMerged); n != 0 {
		t.Fatalf("resume replayed %d dropped PR wakeups", n)
	}
	e.mergedPR(t, 42)
	if n := e.tasks(t, SystemRulePRMerged); n != 1 {
		t.Fatalf("after resume a merged PR woke %d times, want 1", n)
	}
}
