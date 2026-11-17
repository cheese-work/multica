package handler

import (
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/service"
)

// CHE-3548 AC1-3 at the delivery layer: what the DMs are actually told after a
// move. Reporting follows the original commission (CHE-3473 D4 = A), so the
// origin lane keeps hearing about the moved issue and the new parent's lane
// does not. Storage-level rows are covered in dm_progress_reparent_test.go.

type reparentDispatch struct {
	reparentWorld
	d    *service.DMProgressDispatcher
	base time.Time
}

func newReparentDispatch(t *testing.T) *reparentDispatch {
	t.Helper()
	w := newReparentWorld(t)
	dbfx.Exec(t, `DELETE FROM dm_progress_pending WHERE issue_id IN ($1, $2)`, w.moved, w.newParent)
	t.Cleanup(func() {
		dbfx.Exec(t, `DELETE FROM dm_progress_delivery WHERE lineage_id IN ($1, $2)`, w.laneA, w.laneB)
	})
	return &reparentDispatch{
		reparentWorld: w,
		d:             &service.DMProgressDispatcher{Pool: testPool, Queries: testHandler.Queries, Bus: events.New()},
		base:          time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Second),
	}
}

// stamp moves the pending rows captured in the last minute onto the virtual clock.
func (r *reparentDispatch) stamp(t *testing.T) {
	t.Helper()
	dbfx.Exec(t, `UPDATE dm_progress_pending SET created_at = $1 WHERE created_at > now() - interval '1 minute'`, r.base)
}

func (r *reparentDispatch) setStatus(t *testing.T, issue, status string) {
	t.Helper()
	updateIssueField(t, issue, map[string]any{"status": status})
	r.stamp(t)
}

// flush runs the dispatcher after the batch window.
func (r *reparentDispatch) flush(t *testing.T) {
	t.Helper()
	if err := r.d.Run(t.Context(), r.base.Add(20*time.Minute)); err != nil {
		t.Fatalf("dispatcher run: %v", err)
	}
}

// itemKinds returns, per card of lane, the kinds listed for issue.
func (r *reparentDispatch) itemKinds(t *testing.T, lane, issue string) [][]string {
	t.Helper()
	e := dispatchEnv{capWorld: capWorld{lane: lane}}
	var out [][]string
	for _, c := range e.cards(t) {
		for _, it := range c.Payload.Items {
			if it.IssueID == issue {
				out = append(out, it.Kinds)
			}
		}
	}
	return out
}

func (r *reparentDispatch) cardCount(t *testing.T, lane string) int {
	t.Helper()
	e := dispatchEnv{capWorld: capWorld{lane: lane}}
	return len(e.cards(t))
}

func TestDMProgressReparentDispatch_OriginDMKeepsGettingTheMovedIssueAndTheNewParentDMDoesNot(t *testing.T) {
	r := newReparentDispatch(t)
	updateIssueField(t, r.moved, map[string]any{"parent_issue_id": r.newParent})
	dbfx.Exec(t, `DELETE FROM dm_progress_pending WHERE issue_id = $1`, r.moved) // the move itself is AC3

	r.setStatus(t, r.moved, "in_progress")
	r.flush(t)

	got := r.itemKinds(t, r.laneA, r.moved)
	if len(got) != 1 || len(got[0]) != 1 || got[0][0] != "status" {
		t.Fatalf("origin DM items for the moved issue = %v, want one status item", got)
	}
	if n := r.cardCount(t, r.laneB); n != 0 {
		t.Errorf("new parent's DM got %d cards, want 0 without an explicit follow", n)
	}
}

func TestDMProgressReparentDispatch_DetachKeepsTheOriginDMReceiving(t *testing.T) {
	w := newDMWorld(t)
	dm := w.dm(t, testUserID)
	root := w.agentIssue(t, w.task(t, dm, testUserID), nil)
	child := childOf(t, root, "to detach")
	r := &reparentDispatch{
		reparentWorld: reparentWorld{laneA: dm, moved: child, newParent: root},
		d:             &service.DMProgressDispatcher{Pool: testPool, Queries: testHandler.Queries, Bus: events.New()},
		base:          time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Second),
	}
	t.Cleanup(func() { dbfx.Exec(t, `DELETE FROM dm_progress_delivery WHERE lineage_id = $1`, dm) })

	updateIssueField(t, child, map[string]any{"parent_issue_id": nil})
	dbfx.Exec(t, `DELETE FROM dm_progress_pending WHERE lineage_id = $1`, dm)

	r.setStatus(t, child, "in_progress")
	r.flush(t)

	got := r.itemKinds(t, dm, child)
	if len(got) != 1 || len(got[0]) != 1 || got[0][0] != "status" {
		t.Fatalf("origin DM items for the detached issue = %v, want one status item", got)
	}
}

func TestDMProgressReparentDispatch_OriginDMGetsOneMovedUnderItemAndNewParentDMNone(t *testing.T) {
	r := newReparentDispatch(t)

	updateIssueField(t, r.moved, map[string]any{"parent_issue_id": r.newParent})
	r.stamp(t)
	r.flush(t)

	got := r.itemKinds(t, r.laneA, r.moved)
	if len(got) != 1 || len(got[0]) != 1 || got[0][0] != "moved_under" {
		t.Fatalf("origin DM items for the moved issue = %v, want exactly one moved_under item", got)
	}
	if n := r.cardCount(t, r.laneB); n != 0 {
		t.Errorf("new parent's DM got %d cards, want 0", n)
	}
	// Delivered once: a later pass tells the DM nothing more.
	r.flush(t)
	if got := r.itemKinds(t, r.laneA, r.moved); len(got) != 1 {
		t.Errorf("moved_under delivered %d times, want once", len(got))
	}
}
