package handler

import (
	"context"
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// Taint removes operation authority, not only the claim's launch profile
// (CHE-2216 plan 3.1, 3.8; T9). The claim records an elevation before it
// inherits session taint, so the stored row must not stay usable.

// claimTaintedResume claims a task whose resumed session is tainted, on an issue
// holding a live repo_write grant, and returns the claimed task's fixture.
func claimTaintedResume(t *testing.T, rerun bool) elevatedFixture {
	t.Helper()
	ctx := context.Background()
	runtimeID := createClaimReclaimRuntime(t, ctx, "Taint op runtime")
	agentID, issueID := createClaimReclaimAgentAndIssue(t, ctx, runtimeID, "Taint op agent")
	insertGrant(t, issueID, testWorkspaceID, []string{"repo_write:1234"}, "1 minute", "1 hour", false)
	var taskID string
	if rerun {
		clean := dbfx.Task(t, agentID, testutil.Cols{
			"status": "completed", "runtime_id": runtimeID, "issue_id": issueID,
			"session_id": "sess-op", "started_at": testutil.Raw("now() - interval '2 minutes'"), "completed_at": testutil.Raw("now() - interval '90 seconds'"),
		})
		dbfx.Task(t, agentID, testutil.Cols{
			"status": "completed", "runtime_id": runtimeID, "issue_id": issueID,
			"session_id": "sess-op", "started_at": testutil.Raw("now() - interval '1 minute'"), "completed_at": testutil.Raw("now()"),
			"taint_ids": []string{"t1"},
		})
		dbfx.QueryRow(t, `INSERT INTO agent_task_queue (agent_id, runtime_id, issue_id, status, priority, rerun_of_task_id, force_fresh_session)
			VALUES ($1, $2, $3, 'queued', 0, $4, TRUE) RETURNING id`, agentID, runtimeID, issueID, clean).Scan(&taskID)
	} else {
		dbfx.Task(t, agentID, testutil.Cols{
			"status": "completed", "runtime_id": runtimeID, "issue_id": issueID,
			"session_id": "sess-op", "started_at": testutil.Raw("now() - interval '1 minute'"), "completed_at": testutil.Raw("now()"),
			"taint_ids": []string{"t1"},
		})
		taskID = createDispatchedClaimFixtureTask(t, ctx, agentID, runtimeID, issueID, "120 seconds", false)
	}
	claimRestrictedWithTaint(t, runtimeID, []string{"t1"})
	return elevatedFixture{taskID: taskID, issueID: issueID, runtimeID: runtimeID}
}

// refuseEveryEpoch fails when any epoch of the task authorizes the scope.
func (f elevatedFixture) refuseEveryEpoch(t *testing.T, scope string) {
	t.Helper()
	var current int64
	dbfx.QueryRow(t, `SELECT COALESCE(max(epoch), 1) FROM task_elevation WHERE task_id = $1`, f.taskID).Scan(&current)
	for _, epoch := range []int64{1, current} {
		f.authorize(t, epoch, scope, hashA).Want(http.StatusForbidden)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM elevated_op WHERE task_id = $1`, f.taskID); n != 0 {
		t.Fatalf("authorized operations = %d, want 0 for a tainted task", n)
	}
}

func TestNormalResumeOfTaintedSessionRefusesOperationAuthority(t *testing.T) {
	f := claimTaintedResume(t, false)
	f.refuseEveryEpoch(t, "repo_write:1234")
	if state, _ := f.state(t); state == "elevated" {
		t.Fatalf("elevation state = %q after a restricted claim, want it fenced", state)
	}
}

func TestRerunInTaintedSessionRefusesOperationAuthority(t *testing.T) {
	f := claimTaintedResume(t, true)
	f.refuseEveryEpoch(t, "repo_write:1234")
	if state, _ := f.state(t); state == "elevated" {
		t.Fatalf("elevation state = %q after a restricted claim, want it fenced", state)
	}
}

func TestTaintArrivingAfterElevatedClaimRefusesAndFences(t *testing.T) {
	f := claimElevated(t, "repo_write:1234")
	dbfx.Exec(t, `UPDATE agent_task_queue SET taint_ids = ARRAY['t1'] WHERE id = $1`, f.taskID)

	// Refused at once, before any sweep.
	f.authorize(t, 1, "repo_write:1234", hashA).Want(http.StatusForbidden)
	if n := dbfx.Count(t, `SELECT count(*) FROM elevated_op WHERE task_id = $1`, f.taskID); n != 0 {
		t.Fatalf("authorized operations = %d, want 0", n)
	}
	// The fence sweep then reconciles the stored elevation.
	fenceNow(t)
	if state, epoch := f.state(t); state == "elevated" || epoch != 2 {
		t.Fatalf("after sweep state = %q epoch = %d, want fenced at epoch 2", state, epoch)
	}
}
