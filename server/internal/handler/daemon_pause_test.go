package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

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

type pauseJSON struct {
	Reason    string  `json:"reason"`
	PausedBy  *string `json:"paused_by"`
	PausedAt  string  `json:"paused_at"`
	ExpiresAt string  `json:"expires_at"`
}

func putPause(t *testing.T, actorID, runtimeID string, body any) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	testHandler.PauseDaemon(w, withURLParam(newRequestAs(actorID, http.MethodPut, "/api/runtimes/"+runtimeID+"/pause", body), "runtimeId", runtimeID))
	return w
}

func deletePause(t *testing.T, actorID, runtimeID string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	testHandler.ResumeDaemon(w, withURLParam(newRequestAs(actorID, http.MethodDelete, "/api/runtimes/"+runtimeID+"/pause", nil), "runtimeId", runtimeID))
	return w
}

func listRuntimesForTest(t *testing.T) map[string]AgentRuntimeResponse {
	t.Helper()
	list := testutil.Decode[[]AgentRuntimeResponse](t, testHandler.ListAgentRuntimes, newRequest(http.MethodGet, "/api/runtimes", nil), http.StatusOK)
	byID := map[string]AgentRuntimeResponse{}
	for _, rt := range list {
		byID[rt.ID] = rt
	}
	return byID
}

func TestDaemonPause_PauseAndResumeAPI(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	const daemonID = "pause-api-machine"
	runtimeA, _ := seedPausableRuntime(t, ctx, daemonID, "Pause api A")
	// A second provider on the same machine: pausing is per machine, not per runtime.
	runtimeB := dbfx.Runtime(t, "Pause api B", testutil.Cols{"daemon_id": daemonID, "provider": "handler_test_runtime_b"})
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM daemon_pause WHERE workspace_id = $1 AND daemon_id = $2`, testWorkspaceID, daemonID)
	})

	if w := putPause(t, testUserID, runtimeA, map[string]any{"reason": "bogus"}); w.Code != http.StatusBadRequest {
		t.Fatalf("invalid reason = %d, want 400: %s", w.Code, w.Body.String())
	}

	w := putPause(t, testUserID, runtimeA, map[string]any{"reason": "incident"})
	if w.Code != http.StatusOK {
		t.Fatalf("pause = %d, want 200: %s", w.Code, w.Body.String())
	}
	var got pauseJSON
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || got.Reason != "incident" || got.PausedBy == nil || *got.PausedBy != testUserID || got.PausedAt == "" {
		t.Fatalf("pause body = %s (err %v), want reason incident by %s", w.Body.String(), err, testUserID)
	}

	listed := listRuntimesForTest(t)
	for _, id := range []string{runtimeA, runtimeB} {
		if p := listed[id].Pause; p == nil || p.Reason != "incident" {
			t.Fatalf("runtime %s pause = %+v, want incident (both runtimes share the machine)", id, p)
		}
	}
	if n := listed[runtimeA].DaemonRunningTasks; n == nil || *n != 0 {
		t.Fatalf("daemon_running_tasks = %v, want 0 (task is queued, not running)", n)
	}

	// Machine counts dispatched and running tasks; queued ones wait on the server.
	dbfx.Exec(t, `UPDATE agent_task_queue SET status = 'running', started_at = now() WHERE runtime_id = $1`, runtimeA)
	if n := listRuntimesForTest(t)[runtimeB].DaemonRunningTasks; n == nil || *n != 1 {
		t.Fatalf("daemon_running_tasks on sibling runtime = %v, want 1", n)
	}

	if w := deletePause(t, testUserID, runtimeB); w.Code != http.StatusNoContent {
		t.Fatalf("resume = %d, want 204: %s", w.Code, w.Body.String())
	}
	if w := deletePause(t, testUserID, runtimeA); w.Code != http.StatusNoContent {
		t.Fatalf("second resume = %d, want 204 (idempotent): %s", w.Code, w.Body.String())
	}
	if p := listRuntimesForTest(t)[runtimeA].Pause; p != nil {
		t.Fatalf("pause after resume = %+v, want nil", p)
	}
}

func TestDaemonPause_Authorization(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	privateRuntime, _, plainMemberID := runtimeVisibilityFixture(t)
	if w := putPause(t, plainMemberID, privateRuntime, map[string]any{"reason": "maintenance"}); w.Code != http.StatusNotFound {
		t.Fatalf("member pausing another member's private runtime = %d, want 404: %s", w.Code, w.Body.String())
	}

	cloudRuntime := dbfx.Runtime(t, "Pause api cloud", nil)
	if w := putPause(t, testUserID, cloudRuntime, map[string]any{"reason": "maintenance"}); w.Code != http.StatusBadRequest {
		t.Fatalf("pausing a runtime with no daemon = %d, want 400: %s", w.Code, w.Body.String())
	}
}

func TestDaemonPause_WorkspaceDeleteRemovesRows(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ws := dbfx.Workspace(t, "Pause cleanup", "pause-cleanup")
	dbfx.Exec(t, `INSERT INTO daemon_pause (workspace_id, daemon_id, reason) VALUES ($1, 'pause-cleanup-machine', 'maintenance')`, ws)
	if err := testHandler.Queries.DeleteWorkspace(context.Background(), parseUUID(ws)); err != nil {
		t.Fatalf("delete workspace: %v", err)
	}
	var left int
	dbfx.QueryRow(t, `SELECT count(*) FROM daemon_pause WHERE workspace_id = $1`, ws).Scan(&left)
	if left != 0 {
		t.Fatalf("daemon_pause rows after workspace delete = %d, want 0", left)
	}
}

// Auto-expiry (Cheese, 29/10/2026): maintenance lasts 2 h, incident and
// suspended 30 min. An expired pause no longer blocks claims.
func TestDaemonPause_ExpiredPauseDoesNotBlockClaim(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	cases := []struct {
		name        string
		reason      string
		age         string
		wantBlocked bool
	}{
		{"maintenance 3h old expired", "maintenance", "3 hours", false},
		{"maintenance 1h old live", "maintenance", "1 hour", true},
		{"incident 31m old expired", "incident", "31 minutes", false},
		{"incident 29m old live", "incident", "29 minutes", true},
		{"suspended 31m old expired", "suspended", "31 minutes", false},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			daemonID := "pause-expiry-" + string(rune('a'+i))
			runtimeID, taskID := seedPausableRuntime(t, ctx, daemonID, "Pause expiry "+daemonID)
			dbfx.Exec(t, `INSERT INTO daemon_pause (workspace_id, daemon_id, reason, paused_by, paused_at)
				VALUES ($1, $2, $3, $4, now() - $5::interval)`, testWorkspaceID, daemonID, tc.reason, testUserID, tc.age)
			t.Cleanup(func() {
				testPool.Exec(context.Background(), `DELETE FROM daemon_pause WHERE workspace_id = $1 AND daemon_id = $2`, testWorkspaceID, daemonID)
			})
			got := singleClaimTaskID(t, runtimeID)
			if tc.wantBlocked && got != "" {
				t.Fatalf("live %s pause claimed task %s, want none", tc.reason, got)
			}
			if !tc.wantBlocked && got != taskID {
				t.Fatalf("expired %s pause claimed %q, want task %s", tc.reason, got, taskID)
			}
			if p := listRuntimesForTest(t)[runtimeID].Pause; tc.wantBlocked != (p != nil) {
				t.Fatalf("listed pause = %+v, want present=%v", p, tc.wantBlocked)
			}
		})
	}
}

func TestDaemonPause_ResponseCarriesExpiry(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	runtimeID, _ := seedPausableRuntime(t, ctx, "pause-expiry-body", "Pause expiry body")
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM daemon_pause WHERE workspace_id = $1 AND daemon_id = 'pause-expiry-body'`, testWorkspaceID)
	})
	w := putPause(t, testUserID, runtimeID, map[string]any{"reason": "incident"})
	var got pauseJSON
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || got.ExpiresAt == "" {
		t.Fatalf("pause body = %s (err %v), want expires_at", w.Body.String(), err)
	}
	paused, err1 := time.Parse(time.RFC3339Nano, got.PausedAt)
	expires, err2 := time.Parse(time.RFC3339Nano, got.ExpiresAt)
	if err1 != nil || err2 != nil || expires.Sub(paused) != 30*time.Minute {
		t.Fatalf("incident expiry = %s after pause (%v %v), want 30m", expires.Sub(paused), err1, err2)
	}
}

// Only the daemon (runtime) owner and the workspace owner may pause or resume.
// A workspace admin who is neither is refused.
func TestDaemonPause_AdminWhoIsNotOwnerIsRefused(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	runtimeID, runtimeOwnerID, _ := runtimeVisibilityFixture(t)
	dbfx.Exec(t, `UPDATE agent_runtime SET daemon_id = 'pause-authz-machine', visibility = 'public' WHERE id = $1`, runtimeID)
	var adminID string
	if err := testPool.QueryRow(context.Background(), `INSERT INTO "user" (name, email) VALUES ('Pause Admin', 'pause-admin@multica.test') RETURNING id`).Scan(&adminID); err != nil {
		t.Fatalf("create admin user: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM "user" WHERE email = 'pause-admin@multica.test'`)
		testPool.Exec(context.Background(), `DELETE FROM daemon_pause WHERE workspace_id = $1 AND daemon_id = 'pause-authz-machine'`, testWorkspaceID)
	})
	dbfx.Exec(t, `INSERT INTO member (workspace_id, user_id, role) VALUES ($1, $2, 'admin')`, testWorkspaceID, adminID)

	if w := putPause(t, adminID, runtimeID, map[string]any{"reason": "maintenance"}); w.Code != http.StatusForbidden {
		t.Fatalf("admin (not owner) pause = %d, want 403: %s", w.Code, w.Body.String())
	}
	if w := putPause(t, runtimeOwnerID, runtimeID, map[string]any{"reason": "maintenance"}); w.Code != http.StatusOK {
		t.Fatalf("runtime owner pause = %d, want 200: %s", w.Code, w.Body.String())
	}
	if w := deletePause(t, adminID, runtimeID); w.Code != http.StatusForbidden {
		t.Fatalf("admin (not owner) resume = %d, want 403: %s", w.Code, w.Body.String())
	}
	if w := deletePause(t, testUserID, runtimeID); w.Code != http.StatusNoContent {
		t.Fatalf("workspace owner resume = %d, want 204: %s", w.Code, w.Body.String())
	}
}
