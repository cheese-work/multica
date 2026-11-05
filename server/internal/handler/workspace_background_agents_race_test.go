package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// Races of a background-agents write against a concurrent writer (CHE-2908,
// CHE-4095 condition 2). The other side is held open in its own transaction so
// the interleaving is deterministic: a write that skipped its lock never parks
// and finishes while the holder is still uncommitted.

func putBGAsync(wsID string, body any) (<-chan struct{}, func() (int, string)) {
	done := make(chan struct{})
	var code int
	var errCode string
	go func() {
		defer close(done)
		w := httptest.NewRecorder()
		testHandler.UpdateBackgroundAgents(w, withURLParam(newRequest(http.MethodPut, "/api/workspaces/"+wsID+"/background-agents", body), "id", wsID))
		var out struct {
			Code string `json:"code"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		code, errCode = w.Code, out.Code
	}()
	return done, func() (int, string) { <-done; return code, errCode }
}

// An enqueue that holds the DM lock and has inserted a compaction task that is
// not yet committed is invisible to the cancel sweep. The write must park on
// the DM lock, then cancel the task once it commits.
func TestBackgroundAgentsCompactChangeWaitsForAnInFlightEnqueue(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	wsID, a, b, _ := bgFixture(t, "bg-enqueue-race")
	putBG(t, wsID, map[string]any{"compact": map[string]any{"agent_id": a}}, nil).Want(http.StatusOK)
	var runtimeID string
	dbfx.QueryRow(t, `SELECT runtime_id FROM agent WHERE id=$1`, a).Scan(&runtimeID)
	sessionID := dbfx.ChatSession(t, a, testutil.Cols{"workspace_id": wsID})

	tx, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin enqueue tx: %v", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT id FROM chat_session WHERE id = $1 FOR UPDATE`, sessionID); err != nil {
		t.Fatalf("lock chat session: %v", err)
	}
	taskID := uuid.NewString()
	if _, err := tx.Exec(ctx, `
		INSERT INTO agent_task_queue (id, agent_id, runtime_id, status, priority, chat_session_id, task_kind)
		VALUES ($1, $2, $3, 'queued', 2, $4, 'compact')`, taskID, a, runtimeID, sessionID); err != nil {
		t.Fatalf("insert compact task: %v", err)
	}

	done, result := putBGAsync(wsID, map[string]any{"compact": map[string]any{"agent_id": b}})
	if !waitForBlockedBackend(t, done) {
		t.Fatal("the compact change finished while an enqueue held the DM lock: it never took the lock, so its sweep cannot see a task committed behind it")
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit enqueue tx: %v", err)
	}
	if code, _ := result(); code != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200", code)
	}
	wantTasks(t, "cancelled", taskID)
}

// An agent archived between the eligibility check and the settings write must
// not be stored. The check runs in the write's transaction and takes a shared
// lock on the agent row, so a concurrent archive either lands first (the write
// is refused) or waits for the write.
func TestBackgroundAgentsWriteRevalidatesAnAgentArchivedMidWrite(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	wsID, a, b, _ := bgFixture(t, "bg-archive-race")
	putBG(t, wsID, map[string]any{"compact": map[string]any{"agent_id": a}}, nil).Want(http.StatusOK)

	tx, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin archive tx: %v", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `UPDATE agent SET archived_at = now() WHERE id = $1`, b); err != nil {
		t.Fatalf("archive agent: %v", err)
	}

	done, result := putBGAsync(wsID, map[string]any{"compact": map[string]any{"agent_id": b}})
	if !waitForBlockedBackend(t, done) {
		t.Fatal("the write finished while an archive of its agent was uncommitted: its eligibility check took no lock on the agent")
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit archive tx: %v", err)
	}
	if code, errCode := result(); code != http.StatusBadRequest || errCode != codeAgentNotEligible {
		t.Fatalf("PUT = %d %q, want 400 %q", code, errCode, codeAgentNotEligible)
	}
	if got := storedBG(t, wsID); got == nil {
		t.Fatal("the stored value was cleared by a refused write")
	}
}

// A chat enqueue locks the DM, then the agent FOR UPDATE (service.enqueueChatTaskTx).
// The compact change must take its locks in the same order, so an enqueue for the
// newly chosen agent and the change never wait on each other. A change that held
// the agent FOR SHARE while it waited for the DM lock would deadlock here and
// PostgreSQL would abort one side.
func TestBackgroundAgentsCompactChangeAndEnqueueForTheSameAgentDoNotDeadlock(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	wsID, a, b, _ := bgFixture(t, "bg-lock-order")
	putBG(t, wsID, map[string]any{"compact": map[string]any{"agent_id": a}}, nil).Want(http.StatusOK)
	sessionID := dbfx.ChatSession(t, b, testutil.Cols{"workspace_id": wsID})

	tx, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin enqueue tx: %v", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT id FROM chat_session WHERE id = $1 FOR UPDATE`, sessionID); err != nil {
		t.Fatalf("lock chat session: %v", err)
	}

	done, result := putBGAsync(wsID, map[string]any{"compact": map[string]any{"agent_id": b}})
	if !waitForBlockedBackend(t, done) {
		t.Fatal("the compact change finished while an enqueue held the DM lock")
	}
	// The enqueue now reaches for the agent. It must not wait on the change.
	if _, err := tx.Exec(ctx, `SELECT id FROM agent WHERE id = $1 FOR UPDATE`, b); err != nil {
		t.Fatalf("enqueue lock agent: %v (deadlock with the compact change)", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit enqueue tx: %v", err)
	}
	if code, _ := result(); code != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200", code)
	}
}
