package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

type affectedList struct {
	Machines []AffectedMachineResponse `json:"machines"`
}

func affectedAs(t *testing.T, userID string) []AffectedMachineResponse {
	t.Helper()
	return testutil.Decode[affectedList](t, testHandler.ListAffectedMachines,
		newRequestAs(userID, http.MethodGet, "/api/runtimes/affected", nil), http.StatusOK).Machines
}

func affectedFor(machines []AffectedMachineResponse, daemonID string) *AffectedMachineResponse {
	for i := range machines {
		if machines[i].DaemonID == daemonID {
			return &machines[i]
		}
	}
	return nil
}

// seedProjectTask queues a task for runtimeID on an issue of projectID ("" for none).
func seedProjectTask(t *testing.T, runtimeID, name, projectID string) {
	t.Helper()
	agentID := dbfx.Agent(t, name+" agent", runtimeID)
	over := testutil.Cols{}
	if projectID != "" {
		over["project_id"] = projectID
	}
	issueID := dbfx.Issue(t, name+" issue", over)
	dbfx.Task(t, agentID, testutil.Cols{"runtime_id": runtimeID, "issue_id": issueID})
}

func TestAffectedMachines_PausedMachineWithQueuedTaskReachesWorkspaceOwner(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	const daemonID = "affected-paused"
	seedPausableRuntime(t, context.Background(), daemonID, "Affected paused")
	pauseDaemonForTest(t, daemonID)

	got := affectedFor(affectedAs(t, testUserID), daemonID)
	if got == nil {
		t.Fatal("paused machine with a queued task is missing from the workspace owner's list")
	}
	if got.State != "paused" || got.Reason == nil || *got.Reason != "maintenance" || got.WaitingTasks != 1 {
		t.Fatalf("got %+v, want paused/maintenance with 1 waiting task", got)
	}
	if got.ExpiresAt == nil || !strings.HasPrefix(got.EventID, "paused:") {
		t.Fatalf("got expires_at=%v event_id=%q, want an expiry and a paused: event id", got.ExpiresAt, got.EventID)
	}
}

func TestAffectedMachines_OnlineMachineAndMachineWithoutQueuedTaskAreNotAffected(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	seedPausableRuntime(t, ctx, "affected-online", "Affected online")
	dbfx.Runtime(t, "Affected idle", testutil.Cols{"daemon_id": "affected-idle"})
	pauseDaemonForTest(t, "affected-idle")

	machines := affectedAs(t, testUserID)
	for _, id := range []string{"affected-online", "affected-idle"} {
		if affectedFor(machines, id) != nil {
			t.Fatalf("machine %s listed, want none: it is online or nothing waits for it", id)
		}
	}
}

func TestAffectedMachines_OfflineMachineWithQueuedTask(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	const daemonID = "affected-offline"
	runtimeID, _ := seedPausableRuntime(t, context.Background(), daemonID, "Affected offline")
	dbfx.Exec(t, `UPDATE agent_runtime SET status = 'offline' WHERE id = $1`, runtimeID)

	got := affectedFor(affectedAs(t, testUserID), daemonID)
	if got == nil || got.State != "offline" || got.Reason != nil || got.ExpiresAt != nil || got.WaitingTasks != 1 {
		t.Fatalf("got %+v, want offline, no reason, no expiry, 1 waiting task", got)
	}
	if !strings.HasPrefix(got.EventID, "offline:") {
		t.Fatalf("event id %q, want offline: prefix", got.EventID)
	}
}

func TestAffectedMachines_OneOnlineRuntimeKeepsTheMachineOn(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	const daemonID = "affected-partial"
	runtimeA, _ := seedPausableRuntime(t, context.Background(), daemonID, "Affected partial A")
	dbfx.Runtime(t, "Affected partial B", testutil.Cols{"daemon_id": daemonID, "provider": "handler_test_runtime_b"})
	dbfx.Exec(t, `UPDATE agent_runtime SET status = 'offline' WHERE id = $1`, runtimeA)

	if affectedFor(affectedAs(t, testUserID), daemonID) != nil {
		t.Fatal("machine with an online runtime listed as offline")
	}
}

func TestAffectedMachines_ExpiredPauseIsNotAffected(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	const daemonID = "affected-expired"
	seedPausableRuntime(t, context.Background(), daemonID, "Affected expired")
	pauseDaemonForTest(t, daemonID)
	dbfx.Exec(t, `UPDATE daemon_pause SET paused_at = now() - interval '3 hours' WHERE daemon_id = $1`, daemonID)

	if affectedFor(affectedAs(t, testUserID), daemonID) != nil {
		t.Fatal("machine whose pause expired listed as affected")
	}
}

func TestAffectedMachines_ProjectLeadSeesOnlyTheirProjects(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	leadID := dbfx.User(t, "Affected lead", "affected-lead@multica.test")
	dbfx.Member(t, testWorkspaceID, leadID, "member")
	otherID := dbfx.User(t, "Affected other", "affected-other@multica.test")
	dbfx.Member(t, testWorkspaceID, otherID, "member")
	led := dbfx.Project(t, "Affected led", testutil.Cols{"lead_type": "member", "lead_id": leadID})
	notLed := dbfx.Project(t, "Affected not led", testutil.Cols{"lead_type": "member", "lead_id": otherID})

	for _, c := range []struct{ daemon, project string }{{"affected-lead-led", led}, {"affected-lead-other", notLed}, {"affected-lead-none", ""}} {
		runtimeID := dbfx.Runtime(t, "Affected "+c.daemon, testutil.Cols{"daemon_id": c.daemon})
		seedProjectTask(t, runtimeID, c.daemon, c.project)
		pauseDaemonForTest(t, c.daemon)
	}

	lead := affectedAs(t, leadID)
	if affectedFor(lead, "affected-lead-led") == nil {
		t.Fatal("project lead misses the machine that holds their project's task")
	}
	for _, id := range []string{"affected-lead-other", "affected-lead-none"} {
		if affectedFor(lead, id) != nil {
			t.Fatalf("project lead sees %s, which holds no task of their project", id)
		}
	}
	w := httptest.NewRecorder()
	testHandler.ListAffectedMachines(w, newRequestAs(dbfx.User(t, "Affected plain", "affected-plain@multica.test"), http.MethodGet, "/api/runtimes/affected", nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("non-member = %d, want 404", w.Code)
	}
	owner := affectedAs(t, testUserID)
	for _, id := range []string{"affected-lead-led", "affected-lead-other", "affected-lead-none"} {
		if affectedFor(owner, id) == nil {
			t.Fatalf("workspace owner misses %s, a paused machine holding a queued task", id)
		}
	}
}
