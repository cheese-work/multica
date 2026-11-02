package handler

import (
	"context"
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

// (o) After the rebase the task's own completion saves the pointer and counts
// for the meter.
func TestEpochBound_RebasedTaskSavesPointerAndMeters(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	agentID, sessionID, runtimeID, daemonID := epochDM(t)
	taskID := sendDirectChat(t, context.Background(), agentID, sessionID, "again")
	claimedChatTask(t, runtimeID, daemonID)
	markTaskRunning(t, context.Background(), taskID)
	if _, err := testHandler.TaskService.CompleteTask(context.Background(), parseUUID(taskID), []byte(`{}`), "S_new", "/tmp/new", "", false, "", ""); err != nil {
		t.Fatal(err)
	}
	dbfx.Exec(t, `UPDATE agent_task_queue SET context_tokens = 1000, context_window = 200000 WHERE id = $1`, taskID)
	if pointer, _ := chatPointer(t, sessionID); pointer == nil {
		t.Fatal("the rebased task's completion did not save the pointer")
	}
	rows, err := testHandler.Queries.ListLatestRunContextForChatSessions(context.Background(), []pgtype.UUID{parseUUID(sessionID)})
	if err != nil || len(rows) != 1 {
		t.Fatalf("meter rows = %d, err %v; want the current-epoch run", len(rows), err)
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
	if _, err := testHandler.Queries.GetLastAutoCompactTriggerForSession(context.Background(), parseUUID(sessionID)); err != pgx.ErrNoRows {
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
	dm := newAutoCompactDM(t, `{}`, `{}`)
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
