package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// Rule E (CHE-2819): while a compact holds the DM row lock, the claim skips
// that DM's chat tasks for the poll instead of waiting behind it.

type ruleEClaimed struct {
	ID string `json:"id"`
}

func ruleEClaim(t *testing.T, runtimeID string) *ruleEClaimed {
	t.Helper()
	w := httptest.NewRecorder()
	req := newOwnerDaemonRequest("POST", "/api/daemon/runtimes/"+runtimeID+"/claim", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("runtimeId", runtimeID)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	testHandler.ClaimTaskByRuntime(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("ClaimTaskByRuntime = %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Task *ruleEClaimed `json:"task"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode claim: %v", err)
	}
	return resp.Task
}

// ruleETaskState returns the task status and its epoch stamp (-1 when unstamped).
func ruleETaskState(t *testing.T, taskID string) (status string, epoch int64) {
	t.Helper()
	var stamp *int64
	dbfx.QueryRow(t, `SELECT status, chat_compaction_epoch FROM agent_task_queue WHERE id = $1`, taskID).Scan(&status, &stamp)
	if epoch = -1; stamp != nil {
		epoch = *stamp
	}
	return
}

// (q) first half: the DM is locked for compact, so the claim returns no task and
// the task stays queued; the next poll claims it at the new epoch.
func TestRuleE_ClaimSkipsChatTaskWhileCompactHoldsDMLock(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	agentID, runtimeID, _ := createRuntimeGuardAgent(t, ctx)
	chatID := dbfx.ChatSession(t, agentID, testutil.Cols{"title": "rule-e", "compaction_epoch": 0})
	taskID := dbfx.Task(t, agentID, testutil.Cols{"runtime_id": runtimeID, "chat_session_id": chatID, "status": "queued"})

	compact, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer compact.Rollback(ctx)
	if _, err := compact.Exec(ctx, `SELECT id FROM chat_session WHERE id = $1 FOR NO KEY UPDATE`, chatID); err != nil {
		t.Fatal(err)
	}

	if got := ruleEClaim(t, runtimeID); got != nil {
		t.Fatalf("claim during compact returned task %s, want none", got.ID)
	}
	if status, _ := ruleETaskState(t, taskID); status != "queued" {
		t.Fatalf("task status = %q, want queued", status)
	}

	if _, err := compact.Exec(ctx, `UPDATE chat_session SET compaction_epoch = 1 WHERE id = $1`, chatID); err != nil {
		t.Fatal(err)
	}
	if err := compact.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if got := ruleEClaim(t, runtimeID); got == nil {
		t.Fatal("next claim returned no task")
	}
	if status, epoch := ruleETaskState(t, taskID); status != "dispatched" || epoch != 1 {
		t.Fatalf("task = %q epoch %d, want dispatched at epoch 1", status, epoch)
	}
}

// A task that is not a chat task is not held back by a locked DM.
func TestRuleE_ClaimDoesNotSkipOtherDMsOrLockedDMOfAnotherTask(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	agentID, runtimeID, _ := createRuntimeGuardAgent(t, ctx)
	locked := dbfx.ChatSession(t, agentID, testutil.Cols{"title": "locked"})
	free := dbfx.ChatSession(t, agentID, testutil.Cols{"title": "free"})
	dbfx.Task(t, agentID, testutil.Cols{"runtime_id": runtimeID, "chat_session_id": locked, "status": "queued"})
	freeTask := dbfx.Task(t, agentID, testutil.Cols{"runtime_id": runtimeID, "chat_session_id": free, "status": "queued"})

	compact, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer compact.Rollback(ctx)
	if _, err := compact.Exec(ctx, `SELECT id FROM chat_session WHERE id = $1 FOR NO KEY UPDATE`, locked); err != nil {
		t.Fatal(err)
	}
	got := ruleEClaim(t, runtimeID)
	if got == nil || got.ID != freeTask {
		t.Fatalf("claim = %+v, want the task of the unlocked DM %s", got, freeTask)
	}
}

// Stamp: a requeued never-started task rebases on re-claim (replaces
// "redispatch keeps the first stamp").
func TestRuleE_ClaimRestampsRequeuedTask(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	agentID, runtimeID, _ := createRuntimeGuardAgent(t, ctx)
	chatID := dbfx.ChatSession(t, agentID, testutil.Cols{"title": "restamp", "compaction_epoch": 2})
	taskID := dbfx.Task(t, agentID, testutil.Cols{
		"runtime_id": runtimeID, "chat_session_id": chatID, "status": "queued", "chat_compaction_epoch": 1,
	})
	if got := ruleEClaim(t, runtimeID); got == nil {
		t.Fatal("claim returned no task")
	}
	if _, epoch := ruleETaskState(t, taskID); epoch != 2 {
		t.Fatalf("chat_compaction_epoch = %d, want 2", epoch)
	}
}

// (r) stale snapshot, through the claim path. The claim statement computes the
// epoch stamp from its own snapshot; a compact that commits after that snapshot
// leaves the stamp one behind. A BEFORE UPDATE trigger bumps the DM epoch in the
// claim transaction right after the stamp is evaluated, which reproduces that
// state deterministically. The claim must end with the stamp at the DM epoch.
func TestRuleE_ClaimRebasesStaleEpochStamp(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	agentID, runtimeID, _ := createRuntimeGuardAgent(t, ctx)
	chatID := dbfx.ChatSession(t, agentID, testutil.Cols{"title": "stale-stamp", "compaction_epoch": 3})
	taskID := dbfx.Task(t, agentID, testutil.Cols{"runtime_id": runtimeID, "chat_session_id": chatID, "status": "queued"})

	fn := "rule_e_stale_" + strings.ReplaceAll(chatID, "-", "")
	for _, ddl := range []string{
		`CREATE FUNCTION ` + fn + `() RETURNS trigger LANGUAGE plpgsql AS $$
			BEGIN UPDATE chat_session SET compaction_epoch = compaction_epoch + 1 WHERE id = NEW.chat_session_id; RETURN NEW; END $$`,
		`CREATE TRIGGER ` + fn + ` BEFORE UPDATE ON agent_task_queue FOR EACH ROW
			WHEN (OLD.status = 'queued' AND NEW.status = 'dispatched' AND NEW.chat_session_id = '` + chatID + `')
			EXECUTE FUNCTION ` + fn + `()`,
	} {
		if _, err := testPool.Exec(ctx, ddl); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		testPool.Exec(ctx, `DROP TRIGGER IF EXISTS `+fn+` ON agent_task_queue`)
		testPool.Exec(ctx, `DROP FUNCTION IF EXISTS `+fn+`()`)
	})

	if got := ruleEClaim(t, runtimeID); got == nil || got.ID != taskID {
		t.Fatalf("claim = %+v, want task %s", got, taskID)
	}
	var dmEpoch int64
	dbfx.QueryRow(t, `SELECT compaction_epoch FROM chat_session WHERE id = $1`, chatID).Scan(&dmEpoch)
	if dmEpoch != 4 {
		t.Fatalf("DM epoch = %d, want 4 (the trigger moved it once)", dmEpoch)
	}
	if status, epoch := ruleETaskState(t, taskID); status != "dispatched" || epoch != dmEpoch {
		t.Fatalf("task = %q epoch %d, want dispatched at DM epoch %d", status, epoch, dmEpoch)
	}
}
