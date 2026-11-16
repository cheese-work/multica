package handler

import (
	"context"
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// Downgrade handshake, server half (CHE-2216 plan 3.4 and 3.8, slice B4c;
// T26, T28 (a), (b), (i)). The proxy asks for a downgrade; the server fences,
// waits for every epoch-N operation to end, and taints task and issue before it
// confirms that the proxy may deliver the withheld bytes.

func (f elevatedFixture) startDowngrade(t *testing.T, epoch int64) *testutil.Response {
	t.Helper()
	req := newOwnerDaemonRequest("POST", "/api/daemon/tasks/"+f.taskID+"/elevation/downgrade", map[string]any{"epoch": epoch})
	return testutil.Call(t, testHandler.StartTaskDowngrade, withURLParam(req, "taskId", f.taskID))
}

func (f elevatedFixture) deliverDowngrade(t *testing.T) *testutil.Response {
	t.Helper()
	req := newOwnerDaemonRequest("POST", "/api/daemon/tasks/"+f.taskID+"/elevation/downgrade/deliver", map[string]any{})
	return testutil.Call(t, testHandler.DeliverTaskDowngrade, withURLParam(req, "taskId", f.taskID))
}

func (f elevatedFixture) taintCounts(t *testing.T) (taskTaint, issueTaint int) {
	t.Helper()
	dbfx.QueryRow(t, `SELECT cardinality(taint_ids) FROM agent_task_queue WHERE id = $1`, f.taskID).Scan(&taskTaint)
	dbfx.QueryRow(t, `SELECT COALESCE((SELECT cardinality(taint_ids) FROM issue_taint WHERE issue_id = $1), 0)`, f.issueID).Scan(&issueTaint)
	return
}

// T26: the downgrade fences at once. Nothing new is authorized from that moment.
func TestStartDowngradeFencesAndRefusesNewAuthorizations(t *testing.T) {
	f := claimElevated(t, "repo_write:1234")
	var got struct {
		State string `json:"state"`
		Epoch int64  `json:"epoch"`
	}
	f.startDowngrade(t, 1).Want(http.StatusOK).JSON(&got)
	if got.State != "fencing" || got.Epoch != 2 {
		t.Fatalf("start = %+v, want fencing at epoch 2", got)
	}
	if state, epoch := f.state(t); state != "fencing" || epoch != 2 {
		t.Fatalf("row = %s epoch %d, want fencing at epoch 2", state, epoch)
	}
	f.authorize(t, 1, "repo_write:1234", hashA).Want(http.StatusForbidden)
	f.authorize(t, 2, "repo_write:1234", hashA).Want(http.StatusForbidden)
	if a, b := f.taintCounts(t); a != 0 || b != 0 {
		t.Fatalf("taint before delivery = task %d issue %d, want none yet", a, b)
	}
}

func TestStartDowngradeRefusesStaleEpochAndUnelevatedTask(t *testing.T) {
	f := claimElevated(t, "repo_write:1234")
	f.startDowngrade(t, 7).Want(http.StatusForbidden) // epoch the run never held
	if state, epoch := f.state(t); state != "elevated" || epoch != 1 {
		t.Fatalf("a refused downgrade moved the fence: %s epoch %d", state, epoch)
	}
	other := claimTaintedResume(t, false) // restricted claim: no elevation row
	other.startDowngrade(t, 1).Want(http.StatusForbidden)
}

func TestStartDowngradeIsIdempotentAfterTheFence(t *testing.T) {
	f := claimElevated(t, "repo_write:1234")
	f.startDowngrade(t, 1).Want(http.StatusOK)
	var got struct {
		State string `json:"state"`
		Epoch int64  `json:"epoch"`
	}
	f.startDowngrade(t, 1).Want(http.StatusOK).JSON(&got)
	if got.Epoch != 2 {
		t.Fatalf("second start = %+v, want the same epoch 2", got)
	}
}

// T28 (b)/(e) server half: an operation that has not ended holds the fence open,
// so deliver refuses and nothing is tainted: no bytes can follow.
func TestDeliverRefusedWhileAnOperationIsOpen(t *testing.T) {
	f := claimElevated(t, "repo_write:1234")
	opID := f.authorizeOK(t, "repo_write:1234")
	f.startDowngrade(t, 1).Want(http.StatusOK)
	f.deliverDowngrade(t).Want(http.StatusConflict)
	if state, _ := f.state(t); state == "restricted_final" {
		t.Fatal("fence finished with an open operation")
	}
	if a, b := f.taintCounts(t); a != 0 || b != 0 {
		t.Fatalf("refused delivery tainted: task %d issue %d", a, b)
	}
	f.complete(t, opID, "aborted").Want(http.StatusOK)
	f.deliverDowngrade(t).Want(http.StatusOK)
}

func TestDeliverRefusedBeforeAnyFence(t *testing.T) {
	f := claimElevated(t, "repo_write:1234")
	f.deliverDowngrade(t).Want(http.StatusConflict)
	if a, b := f.taintCounts(t); a != 0 || b != 0 {
		t.Fatalf("delivery without a fence tainted: task %d issue %d", a, b)
	}
}

func TestDeliverRefusedWhileLaunchScopeProcessesLive(t *testing.T) {
	f := claimElevated(t, "docker")
	f.startDowngrade(t, 1).Want(http.StatusOK)
	f.deliverDowngrade(t).Want(http.StatusConflict) // T28 (j): processes must exit first
	f.launchScopesEnded(t).Want(http.StatusOK)
	f.deliverDowngrade(t).Want(http.StatusOK)
}

// T26/T28 (i): a completed downgrade finishes the fence and taints the task and
// the issue in the same call; every later run on the issue is restricted.
func TestDeliverTaintsTaskAndIssueAndNothingReelevates(t *testing.T) {
	ctx := context.Background()
	f := claimElevated(t, "repo_write:1234")
	f.startDowngrade(t, 1).Want(http.StatusOK)
	var got struct {
		State string `json:"state"`
		Epoch int64  `json:"epoch"`
	}
	f.deliverDowngrade(t).Want(http.StatusOK).JSON(&got)
	if got.State != "restricted_final" || got.Epoch != 2 {
		t.Fatalf("deliver = %+v, want restricted_final at epoch 2", got)
	}
	if a, b := f.taintCounts(t); a != 1 || b != 1 {
		t.Fatalf("taint after delivery = task %d issue %d, want 1 and 1", a, b)
	}
	f.deliverDowngrade(t).Want(http.StatusOK) // the proxy may repeat it: one taint ID, not two
	if a, b := f.taintCounts(t); a != 1 || b != 1 {
		t.Fatalf("repeat delivery changed taint: task %d issue %d", a, b)
	}

	// (i) a new grant on the issue does not elevate the same task re-claimed ...
	insertGrant(t, f.issueID, testWorkspaceID, []string{"repo_write:1234"}, "1 minute", "1 hour", false)
	var taskTaint []string
	dbfx.QueryRow(t, `SELECT taint_ids FROM agent_task_queue WHERE id = $1`, f.taskID).Scan(&taskTaint)
	if got := testHandler.runProfileForTask(ctx, parseUUID(testWorkspaceID), parseUUID(f.issueID), parseUUID(f.taskID), taskTaint); got.Profile != "restricted" {
		t.Fatalf("re-claimed task profile = %+v, want restricted", got)
	}
	f.authorize(t, 2, "repo_write:1234", hashA).Want(http.StatusForbidden)

	// ... nor a continuation task, nor any other task on the issue.
	var agentID string
	dbfx.QueryRow(t, `SELECT agent_id FROM agent_task_queue WHERE id = $1`, f.taskID).Scan(&agentID)
	dbfx.Exec(t, `UPDATE agent_task_queue SET status = 'completed', completed_at = now() WHERE id = $1`, f.taskID)
	next := createDispatchedClaimFixtureTask(t, ctx, agentID, f.runtimeID, f.issueID, "120 seconds", false)
	dbfx.Exec(t, `UPDATE agent_task_queue SET parent_task_id = $2 WHERE id = $1`, next, f.taskID)
	req := withURLParam(newOwnerDaemonRequest("POST", "/api/daemon/runtimes/"+f.runtimeID+"/tasks/claim", nil), "runtimeId", f.runtimeID)
	var resp struct {
		Task *struct {
			ID      string `json:"id"`
			Profile string `json:"profile"`
		} `json:"task"`
	}
	testutil.Call(t, testHandler.ClaimTaskByRuntime, req).Want(http.StatusOK).JSON(&resp)
	if resp.Task == nil || resp.Task.ID != next || resp.Task.Profile != "restricted" {
		t.Fatalf("continuation claim = %+v, want task %s restricted", resp.Task, next)
	}
	var inherited int
	dbfx.QueryRow(t, `SELECT cardinality(taint_ids) FROM agent_task_queue WHERE id = $1`, next).Scan(&inherited)
	if inherited != 1 {
		t.Fatalf("continuation inherited %d taint IDs, want 1", inherited)
	}
}
