package handler

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// CHE-2819 L1a-2: every resume source, the context meter, the auto-compact
// re-arm and the auto-compact admission are bound to the DM's compaction epoch.

// claimedChatTask claims the runtime's next task through the real handler.
func claimedChatTask(t *testing.T, runtimeID, daemonID string) AgentTaskResponse {
	t.Helper()
	request := newDaemonTokenRequest(http.MethodPost, "/api/daemon/runtimes/"+runtimeID+"/claim", nil, testWorkspaceID, daemonID)
	request = withURLParam(request, "runtimeId", runtimeID)
	var response struct {
		Task *AgentTaskResponse `json:"task"`
	}
	testutil.Call(t, testHandler.ClaimTaskByRuntime, request).Want(http.StatusOK).JSON(&response)
	if response.Task == nil {
		t.Fatal("no task was claimed")
	}
	return *response.Task
}

// epochDM builds a DM at epoch 1 (one compact happened) with an epoch-0 task
// that finished holding "S_old" on the same runtime, then queues the next turn.
func epochDM(t *testing.T) (agentID, sessionID, runtimeID, daemonID string) {
	t.Helper()
	ctx := context.Background()
	agentID, sessionID, runtimeID, daemonID = setupDirectChatSession(t, ctx, "epoch-bound "+t.Name())
	dbfx.Exec(t, `UPDATE chat_session SET compaction_epoch = 1 WHERE id = $1`, sessionID)
	dbfx.Exec(t, `
		INSERT INTO agent_task_queue (agent_id, runtime_id, chat_session_id, status, priority, started_at, completed_at, chat_compaction_epoch, session_id, work_dir)
		VALUES ($1, $2, $3, 'completed', 0, now() - interval '2 minutes', now() - interval '1 minute', 0, 'S_old', '/tmp/old')`,
		agentID, runtimeID, sessionID)
	return
}

// (c) A completed old-epoch task holding a valid session must not seed the
// claim after a compact.
func TestEpochBound_ClaimFallbackIgnoresOldEpochSession(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	agentID, sessionID, runtimeID, daemonID := epochDM(t)
	sendDirectChat(t, context.Background(), agentID, sessionID, "after compact")
	if got := claimedChatTask(t, runtimeID, daemonID); got.PriorSessionID != "" {
		t.Fatalf("PriorSessionID = %q, want empty after a compact", got.PriorSessionID)
	}
}

// (c) A stale pointer write that slipped through must not seed the claim either.
func TestEpochBound_ClaimIgnoresStalePointer(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	agentID, sessionID, runtimeID, daemonID := epochDM(t)
	dbfx.Exec(t, `UPDATE chat_session SET session_id = 'S_old', runtime_id = $2 WHERE id = $1`, sessionID, runtimeID)
	sendDirectChat(t, context.Background(), agentID, sessionID, "after compact")
	if got := claimedChatTask(t, runtimeID, daemonID); got.PriorSessionID != "" {
		t.Fatalf("PriorSessionID = %q, want empty: the pointer names a pre-compact session", got.PriorSessionID)
	}
}

// Controls: a session recorded under the current epoch is still resumed, by the
// fallback and by the pointer.
func TestEpochBound_ClaimResumesCurrentEpochSession(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	for _, viaPointer := range []bool{false, true} {
		name := map[bool]string{false: "fallback", true: "pointer"}[viaPointer]
		t.Run(name, func(t *testing.T) {
			agentID, sessionID, runtimeID, daemonID := epochDM(t)
			dbfx.Exec(t, `
				INSERT INTO agent_task_queue (agent_id, runtime_id, chat_session_id, status, priority, started_at, completed_at, chat_compaction_epoch, session_id, work_dir)
				VALUES ($1, $2, $3, 'completed', 0, now() - interval '50 seconds', now() - interval '30 seconds', 1, 'S_new', '/tmp/new')`,
				agentID, runtimeID, sessionID)
			if viaPointer {
				dbfx.Exec(t, `UPDATE chat_session SET session_id = 'S_new', runtime_id = $2 WHERE id = $1`, sessionID, runtimeID)
			}
			sendDirectChat(t, context.Background(), agentID, sessionID, "next")
			if got := claimedChatTask(t, runtimeID, daemonID); got.PriorSessionID != "S_new" {
				t.Fatalf("PriorSessionID = %q, want S_new", got.PriorSessionID)
			}
		})
	}
}

// currentEpochSession records a healthy session "S_new" under the DM's current
// epoch, completed after the old-epoch "S_old" row.
func currentEpochSession(t *testing.T, agentID, sessionID, runtimeID string) {
	t.Helper()
	dbfx.Exec(t, `
		INSERT INTO agent_task_queue (agent_id, runtime_id, chat_session_id, status, priority, started_at, completed_at, chat_compaction_epoch, session_id, work_dir)
		VALUES ($1, $2, $3, 'completed', 0, now() - interval '50 seconds', now() - interval '30 seconds', 1, 'S_new', '/tmp/new')`,
		agentID, runtimeID, sessionID)
}

// An old-epoch overflow that finishes after a healthy current-epoch turn must
// not hide that turn: the overflow cutoff and the retired set are per epoch.
func TestEpochBound_OldEpochOverflowDoesNotHideCurrentSession(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	agentID, sessionID, runtimeID, daemonID := epochDM(t)
	currentEpochSession(t, agentID, sessionID, runtimeID)
	dbfx.Exec(t, `
		INSERT INTO agent_task_queue (agent_id, runtime_id, chat_session_id, status, priority, started_at, completed_at, chat_compaction_epoch, failure_reason)
		VALUES ($1, $2, $3, 'failed', 0, now() - interval '5 seconds', now(), 0, 'codex_resume_oversized')`,
		agentID, runtimeID, sessionID)
	sendDirectChat(t, context.Background(), agentID, sessionID, "next")
	if got := claimedChatTask(t, runtimeID, daemonID); got.PriorSessionID != "S_new" {
		t.Fatalf("PriorSessionID = %q, want S_new: an old-epoch overflow must not hide it", got.PriorSessionID)
	}
}

func TestEpochBound_OldEpochRetirementDoesNotHideCurrentSession(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	agentID, sessionID, runtimeID, daemonID := epochDM(t)
	currentEpochSession(t, agentID, sessionID, runtimeID)
	dbfx.Exec(t, `
		INSERT INTO agent_task_queue (agent_id, runtime_id, chat_session_id, status, priority, completed_at, chat_compaction_epoch, retired_session_id)
		VALUES ($1, $2, $3, 'failed', 0, now(), 0, 'S_new')`,
		agentID, runtimeID, sessionID)
	sendDirectChat(t, context.Background(), agentID, sessionID, "next")
	if got := claimedChatTask(t, runtimeID, daemonID); got.PriorSessionID != "S_new" {
		t.Fatalf("PriorSessionID = %q, want S_new: an old-epoch retirement must not hide it", got.PriorSessionID)
	}
}

// (o) A dispatched, never-started task is reclaimed after the epoch moved (its
// requeue path): the reclaim delivery is rebased before any payload read.
func TestEpochBound_ReclaimedChatTaskIsRebased(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	agentID, sessionID, runtimeID, daemonID := epochDM(t)
	taskID := sendDirectChat(t, context.Background(), agentID, sessionID, "stuck")
	dbfx.Exec(t, `UPDATE agent_task_queue SET status = 'dispatched', dispatched_at = now() - interval '300 seconds', chat_compaction_epoch = 0 WHERE id = $1`, taskID)
	dbfx.Exec(t, `UPDATE chat_session SET session_id = 'S_old', runtime_id = $2 WHERE id = $1`, sessionID, runtimeID)

	got := claimedChatTask(t, runtimeID, daemonID)
	if got.ID != taskID {
		t.Fatalf("reclaimed %s, want %s", got.ID, taskID)
	}
	var epoch int64
	dbfx.QueryRow(t, `SELECT chat_compaction_epoch FROM agent_task_queue WHERE id = $1`, taskID).Scan(&epoch)
	if epoch != 1 {
		t.Fatalf("reclaimed task epoch = %d, want 1", epoch)
	}
	if got.PriorSessionID != "" {
		t.Fatalf("PriorSessionID = %q, want empty", got.PriorSessionID)
	}
}

// (o) end to end: claim at epoch N, release before delivery, human compact to
// N+1, claim again, complete. The re-claim is stamped N+1 and carries no old
// session, the completion saves the pointer, the meter counts the run, and a
// late completion of a run started at N does not overwrite the pointer.
func TestEpochBound_RequeueCompactReclaimLifecycle(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	agentID, sessionID, runtimeID, daemonID := setupDirectChatSession(t, ctx, "epoch lifecycle")
	taskID := sendDirectChat(t, ctx, agentID, sessionID, "first")
	if got := claimedChatTask(t, runtimeID, daemonID); got.ID != taskID {
		t.Fatalf("claimed %s, want %s", got.ID, taskID)
	}
	claimed, err := testHandler.Queries.GetAgentTask(ctx, parseUUID(taskID))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := testHandler.TaskService.RequeueTaskAfterClaimFailure(ctx, claimed); err != nil {
		t.Fatal(err)
	}

	if response := continueChatRequest(t, sessionID, "Goal: first.", ""); response.Code != http.StatusOK {
		t.Fatalf("compact status = %d, body %s", response.Code, response.Body.String())
	}

	got := claimedChatTask(t, runtimeID, daemonID)
	if got.ID != taskID || got.PriorSessionID != "" {
		t.Fatalf("re-claim = task %s prior %q, want task %s and no prior session", got.ID, got.PriorSessionID, taskID)
	}
	var epoch int64
	dbfx.QueryRow(t, `SELECT chat_compaction_epoch FROM agent_task_queue WHERE id = $1`, taskID).Scan(&epoch)
	if epoch != 1 {
		t.Fatalf("re-claimed task epoch = %d, want 1", epoch)
	}

	markTaskRunning(t, ctx, taskID)
	if _, err := testHandler.TaskService.CompleteTask(ctx, parseUUID(taskID), []byte(`{}`), "S_new", "/tmp/new", "", false, "", ""); err != nil {
		t.Fatal(err)
	}
	dbfx.Exec(t, `UPDATE agent_task_queue SET context_tokens = 1000, context_window = 200000 WHERE id = $1`, taskID)
	if pointer, _ := chatPointer(t, sessionID); pointer == nil || *pointer != "S_new" {
		t.Fatalf("pointer = %v, want S_new", pointer)
	}
	rows, err := testHandler.Queries.ListLatestRunContextForChatSessions(ctx, []pgtype.UUID{parseUUID(sessionID)})
	if err != nil || len(rows) != 1 {
		t.Fatalf("meter rows = %d, err %v; want the re-claimed run", len(rows), err)
	}

	staleID := dbfx.Task(t, agentID, testutil.Cols{
		"runtime_id": runtimeID, "chat_session_id": sessionID, "status": "running", "started_at": testutil.Raw("now()"),
		"chat_compaction_epoch": 0, "session_id": "S_old", "work_dir": "/tmp/old",
	})
	if _, err := testHandler.TaskService.CompleteTask(ctx, parseUUID(staleID), []byte(`{}`), "S_old", "/tmp/old", "", false, "", ""); err != nil {
		t.Fatal(err)
	}
	if pointer, _ := chatPointer(t, sessionID); pointer == nil || *pointer != "S_new" {
		t.Fatalf("pointer = %v after a stale completion, want S_new", pointer)
	}
}

// The meter counts only runs of the current epoch, so it hides after a compact.
func TestEpochBound_MeterHidesAfterCompact(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	_, sessionID, _, _ := epochDM(t)
	dbfx.Exec(t, `UPDATE agent_task_queue SET context_tokens = 5000, context_window = 200000 WHERE chat_session_id = $1`, sessionID)
	rows, err := testHandler.Queries.ListLatestRunContextForChatSessions(context.Background(), []pgtype.UUID{parseUUID(sessionID)})
	if err != nil || len(rows) != 0 {
		t.Fatalf("meter rows = %d, err %v; want none for a pre-compact run", len(rows), err)
	}
}

// The 1.1x back-off looks at compactions of the current epoch only: a success
// bumps the epoch, so it clears the guard.
func TestEpochBound_AutoCompactTriggerIgnoresOldEpoch(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	agentID, sessionID, runtimeID, _ := epochDM(t)
	dbfx.Exec(t, `
		INSERT INTO agent_task_queue (agent_id, runtime_id, chat_session_id, status, priority, task_kind, compact_reason, compact_trigger_tokens, chat_compaction_epoch)
		VALUES ($1, $2, $3, 'completed', 0, 'compact', 'r', 9000, 0)`, agentID, runtimeID, sessionID)
	if _, err := testHandler.Queries.GetLastAutoCompactTriggerForSession(context.Background(), parseUUID(sessionID)); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("old-epoch trigger err = %v, want no rows", err)
	}
	dbfx.Exec(t, `UPDATE agent_task_queue SET chat_compaction_epoch = 1 WHERE chat_session_id = $1 AND task_kind = 'compact'`, sessionID)
	if _, err := testHandler.Queries.GetLastAutoCompactTriggerForSession(context.Background(), parseUUID(sessionID)); err != nil {
		t.Fatalf("current-epoch trigger err = %v, want a row", err)
	}
}

// (n) A run that finishes after a manual compact reports a measurement of the
// old epoch; it queues nothing. A current-epoch run crossing the threshold does.
func TestEpochBound_AutoCompactAdmissionRejectsStaleEpochRun(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	dm := newAutoCompactDM(t, legacyPct80, `{}`)
	oldRun := dm.sendRunning(t, "long turn")
	dbfx.Exec(t, `UPDATE agent_task_queue SET chat_compaction_epoch = 0 WHERE id = $1`, oldRun)
	dbfx.Exec(t, `UPDATE chat_session SET compaction_epoch = 1 WHERE id = $1`, dm.sessionID)
	dm.complete(t, oldRun, 190000, 200000)
	if got := dm.compactionTasks(t); len(got) != 0 {
		t.Fatalf("a stale-epoch run queued %d compactions, want none", len(got))
	}

	newRun := dm.sendRunning(t, "next turn")
	dbfx.Exec(t, `UPDATE agent_task_queue SET chat_compaction_epoch = 1 WHERE id = $1`, newRun)
	dm.complete(t, newRun, 190000, 200000)
	if got := dm.compactionTasks(t); len(got) != 1 {
		t.Fatalf("a current-epoch run queued %d compactions, want 1", len(got))
	}
}
