package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// A daemon_id is chosen by the client at registration, so it is not an identity
// on its own: another owner can register the same string. The machine is
// (runtime owner, daemon_id).

// seedForeignOwnerRuntime registers a runtime for a different user, in that
// user's own workspace, on a daemon_id copied from someone else's machine.
func seedForeignOwnerRuntime(t *testing.T, daemonID, slug string) (ownerID, workspaceID, runtimeID, taskID string) {
	t.Helper()
	ownerID = dbfx.User(t, "Pause "+slug+" owner", slug+"@multica.test")
	workspaceID = dbfx.Workspace(t, "Pause "+slug, slug)
	dbfx.Member(t, workspaceID, ownerID, "owner")
	runtimeID = dbfx.Runtime(t, "Pause "+slug+" runtime", testutil.Cols{"workspace_id": workspaceID, "daemon_id": daemonID, "owner_id": ownerID})
	agentID := dbfx.Agent(t, "Pause "+slug+" agent", runtimeID, testutil.Cols{"workspace_id": workspaceID, "owner_id": ownerID})
	issueID := dbfx.Issue(t, "Pause "+slug+" issue", testutil.Cols{"workspace_id": workspaceID})
	taskID = dbfx.Task(t, agentID, testutil.Cols{"runtime_id": runtimeID, "issue_id": issueID})
	return ownerID, workspaceID, runtimeID, taskID
}

// claimAs runs the single-runtime claim as the runtime's own owner and workspace.
func claimAs(t *testing.T, userID, workspaceID, runtimeID string) string {
	t.Helper()
	req := newRequestAs(userID, http.MethodPost, "/api/daemon/runtimes/"+runtimeID+"/tasks/claim", nil)
	req.Header.Set("X-Workspace-ID", workspaceID)
	resp := testutil.Decode[struct {
		Task *struct {
			ID string `json:"id"`
		} `json:"task"`
	}](t, testHandler.ClaimTaskByRuntime, withURLParam(req, "runtimeId", runtimeID), http.StatusOK)
	if resp.Task == nil {
		return ""
	}
	return resp.Task.ID
}

func cleanupPausesFor(t *testing.T, daemonID string) {
	t.Helper()
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM daemon_pause WHERE daemon_id = $1`, daemonID)
	})
}

func listRuntimesAs(t *testing.T, userID, workspaceID string) map[string]AgentRuntimeResponse {
	t.Helper()
	req := newRequestAs(userID, http.MethodGet, "/api/runtimes", nil)
	req.Header.Set("X-Workspace-ID", workspaceID)
	list := testutil.Decode[[]AgentRuntimeResponse](t, testHandler.ListAgentRuntimes, req, http.StatusOK)
	byID := map[string]AgentRuntimeResponse{}
	for _, rt := range list {
		byID[rt.ID] = rt
	}
	return byID
}

func TestDaemonPause_CopiedDaemonIDFromAnotherOwnerIsNotTheSameMachine(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	const daemonID = "pause-copied-id-machine"
	cleanupPausesFor(t, daemonID)
	victimRuntime, victimTask := seedPausableRuntime(t, ctx, daemonID, "Pause copied victim")
	intruderID, intruderWS, intruderRuntime, intruderTask := seedForeignOwnerRuntime(t, daemonID, "pause-copied-intruder")

	// The victim pauses their machine. That must not hold the intruder's.
	if w := putPause(t, testUserID, victimRuntime, map[string]any{"reason": "incident"}); w.Code != http.StatusOK {
		t.Fatalf("victim pause = %d: %s", w.Code, w.Body.String())
	}
	if got := claimAs(t, intruderID, intruderWS, intruderRuntime); got != intruderTask {
		t.Fatalf("a pause on the victim's machine held another owner's runtime: claimed %q, want %s", got, intruderTask)
	}

	// The intruder cannot lift the victim's pause with a copied daemon_id.
	if w := deletePause(t, intruderID, intruderRuntime); w.Code != http.StatusNoContent {
		t.Fatalf("intruder resume = %d, want 204: %s", w.Code, w.Body.String())
	}
	if got := singleClaimTaskID(t, victimRuntime); got != "" {
		t.Fatalf("intruder resume lifted the victim's pause: claimed %s", got)
	}

	// Nor read it, nor the victim's running tasks.
	dbfx.Exec(t, `UPDATE agent_task_queue SET status = 'running', started_at = now() WHERE runtime_id = $1`, victimRuntime)
	seen := listRuntimesAs(t, intruderID, intruderWS)[intruderRuntime]
	if seen.Pause != nil {
		t.Fatalf("intruder sees the victim's pause: %+v", seen.Pause)
	}
	// The intruder's own claimed task counts (1); the victim's running one does not.
	if seen.DaemonRunningTasks == nil || *seen.DaemonRunningTasks != 1 {
		got := -1
		if seen.DaemonRunningTasks != nil {
			got = *seen.DaemonRunningTasks
		}
		t.Fatalf("intruder sees daemon_running_tasks = %d, want 1 (only their own task)", got)
	}

	// And the intruder pausing their own runtime does not stop the victim.
	dbfx.Exec(t, `UPDATE agent_task_queue SET status = 'queued', started_at = NULL WHERE runtime_id = $1`, victimRuntime)
	if w := deletePause(t, testUserID, victimRuntime); w.Code != http.StatusNoContent {
		t.Fatalf("victim resume = %d: %s", w.Code, w.Body.String())
	}
	if w := putPause(t, intruderID, intruderRuntime, map[string]any{"reason": "incident"}); w.Code != http.StatusOK {
		t.Fatalf("intruder pause = %d: %s", w.Code, w.Body.String())
	}
	if got := singleClaimTaskID(t, victimRuntime); got != victimTask {
		t.Fatalf("another owner's pause held the victim's machine: claimed %q, want %s", got, victimTask)
	}
}

// One owner's machine joined to two workspaces stays one machine.
func TestDaemonPause_SameOwnerMachineIsSharedAcrossWorkspaces(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	const daemonID = "pause-same-owner-machine"
	cleanupPausesFor(t, daemonID)
	runtimeID, _ := seedPausableRuntime(t, ctx, daemonID, "Pause same owner")
	otherWS, otherRuntime, otherTask := seedSecondWorkspaceRuntime(t, ctx, daemonID, "pause-same-owner")

	if w := putPause(t, testUserID, runtimeID, map[string]any{"reason": "maintenance"}); w.Code != http.StatusOK {
		t.Fatalf("pause = %d: %s", w.Code, w.Body.String())
	}
	if got := singleClaimTaskID(t, otherRuntime); got != "" {
		t.Fatalf("same owner's other workspace claimed %s while the machine is paused", got)
	}
	if p := listRuntimesAs(t, testUserID, otherWS)[otherRuntime].Pause; p == nil {
		t.Fatal("the pause is not visible from the owner's other workspace")
	}
	if w := deletePause(t, testUserID, otherRuntime); w.Code != http.StatusNoContent {
		t.Fatalf("resume from the other workspace = %d: %s", w.Code, w.Body.String())
	}
	if got := singleClaimTaskID(t, otherRuntime); got != otherTask {
		t.Fatalf("after resume the other workspace claimed %q, want %s", got, otherTask)
	}
}

// Two workspaces can set different reasons on one machine. The gate, the
// stored row and the expires_at the client counts down to must agree.
func TestDaemonPause_MixedReasonsAcrossWorkspacesExpireTogether(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	const daemonID = "pause-mixed-expiry-machine"
	cleanupPausesFor(t, daemonID)
	runtimeA, taskA := seedPausableRuntime(t, ctx, daemonID, "Pause mixed A")
	_, runtimeB, _ := seedSecondWorkspaceRuntime(t, ctx, daemonID, "pause-mixed-b")

	// 40 min ago a maintenance pause (2 h) from one workspace, 20 min ago an
	// incident pause (30 min) from the other.
	if w := putPause(t, testUserID, runtimeA, map[string]any{"reason": "maintenance"}); w.Code != http.StatusOK {
		t.Fatalf("maintenance pause = %d: %s", w.Code, w.Body.String())
	}
	dbfx.Exec(t, `UPDATE daemon_pause SET paused_at = now() - interval '40 minutes' WHERE daemon_id = $1`, daemonID)
	if w := putPause(t, testUserID, runtimeB, map[string]any{"reason": "incident"}); w.Code != http.StatusOK {
		t.Fatalf("incident pause = %d: %s", w.Code, w.Body.String())
	}
	dbfx.Exec(t, `UPDATE daemon_pause SET paused_at = now() - interval '20 minutes' WHERE daemon_id = $1 AND reason = 'incident'`, daemonID)

	pause := listRuntimesForTest(t)[runtimeA].Pause
	if pause == nil {
		t.Fatal("machine shows no pause, want one")
	}
	expires, err := time.Parse(time.RFC3339Nano, pause.ExpiresAt)
	if err != nil {
		t.Fatalf("expires_at %q: %v", pause.ExpiresAt, err)
	}
	if got := singleClaimTaskID(t, runtimeA); got != "" {
		t.Fatalf("pause did not hold the gate: claimed %s", got)
	}

	// Time passes until the moment the client says the pause ends.
	dbfx.Exec(t, `UPDATE daemon_pause SET paused_at = paused_at - make_interval(secs => $2) WHERE daemon_id = $1`,
		daemonID, (time.Until(expires) + time.Minute).Seconds())
	if p := listRuntimesForTest(t)[runtimeA].Pause; p != nil {
		t.Fatalf("pause still listed after the advertised expiry: %+v", p)
	}
	if got := singleClaimTaskID(t, runtimeA); got != taskA {
		t.Fatalf("the gate still holds after the advertised expiry %s: claimed %q, want %s", pause.ExpiresAt, got, taskA)
	}
}

// refreshRecorder records the workspaces that get a runtime refresh.
type refreshRecorder struct {
	mu     sync.Mutex
	active bool
	byWS   map[string][]string
}

func recordRuntimeRefreshes(t *testing.T) *refreshRecorder {
	t.Helper()
	rec := &refreshRecorder{active: true, byWS: map[string][]string{}}
	testHandler.Bus.Subscribe(protocol.EventDaemonRegister, func(e events.Event) {
		rec.mu.Lock()
		defer rec.mu.Unlock()
		if !rec.active {
			return
		}
		payload, _ := e.Payload.(map[string]any)
		action, _ := payload["action"].(string)
		rec.byWS[e.WorkspaceID] = append(rec.byWS[e.WorkspaceID], action)
	})
	t.Cleanup(func() {
		rec.mu.Lock()
		rec.active = false
		rec.mu.Unlock()
	})
	return rec
}

func (r *refreshRecorder) actions(workspaceID string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := append([]string(nil), r.byWS[workspaceID]...)
	sort.Strings(out)
	return out
}

// Pause and Resume must reach every workspace the machine serves for its owner,
// or the Machine card and banner there keep showing the old state. They must not
// reach the workspace of another owner who copied the daemon_id.
func TestDaemonPause_PauseAndResumeRefreshEveryWorkspaceOfTheMachine(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	const daemonID = "pause-refresh-machine"
	cleanupPausesFor(t, daemonID)
	runtimeA, _ := seedPausableRuntime(t, ctx, daemonID, "Pause refresh A")
	otherWS, _, _ := seedSecondWorkspaceRuntime(t, ctx, daemonID, "pause-refresh-b")
	_, intruderWS, _, _ := seedForeignOwnerRuntime(t, daemonID, "pause-refresh-intruder")
	rec := recordRuntimeRefreshes(t)

	if w := putPause(t, testUserID, runtimeA, map[string]any{"reason": "maintenance"}); w.Code != http.StatusOK {
		t.Fatalf("pause = %d: %s", w.Code, w.Body.String())
	}
	if got := rec.actions(otherWS); len(got) != 1 || got[0] != "pause" {
		t.Fatalf("owner's other workspace refresh after pause = %v, want [pause]", got)
	}
	if got := rec.actions(testWorkspaceID); len(got) != 1 || got[0] != "pause" {
		t.Fatalf("own workspace refresh after pause = %v, want [pause]", got)
	}
	if w := deletePause(t, testUserID, runtimeA); w.Code != http.StatusNoContent {
		t.Fatalf("resume = %d: %s", w.Code, w.Body.String())
	}
	if got := rec.actions(otherWS); len(got) != 2 || got[0] != "pause" || got[1] != "resume" {
		t.Fatalf("owner's other workspace refresh after resume = %v, want [pause resume]", got)
	}
	if got := rec.actions(intruderWS); len(got) != 0 {
		t.Fatalf("another owner's workspace was refreshed: %v", got)
	}
}

// A task of one workspace starting or ending changes the drain count the
// Machine card shows in every workspace the machine serves. Only the task's own
// workspace hears the task event, so the others must get a runtime refresh, and
// only while the machine is paused. Another owner's workspace gets nothing.
func TestRefreshMachineOfTask_DrainChangeReachesTheMachinesOtherWorkspaces(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	const daemonID = "pause-drain-refresh-machine"
	cleanupPausesFor(t, daemonID)
	_, taskA := seedPausableRuntime(t, ctx, daemonID, "Pause drain A")
	otherWS, _, _ := seedSecondWorkspaceRuntime(t, ctx, daemonID, "pause-drain-b")
	_, intruderWS, _, _ := seedForeignOwnerRuntime(t, daemonID, "pause-drain-intruder")
	rec := recordRuntimeRefreshes(t)

	RefreshMachineOfTask(ctx, testHandler.Queries, testHandler.Bus, taskA, testWorkspaceID)
	if n := len(rec.actions(otherWS)); n != 0 {
		t.Fatalf("a machine that is not paused refreshed %d times, want 0 (task events are frequent)", n)
	}

	pauseDaemonForTest(t, daemonID)
	RefreshMachineOfTask(ctx, testHandler.Queries, testHandler.Bus, taskA, testWorkspaceID)
	if got := rec.actions(otherWS); len(got) != 1 || got[0] != "drain" {
		t.Fatalf("owner's other workspace refresh = %v, want [drain]", got)
	}
	if got := rec.actions(testWorkspaceID); len(got) != 0 {
		t.Fatalf("the task's own workspace was refreshed twice over (it hears the task event): %v", got)
	}
	if got := rec.actions(intruderWS); len(got) != 0 {
		t.Fatalf("another owner's workspace was refreshed: %v", got)
	}
}

// A pause belongs to the workspace that set it, so deleting that workspace
// lifts it for the whole machine. The surviving workspaces must hear that, or
// the one that shows "Safe to shut down" keeps saying so while the machine
// takes work again.
func TestDaemonPause_WorkspaceDeleteRefreshesTheSurvivingWorkspaces(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	const daemonID = "pause-delete-refresh-machine"
	cleanupPausesFor(t, daemonID)
	runtimeA, _ := seedPausableRuntime(t, ctx, daemonID, "Pause delete A")
	wsB, runtimeB, _ := seedSecondWorkspaceRuntime(t, ctx, daemonID, "pause-delete-b")
	wsC, runtimeC, _ := seedSecondWorkspaceRuntime(t, ctx, daemonID, "pause-delete-c")
	_, intruderWS, _, _ := seedForeignOwnerRuntime(t, daemonID, "pause-delete-intruder")

	if w := putPause(t, testUserID, runtimeB, map[string]any{"reason": "maintenance"}); w.Code != http.StatusOK {
		t.Fatalf("pause = %d: %s", w.Code, w.Body.String())
	}
	if p := listRuntimesAs(t, testUserID, testWorkspaceID)[runtimeA].Pause; p == nil {
		t.Fatal("the pause is not visible from the owner's first workspace")
	}
	rec := recordRuntimeRefreshes(t)

	w := httptest.NewRecorder()
	req := withURLParam(newRequest(http.MethodDelete, "/api/workspaces/"+wsB, nil), "id", wsB)
	testHandler.DeleteWorkspace(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("DeleteWorkspace = %d: %s", w.Code, w.Body.String())
	}

	for name, ws := range map[string]string{"first": testWorkspaceID, "third": wsC} {
		if got := rec.actions(ws); len(got) != 1 || got[0] != "resume" {
			t.Fatalf("surviving %s workspace refresh after the pausing workspace was deleted = %v, want [resume]", name, got)
		}
	}
	if got := rec.actions(wsB); len(got) != 0 {
		t.Fatalf("the deleted workspace was refreshed: %v", got)
	}
	if got := rec.actions(intruderWS); len(got) != 0 {
		t.Fatalf("another owner's workspace was refreshed: %v", got)
	}
	// What the refetch after that event shows: the machine takes work again.
	if p := listRuntimesAs(t, testUserID, testWorkspaceID)[runtimeA].Pause; p != nil {
		t.Fatalf("pause still listed after its workspace was deleted: %+v", p)
	}
	_ = runtimeC
}

// beforeSweepStarter runs hook once, inside the workspace delete transaction,
// after the delete has taken its lock and snapshots and before it sweeps any rows.
type beforeSweepStarter struct {
	inner txStarter
	hook  func()
}

func (s beforeSweepStarter) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := s.inner.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return &beforeSweepTx{Tx: tx, hook: s.hook}, nil
}

type beforeSweepTx struct {
	pgx.Tx
	hook func()
}

func (tx *beforeSweepTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if tx.hook != nil && strings.Contains(sql, "DELETE FROM daemon_token WHERE id IN (SELECT token_id FROM cloud_agent_connection") {
		hook := tx.hook
		tx.hook = nil
		hook()
	}
	return tx.Tx.Exec(ctx, sql, args...)
}

// pauseLockStarter reports, once, that a transaction is about to ask for the
// workspace lock Pause shares with the delete, so a test can release the delete
// only after the Pause is past its checks and queued behind it.
type pauseLockStarter struct {
	inner   txStarter
	reached chan struct{}
}

func (s pauseLockStarter) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := s.inner.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return &pauseLockTx{Tx: tx, reached: s.reached}, nil
}

type pauseLockTx struct {
	pgx.Tx
	reached chan struct{}
}

func (tx *pauseLockTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if tx.reached != nil && strings.Contains(sql, "FROM workspace WHERE id = $1 FOR SHARE") {
		close(tx.reached)
		tx.reached = nil
	}
	return tx.Tx.QueryRow(ctx, sql, args...)
}

// A Pause through B that starts while B's delete holds the workspace row must
// not leave A and C paused behind a sweep nobody refreshes: it waits for the
// delete, finds the workspace gone, writes nothing and publishes nothing.
func TestDaemonPause_PauseThroughADeletingWorkspaceIsRefusedAndLeavesNoPause(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	const daemonID = "pause-delete-lock-machine"
	cleanupPausesFor(t, daemonID)
	_, _ = seedPausableRuntime(t, ctx, daemonID, "Pause lock A")
	wsC, _, _ := seedSecondWorkspaceRuntime(t, ctx, daemonID, "pause-lock-c")
	wsB := dbfx.Workspace(t, "Pause pause-lock-b", "pause-lock-b")
	dbfx.Member(t, wsB, testUserID, "owner")
	runtimeB := dbfx.Runtime(t, "Pause lock B runtime", testutil.Cols{"workspace_id": wsB, "daemon_id": daemonID})
	rec := recordRuntimeRefreshes(t)

	pauseHandler := *testHandler
	reached := make(chan struct{})
	pauseHandler.TxStarter = pauseLockStarter{inner: testHandler.TxStarter, reached: reached}
	pauseDone := make(chan *httptest.ResponseRecorder, 1)

	deleteHandler := *testHandler
	deleteHandler.TxStarter = beforeSweepStarter{inner: testHandler.TxStarter, hook: func() {
		// The delete holds B's workspace row and has not swept anything yet.
		go func() {
			w := httptest.NewRecorder()
			pauseHandler.PauseDaemon(w, withURLParam(newRequestAs(testUserID, http.MethodPut, "/api/runtimes/"+runtimeB+"/pause", map[string]any{"reason": "maintenance"}), "runtimeId", runtimeB))
			pauseDone <- w
		}()
		select {
		case <-reached:
		case got := <-pauseDone: // finished without ever queueing behind the delete
			pauseDone <- got
		}
	}}
	w := httptest.NewRecorder()
	deleteHandler.DeleteWorkspace(w, withURLParam(newRequest(http.MethodDelete, "/api/workspaces/"+wsB, nil), "id", wsB))
	if w.Code != http.StatusNoContent {
		t.Fatalf("DeleteWorkspace = %d: %s", w.Code, w.Body.String())
	}

	if got := <-pauseDone; got.Code != http.StatusNotFound {
		t.Fatalf("pause through the deleted workspace = %d, want 404: %s", got.Code, got.Body.String())
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM daemon_pause WHERE daemon_id = $1`, daemonID); n != 0 {
		t.Fatalf("pause rows after the refused pause: %d, want 0", n)
	}
	for name, ws := range map[string]string{"first": testWorkspaceID, "third": wsC} {
		// The delete's own refresh may still say "resume" (a harmless refetch);
		// what must never happen is a "pause" they hear and nothing lifts.
		for _, action := range rec.actions(ws) {
			if action == "pause" {
				t.Fatalf("surviving %s workspace heard a pause from the refused request", name)
			}
		}
	}
}

// A Pause that finished before the delete asked for its lock is in the machine
// read, so the surviving workspaces hear the pause and then the resume.
func TestDaemonPause_PauseBeforeTheDeleteLockIsSeenAndRefreshed(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	const daemonID = "pause-before-lock-machine"
	cleanupPausesFor(t, daemonID)
	_, _ = seedPausableRuntime(t, ctx, daemonID, "Pause before-lock A")
	wsC, _, _ := seedSecondWorkspaceRuntime(t, ctx, daemonID, "pause-before-lock-c")
	wsB := dbfx.Workspace(t, "Pause pause-before-lock-b", "pause-before-lock-b")
	dbfx.Member(t, wsB, testUserID, "owner")
	runtimeB := dbfx.Runtime(t, "Pause before-lock B runtime", testutil.Cols{"workspace_id": wsB, "daemon_id": daemonID})
	rec := recordRuntimeRefreshes(t)

	if w := putPause(t, testUserID, runtimeB, map[string]any{"reason": "maintenance"}); w.Code != http.StatusOK {
		t.Fatalf("pause = %d: %s", w.Code, w.Body.String())
	}
	w := httptest.NewRecorder()
	testHandler.DeleteWorkspace(w, withURLParam(newRequest(http.MethodDelete, "/api/workspaces/"+wsB, nil), "id", wsB))
	if w.Code != http.StatusNoContent {
		t.Fatalf("DeleteWorkspace = %d: %s", w.Code, w.Body.String())
	}
	for name, ws := range map[string]string{"first": testWorkspaceID, "third": wsC} {
		if got := rec.actions(ws); len(got) != 2 || got[0] != "pause" || got[1] != "resume" {
			t.Fatalf("surviving %s workspace refreshes = %v, want [pause resume]", name, got)
		}
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM daemon_pause WHERE daemon_id = $1`, daemonID); n != 0 {
		t.Fatalf("pause rows left after the delete: %d", n)
	}
}
