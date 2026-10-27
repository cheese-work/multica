package handler

import (
	"context"
	"testing"

	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// A compacted DM has no resume session (session_id NULL) and a bumped
// compaction_epoch. A task that was already running when the boundary landed
// still holds the pre-boundary session; whichever way it ends, it must not put
// that session back on the chat. These tests pin each resume-pointer writer.

// compactedChat builds a DM at compaction epoch 1 with an empty resume pointer
// and a task that started before the boundary (epoch 0) and holds "S_old".
func compactedChat(t *testing.T, taskStatus string) (chatSessionID, taskID, runtimeID, daemonID string) {
	t.Helper()
	ctx := context.Background()
	var agentID string
	agentID, runtimeID, daemonID = createRuntimeGuardAgent(t, ctx)
	chatSessionID = dbfx.ChatSession(t, agentID, testutil.Cols{
		"title":            "compacted",
		"compaction_epoch": 1,
	})
	taskID = dbfx.Task(t, agentID, testutil.Cols{
		"runtime_id":            runtimeID,
		"chat_session_id":       chatSessionID,
		"status":                taskStatus,
		"started_at":            testutil.Raw("now()"),
		"chat_compaction_epoch": 0,
		"session_id":            "S_old",
		"work_dir":              "/tmp/old-workdir",
	})
	return
}

func chatPointer(t *testing.T, chatSessionID string) (sessionID, runtimeID *string) {
	t.Helper()
	dbfx.QueryRow(t, `SELECT session_id, runtime_id::text FROM chat_session WHERE id = $1`, chatSessionID).Scan(&sessionID, &runtimeID)
	return
}

func requireEmptyPointer(t *testing.T, chatSessionID string) {
	t.Helper()
	if sessionID, _ := chatPointer(t, chatSessionID); sessionID != nil {
		t.Fatalf("chat_session.session_id = %q, want NULL (a pre-compaction session must not come back)", *sessionID)
	}
}

// G1
func TestCompactionEpoch_CompleteFromBeforeBoundaryDoesNotRepinPointer(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	chatID, taskID, _, _ := compactedChat(t, "running")
	if _, err := testHandler.TaskService.CompleteTask(context.Background(), parseUUID(taskID), []byte(`{}`), "S_old", "/tmp/old-workdir", "", false, "", ""); err != nil {
		t.Fatalf("complete: %v", err)
	}
	requireEmptyPointer(t, chatID)
}

// G2
func TestCompactionEpoch_ResumeSafeFailFromBeforeBoundaryDoesNotRepinPointer(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	chatID, taskID, _, _ := compactedChat(t, "running")
	if _, err := testHandler.TaskService.FailTask(context.Background(), parseUUID(taskID), "boom", "S_old", "/tmp/old-workdir", "", "agent_error", false, "", ""); err != nil {
		t.Fatalf("fail: %v", err)
	}
	requireEmptyPointer(t, chatID)
}

// G3
func TestCompactionEpoch_CancelFromBeforeBoundaryDoesNotAdvancePointer(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	chatID, taskID, _, _ := compactedChat(t, "running")
	if _, err := testHandler.TaskService.CancelTaskWithResult(context.Background(), parseUUID(taskID),
		service.CancelTaskOptions{ClientSupportsDraftRestore: true}); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	requireEmptyPointer(t, chatID)
}

// G4
func TestCompactionEpoch_LateCancelledPinFromBeforeBoundaryDoesNotAdvancePointer(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	chatID, taskID, _, daemonID := compactedChat(t, "cancelled")
	dbfx.Exec(t, `UPDATE agent_task_queue SET session_id = NULL, work_dir = NULL, completed_at = now() WHERE id = $1`, taskID)
	pinTaskSessionViaAPI(t, taskID, daemonID, "S_old", "/tmp/old-workdir")
	requireEmptyPointer(t, chatID)
}

// G5
func TestCompactionEpoch_RuntimeAccessCancellationDoesNotAdvancePointer(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	chatID, taskID, _, _ := compactedChat(t, "cancelled")
	dbfx.Exec(t, `UPDATE agent_task_queue SET completed_at = now() WHERE id = $1`, taskID)

	tx, err := testHandler.TxStarter.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	qtx := testHandler.Queries.WithTx(tx)
	task, err := qtx.GetAgentTask(ctx, parseUUID(taskID))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.PrepareRuntimeAccessCancellation(ctx, qtx, []db.AgentTaskQueue{task}); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	requireEmptyPointer(t, chatID)
}

// G6 pins the existing GH #6340 behaviour when no boundary exists.
func TestCompactionEpoch_NoBoundaryLateCancelledPinStillAdvancesPointer(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	agentID, runtimeID, daemonID := createRuntimeGuardAgent(t, ctx)
	chatID := dbfx.ChatSession(t, agentID, testutil.Cols{"title": "no boundary", "session_id": "turn1", "runtime_id": runtimeID})
	taskID := dbfx.Task(t, agentID, testutil.Cols{
		"runtime_id": runtimeID, "chat_session_id": chatID, "status": "cancelled",
		"started_at": testutil.Raw("now()"), "completed_at": testutil.Raw("now()"),
		"chat_compaction_epoch": 0,
	})
	pinTaskSessionViaAPI(t, taskID, daemonID, "turn2", "/tmp/turn2")
	if sessionID, _ := chatPointer(t, chatID); sessionID == nil || *sessionID != "turn2" {
		t.Fatalf("chat_session.session_id = %v, want turn2", sessionID)
	}
}

// G7: a task started after the boundary records its own session.
func TestCompactionEpoch_PostBoundaryCompleteRecordsPointer(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	chatID, taskID, runtimeID, _ := compactedChat(t, "running")
	dbfx.Exec(t, `UPDATE agent_task_queue SET chat_compaction_epoch = 1, session_id = NULL WHERE id = $1`, taskID)
	if _, err := testHandler.TaskService.CompleteTask(context.Background(), parseUUID(taskID), []byte(`{}`), "S_new", "/tmp/new-workdir", "", false, "", ""); err != nil {
		t.Fatalf("complete: %v", err)
	}
	sessionID, ptrRuntime := chatPointer(t, chatID)
	if sessionID == nil || *sessionID != "S_new" || ptrRuntime == nil || *ptrRuntime != runtimeID {
		t.Fatalf("pointer = %v/%v, want S_new on runtime %s", sessionID, ptrRuntime, runtimeID)
	}
}

// ClaimAgentTask stamps the session's epoch on first dispatch and keeps it.
func TestCompactionEpoch_ClaimStampsTaskOnce(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	for _, tc := range []struct {
		name  string
		stamp any
		want  int64
	}{
		{"first dispatch takes the session epoch", nil, 2},
		{"redispatch keeps the first stamp", 1, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			agentID, runtimeID, daemonID := createRuntimeGuardAgent(t, ctx)
			chatID := dbfx.ChatSession(t, agentID, testutil.Cols{"title": "stamp", "compaction_epoch": 2})
			cols := testutil.Cols{"runtime_id": runtimeID, "chat_session_id": chatID, "status": "queued"}
			if tc.stamp != nil {
				cols["chat_compaction_epoch"] = tc.stamp
			}
			taskID := dbfx.Task(t, agentID, cols)
			claimTaskForRuntimeGuard(t, runtimeID, daemonID)
			var got *int64
			dbfx.QueryRow(t, `SELECT chat_compaction_epoch FROM agent_task_queue WHERE id = $1`, taskID).Scan(&got)
			if got == nil || *got != tc.want {
				t.Fatalf("chat_compaction_epoch = %v, want %d", got, tc.want)
			}
		})
	}
}
