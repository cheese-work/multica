package handler

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// CHE-3114 part 2: the claiming daemon's live task set opens the stop gate for
// a cancelled run it no longer holds. Only an online runtime can attest.

func reconcileRuntime(t *testing.T, status string) (agentID, runtimeID string) {
	t.Helper()
	runtimeID = dbfx.Runtime(t, "stop gate reconcile "+status, testutil.Cols{
		"status":       status,
		"last_seen_at": testutil.Raw("now()"),
	})
	return dbfx.Agent(t, "stop gate reconcile agent "+status, runtimeID), runtimeID
}

func reconcile(t *testing.T, runtimeID string, live []string) {
	t.Helper()
	if _, err := service.ReleaseStopsAttestedByDaemon(context.Background(), testHandler.Queries, parseUUID(runtimeID), live, time.Hour); err != nil {
		t.Fatalf("ReleaseStopsAttestedByDaemon: %v", err)
	}
}

func ackReason(t *testing.T, taskID string) (reason string, set bool) {
	t.Helper()
	var r *string
	dbfx.QueryRow(t, `SELECT cancel_ack_reason FROM agent_task_queue WHERE id = $1`, taskID).Scan(&r)
	if r == nil {
		return "", false
	}
	return *r, true
}

func TestStopGateReconcile_RunAbsentFromLiveSetOpensGate(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	agentID, runtimeID := reconcileRuntime(t, "online")
	issueID := dbfx.Issue(t, "reconcile absent", testutil.Cols{"status": "todo"})
	run := supersededRun(t, agentID, runtimeID, issueID, "todo")
	stopGateClosedFor(t, issueID, run)

	reconcile(t, runtimeID, []string{})

	stopGateOpen(t, issueID)
	if _, ok := cancelAckedAt(t, run); !ok {
		t.Fatal("cancel_acked_at not set")
	}
	if got, _ := ackReason(t, run); got != service.StopReleaseDaemonReconcile {
		t.Fatalf("cancel_ack_reason = %q, want %q", got, service.StopReleaseDaemonReconcile)
	}
}

func TestStopGateReconcile_RunStillLiveKeepsGateClosed(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	agentID, runtimeID := reconcileRuntime(t, "online")
	issueID := dbfx.Issue(t, "reconcile live", testutil.Cols{"status": "todo"})
	run := supersededRun(t, agentID, runtimeID, issueID, "todo")

	reconcile(t, runtimeID, []string{run})

	stopGateClosedFor(t, issueID, run)
	if _, ok := cancelAckedAt(t, run); ok {
		t.Fatal("cancel_acked_at set for a run the daemon still holds")
	}
}

func TestStopGateReconcile_OfflineRuntimeKeepsGateClosed(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	agentID, runtimeID := reconcileRuntime(t, "offline")
	issueID := dbfx.Issue(t, "reconcile offline", testutil.Cols{"status": "todo"})
	run := supersededRun(t, agentID, runtimeID, issueID, "todo")

	reconcile(t, runtimeID, []string{})

	stopGateClosedFor(t, issueID, run)
	if _, ok := cancelAckedAt(t, run); ok {
		t.Fatal("an offline runtime opened the gate")
	}
}

func heartbeatLiveSet(t *testing.T, runtimeID string, live *[]string) {
	t.Helper()
	body := map[string]any{"runtime_id": runtimeID}
	if live != nil {
		body["live_task_ids"] = *live
		body["daemon_uptime_ms"] = time.Hour.Milliseconds()
	}
	testutil.Decode[heartbeatJSON](t, testHandler.DaemonHeartbeat, newOwnerDaemonRequest(http.MethodPost, "/api/daemon/heartbeat", body), http.StatusOK)
}

// The heartbeat carries the live set. A daemon that does not report one (nil)
// attests nothing; one that reports an empty set holds no task.
func TestStopGateReconcile_HeartbeatLiveSetOpensGateOnlyWhenReported(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	agentID, runtimeID := reconcileRuntime(t, "online")
	issueID := dbfx.Issue(t, "reconcile heartbeat", testutil.Cols{"status": "todo"})
	run := supersededRun(t, agentID, runtimeID, issueID, "todo")

	heartbeatLiveSet(t, runtimeID, nil)
	stopGateClosedFor(t, issueID, run)

	holds := []string{run}
	heartbeatLiveSet(t, runtimeID, &holds)
	stopGateClosedFor(t, issueID, run)

	none := []string{}
	heartbeatLiveSet(t, runtimeID, &none)
	stopGateOpen(t, issueID)
}

// heartbeatLiveSetUptime posts a heartbeat that reports the live set and how
// long the daemon process has been up (nil = not reported).
func heartbeatLiveSetUptime(t *testing.T, runtimeID string, live []string, uptimeMs *int64) {
	t.Helper()
	body := map[string]any{"runtime_id": runtimeID, "live_task_ids": live}
	if uptimeMs != nil {
		body["daemon_uptime_ms"] = *uptimeMs
	}
	testutil.Decode[heartbeatJSON](t, testHandler.DaemonHeartbeat, newOwnerDaemonRequest(http.MethodPost, "/api/daemon/heartbeat", body), http.StatusOK)
}

// A restarted daemon starts with an empty live set, but the runner of a run it
// did not start may still be alive (independent process group). The run was
// dispatched (10 min ago) before this daemon booted (1 min ago): not attested.
func TestStopGateReconcile_RunFromBeforeDaemonRestartKeepsGateClosed(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	agentID, runtimeID := reconcileRuntime(t, "online")
	issueID := dbfx.Issue(t, "reconcile restart", testutil.Cols{"status": "todo"})
	run := supersededRun(t, agentID, runtimeID, issueID, "todo")

	oneMinute := int64(60_000)
	heartbeatLiveSetUptime(t, runtimeID, []string{}, &oneMinute)

	stopGateClosedFor(t, issueID, run)
	if _, ok := cancelAckedAt(t, run); ok {
		t.Fatal("a daemon that booted after the run was dispatched attested it stopped")
	}
}

// The same run is attested once the daemon that reports its absence has been up
// since before the run was dispatched (up 30 min, dispatched 10 min ago).
func TestStopGateReconcile_RunDispatchedSinceDaemonBootOpensGate(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	agentID, runtimeID := reconcileRuntime(t, "online")
	issueID := dbfx.Issue(t, "reconcile since boot", testutil.Cols{"status": "todo"})
	run := supersededRun(t, agentID, runtimeID, issueID, "todo")

	thirtyMinutes := int64(30 * 60_000)
	heartbeatLiveSetUptime(t, runtimeID, []string{}, &thirtyMinutes)

	stopGateOpen(t, issueID)
	if _, ok := cancelAckedAt(t, run); !ok {
		t.Fatal("cancel_acked_at not set")
	}
}

// Without an uptime the server cannot tell a restart from a long-lived daemon.
func TestStopGateReconcile_LiveSetWithoutUptimeKeepsGateClosed(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	agentID, runtimeID := reconcileRuntime(t, "online")
	issueID := dbfx.Issue(t, "reconcile no uptime", testutil.Cols{"status": "todo"})
	run := supersededRun(t, agentID, runtimeID, issueID, "todo")

	heartbeatLiveSetUptime(t, runtimeID, []string{}, nil)

	stopGateClosedFor(t, issueID, run)
}
