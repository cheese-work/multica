package handler

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// seedActiveBrowserLease inserts an active lease for taskID and returns its id. The grant path
// is W3-3, so the row is written directly.
func seedActiveBrowserLease(t *testing.T, taskID string) string {
	t.Helper()
	id := uuid.NewString()
	dbfx.Exec(t, `INSERT INTO browser_lease (id, workspace_id, instance_id, profile_id, agent_id, task_id, issue_id, user_id, class, state, epoch, granted_at, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'S0', 'active', 1, now(), now() + interval '15 minutes')`,
		id, testWorkspaceID, uuid.NewString(), uuid.NewString(), uuid.NewString(), taskID, uuid.NewString(), testUserID)
	t.Cleanup(func() { dbfx.Exec(t, `DELETE FROM browser_lease WHERE id = $1`, id) })
	return id
}

func browserLeaseState(t *testing.T, id string) (state, reason string) {
	t.Helper()
	var r *string
	if err := testPool.QueryRow(context.Background(), `SELECT state, end_reason FROM browser_lease WHERE id = $1`, id).Scan(&state, &r); err != nil {
		t.Fatalf("read lease: %v", err)
	}
	if r != nil {
		reason = *r
	}
	return state, reason
}

func TestReleaseBrowserLeaseEndsOnlyTheCallersTaskLease(t *testing.T) {
	runtimeID := dbfx.Runtime(t, "CHE-4734 release runtime")
	agentID := dbfx.Agent(t, "CHE-4734 release agent", runtimeID)
	issueID := dbfx.Issue(t, "CHE-4734 release issue")
	mine := dbfx.Task(t, agentID, testutil.Cols{"runtime_id": runtimeID, "issue_id": issueID, "status": "running", "started_at": testutil.Raw("now()")})
	theirs := dbfx.Task(t, agentID, testutil.Cols{"runtime_id": runtimeID, "issue_id": issueID, "status": "running", "started_at": testutil.Raw("now()")})
	myLease := seedActiveBrowserLease(t, mine)
	theirLease := seedActiveBrowserLease(t, theirs)

	call := func(taskID, leaseID string) map[string]int {
		req := newOwnerDaemonRequest(http.MethodPost, "/api/daemon/tasks/"+taskID+"/browser-leases/"+leaseID+"/release", nil)
		req = withURLParams(req, "id", taskID, "leaseId", leaseID)
		var out map[string]int
		testutil.Call(t, testHandler.ReleaseBrowserLease, req).Want(http.StatusOK).JSON(&out)
		return out
	}

	if got := call(mine, theirLease)["revoked"]; got != 0 {
		t.Fatalf("releasing another task's lease revoked %d", got)
	}
	if state, _ := browserLeaseState(t, theirLease); state != "active" {
		t.Fatalf("another task's lease state = %q, want active", state)
	}
	if got := call(mine, myLease)["revoked"]; got != 1 {
		t.Fatalf("release revoked %d, want 1", got)
	}
	if state, reason := browserLeaseState(t, myLease); state != "ended" || reason != "release" {
		t.Fatalf("lease state=%q reason=%q, want ended release", state, reason)
	}
	if got := call(mine, myLease)["revoked"]; got != 0 {
		t.Fatalf("second release revoked %d, want an idempotent 0", got)
	}
}

func TestRevokeAllBrowserLeasesEndsTheWorkspace(t *testing.T) {
	runtimeID := dbfx.Runtime(t, "CHE-4734 revoke-all runtime")
	agentID := dbfx.Agent(t, "CHE-4734 revoke-all agent", runtimeID)
	issueID := dbfx.Issue(t, "CHE-4734 revoke-all issue")
	task := dbfx.Task(t, agentID, testutil.Cols{"runtime_id": runtimeID, "issue_id": issueID, "status": "running", "started_at": testutil.Raw("now()")})
	lease := seedActiveBrowserLease(t, task)

	req := withURLParam(newRequest(http.MethodPost, "/api/workspaces/"+testWorkspaceID+"/browser-leases/revoke-all", nil), "id", testWorkspaceID)
	var out map[string]int
	testutil.Call(t, testHandler.RevokeAllBrowserLeases, req).Want(http.StatusOK).JSON(&out)
	if out["revoked"] < 1 {
		t.Fatalf("revoked = %d, want at least 1", out["revoked"])
	}
	if state, reason := browserLeaseState(t, lease); state != "ended" || reason != "revoke_all" {
		t.Fatalf("lease state=%q reason=%q, want ended revoke_all", state, reason)
	}
}

func TestDisablingTheSteelPluginRevokesLeases(t *testing.T) {
	runtimeID := dbfx.Runtime(t, "CHE-4734 plugin runtime")
	agentID := dbfx.Agent(t, "CHE-4734 plugin agent", runtimeID)
	issueID := dbfx.Issue(t, "CHE-4734 plugin issue")
	task := dbfx.Task(t, agentID, testutil.Cols{"runtime_id": runtimeID, "issue_id": issueID, "status": "running", "started_at": testutil.Raw("now()")})
	lease := seedActiveBrowserLease(t, task)
	wsUUID := parseUUID(testWorkspaceID)
	req := newRequest(http.MethodPost, "/x", nil)

	testHandler.revokeLeasesOfDisabledPlugin(req, db.PluginInstallation{PluginKey: "some.other.plugin", WorkspaceID: wsUUID})
	if state, _ := browserLeaseState(t, lease); state != "active" {
		t.Fatalf("an unrelated plugin revoked the lease: state = %q", state)
	}
	testHandler.revokeLeasesOfDisabledPlugin(req, db.PluginInstallation{PluginKey: SteelBrowserPluginKey, WorkspaceID: wsUUID})
	if state, reason := browserLeaseState(t, lease); state != "ended" || reason != "plugin_disabled" {
		t.Fatalf("lease state=%q reason=%q, want ended plugin_disabled", state, reason)
	}
}
