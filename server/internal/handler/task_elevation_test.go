package handler

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// Elevation state, per-operation authority and the fence (CHE-2216 plan 3.8,
// slice B4a; T28 (f) to (h)).

const hashA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

type elevatedFixture struct {
	taskID, issueID, runtimeID string
}

// claimElevated claims a task on an issue holding a live grant for scopes and
// returns the task once the claim has recorded its elevation.
func claimElevated(t *testing.T, scopes ...string) elevatedFixture {
	t.Helper()
	ctx := context.Background()
	name := "Elevation " + uuid.NewString()[:8]
	runtimeID := createClaimReclaimRuntime(t, ctx, name+" runtime")
	agentID, issueID := createClaimReclaimAgentAndIssue(t, ctx, runtimeID, name+" agent")
	insertGrant(t, issueID, testWorkspaceID, scopes, "1 minute", "1 hour", false)
	taskID := createDispatchedClaimFixtureTask(t, ctx, agentID, runtimeID, issueID, "120 seconds", false)
	req := withURLParam(newOwnerDaemonRequest("POST", "/api/daemon/runtimes/"+runtimeID+"/tasks/claim", nil), "runtimeId", runtimeID)
	var resp struct {
		Task *struct {
			ID      string `json:"id"`
			Profile string `json:"profile"`
			Epoch   int64  `json:"epoch"`
		} `json:"task"`
	}
	testutil.Call(t, testHandler.ClaimTaskByRuntime, req).Want(http.StatusOK).JSON(&resp)
	if resp.Task == nil || resp.Task.ID != taskID || resp.Task.Profile != "elevated" || resp.Task.Epoch != 1 {
		t.Fatalf("claim = %+v, want elevated epoch 1 for task %s", resp.Task, taskID)
	}
	return elevatedFixture{taskID: taskID, issueID: issueID, runtimeID: runtimeID}
}

func (f elevatedFixture) authorize(t *testing.T, epoch int64, scope, hash string) *testutil.Response {
	t.Helper()
	req := newOwnerDaemonRequest("POST", "/api/daemon/tasks/"+f.taskID+"/elevation/authorize",
		map[string]any{"epoch": epoch, "scope": scope, "input_hash": hash})
	return testutil.Call(t, testHandler.AuthorizeElevatedOp, withURLParam(req, "taskId", f.taskID))
}

func (f elevatedFixture) complete(t *testing.T, opID, result string) *testutil.Response {
	t.Helper()
	req := newOwnerDaemonRequest("POST", "/api/daemon/tasks/"+f.taskID+"/elevation/ops/"+opID+"/complete", map[string]any{"result": result})
	return testutil.Call(t, testHandler.CompleteElevatedOp, withURLParams(req, "taskId", f.taskID, "opId", opID))
}

func (f elevatedFixture) authorizeOK(t *testing.T, scope string) string {
	t.Helper()
	var resp struct {
		OpID string `json:"op_id"`
	}
	f.authorize(t, 1, scope, hashA).Want(http.StatusOK).JSON(&resp)
	if resp.OpID == "" {
		t.Fatal("authorize returned no op_id")
	}
	return resp.OpID
}

func (f elevatedFixture) state(t *testing.T) (state string, epoch int64) {
	t.Helper()
	dbfx.QueryRow(t, `SELECT state, epoch FROM task_elevation WHERE task_id = $1`, f.taskID).Scan(&state, &epoch)
	return
}

func fenceNow(t *testing.T) {
	t.Helper()
	if err := testHandler.fenceRound(context.Background()); err != nil {
		t.Fatalf("fence round: %v", err)
	}
}

func (f elevatedFixture) expireGrant(t *testing.T) {
	t.Helper()
	dbfx.Exec(t, `UPDATE run_capability_grant SET created_at = now() - interval '2 hours', expires_at = now() - interval '1 hour' WHERE issue_id = $1`, f.issueID)
}

func (f elevatedFixture) revokeGrant(t *testing.T) {
	t.Helper()
	dbfx.Exec(t, `UPDATE run_capability_grant SET revoked_at = now() WHERE issue_id = $1`, f.issueID)
}

func TestClaimRecordsElevationOnce(t *testing.T) {
	f := claimElevated(t, "repo_write:1234", "docker")
	var scopes []string
	dbfx.QueryRow(t, `SELECT scopes FROM task_elevation WHERE task_id = $1`, f.taskID).Scan(&scopes)
	if len(scopes) != 2 || scopes[0] != "docker" || scopes[1] != "repo_write:1234" {
		t.Fatalf("recorded scopes = %v", scopes)
	}
	// A re-claim of the same task keeps its row and its epoch.
	if got := testHandler.runProfileForTask(context.Background(), parseUUID(testWorkspaceID), parseUUID(f.issueID), parseUUID(f.taskID)); got.Profile != "elevated" || got.Epoch != 1 {
		t.Fatalf("re-claim profile = %+v", got)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM task_elevation WHERE task_id = $1`, f.taskID); n != 1 {
		t.Fatalf("rows = %d, want 1", n)
	}
}

func TestAuthorizeRecordsBoundOpAndRefusesBadInput(t *testing.T) {
	f := claimElevated(t, "repo_write:1234", "docker")
	opID := f.authorizeOK(t, "repo_write:1234")
	var scope, hash, state string
	var epoch int64
	var leaseSecs float64
	dbfx.QueryRow(t, `SELECT scope, input_hash, state, epoch, extract(epoch FROM lease_until - now()) FROM elevated_op WHERE op_id = $1`, opID).Scan(&scope, &hash, &state, &epoch, &leaseSecs)
	if scope != "repo_write:1234" || hash != hashA || state != "authorized" || epoch != 1 || leaseSecs < 590 || leaseSecs > 600 {
		t.Fatalf("op = %s %s %s epoch %d lease %.0fs", scope, hash, state, epoch, leaseSecs)
	}
	for label, c := range map[string]struct {
		epoch       int64
		scope, hash string
		want        int
	}{
		"wrong epoch":            {2, "repo_write:1234", hashA, http.StatusForbidden},
		"scope not in the grant": {1, "repo_write:9999", hashA, http.StatusForbidden},
		"launch-time scope":      {1, "docker", hashA, http.StatusForbidden},
		"short hash":             {1, "repo_write:1234", "abc", http.StatusBadRequest},
		"upper-case hash":        {1, "repo_write:1234", strings.ToUpper(hashA), http.StatusBadRequest},
		"free-text scope":        {1, "anything", hashA, http.StatusBadRequest},
	} {
		if r := f.authorize(t, c.epoch, c.scope, c.hash); r.Code != c.want {
			t.Errorf("%s: status %d, want %d", label, r.Code, c.want)
		}
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM elevated_op WHERE task_id = $1`, f.taskID); n != 1 {
		t.Fatalf("refused calls left %d ops, want 1", n)
	}
}

func TestAuthorizeRefusedForAnotherWorkspacesDaemon(t *testing.T) {
	f := claimElevated(t, "repo_write:1234")
	req := newDaemonTokenRequest("POST", "/api/daemon/tasks/"+f.taskID+"/elevation/authorize",
		map[string]any{"epoch": 1, "scope": "repo_write:1234", "input_hash": hashA}, "00000000-0000-0000-0000-000000000000", "attacker-daemon")
	testutil.Call(t, testHandler.AuthorizeElevatedOp, withURLParam(req, "taskId", f.taskID)).Want(http.StatusNotFound)
}

// T28 (g): grant expiry during a run.
func TestExpiryFencesTheTask(t *testing.T) {
	f := claimElevated(t, "repo_write:1234", "multica_cross_issue")
	f.authorizeOK(t, "multica_cross_issue") // control: authorized while the grant is live
	f.expireGrant(t)
	// The live check refuses before any job runs.
	f.authorize(t, 1, "repo_write:1234", hashA).Want(http.StatusForbidden)
	f.authorize(t, 1, "multica_cross_issue", hashA).Want(http.StatusForbidden)
	fenceNow(t)
	// The control op from before expiry is still open, so the fence waits.
	if s, e := f.state(t); s != "fencing" || e != 2 {
		t.Fatalf("state = %s epoch %d, want fencing epoch 2", s, e)
	}
}

// T28 (h): member revocation during a run.
func TestRevocationFencesTheTaskAndEndsInRestrictedFinal(t *testing.T) {
	f := claimElevated(t, "repo_write:1234")
	f.revokeGrant(t)
	f.authorize(t, 1, "repo_write:1234", hashA).Want(http.StatusForbidden)
	fenceNow(t)
	if s, e := f.state(t); s != "restricted_final" || e != 2 {
		t.Fatalf("state = %s epoch %d, want restricted_final epoch 2", s, e)
	}
	// T28 (f): authorize at the old epoch, and at the new one, after the fence.
	f.authorize(t, 1, "repo_write:1234", hashA).Want(http.StatusForbidden)
	f.authorize(t, 2, "repo_write:1234", hashA).Want(http.StatusForbidden)
	// The task is never elevated again, even with a fresh grant.
	insertGrant(t, f.issueID, testWorkspaceID, []string{"repo_write:1234"}, "1 minute", "1 hour", false)
	if got := testHandler.runProfileForTask(context.Background(), parseUUID(testWorkspaceID), parseUUID(f.issueID), parseUUID(f.taskID)); got.Profile != "restricted" {
		t.Fatalf("fenced task re-elevated: %+v", got)
	}
}

// T28 (f): authorize refuses at epoch N once the fence has started, while the
// fence is still draining.
func TestFencingTaskDrainsOpenOpsBeforeRestrictedFinal(t *testing.T) {
	f := claimElevated(t, "repo_write:1234")
	opID := f.authorizeOK(t, "repo_write:1234")
	f.revokeGrant(t)
	fenceNow(t)
	if s, _ := f.state(t); s != "fencing" {
		t.Fatalf("state = %s, want fencing while an op is open", s)
	}
	f.authorize(t, 1, "repo_write:1234", hashA).Want(http.StatusForbidden)
	f.authorize(t, 2, "repo_write:1234", hashA).Want(http.StatusForbidden)
	f.complete(t, opID, "completed").Want(http.StatusOK)
	fenceNow(t)
	if s, _ := f.state(t); s != "restricted_final" {
		t.Fatalf("state = %s, want restricted_final once the op reported", s)
	}
}

func TestCompleteOnlyAnOpenOpOfThisTask(t *testing.T) {
	f := claimElevated(t, "repo_write:1234")
	other := claimElevated(t, "repo_write:1234")
	opID := f.authorizeOK(t, "repo_write:1234")
	other.complete(t, opID, "completed").Want(http.StatusNotFound) // another task's op
	f.complete(t, opID, "bogus").Want(http.StatusBadRequest)
	f.complete(t, opID, "failed").Want(http.StatusOK)
	f.complete(t, opID, "completed").Want(http.StatusNotFound) // already final
	var state string
	dbfx.QueryRow(t, `SELECT state FROM elevated_op WHERE op_id = $1`, opID).Scan(&state)
	if state != "failed" {
		t.Fatalf("op state = %s, want failed", state)
	}
}

// T28 (e): an op whose lease expires with no report is unknown and blocks the
// fence until the actor reports.
func TestLapsedLeaseIsUnknownAndBlocksTheFence(t *testing.T) {
	f := claimElevated(t, "repo_write:1234")
	opID := f.authorizeOK(t, "repo_write:1234")
	dbfx.Exec(t, `UPDATE elevated_op SET lease_until = now() - interval '1 second' WHERE op_id = $1`, opID)
	f.revokeGrant(t)
	fenceNow(t)
	var state string
	dbfx.QueryRow(t, `SELECT state FROM elevated_op WHERE op_id = $1`, opID).Scan(&state)
	if s, _ := f.state(t); state != "unknown" || s != "fencing" {
		t.Fatalf("op %s, task %s; want unknown and fencing", state, s)
	}
	f.complete(t, opID, "completed").Want(http.StatusOK)
	fenceNow(t)
	if s, _ := f.state(t); s != "restricted_final" {
		t.Fatalf("state = %s after the late report, want restricted_final", s)
	}
}

// Launch-time scopes end with the run: the fence waits for the daemon to say
// their processes exited (written by B4b).
func TestLaunchScopeHoldsTheFenceUntilItEnds(t *testing.T) {
	f := claimElevated(t, "docker")
	f.revokeGrant(t)
	fenceNow(t)
	if s, _ := f.state(t); s != "fencing" {
		t.Fatalf("state = %s, want fencing while a launch-time scope runs", s)
	}
	dbfx.Exec(t, `UPDATE task_elevation SET launch_scopes_ended_at = now() WHERE task_id = $1`, f.taskID)
	fenceNow(t)
	if s, _ := f.state(t); s != "restricted_final" {
		t.Fatalf("state = %s, want restricted_final", s)
	}
}

func TestFenceLeavesLiveElevationsAlone(t *testing.T) {
	f := claimElevated(t, "repo_write:1234")
	fenceNow(t)
	if s, e := f.state(t); s != "elevated" || e != 1 {
		t.Fatalf("state = %s epoch %d, want untouched", s, e)
	}
	f.authorizeOK(t, "repo_write:1234")
}
