package handler

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Regressions for the review findings on the DM wakeup (CHE-1874): OCR-1874-1
// (a queued wakeup stays in the old DM after a continuation), OCR-1874-2 (the
// queue preview shows the hidden instruction) and OCR-1874-4 (firing and DM
// deletion take their locks in opposite orders).

func TestQueuedWakeupTaskFollowsAContinuationToTheNewDM(t *testing.T) {
	resetDMInbox(t)
	dm := newWakeupDM(t) // dm.taskID is running and will continue the DM
	w := dm.create(t, "Report after the continuation.", time.Hour)
	dm.makeDue(t, w.ID)
	dm.tick(t) // the wakeup is due while the turn runs: its task queues behind it
	queued := dm.wakeupTasks(t, dm.sessionID)
	if len(queued) != 1 {
		t.Fatalf("want one queued wakeup task in the old DM, got %v", queued)
	}

	if r := continueChatRequest(t, dm.sessionID, "Summary.", dm.taskID); r.Code != http.StatusCreated {
		t.Fatalf("continue: %d %s", r.Code, r.Body.String())
	}
	var successor string
	dbfx.QueryRow(t, `SELECT continued_into_session_id::text FROM chat_session WHERE id = $1`, dm.sessionID).Scan(&successor)
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE chat_session_id = $1`, successor)
		testPool.Exec(context.Background(), `DELETE FROM chat_message WHERE chat_session_id = $1`, successor)
		testPool.Exec(context.Background(), `DELETE FROM chat_session WHERE id = $1`, successor)
	})

	var taskSession, messageSession, wakeupSession string
	dbfx.QueryRow(t, `SELECT chat_session_id::text FROM agent_task_queue WHERE id = $1`, queued[0]).Scan(&taskSession)
	dbfx.QueryRow(t, `SELECT chat_session_id::text FROM chat_message WHERE task_id = $1 AND role = 'user'`, queued[0]).Scan(&messageSession)
	dbfx.QueryRow(t, `SELECT chat_session_id::text FROM chat_wakeup WHERE id = $1`, w.ID).Scan(&wakeupSession)
	for what, got := range map[string]string{"task": taskSession, "hidden input": messageSession, "wakeup row": wakeupSession} {
		if got != successor {
			t.Errorf("%s stays in session %s after the continuation, want the new DM %s", what, got, successor)
		}
	}

	// The follow-up runs after the continuing turn and notifies the member in the new DM.
	if _, err := testHandler.TaskService.CompleteTask(context.Background(), parseUUID(dm.taskID), completeResult(t, "continued"), "", "", "", false, "", ""); err != nil {
		t.Fatal(err)
	}
	claimed := dm.claim(t)
	if claimed == nil || claimed.ID != queued[0] || !strings.Contains(claimed.ChatMessage, "Report after the continuation.") {
		t.Fatalf("claim = %+v, want the moved wakeup task with its instruction", claimed)
	}
	markTaskRunning(t, context.Background(), queued[0])
	if _, err := testHandler.TaskService.CompleteTask(context.Background(), parseUUID(queued[0]), completeResult(t, "Result of the follow-up."), "", "", "", false, "", ""); err != nil {
		t.Fatal(err)
	}
	// The continuation summary was the first reply in the new DM; the follow-up is the second.
	var replies int
	dbfx.QueryRow(t, `SELECT COALESCE((details->>'reply_count')::int, 0) FROM inbox_item WHERE chat_session_id = $1 AND type = 'dm_reply'`, successor).Scan(&replies)
	if replies != 2 {
		t.Fatalf("new DM notification counts %d replies, want 2 (summary, then the follow-up)", replies)
	}
}

func TestPendingTaskQueueNeverExposesTheWakeupInstruction(t *testing.T) {
	dm := newWakeupDM(t) // running turn is the head, so the wakeup task queues behind it
	w := dm.create(t, "SECRET-INSTRUCTION do the hidden thing.", time.Hour)
	dm.makeDue(t, w.ID)
	dm.tick(t)

	req := withChatTestWorkspaceCtx(t, withURLParam(newRequest(http.MethodGet, "/api/chat/sessions/"+dm.sessionID+"/pending-task", nil), "sessionId", dm.sessionID))
	var out PendingChatTaskResponse
	body := testutil.Call(t, testHandler.GetPendingChatTask, req).Want(http.StatusOK).JSON(&out).Text()
	if strings.Contains(body, "SECRET-INSTRUCTION") {
		t.Fatalf("the hidden wakeup instruction is in the queue preview: %s", body)
	}
	if len(out.QueuedTasks) != 1 || out.QueuedTasks[0].TaskKind != "wakeup" {
		t.Fatalf("queued tasks = %+v, want one task marked as a wakeup", out.QueuedTasks)
	}
}

// Deleting a DM locks the session, then deletes its wakeups. Firing must also
// take the session lock first, or the two deadlock.
func TestFiringAndDeletingADMDoNotDeadlock(t *testing.T) {
	dm := newWakeupDM(t)
	w := dm.create(t, "Fire while the DM is being deleted.", time.Hour)
	dm.makeDue(t, w.ID)
	ctx := context.Background()

	deadlocks := func() int64 {
		time.Sleep(2 * time.Second) // statistics reach the shared view with a short delay
		var n int64
		dbfx.QueryRow(t, `SELECT deadlocks FROM pg_stat_database WHERE datname = current_database()`).Scan(&n)
		return n
	}
	before := deadlocks()

	deleter, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer deleter.Rollback(ctx)
	dq := db.New(deleter)
	if _, err := dq.LockChatSessionForDelete(ctx, parseUUID(dm.sessionID)); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); dm.tick(t) }() // fires: blocks on the session lock the deleter holds
	time.Sleep(400 * time.Millisecond)          // let the firing transaction reach its first lock

	deleted := make(chan error, 1)
	go func() {
		if err := dq.DeleteChatWakeupsBySession(ctx, parseUUID(dm.sessionID)); err != nil {
			deleted <- err
			return
		}
		deleted <- deleter.Commit(ctx)
	}()
	select {
	case err := <-deleted:
		if err != nil {
			t.Fatalf("deletion hit a lock conflict with the firing transaction: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("deletion did not finish: it waits on the firing transaction, which waits on it")
	}
	wg.Wait()
	// PostgreSQL aborts one side of a deadlock after a second, so the loser may
	// be the firing transaction, which only logs. The counter still sees it.
	if after := deadlocks(); after != before {
		t.Fatalf("firing and deleting deadlocked %d time(s)", after-before)
	}
}
