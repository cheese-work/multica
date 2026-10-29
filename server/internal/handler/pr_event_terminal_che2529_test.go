package handler

import (
	"context"
	"testing"

	"github.com/multica-ai/multica/server/internal/integrations/ghsnapshot"
	"github.com/multica-ai/multica/server/internal/service"
)

// CHE-2529: a snapshot that applies after the PR merged or closed must not raise
// conflict or checks-failed events; the lifecycle stop already ran and would not
// see them.

// terminate moves the fixture's PR to a terminal state the way the webhook does.
func (f liveDeliveryFixture) terminate(t *testing.T, state string) {
	t.Helper()
	f.refresh(t, ghsnapshot.PRSnapshot{HeadSHA: "che2436-head", BaseSHA: "base-1", Mergeable: "MERGEABLE", MergeStateStatus: "CLEAN"})
	dbfx.Exec(t, `UPDATE github_pull_request SET state = $2 WHERE id = $1`, f.prID, state)
	if err := testHandler.TaskService.ApplyPRLifecycleEvent(context.Background(), service.PRLifecycleEvent{PRID: f.prID, Kind: state}); err != nil {
		t.Fatal(err)
	}
}

func (f liveDeliveryFixture) actionRequiredEvents(t *testing.T) int {
	t.Helper()
	return dbfx.Count(t, `SELECT count(*) FROM issue_pr_event WHERE pull_request_id = $1 AND kind IN ('conflict', 'checks_failed', 'head_changed')`, f.prID)
}

func (f liveDeliveryFixture) prEventReceipts(t *testing.T) int {
	t.Helper()
	return dbfx.Count(t, `SELECT count(*) FROM issue_wakeup_receipt r JOIN issue_wakeup w ON w.id = r.wakeup_id WHERE w.issue_id = $1 AND r.event_type = 'pr.event'`, f.issueID)
}

func (f liveDeliveryFixture) taskCount(t *testing.T) int {
	t.Helper()
	return dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1`, f.issueID)
}

func TestSnapshotCauseAfterTerminalPRIsNotActionable(t *testing.T) {
	causes := map[string]func(liveDeliveryFixture) ghsnapshot.PRSnapshot{
		"conflict":      liveDeliveryFixture.conflicting,
		"checks_failed": liveDeliveryFixture.checksFailed,
	}
	for _, state := range []string{"merged", "closed"} {
		for cause, snapshot := range causes {
			for _, withChannel := range []bool{true, false} {
				name := state + "/" + cause + "/legacy"
				if withChannel {
					name = state + "/" + cause + "/channel"
				}
				t.Run(name, func(t *testing.T) {
					f := newLiveDeliveryFixture(t, "running", withChannel)
					f.terminate(t, state)
					tasks, receipts := f.taskCount(t), f.prEventReceipts(t)
					f.refresh(t, snapshot(f))
					if got := f.actionRequiredEvents(t); got != 0 {
						t.Fatalf("%s PR stored %d action-required events from a late %s snapshot, want 0", state, got, cause)
					}
					if got := f.deliveryRows(t); got != 0 {
						t.Fatalf("delivery rows = %d, want 0", got)
					}
					if kinds := f.nextTurnKinds(t); len(kinds) != 0 {
						t.Fatalf("next-turn fallback selected %v for a %s PR, want none", kinds, state)
					}
					if got := f.prEventReceipts(t); got != receipts {
						t.Fatalf("pr.event receipts %d -> %d, want unchanged", receipts, got)
					}
					if got := f.taskCount(t); got != tasks {
						t.Fatalf("tasks %d -> %d, want unchanged", tasks, got)
					}
				})
			}
		}
	}
}

// Reopen: causes observed after the PR is open again are actionable again.
func TestSnapshotCauseAfterReopenIsActionable(t *testing.T) {
	f := newLiveDeliveryFixture(t, "running", true)
	f.terminate(t, "closed")
	f.refresh(t, f.conflicting())
	if got := f.actionRequiredEvents(t); got != 0 {
		t.Fatalf("closed PR stored %d action-required events, want 0", got)
	}
	dbfx.Exec(t, `UPDATE github_pull_request SET state = 'open' WHERE id = $1`, f.prID)
	f.refresh(t, ghsnapshot.PRSnapshot{HeadSHA: "che2436-head", BaseSHA: "base-1", Mergeable: "MERGEABLE", MergeStateStatus: "CLEAN"})
	f.refresh(t, f.conflicting())
	got, err := f.claim(t)
	if err != nil {
		t.Fatalf("ClaimNextTaskPREvent after reopen: %v", err)
	}
	if got.Kind != "conflict" {
		t.Fatalf("claimed %+v, want the conflict raised after reopen", got)
	}
}
