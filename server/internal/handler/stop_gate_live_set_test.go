package handler

import (
	"context"
	"time"

	"github.com/multica-ai/multica/server/internal/daemonws"
	"github.com/multica-ai/multica/server/pkg/protocol"
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// liveSetHeartbeat posts the heartbeat of a daemon that reports its live task
// set. A nil set is a daemon that does not report one (older build).
func liveSetHeartbeat(t *testing.T, runtimeID string, live []string) {
	t.Helper()
	body := map[string]any{"runtime_id": runtimeID}
	if live != nil {
		body["live_task_ids"] = live
	}
	testutil.Call(t, testHandler.DaemonHeartbeat,
		newOwnerDaemonRequest(http.MethodPost, "/api/daemon/heartbeat", body)).Want(http.StatusOK)
}

func cancelAckReason(t *testing.T, taskID string) string {
	t.Helper()
	var reason *string
	dbfx.QueryRow(t, `SELECT cancel_ack_reason FROM agent_task_queue WHERE id = $1`, taskID).Scan(&reason)
	if reason == nil {
		return ""
	}
	return *reason
}

// CHE-3114: a daemon that no longer holds a cancelled, unacked task proves the
// runner is gone, so its heartbeat releases the gate and records why.
func TestStopGate_LiveSetWithoutRunOpensGate(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	agentID, runtimeID := mul8188Agent(t)
	issueID := dbfx.Issue(t, "stop gate live set gone", testutil.Cols{"status": "agent_review"})
	run := supersededRun(t, agentID, runtimeID, issueID, "agent_review")
	stopGateClosedFor(t, issueID, run)

	liveSetHeartbeat(t, runtimeID, []string{})

	stopGateOpen(t, issueID)
	if _, ok := cancelAckedAt(t, run); !ok {
		t.Fatal("cancel_acked_at not set")
	}
	if got := cancelAckReason(t, run); got != "daemon_reconcile" {
		t.Fatalf("cancel_ack_reason = %q, want daemon_reconcile", got)
	}
}

// The same run still held by the daemon may still be running: gate stays closed.
func TestStopGate_LiveSetWithRunStaysClosed(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	agentID, runtimeID := mul8188Agent(t)
	issueID := dbfx.Issue(t, "stop gate live set holds", testutil.Cols{"status": "agent_review"})
	run := supersededRun(t, agentID, runtimeID, issueID, "agent_review")

	liveSetHeartbeat(t, runtimeID, []string{run})

	stopGateClosedFor(t, issueID, run)
	if _, ok := cancelAckedAt(t, run); ok {
		t.Fatal("cancel_acked_at set while the run is in the live set")
	}
}

// An offline runtime proves nothing: another runtime's empty live set must not
// release its run, and no heartbeat at all leaves the gate closed.
func TestStopGate_OfflineRuntimeStaysClosedOnLiveSet(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	_, onlineRuntimeID := mul8188Agent(t)
	offlineRuntimeID := dbfx.Runtime(t, "stop gate live set offline", testutil.Cols{
		"status":       "offline",
		"last_seen_at": testutil.Raw("now() - interval '3 hours'"),
	})
	agentID := dbfx.Agent(t, "stop gate live set offline agent", offlineRuntimeID)
	issueID := dbfx.Issue(t, "stop gate live set offline", testutil.Cols{"status": "agent_review"})
	run := supersededRun(t, agentID, offlineRuntimeID, issueID, "agent_review")

	liveSetHeartbeat(t, onlineRuntimeID, []string{})

	stopGateClosedFor(t, issueID, run)
}

// A daemon that does not report a live set (older build) proves nothing.
func TestStopGate_UnreportedLiveSetStaysClosed(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	agentID, runtimeID := mul8188Agent(t)
	issueID := dbfx.Issue(t, "stop gate live set unreported", testutil.Cols{"status": "agent_review"})
	run := supersededRun(t, agentID, runtimeID, issueID, "agent_review")

	liveSetHeartbeat(t, runtimeID, nil)

	stopGateClosedFor(t, issueID, run)
}

// A claim still travelling to the daemon is absent from its live set; the
// release waits out that window.
func TestStopGate_RecentlyDispatchedRunStaysClosed(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	agentID, runtimeID := mul8188Agent(t)
	issueID := dbfx.Issue(t, "stop gate live set recent", testutil.Cols{"status": "agent_review"})
	run := supersededRun(t, agentID, runtimeID, issueID, "agent_review",
		testutil.Cols{"dispatched_at": testutil.Raw("now() - interval '10 seconds'")})

	liveSetHeartbeat(t, runtimeID, []string{})

	stopGateClosedFor(t, issueID, run)
}

// The WebSocket heartbeat carries the same live set as the HTTP one.
func TestStopGate_LiveSetOverWebSocketOpensGate(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	agentID, runtimeID := mul8188Agent(t)
	issueID := dbfx.Issue(t, "stop gate live set ws", testutil.Cols{"status": "agent_review"})
	run := supersededRun(t, agentID, runtimeID, issueID, "agent_review")

	ack, err := testHandler.HandleDaemonWSHeartbeat(context.Background(),
		daemonws.ClientIdentity{
			WorkspaceIDs: []string{testWorkspaceID},
			RuntimeLeases: map[string]*daemonws.RuntimeLease{
				runtimeID: daemonws.NewRuntimeLease(testWorkspaceID, "online", time.Now(), true),
			},
		},
		protocol.DaemonHeartbeatRequestPayload{RuntimeID: runtimeID, LiveTaskIDs: []string{}})
	if err != nil || ack == nil {
		t.Fatalf("HandleDaemonWSHeartbeat = %+v, %v", ack, err)
	}

	stopGateOpen(t, issueID)
	if got := cancelAckReason(t, run); got != "daemon_reconcile" {
		t.Fatalf("cancel_ack_reason = %q, want daemon_reconcile", got)
	}
}
