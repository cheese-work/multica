package handler

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// The continuation lineage of a DM has no fixed depth: a wakeup set in an early
// DM is listed, cancelled, counted and fired through every later continuation.

// continued continues the DM through its running task and returns a DM value
// for the successor with its own running turn.
func (dm wakeupDM) continued(t *testing.T) wakeupDM {
	t.Helper()
	if r := continueChatRequest(t, dm.sessionID, "Summary.", dm.taskID); r.Code != http.StatusCreated {
		t.Fatalf("continue: %d %s", r.Code, r.Body.String())
	}
	next := dm
	dbfx.QueryRow(t, `SELECT continued_into_session_id::text FROM chat_session WHERE id = $1`, dm.sessionID).Scan(&next.sessionID)
	ctx := context.Background()
	t.Cleanup(func() {
		testPool.Exec(ctx, `DELETE FROM chat_wakeup WHERE chat_session_id = $1`, next.sessionID)
		testPool.Exec(ctx, `DELETE FROM agent_task_queue WHERE chat_session_id = $1`, next.sessionID)
		testPool.Exec(ctx, `DELETE FROM chat_message WHERE chat_session_id = $1`, next.sessionID)
		testPool.Exec(ctx, `DELETE FROM chat_session WHERE id = $1`, next.sessionID)
	})
	next.taskID = sendDirectChat(t, ctx, dm.agentID, next.sessionID, "next")
	markTaskRunning(t, ctx, next.taskID)
	return next
}

// cancel cancels a wakeup through the agent endpoint, as this DM's running task.
func (dm wakeupDM) cancel(t *testing.T, wakeupID string) *testutil.Response {
	t.Helper()
	req := newRequest(http.MethodDelete, "/api/chat/wakeups/"+wakeupID, nil)
	req.Header.Set("X-Actor-Source", "task_token")
	req.Header.Set("X-Task-ID", dm.taskID)
	return testutil.Call(t, testHandler.CancelChatWakeup, withURLParam(withChatTestWorkspaceCtx(t, req), "wakeupId", wakeupID))
}

func TestWakeupCapAndAgentViewFollowTheContinuationLineage(t *testing.T) {
	dm := newWakeupDM(t)
	var ids []string
	for i := 0; i < service.ChatWakeupMaxPending; i++ {
		ids = append(ids, dm.create(t, "inherited", time.Hour).ID)
	}
	next := dm.continued(t)

	next.request(t, http.MethodPost, "/api/chat/wakeups", map[string]any{"instruction": "one too many", "after_seconds": 600}, testHandler.CreateChatWakeup).
		Want(http.StatusConflict)

	var listed struct {
		Wakeups []wakeupJSON `json:"wakeups"`
	}
	next.request(t, http.MethodGet, "/api/chat/wakeups", nil, testHandler.ListChatWakeups).Want(http.StatusOK).JSON(&listed)
	if len(listed.Wakeups) != len(ids) {
		t.Fatalf("agent in the successor sees %d wakeups, want %d inherited", len(listed.Wakeups), len(ids))
	}
}

func TestAgentCancelsAnInheritedWakeupFromTheSuccessorDM(t *testing.T) {
	dm := newWakeupDM(t)
	w := dm.create(t, "Cancel me from the next DM.", time.Hour)
	next := dm.continued(t)

	cancelFromNext := next.cancel(t, w.ID)
	cancelFromNext.Want(http.StatusOK)
	if status, _ := wakeupStatus(t, w.ID); status != "cancelled" {
		t.Fatalf("status = %s, want cancelled", status)
	}
}

// A wakeup set in the oldest DM of a long chain still fires into the newest one.
func TestWakeupFiresThroughALongContinuationChain(t *testing.T) {
	dm := newWakeupDM(t)
	w := dm.create(t, "Fire through eight continuations.", time.Hour)

	previous := dm.sessionID
	for i := 0; i < 8; i++ {
		var next string
		dbfx.QueryRow(t, `INSERT INTO chat_session (workspace_id, agent_id, creator_id, title, explicitly_created_at, continued_from_session_id)
			VALUES ($1, $2, $3, 'Continuation', now(), $4) RETURNING id::text`, testWorkspaceID, dm.agentID, testUserID, previous).Scan(&next)
		dbfx.Exec(t, `UPDATE chat_session SET status = 'archived', continued_into_session_id = $2 WHERE id = $1`, previous, next)
		t.Cleanup(func() {
			testPool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE chat_session_id = $1`, next)
			testPool.Exec(context.Background(), `DELETE FROM chat_message WHERE chat_session_id = $1`, next)
			testPool.Exec(context.Background(), `DELETE FROM chat_session WHERE id = $1`, next)
		})
		previous = next
	}
	newest := previous

	dm.makeDue(t, w.ID)
	dm.tick(t)
	if got := dm.wakeupTasks(t, newest); len(got) != 1 {
		t.Fatalf("%d wakeup tasks in the newest DM, want 1", len(got))
	}
	if status, session := wakeupStatus(t, w.ID); status != "fired" || session != newest {
		t.Fatalf("wakeup = %s in %s, want fired in %s", status, session, newest)
	}
}

// A corrupt cycle in the lineage must not hang or fire into a loop: the wakeup
// expires instead of retrying forever.
func TestWakeupExpiresWhenTheContinuationChainIsACycle(t *testing.T) {
	dm := newWakeupDM(t)
	w := dm.create(t, "Cycle.", time.Hour)
	var other string
	dbfx.QueryRow(t, `INSERT INTO chat_session (workspace_id, agent_id, creator_id, title, explicitly_created_at, continued_from_session_id)
		VALUES ($1, $2, $3, 'Cycle', now(), $4) RETURNING id::text`, testWorkspaceID, dm.agentID, testUserID, dm.sessionID).Scan(&other)
	t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM chat_session WHERE id = $1`, other) })
	dbfx.Exec(t, `UPDATE chat_session SET continued_into_session_id = $2 WHERE id = $1`, dm.sessionID, other)
	dbfx.Exec(t, `UPDATE chat_session SET continued_into_session_id = $2 WHERE id = $1`, other, dm.sessionID)

	dm.makeDue(t, w.ID)
	dm.tick(t)
	if status, _ := wakeupStatus(t, w.ID); status != "expired" {
		t.Fatalf("status = %s, want expired", status)
	}
}
