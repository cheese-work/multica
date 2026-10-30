package handler

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/integrations/ghsnapshot"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// CHE-2600: a typed live notice that the run accepts handles the cause. The
// legacy PR rule must not leave a receipt that starts another run once the live
// run ends; a failed or unacknowledged notice keeps the one fallback wake.

// liveRun starts a running task of the fixture's agent that negotiated typed
// PR-event delivery, and returns its id.
func (fx coalesceFixture) liveRun(t *testing.T) string {
	t.Helper()
	var runtimeID string
	if err := testPool.QueryRow(context.Background(), `SELECT runtime_id FROM agent WHERE id=$1`, fx.agentID).Scan(&runtimeID); err != nil {
		t.Fatal(err)
	}
	taskID := dbfx.Task(t, fx.agentID, testutil.Cols{
		"issue_id": fx.issueID, "runtime_id": runtimeID, "status": "running", "started_at": testutil.Raw("now()"),
	})
	dbfx.Exec(t, `INSERT INTO task_pr_event_capability (task_id, workspace_id, issue_id, capability) VALUES ($1, $2, $3, $4)`,
		taskID, testWorkspaceID, fx.issueID, protocol.DaemonCapabilityTaskSupplementPREventV1)
	dbfx.Cleanup(t, `DELETE FROM task_pr_event_capability WHERE task_id = $1`, taskID)
	dbfx.Cleanup(t, `DELETE FROM task_pr_event_delivery WHERE task_id = $1`, taskID)
	return taskID
}

func claimAndAck(t *testing.T, taskID string, delivered bool) {
	t.Helper()
	ctx := context.Background()
	claimed, err := testHandler.Queries.ClaimNextTaskPREvent(ctx, parseUUID(taskID))
	if err != nil {
		t.Fatalf("ClaimNextTaskPREvent: %v", err)
	}
	if delivered {
		_, err = testHandler.Queries.AckTaskPREventDelivered(ctx, db.AckTaskPREventDeliveredParams{TaskID: parseUUID(taskID), EventID: claimed.EventID})
	} else {
		_, err = testHandler.Queries.AckTaskPREventFailed(ctx, db.AckTaskPREventFailedParams{TaskID: parseUUID(taskID), EventID: claimed.EventID})
	}
	if err != nil {
		t.Fatalf("ack (delivered=%v): %v", delivered, err)
	}
}

// endRun ends the live run the way a terminal transition does: the status moves
// and undelivered deliveries settle back to the next-turn path.
func endRun(t *testing.T, taskID string) {
	t.Helper()
	dbfx.Exec(t, `UPDATE agent_task_queue SET status='completed',completed_at=clock_timestamp() WHERE id=$1`, taskID)
	if _, err := testHandler.Queries.SettleTerminalTaskPREventDeliveries(context.Background(), []pgtype.UUID{parseUUID(taskID)}); err != nil {
		t.Fatal(err)
	}
}

func TestLiveNoticeAckLeavesNoRedundantRun(t *testing.T) {
	for _, tc := range []struct {
		name string
		snap func(head string) ghsnapshot.PRSnapshot
		n    int32
	}{
		{"conflict", func(h string) ghsnapshot.PRSnapshot { return conflictingSnapshot(h, "base-1") }, 260001},
		{"checks failed", failingSnapshot, 260002},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if testHandler == nil {
				t.Skip("database not available")
			}
			head := "che2600-ack-" + tc.name[:4]
			fx := newCoalesceFixture(t, int64(tc.n), tc.n, head)
			task := fx.liveRun(t)
			fx.set(tc.snap(head))
			fx.refresh()
			claimAndAck(t, task, true)
			endRun(t, task)
			fx.sweep()
			if got := fx.runs(); got != 0 {
				t.Fatalf("a delivered live notice still started %d runs, want 0", got)
			}
		})
	}
}

func TestLiveNoticeNotAckedKeepsOneFallbackRun(t *testing.T) {
	for _, tc := range []struct {
		name      string
		snap      func(head string) ghsnapshot.PRSnapshot
		n         int32
		ackFailed bool
	}{
		{"conflict delivery failed", func(h string) ghsnapshot.PRSnapshot { return conflictingSnapshot(h, "base-1") }, 260003, true},
		{"checks failed delivery failed", failingSnapshot, 260004, true},
		{"conflict turn ended", func(h string) ghsnapshot.PRSnapshot { return conflictingSnapshot(h, "base-1") }, 260005, false},
		{"checks failed turn ended", failingSnapshot, 260006, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if testHandler == nil {
				t.Skip("database not available")
			}
			head := "che2600-nak-" + tc.name[:4] + tc.name[len(tc.name)-5:]
			fx := newCoalesceFixture(t, int64(tc.n), tc.n, head)
			task := fx.liveRun(t)
			fx.set(tc.snap(head))
			fx.refresh()
			if tc.ackFailed {
				claimAndAck(t, task, false)
			}
			endRun(t, task)
			fx.sweep()
			if got := fx.runs(); got != 1 {
				t.Fatalf("an unacknowledged live notice started %d runs, want 1", got)
			}
		})
	}
}
