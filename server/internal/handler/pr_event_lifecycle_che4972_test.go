package handler

import (
	"context"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/integrations/ghsnapshot"
)

// CHE-4972: merged reports PR state only. The pr_merged rule already wakes the
// issue, so the next-turn fallback must not start a second pr_needs_attention
// run that ends with no change. closed has no rule and stays selected.
func TestLifecycleEventsDoNotQueuePRAttentionWake(t *testing.T) {
	for _, kind := range []ghsnapshot.PREventKind{ghsnapshot.PREventMerged} {
		t.Run(string(kind), func(t *testing.T) {
			f := newLiveDeliveryFixture(t, "running", false)
			tasks, receipts := f.taskCount(t), f.prEventReceipts(t)
			if _, err := ghsnapshot.CapturePREvent(context.Background(), testHandler.Queries, ghsnapshot.PREventInput{
				PRID: f.prID, Kind: kind, HeadSHA: "che2436-head", ObservedAt: time.Now(),
			}); err != nil {
				t.Fatal(err)
			}
			if got := dbfx.Count(t, `SELECT count(*) FROM issue_pr_event WHERE pull_request_id = $1 AND kind = $2`, f.prID, string(kind)); got != 1 {
				t.Fatalf("stored %d %s events, want 1", got, kind)
			}
			tick(t)
			if kinds := f.nextTurnKinds(t); len(kinds) != 0 {
				t.Fatalf("next-turn fallback selected %v, want none", kinds)
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
