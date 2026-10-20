package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// CHE-1874: an agent in a web DM schedules its own follow-up turn. Everything
// goes through the public handlers, the scheduler Tick and raw SQL.

type wakeupDM struct {
	agentID, sessionID, runtimeID, daemonID string
	taskID                                  string // the running turn that schedules wakeups
}

func newWakeupDM(t *testing.T) wakeupDM {
	t.Helper()
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	agentID, sessionID, runtimeID, daemonID := setupDirectChatSession(t, ctx, "Wakeup "+t.Name())
	t.Cleanup(func() {
		testPool.Exec(ctx, `DELETE FROM chat_wakeup WHERE chat_session_id = $1`, sessionID)
		testPool.Exec(ctx, `DELETE FROM agent_task_queue WHERE chat_session_id = $1`, sessionID)
		testPool.Exec(ctx, `DELETE FROM chat_message WHERE chat_session_id = $1`, sessionID)
	})
	taskID := sendDirectChat(t, ctx, agentID, sessionID, "remind me later")
	markTaskRunning(t, ctx, taskID)
	return wakeupDM{agentID, sessionID, runtimeID, daemonID, taskID}
}

func (dm wakeupDM) request(t *testing.T, method, path string, body any, handler http.HandlerFunc) *testutil.Response {
	t.Helper()
	req := newRequest(method, path, body)
	req.Header.Set("X-Actor-Source", "task_token")
	req.Header.Set("X-Task-ID", dm.taskID)
	return testutil.Call(t, handler, withChatTestWorkspaceCtx(t, req))
}

type wakeupJSON struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	FireAt string `json:"fire_at"`
}

func (dm wakeupDM) create(t *testing.T, instruction string, after time.Duration) wakeupJSON {
	t.Helper()
	var out wakeupJSON
	dm.request(t, http.MethodPost, "/api/chat/wakeups",
		map[string]any{"instruction": instruction, "after_seconds": int64(after / time.Second)}, testHandler.CreateChatWakeup).
		Want(http.StatusCreated).JSON(&out)
	return out
}

// otherDM is a second DM with the same agent and its own running turn.
func (dm wakeupDM) otherDM(t *testing.T) wakeupDM {
	t.Helper()
	ctx := context.Background()
	other := dm
	dbfx.QueryRow(t, `INSERT INTO chat_session (workspace_id, agent_id, creator_id, title, explicitly_created_at)
		VALUES ($1, $2, $3, 'Other DM', now()) RETURNING id::text`, testWorkspaceID, dm.agentID, testUserID).Scan(&other.sessionID)
	t.Cleanup(func() {
		testPool.Exec(ctx, `DELETE FROM chat_wakeup WHERE chat_session_id = $1`, other.sessionID)
		testPool.Exec(ctx, `DELETE FROM agent_task_queue WHERE chat_session_id = $1`, other.sessionID)
		testPool.Exec(ctx, `DELETE FROM chat_message WHERE chat_session_id = $1`, other.sessionID)
		testPool.Exec(ctx, `DELETE FROM chat_session WHERE id = $1`, other.sessionID)
	})
	other.taskID = sendDirectChat(t, ctx, dm.agentID, other.sessionID, "other")
	markTaskRunning(t, ctx, other.taskID)
	return other
}

// claim is a daemon claim on the DM's runtime; nil when nothing is claimable.
func (dm wakeupDM) claim(t *testing.T) *AgentTaskResponse {
	t.Helper()
	request := withURLParam(newDaemonTokenRequest(http.MethodPost, "/api/daemon/runtimes/"+dm.runtimeID+"/claim", nil, testWorkspaceID, dm.daemonID), "runtimeId", dm.runtimeID)
	var response struct {
		Task *AgentTaskResponse `json:"task"`
	}
	testutil.Call(t, testHandler.ClaimTaskByRuntime, request).Want(http.StatusOK).JSON(&response)
	return response.Task
}

func (dm wakeupDM) makeDue(t *testing.T, wakeupID string) {
	t.Helper()
	dbfx.Exec(t, `UPDATE chat_wakeup SET fire_at = now() - interval '1 second' WHERE id = $1`, wakeupID)
}

func (dm wakeupDM) tick(t *testing.T) {
	t.Helper()
	svc := &service.ChatWakeupService{Tasks: testHandler.TaskService}
	if err := svc.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func (dm wakeupDM) wakeupTasks(t *testing.T, sessionID string) []string {
	t.Helper()
	rows, err := testPool.Query(context.Background(),
		`SELECT id::text FROM agent_task_queue WHERE chat_session_id = $1 AND task_kind = 'wakeup' ORDER BY created_at`, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	return ids
}

func wakeupStatus(t *testing.T, id string) (status string, sessionID string) {
	t.Helper()
	dbfx.QueryRow(t, `SELECT status, chat_session_id::text FROM chat_wakeup WHERE id = $1`, id).Scan(&status, &sessionID)
	return status, sessionID
}

func TestChatWakeupFiresOnceWithHiddenInstruction(t *testing.T) {
	dm := newWakeupDM(t)
	w := dm.create(t, "Check CHE-1 and tell Cheese the result.", 30*time.Minute)
	dm.tick(t)
	if got := dm.wakeupTasks(t, dm.sessionID); len(got) != 0 {
		t.Fatalf("fired before due: %v", got)
	}

	dm.makeDue(t, w.ID)
	dm.tick(t)
	dm.tick(t)
	tasks := dm.wakeupTasks(t, dm.sessionID)
	if len(tasks) != 1 {
		t.Fatalf("%d wakeup tasks after two ticks, want exactly 1", len(tasks))
	}
	if status, _ := wakeupStatus(t, w.ID); status != "fired" {
		t.Fatalf("status = %s, want fired", status)
	}
	var kind, content, owner string
	dbfx.QueryRow(t, `SELECT message_kind, content, task_id::text FROM chat_message WHERE task_id = $1 AND role = 'user'`, tasks[0]).Scan(&kind, &content, &owner)
	if kind != "wakeup_trigger" || !strings.Contains(content, "Check CHE-1 and tell Cheese the result.") {
		t.Fatalf("trigger = %s %q", kind, content)
	}
	var inputOwner, initiator string
	dbfx.QueryRow(t, `SELECT chat_input_task_id::text, initiator_user_id::text FROM agent_task_queue WHERE id = $1`, tasks[0]).Scan(&inputOwner, &initiator)
	if inputOwner != tasks[0] || initiator != testUserID {
		t.Fatalf("input owner %s initiator %s, want itself and the DM creator", inputOwner, initiator)
	}

	// The member never sees the trigger, in the list or the page.
	req := withChatTestWorkspaceCtx(t, withURLParam(newRequest(http.MethodGet, "/api/chat/sessions/"+dm.sessionID+"/messages", nil), "sessionId", dm.sessionID))
	body := testutil.Call(t, testHandler.ListChatMessages, req).Want(http.StatusOK).Text()
	if strings.Contains(body, "Check CHE-1") {
		t.Fatalf("wakeup instruction leaked into the visible transcript: %s", body)
	}
}

func TestChatWakeupQueuesBehindARunningTurnAndPromptCarriesInstruction(t *testing.T) {
	dm := newWakeupDM(t) // dm.taskID is running
	w := dm.create(t, "Report the deploy status.", time.Hour)
	dm.makeDue(t, w.ID)
	dm.tick(t)
	tasks := dm.wakeupTasks(t, dm.sessionID)
	if len(tasks) != 1 {
		t.Fatalf("want one queued wakeup task, got %v", tasks)
	}
	if claimed := dm.claim(t); claimed != nil {
		t.Fatalf("wakeup claimed while the DM's turn still runs: %+v", claimed)
	}

	if _, err := testHandler.TaskService.CompleteTask(context.Background(), parseUUID(dm.taskID), completeResult(t, "ok"), "", "", "", false, "", ""); err != nil {
		t.Fatal(err)
	}
	claimed := dm.claim(t)
	if claimed == nil || claimed.ID != tasks[0] {
		t.Fatalf("claim = %+v, want the wakeup task %s", claimed, tasks[0])
	}
	for _, want := range []string{"Scheduled wakeup", "Report the deploy status."} {
		if !strings.Contains(claimed.ChatMessage, want) {
			t.Errorf("prompt lacks %q: %s", want, claimed.ChatMessage)
		}
	}
}

func TestChatWakeupFollowsContinuationToTheNewDM(t *testing.T) {
	dm := newWakeupDM(t)
	w := dm.create(t, "Follow up in the new DM.", time.Hour)
	response := continueChatRequest(t, dm.sessionID, "Summary of the old DM.", dm.taskID)
	if response.Code != http.StatusCreated {
		t.Fatalf("continue: %d %s", response.Code, response.Body.String())
	}
	var successor string
	dbfx.QueryRow(t, `SELECT continued_into_session_id::text FROM chat_session WHERE id = $1`, dm.sessionID).Scan(&successor)
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE chat_session_id = $1`, successor)
		testPool.Exec(context.Background(), `DELETE FROM chat_message WHERE chat_session_id = $1`, successor)
		testPool.Exec(context.Background(), `DELETE FROM chat_session WHERE id = $1`, successor)
	})

	dm.makeDue(t, w.ID)
	dm.tick(t)
	if got := dm.wakeupTasks(t, dm.sessionID); len(got) != 0 {
		t.Fatalf("task landed in the archived DM: %v", got)
	}
	if got := dm.wakeupTasks(t, successor); len(got) != 1 {
		t.Fatalf("successor DM has %d wakeup tasks, want 1", len(got))
	}
	if status, session := wakeupStatus(t, w.ID); status != "fired" || session != successor {
		t.Fatalf("wakeup = %s in %s, want fired in %s", status, session, successor)
	}
}

func TestChatWakeupIsRefusedOrExpiredForChannelBoundDMs(t *testing.T) {
	dm := newWakeupDM(t)
	w := dm.create(t, "Will be bound before firing.", time.Hour)
	dbfx.Exec(t, `INSERT INTO channel_chat_session_binding (chat_session_id, installation_id, channel_type, channel_chat_id, chat_type)
		VALUES ($1, $2, 'slack', $3, 'p2p')`, dm.sessionID, uuid.NewString(), "chan-"+uuid.NewString())
	dbfx.Cleanup(t, `DELETE FROM channel_chat_session_binding WHERE chat_session_id = $1`, dm.sessionID)

	dm.request(t, http.MethodPost, "/api/chat/wakeups",
		map[string]any{"instruction": "x", "after_seconds": 600}, testHandler.CreateChatWakeup).Want(http.StatusConflict)

	dm.makeDue(t, w.ID)
	dm.tick(t)
	if got := dm.wakeupTasks(t, dm.sessionID); len(got) != 0 {
		t.Fatalf("channel-bound DM got a wakeup turn: %v", got)
	}
	if status, _ := wakeupStatus(t, w.ID); status != "expired" {
		t.Fatalf("status = %s, want expired", status)
	}
}

func TestChatWakeupWaitsWhileTheDMIsCompacting(t *testing.T) {
	dm := newWakeupDM(t)
	w := dm.create(t, "After the compaction.", time.Hour)
	dbfx.Exec(t, `UPDATE agent_task_queue SET task_kind = 'compact' WHERE id = $1`, dm.taskID)
	dm.makeDue(t, w.ID)
	dm.tick(t)
	if status, _ := wakeupStatus(t, w.ID); status != "pending" {
		t.Fatalf("status = %s, want pending while compacting", status)
	}
	if got := dm.wakeupTasks(t, dm.sessionID); len(got) != 0 {
		t.Fatalf("wakeup turn queued during compaction: %v", got)
	}
}

func TestChatWakeupCapAndValidation(t *testing.T) {
	dm := newWakeupDM(t)
	for i := 0; i < service.ChatWakeupMaxPending; i++ {
		dm.create(t, "pending", time.Hour)
	}
	post := func(body map[string]any) *testutil.Response {
		return dm.request(t, http.MethodPost, "/api/chat/wakeups", body, testHandler.CreateChatWakeup)
	}
	post(map[string]any{"instruction": "one too many", "after_seconds": 600}).Want(http.StatusConflict)

	other := dm.otherDM(t)
	for _, body := range map[string]map[string]any{
		"empty instruction": {"instruction": "  ", "after_seconds": 600},
		"over 24 hours":     {"instruction": "x", "after_seconds": 25 * 3600},
		"in the past":       {"instruction": "x", "at": time.Now().Add(-time.Minute).Format(time.RFC3339)},
		"both at and after": {"instruction": "x", "after_seconds": 60, "at": time.Now().Add(time.Hour).Format(time.RFC3339)},
		"neither":           {"instruction": "x"},
		"too long":          {"instruction": strings.Repeat("x", service.ChatWakeupMaxInstruction+1), "after_seconds": 60},
	} {
		other.request(t, http.MethodPost, "/api/chat/wakeups", body, testHandler.CreateChatWakeup).Want(http.StatusBadRequest)
	}

	// A member's own request, not an agent's task, cannot schedule one.
	req := withChatTestWorkspaceCtx(t, newRequest(http.MethodPost, "/api/chat/wakeups", map[string]any{"instruction": "x", "after_seconds": 60}))
	testutil.Call(t, testHandler.CreateChatWakeup, req).Want(http.StatusForbidden)
}

func TestChatWakeupCancelStopsIt(t *testing.T) {
	dm := newWakeupDM(t)
	w := dm.create(t, "Cancelled before it fires.", time.Hour)
	other := dm.otherDM(t)

	del := func(from wakeupDM, id string) *httptest.ResponseRecorder {
		req := newRequest(http.MethodDelete, "/api/chat/wakeups/"+id, nil)
		req.Header.Set("X-Actor-Source", "task_token")
		req.Header.Set("X-Task-ID", from.taskID)
		rec := httptest.NewRecorder()
		testHandler.CancelChatWakeup(rec, withURLParam(withChatTestWorkspaceCtx(t, req), "wakeupId", id))
		return rec
	}
	if rec := del(other, w.ID); rec.Code != http.StatusNotFound {
		t.Fatalf("another DM's task cancelled it: %d", rec.Code)
	}
	if rec := del(dm, w.ID); rec.Code != http.StatusOK {
		t.Fatalf("cancel: %d %s", rec.Code, rec.Body.String())
	}
	dm.makeDue(t, w.ID)
	dm.tick(t)
	if got := dm.wakeupTasks(t, dm.sessionID); len(got) != 0 {
		t.Fatalf("cancelled wakeup fired: %v", got)
	}
	if status, _ := wakeupStatus(t, w.ID); status != "cancelled" {
		t.Fatalf("status = %s, want cancelled", status)
	}
}

func TestDeletingAChatSessionDeletesItsWakeups(t *testing.T) {
	dm := newWakeupDM(t)
	w := dm.create(t, "Orphaned by deletion.", time.Hour)
	dbfx.Exec(t, `UPDATE agent_task_queue SET status = 'completed' WHERE id = $1`, dm.taskID)
	req := withChatTestWorkspaceCtx(t, withURLParam(newRequest(http.MethodDelete, "/api/chat/sessions/"+dm.sessionID, nil), "sessionId", dm.sessionID))
	testutil.Call(t, testHandler.DeleteChatSession, req).Want(http.StatusNoContent)
	if n := dbfx.Count(t, `SELECT count(*) FROM chat_wakeup WHERE id = $1`, w.ID); n != 0 {
		t.Fatalf("%d wakeup rows survive the DM", n)
	}
}
