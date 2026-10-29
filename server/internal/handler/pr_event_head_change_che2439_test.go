package handler

import (
	"testing"

	"github.com/multica-ai/multica/server/internal/integrations/ghsnapshot"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// CHE-2439: a snapshot that observes a new head for a linked PR raises one
// head_changed event, delivered like the conflict and checks-failed ones.

const headChangeNewHead = "che2439-head-b"

// headChange plants the first snapshot of the fixture's head, as a webhook
// synchronize would move the PR row to the new head, then refreshes the new one.
func (f liveDeliveryFixture) headChange(t *testing.T) {
	t.Helper()
	f.refresh(t, ghsnapshot.PRSnapshot{HeadSHA: "che2436-head", BaseSHA: "base-1", Mergeable: "MERGEABLE", MergeStateStatus: "CLEAN"})
	dbfx.Exec(t, `UPDATE github_pull_request SET head_sha = $2, pr_updated_at = now() WHERE id = $1`, f.prID, headChangeNewHead)
	f.refresh(t, ghsnapshot.PRSnapshot{HeadSHA: headChangeNewHead, BaseSHA: "base-1", Mergeable: "MERGEABLE", MergeStateStatus: "CLEAN"})
}

func TestSnapshotHeadChangeReachesLiveRunWithPREventChannel(t *testing.T) {
	f := newLiveDeliveryFixture(t, "running", true)
	f.headChange(t)
	got, err := f.claim(t)
	if err != nil {
		t.Fatalf("ClaimNextTaskPREvent: %v", err)
	}
	if got.Kind != "head_changed" || got.HeadSha != headChangeNewHead || got.OldHeadSha != "che2436-head" {
		t.Fatalf("claimed %+v, want head_changed che2436-head -> %s", got, headChangeNewHead)
	}
}

func TestSnapshotHeadChangeRepeatRaisesNoSecondEvent(t *testing.T) {
	f := newLiveDeliveryFixture(t, "running", true)
	f.headChange(t)
	f.refresh(t, ghsnapshot.PRSnapshot{HeadSHA: headChangeNewHead, BaseSHA: "base-1", Mergeable: "MERGEABLE", MergeStateStatus: "CLEAN"})
	if got := dbfx.Count(t, `SELECT count(*) FROM issue_pr_event WHERE pull_request_id = $1 AND kind = 'head_changed'`, f.prID); got != 1 {
		t.Fatalf("head_changed events after a repeated snapshot = %d, want 1", got)
	}
	if got := f.deliveryRows(t); got != 1 {
		t.Fatalf("delivery rows = %d, want 1", got)
	}
}

func TestSnapshotHeadChangeWithNoCapableRunUsesNextTurnFallback(t *testing.T) {
	f := newLiveDeliveryFixture(t, "running", false)
	f.headChange(t)
	if got := f.deliveryRows(t); got != 0 {
		t.Fatalf("legacy run got %d delivery rows, want 0", got)
	}
	if kinds := f.nextTurnKinds(t); len(kinds) != 1 || kinds[0] != "head_changed" {
		t.Fatalf("next-turn fallback kinds = %v, want [head_changed]", kinds)
	}
}

// Guard over unchanged behavior (b2): the review keeps its own supersede
// trigger, so the head change cancels it once and records one activity.
func TestSnapshotHeadChangeSupersedesLiveReviewOnce(t *testing.T) {
	f := newLiveDeliveryFixture(t, "running", true)
	dbfx.Exec(t, `UPDATE issue SET status = 'in_review' WHERE id = $1`, f.issueID)
	review := dbfx.Task(t, f.agentID, testutil.Cols{
		"runtime_id": f.runtimeID, "issue_id": f.issueID, "status": "running", "workflow_step": "in_review",
		"pull_request_id": f.prID, "context": testutil.Raw(`'{"head_sha":"che2436-head"}'::jsonb`),
		"dispatched_at": testutil.Raw("now() - interval '5 minutes'"), "started_at": testutil.Raw("now() - interval '4 minutes'"),
	})
	f.headChange(t)
	if got := taskStatus(t, review); got != "cancelled" {
		t.Fatalf("old review status = %q, want cancelled", got)
	}
	if got := failureReason(t, review); got != service.FailureReasonSupersededHead {
		t.Fatalf("old review failure_reason = %q, want %q", got, service.FailureReasonSupersededHead)
	}
	if got := dbfx.Count(t, `SELECT count(*) FROM activity_log WHERE issue_id = $1 AND action = 'review_superseded'`, f.issueID); got != 1 {
		t.Fatalf("review_superseded activities = %d, want 1", got)
	}
	if got := f.deliveryRows(t); got != 1 {
		t.Fatalf("delivery rows to the capable run = %d, want 1", got)
	}
}
