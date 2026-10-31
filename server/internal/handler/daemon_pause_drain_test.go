package handler

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/daemonws"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// CHE-2673 L2: the heartbeat carries the pause to the daemon and the daemon's
// in-flight count back; a paused machine that reaches 0 notifies its owner once.

type heartbeatPauseJSON struct {
	Reason    string `json:"reason"`
	PausedAt  string `json:"paused_at"`
	ExpiresAt string `json:"expires_at"`
}

type heartbeatJSON struct {
	Status string              `json:"status"`
	Paused *heartbeatPauseJSON `json:"paused"`
}

// heartbeatAs posts one HTTP heartbeat. inFlight nil means the daemon did not report a count.
func heartbeatAs(t *testing.T, runtimeID string, inFlight *int) heartbeatJSON {
	t.Helper()
	body := map[string]any{"runtime_id": runtimeID}
	if inFlight != nil {
		body["in_flight_tasks"] = *inFlight
	}
	return testutil.Decode[heartbeatJSON](t, testHandler.DaemonHeartbeat, newOwnerDaemonRequest(http.MethodPost, "/api/daemon/heartbeat", body), http.StatusOK)
}

func wsHeartbeatAs(t *testing.T, runtimeID string, inFlight *int) *protocol.DaemonHeartbeatAckPayload {
	t.Helper()
	ack, err := testHandler.HandleDaemonWSHeartbeat(context.Background(),
		daemonws.ClientIdentity{
			WorkspaceIDs: []string{testWorkspaceID},
			RuntimeLeases: map[string]*daemonws.RuntimeLease{
				runtimeID: daemonws.NewRuntimeLease(testWorkspaceID, "online", time.Now(), true),
			},
		},
		runtimeID, false, inFlight)
	if err != nil || ack == nil {
		t.Fatalf("HandleDaemonWSHeartbeat = %+v, %v", ack, err)
	}
	return ack
}

func intp(n int) *int { return &n }

func countMachineDrainedItems(t *testing.T) int {
	t.Helper()
	var n int
	dbfx.QueryRow(t, `SELECT count(*) FROM inbox_item WHERE workspace_id = $1 AND recipient_id = $2 AND type = 'machine_drained'`,
		testWorkspaceID, testUserID).Scan(&n)
	return n
}

func cleanMachineDrainedItems(t *testing.T) {
	t.Helper()
	del := func() {
		testPool.Exec(context.Background(), `DELETE FROM inbox_item WHERE workspace_id = $1 AND type = 'machine_drained'`, testWorkspaceID)
	}
	del()
	t.Cleanup(del)
}

func TestDaemonHeartbeat_AckCarriesPause(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	const daemonID = "drain-ack-machine"
	runtimeID, _ := seedPausableRuntime(t, context.Background(), daemonID, "Drain ack")

	if got := heartbeatAs(t, runtimeID, nil); got.Paused != nil {
		t.Fatalf("unpaused machine heartbeat carries pause %+v, want none", got.Paused)
	}
	pauseDaemonForTest(t, daemonID)
	got := heartbeatAs(t, runtimeID, nil)
	if got.Paused == nil || got.Paused.Reason != "maintenance" || got.Paused.ExpiresAt == "" || got.Paused.PausedAt == "" {
		t.Fatalf("paused machine heartbeat pause = %+v, want maintenance with paused_at and expires_at", got.Paused)
	}
	dbfx.Exec(t, `DELETE FROM daemon_pause WHERE daemon_id = $1`, daemonID)
	if got := heartbeatAs(t, runtimeID, nil); got.Paused != nil {
		t.Fatalf("resumed machine heartbeat carries pause %+v, want none", got.Paused)
	}
}

func TestDaemonHeartbeat_DrainedMachineNotifiesOwnerOnce(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	cleanMachineDrainedItems(t)
	const daemonID = "drain-notify-machine"
	runtimeID, _ := seedPausableRuntime(t, context.Background(), daemonID, "Drain notify")
	pauseDaemonForTest(t, daemonID)

	heartbeatAs(t, runtimeID, intp(0))
	if got := countMachineDrainedItems(t); got != 1 {
		t.Fatalf("drained items after first idle heartbeat = %d, want 1", got)
	}
	heartbeatAs(t, runtimeID, intp(0))
	heartbeatAs(t, runtimeID, intp(0))
	if got := countMachineDrainedItems(t); got != 1 {
		t.Fatalf("drained items after repeated heartbeats = %d, want still 1", got)
	}

	// A new pause is a new event.
	if w := putPause(t, testUserID, runtimeID, map[string]any{"reason": "maintenance"}); w.Code != http.StatusOK {
		t.Fatalf("re-pause = %d: %s", w.Code, w.Body.String())
	}
	heartbeatAs(t, runtimeID, intp(0))
	if got := countMachineDrainedItems(t); got != 2 {
		t.Fatalf("drained items after re-pause = %d, want 2", got)
	}
}

func TestDaemonHeartbeat_NoDrainNoticeWhileBusyOrUnreported(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	cleanMachineDrainedItems(t)
	ctx := context.Background()
	const daemonID = "drain-busy-machine"
	runtimeID, taskID := seedPausableRuntime(t, ctx, daemonID, "Drain busy")
	pauseDaemonForTest(t, daemonID)

	heartbeatAs(t, runtimeID, intp(2))
	heartbeatAs(t, runtimeID, nil)
	if got := countMachineDrainedItems(t); got != 0 {
		t.Fatalf("notified with daemon busy or silent: %d items", got)
	}

	// The daemon says idle, but the server still holds a running task for the machine.
	dbfx.Exec(t, `UPDATE agent_task_queue SET status = 'running' WHERE id = $1`, taskID)
	heartbeatAs(t, runtimeID, intp(0))
	if got := countMachineDrainedItems(t); got != 0 {
		t.Fatalf("notified while server counts a running task: %d items", got)
	}

	// Task ends: the next idle heartbeat notifies.
	dbfx.Exec(t, `UPDATE agent_task_queue SET status = 'completed' WHERE id = $1`, taskID)
	heartbeatAs(t, runtimeID, intp(0))
	if got := countMachineDrainedItems(t); got != 1 {
		t.Fatalf("drained items after the last task ended = %d, want 1", got)
	}
}

func TestDaemonHeartbeat_NoDrainNoticeWhenNotPaused(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	cleanMachineDrainedItems(t)
	runtimeID, _ := seedPausableRuntime(t, context.Background(), "drain-unpaused-machine", "Drain unpaused")
	heartbeatAs(t, runtimeID, intp(0))
	if got := countMachineDrainedItems(t); got != 0 {
		t.Fatalf("unpaused idle machine notified: %d items", got)
	}
}

func TestDaemonPause_WSHeartbeatCarriesPauseAndNotifies(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	cleanMachineDrainedItems(t)
	const daemonID = "drain-ws-machine"
	runtimeID, _ := seedPausableRuntime(t, context.Background(), daemonID, "Drain ws")
	pauseDaemonForTest(t, daemonID)

	ack := wsHeartbeatAs(t, runtimeID, intp(0))
	if ack.Paused == nil || ack.Paused.Reason != "maintenance" {
		t.Fatalf("ws ack pause = %+v, want maintenance", ack.Paused)
	}
	if got := countMachineDrainedItems(t); got != 1 {
		t.Fatalf("drained items after ws heartbeat = %d, want 1", got)
	}
}

// A heartbeat request that is cancelled after the notice is claimed must not
// strand the claim: the inbox item is still written.
func TestDaemonPause_DrainNoticeSurvivesCancelledRequest(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	cleanMachineDrainedItems(t)
	const daemonID = "drain-cancel-machine"
	runtimeID, _ := seedPausableRuntime(t, context.Background(), daemonID, "Drain cancel")
	pauseDaemonForTest(t, daemonID)

	pause, err := testHandler.Queries.GetLiveDaemonPauseByRuntime(context.Background(), db.GetLiveDaemonPauseByRuntimeParams{
		ID:              parseUUID(runtimeID),
		MaintenanceSecs: int32(maintenancePauseTTL.Seconds()),
		OtherSecs:       int32(otherPauseTTL.Seconds()),
	})
	if err != nil {
		t.Fatalf("GetLiveDaemonPauseByRuntime: %v", err)
	}
	claimed, err := testHandler.Queries.ClaimDaemonPauseDrainNotice(context.Background(), db.ClaimDaemonPauseDrainNoticeParams{
		MachineOwnerID: pause.MachineOwnerID, DaemonID: pause.DaemonID, PausedAt: pause.PausedAt,
	})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	testHandler.deliverDrainNotice(ctx, claimed)
	if got := countMachineDrainedItems(t); got != 1 {
		t.Fatalf("drained items after a cancelled request = %d, want 1", got)
	}
}
