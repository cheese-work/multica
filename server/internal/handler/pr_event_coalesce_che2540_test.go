package handler

import (
	"context"
	"testing"

	"github.com/multica-ai/multica/server/internal/integrations/ghsnapshot"
	"github.com/multica-ai/multica/server/internal/service"
)

// CHE-2540: one PR event wakes an issue once. These tests drive the real
// snapshot pipeline: capture in the snapshot transaction, the legacy PR rule
// from the applied callback, then the pr.event next-turn fallback.

type coalesceFixture struct {
	set     func(snap ghsnapshot.PRSnapshot)
	refresh func()
	sweep   func()
	runs    func() int
	issueID string
	agentID string
}

func newCoalesceFixture(t *testing.T, installationID int64, prNumber int32, head string) coalesceFixture {
	t.Helper()
	fx := newPRMergeWebhookFixture(t, installationID)
	agentID := createHandlerTestAgent(t, "CHE-2540 coalesce", nil)
	setIssueAssigneeDirect(t, fx.child.ID, "agent", agentID)
	pr := linkedPRForWakeup(t, fx, installationID, prNumber, head)
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM issue_pr_event WHERE pull_request_id=$1`, pr.ID)
	})
	setWorkspacePRWakeSetting(t, "github_enabled", true)

	current := ghsnapshot.PRSnapshot{HeadSHA: head, BaseRef: "main", BaseSHA: "base-1", Mergeable: "MERGEABLE", MergeStateStatus: "CLEAN"}
	refresh := startSnapshotRefresh(t, installationID, prNumber, func() ghsnapshot.PRSnapshot { return current })
	finish := func() {
		testPool.Exec(context.Background(), `UPDATE agent_task_queue SET status='completed',started_at=COALESCE(started_at,clock_timestamp()),completed_at=clock_timestamp() WHERE issue_id=$1 AND status IN ('queued','dispatched','running')`, fx.child.ID)
	}
	return coalesceFixture{
		set:     func(snap ghsnapshot.PRSnapshot) { current = snap },
		refresh: refresh,
		// An issue holds one run at a time, so a duplicate cause only shows once
		// the first run has started and ended.
		sweep: func() {
			for range 4 {
				finish()
				if err := (&service.IssueWakeupService{Tasks: testHandler.TaskService}).TickWorkspaces(context.Background(), parseUUID(testWorkspaceID)); err != nil {
					t.Fatal(err)
				}
				finish()
			}
		},
		issueID: fx.child.ID, agentID: agentID,
		runs: func() int {
			return prRuleWakeups(t, fx.child.ID, agentID, "pr_needs_attention") + prRuleWakeups(t, fx.child.ID, agentID, "pr_checks_failed")
		},
	}
}

func conflictingSnapshot(head, base string) ghsnapshot.PRSnapshot {
	return ghsnapshot.PRSnapshot{HeadSHA: head, BaseRef: "main", BaseSHA: base, Mergeable: "CONFLICTING", MergeStateStatus: "DIRTY"}
}

func failingSnapshot(head string) ghsnapshot.PRSnapshot {
	return ghsnapshot.PRSnapshot{HeadSHA: head, BaseRef: "main", BaseSHA: "base-1", Mergeable: "MERGEABLE", MergeStateStatus: "CLEAN", HasChecks: true, RollupState: "FAILURE"}
}

func setPREventCapture(t *testing.T, kind string, on bool) {
	t.Helper()
	var original []byte
	if err := testPool.QueryRow(context.Background(), `SELECT COALESCE(settings, '{}'::jsonb) FROM workspace WHERE id=$1`, testWorkspaceID).Scan(&original); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `UPDATE workspace SET settings=$2::jsonb WHERE id=$1`, testWorkspaceID, original)
	})
	if _, err := testPool.Exec(context.Background(),
		`UPDATE workspace SET settings=COALESCE(settings, '{}'::jsonb)||jsonb_build_object('pr_event_capture', jsonb_build_object($2::text, $3::boolean)) WHERE id=$1`,
		testWorkspaceID, kind, on); err != nil {
		t.Fatal(err)
	}
}

func TestSnapshotConflictWakesOnceThroughLegacyRuleAndFallback(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	fx := newCoalesceFixture(t, 254001, 254001, "che2540-conflict")
	fx.set(conflictingSnapshot("che2540-conflict", "base-1"))
	fx.refresh()
	fx.sweep()
	if got := fx.runs(); got != 1 {
		t.Fatalf("one conflict started %d runs, want 1", got)
	}
}

func TestSnapshotChecksFailedWakesOnceThroughLegacyRuleAndFallback(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	fx := newCoalesceFixture(t, 254002, 254002, "che2540-checks")
	fx.set(failingSnapshot("che2540-checks"))
	fx.refresh()
	fx.sweep()
	if got := fx.runs(); got != 1 {
		t.Fatalf("one failing head started %d runs, want 1", got)
	}
}

// The conflict clears and returns on a new base commit under the same head: a
// new event, while the legacy pair key (head and base branch) stays silent, so
// only the fallback carries it.
func TestSnapshotConflictOnNewBaseOfSameHeadWakesAgain(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	fx := newCoalesceFixture(t, 254003, 254003, "che2540-base")
	fx.set(conflictingSnapshot("che2540-base", "base-1"))
	fx.refresh()
	fx.sweep()
	fx.set(ghsnapshot.PRSnapshot{HeadSHA: "che2540-base", BaseRef: "main", BaseSHA: "base-2", Mergeable: "MERGEABLE", MergeStateStatus: "CLEAN"})
	fx.refresh()
	fx.set(conflictingSnapshot("che2540-base", "base-3"))
	fx.refresh()
	fx.sweep()
	if got := fx.runs(); got != 2 {
		t.Fatalf("a conflict on a new base started %d runs in total, want 2", got)
	}
}

// Guards, green before the change: with event capture off the legacy rule is
// the only carrier and still wakes once.
func TestSnapshotConflictWithCaptureOffWakesOnceThroughLegacyRule(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	setPREventCapture(t, "conflict", false)
	fx := newCoalesceFixture(t, 254004, 254004, "che2540-off")
	fx.set(conflictingSnapshot("che2540-off", "base-1"))
	fx.refresh()
	fx.sweep()
	if got := fx.runs(); got != 1 {
		t.Fatalf("legacy-only conflict started %d runs, want 1", got)
	}
	if got := dbfx.Count(t, `SELECT count(*) FROM issue_pr_event e JOIN github_pull_request pr ON pr.id=e.pull_request_id WHERE pr.pr_number=254004`); got != 0 {
		t.Fatalf("capture off stored %d events, want 0", got)
	}
}
