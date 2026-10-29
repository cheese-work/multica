package handler

import (
	"context"
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// seedPausableRuntime creates a runtime pinned to daemonID with one queued task.
func seedPausableRuntime(t *testing.T, ctx context.Context, daemonID, name string) (runtimeID, taskID string) {
	t.Helper()
	runtimeID = dbfx.Runtime(t, name, testutil.Cols{"daemon_id": daemonID})
	agentID, issueID := createClaimReclaimAgentAndIssue(t, ctx, runtimeID, name+" agent")
	taskID = seedQueuedIssueTask(t, ctx, agentID, runtimeID, issueID)
	return runtimeID, taskID
}

func pauseDaemonForTest(t *testing.T, daemonID string) {
	t.Helper()
	dbfx.Exec(t, `INSERT INTO daemon_pause (workspace_id, daemon_id, reason, paused_by) VALUES ($1, $2, 'maintenance', $3)`,
		testWorkspaceID, daemonID, testUserID)
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM daemon_pause WHERE workspace_id = $1 AND daemon_id = $2`, testWorkspaceID, daemonID)
	})
}

func singleClaimTaskID(t *testing.T, runtimeID string) string {
	t.Helper()
	req := withURLParam(newOwnerDaemonRequest(http.MethodPost, "/api/daemon/runtimes/"+runtimeID+"/tasks/claim", nil), "runtimeId", runtimeID)
	resp := testutil.Decode[struct {
		Task *struct {
			ID string `json:"id"`
		} `json:"task"`
	}](t, testHandler.ClaimTaskByRuntime, req, http.StatusOK)
	if resp.Task == nil {
		return ""
	}
	return resp.Task.ID
}

func TestDaemonPause_SingleClaimRefusedUntilResume(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	pausedRuntime, pausedTask := seedPausableRuntime(t, ctx, "pause-gate-single", "Pause gate single")
	otherRuntime, otherTask := seedPausableRuntime(t, ctx, "pause-gate-single-other", "Pause gate other machine")
	pauseDaemonForTest(t, "pause-gate-single")

	if got := singleClaimTaskID(t, pausedRuntime); got != "" {
		t.Fatalf("paused daemon claimed task %s, want none", got)
	}
	if got := singleClaimTaskID(t, otherRuntime); got != otherTask {
		t.Fatalf("unpaused daemon in the same workspace claimed %q, want %s", got, otherTask)
	}

	dbfx.Exec(t, `DELETE FROM daemon_pause WHERE workspace_id = $1 AND daemon_id = $2`, testWorkspaceID, "pause-gate-single")
	if got := singleClaimTaskID(t, pausedRuntime); got != pausedTask {
		t.Fatalf("resumed daemon claimed %q, want queued task %s", got, pausedTask)
	}
}

func TestDaemonPause_BatchClaimRefusedUntilResume(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	const daemonID = "pause-gate-batch"
	runtimeID, taskID := seedPausableRuntime(t, ctx, daemonID, "Pause gate batch")
	pauseDaemonForTest(t, daemonID)

	claim := func() batchClaimResponse {
		req := newOwnerDaemonRequest(http.MethodPost, "/api/daemon/tasks/claim",
			map[string]any{"daemon_id": daemonID, "runtime_ids": []string{runtimeID}, "max_tasks": 4})
		return testutil.Decode[batchClaimResponse](t, testHandler.ClaimTasksByRuntime, req, http.StatusOK)
	}

	if got := claim(); len(got.Tasks) != 0 {
		t.Fatalf("paused daemon batch-claimed %d tasks, want none", len(got.Tasks))
	}
	dbfx.Exec(t, `DELETE FROM daemon_pause WHERE workspace_id = $1 AND daemon_id = $2`, testWorkspaceID, daemonID)
	if got := claim(); len(got.Tasks) != 1 || got.Tasks[0].ID != taskID {
		t.Fatalf("resumed daemon batch claim = %+v, want task %s", got.Tasks, taskID)
	}
}
