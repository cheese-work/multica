package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func TestCreateChatWakeupRejectsOverflowingDelay(t *testing.T) {
	for name, afterSeconds := range map[string]int64{
		"just over 24h":    86401,
		"overflows 64-bit": 18446744075,
		"max int64":        1<<63 - 1,
	} {
		req := newRequest(http.MethodPost, "/api/chat/wakeups", map[string]any{"after_seconds": afterSeconds, "instruction": "refresh"})
		req.Header.Set("X-Actor-Source", "task_token")
		req.Header.Set("X-Task-ID", uuid.NewString())
		w := httptest.NewRecorder()
		testHandler.CreateChatWakeup(w, req)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400 (%s)", name, w.Code, w.Body.String())
		}
	}
}

func memberWakeupRequest(t *testing.T, method, sessionID, wakeupID string) *http.Request {
	t.Helper()
	path := "/api/chat/sessions/" + sessionID + "/wakeups"
	if wakeupID != "" {
		path += "/" + wakeupID
	}
	req := withURLParams(newRequest(method, path, nil), "sessionId", sessionID, "wakeupId", wakeupID)
	return withChatTestWorkspaceCtx(t, req)
}

func insertPendingWakeup(t *testing.T, sessionID, agentID, instruction string, fireAt time.Time) string {
	t.Helper()
	return dbfx.Insert(t, "chat_wakeup", testutil.Cols{
		"workspace_id": testWorkspaceID, "chat_session_id": sessionID, "agent_id": agentID,
		"instruction": instruction, "fire_at": fireAt, "status": "pending",
	})
}

func TestMemberListsAndCancelsPendingChatWakeups(t *testing.T) {
	agentID := createHandlerTestAgent(t, "member-wakeup-agent", []byte("[]"))
	sessionID := createHandlerTestChatSession(t, agentID)
	later := insertPendingWakeup(t, sessionID, agentID, "second", time.Now().Add(2*time.Hour))
	first := insertPendingWakeup(t, sessionID, agentID, "first", time.Now().Add(time.Hour))
	fired := insertPendingWakeup(t, sessionID, agentID, "already fired", time.Now().Add(time.Hour))
	dbfx.Exec(t, `UPDATE chat_wakeup SET status = 'fired' WHERE id = $1`, fired)

	var listed []ChatWakeupResponse
	testutil.Call(t, testHandler.ListChatSessionWakeups, memberWakeupRequest(t, http.MethodGet, sessionID, "")).Want(http.StatusOK).JSON(&listed)
	if len(listed) != 2 || listed[0].ID != first || listed[1].ID != later {
		t.Fatalf("want pending wakeups [first, later] in fire order, got %+v", listed)
	}
	if listed[0].AgentID != agentID {
		t.Errorf("agent_id = %q, want %q", listed[0].AgentID, agentID)
	}

	testutil.Call(t, testHandler.CancelChatSessionWakeup, memberWakeupRequest(t, http.MethodDelete, sessionID, first)).Want(http.StatusNoContent)
	testutil.Call(t, testHandler.CancelChatSessionWakeup, memberWakeupRequest(t, http.MethodDelete, sessionID, first)).Want(http.StatusNotFound)

	listed = nil
	testutil.Call(t, testHandler.ListChatSessionWakeups, memberWakeupRequest(t, http.MethodGet, sessionID, "")).Want(http.StatusOK).JSON(&listed)
	if len(listed) != 1 || listed[0].ID != later {
		t.Fatalf("want only [later] after cancel, got %+v", listed)
	}
}

func TestMemberCannotManageWakeupsOfSomeoneElsesDM(t *testing.T) {
	agentID := createHandlerTestAgent(t, "member-wakeup-other", []byte("[]"))
	sessionID := createHandlerTestChatSession(t, agentID)
	dbfx.Exec(t, `UPDATE chat_session SET creator_id = $2 WHERE id = $1`, sessionID, createSecondWorkspaceMember(t))
	wakeup := insertPendingWakeup(t, sessionID, agentID, "private", time.Now().Add(time.Hour))

	testutil.Call(t, testHandler.ListChatSessionWakeups, memberWakeupRequest(t, http.MethodGet, sessionID, "")).Want(http.StatusForbidden)
	testutil.Call(t, testHandler.CancelChatSessionWakeup, memberWakeupRequest(t, http.MethodDelete, sessionID, wakeup)).Want(http.StatusForbidden)
}

func sameInstant(got any, wantRFC3339 string) bool {
	text, _ := got.(string)
	gotTime, err := time.Parse(time.RFC3339, text)
	wantTime, wantErr := time.Parse(time.RFC3339, wantRFC3339)
	return err == nil && wantErr == nil && gotTime.Equal(wantTime)
}

func TestCreateChatWakeupRejectsOverlongTitle(t *testing.T) {
	req := newRequest(http.MethodPost, "/api/chat/wakeups", map[string]any{"after_seconds": 60, "instruction": "refresh", "title": strings.Repeat("t", 121)})
	req.Header.Set("X-Actor-Source", "task_token")
	req.Header.Set("X-Task-ID", uuid.NewString())
	w := httptest.NewRecorder()
	testHandler.CreateChatWakeup(w, req)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "title") {
		t.Fatalf("status = %d body = %s, want 400 naming the title", w.Code, w.Body.String())
	}
}

func TestChatWakeupResponseIncludesTitle(t *testing.T) {
	out, err := json.Marshal(chatWakeupToResponse(db.ChatWakeup{Title: "Check deploy", Instruction: "look"}))
	if err != nil || !strings.Contains(string(out), `"title":"Check deploy"`) {
		t.Fatalf("response = %s, %v; want the title", out, err)
	}
}

// A user message sent by a fired wakeup carries scheduled_wakeup on every
// endpoint that lists messages; other messages, and a task id fired by another
// DM's wakeup, do not. content and message_kind stay the model-facing values.
func TestChatMessagesCarryScheduledWakeup(t *testing.T) {
	ctx := context.Background()
	agentID := createHandlerTestAgent(t, "ChatWakeupMessageAgent", []byte("[]"))
	sessionID := createHandlerTestChatSession(t, agentID)
	otherSessionID := createHandlerTestChatSession(t, agentID)
	firedTask, foreignTask := uuid.NewString(), uuid.NewString()
	const header = "[Scheduled wakeup — you set this earlier in this DM; no person wrote this message.]\nInstruction: look"

	var wakeupID, firedAt, createdAt string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO chat_wakeup (workspace_id, chat_session_id, agent_id, title, instruction, fire_at, status, fired_task_id)
		VALUES ($1, $2, $3, 'Check deploy', 'look', '2026-01-01T00:00:10Z', 'fired', $4)
		RETURNING id, to_char(fire_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'), created_at::text`,
		testWorkspaceID, sessionID, agentID, firedTask).Scan(&wakeupID, &firedAt, &createdAt); err != nil {
		t.Fatalf("seed wakeup: %v", err)
	}
	if _, err := testPool.Exec(ctx, `
		INSERT INTO chat_wakeup (workspace_id, chat_session_id, agent_id, title, instruction, fire_at, status, fired_task_id)
		VALUES ($1, $2, $3, 'Other DM', 'other', now(), 'fired', $4)`,
		testWorkspaceID, otherSessionID, agentID, foreignTask); err != nil {
		t.Fatalf("seed other wakeup: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(ctx, `DELETE FROM chat_wakeup WHERE chat_session_id = ANY($1)`, []string{sessionID, otherSessionID})
	})
	var wakeupMessageID string
	for i, m := range []struct{ content, task string }{
		{"hello", ""}, {header, firedTask}, {"reused task id", foreignTask},
	} {
		var id string
		var task any
		if m.task != "" {
			task = m.task
		}
		if err := testPool.QueryRow(ctx, `
			INSERT INTO chat_message (chat_session_id, role, content, task_id, created_at)
			VALUES ($1, 'user', $2, $3, timestamp '2026-01-01 00:00:00' + ($4::int * interval '1 second'))
			RETURNING id`, sessionID, m.content, task, i).Scan(&id); err != nil {
			t.Fatalf("insert message %d: %v", i, err)
		}
		if m.task == firedTask {
			wakeupMessageID = id
		}
	}

	check := func(endpoint string, rows []map[string]any) {
		t.Helper()
		if len(rows) != 3 {
			t.Fatalf("%s: %d messages, want 3", endpoint, len(rows))
		}
		for _, row := range rows {
			sw, has := row["scheduled_wakeup"]
			if row["id"] != wakeupMessageID {
				if has {
					t.Errorf("%s: message %q has scheduled_wakeup %v, want none", endpoint, row["content"], sw)
				}
				continue
			}
			got, _ := sw.(map[string]any)
			if got["id"] != wakeupID || got["agent_id"] != agentID || got["title"] != "Check deploy" ||
				got["instruction"] != "look" || !sameInstant(got["fire_at"], firedAt) || got["created_at"] == nil || got["created_at"] == "" {
				t.Errorf("%s: scheduled_wakeup = %v, want id %s agent %s title/instruction/fire_at %s", endpoint, sw, wakeupID, agentID, firedAt)
			}
			if row["content"] != header || row["message_kind"] != "message" || row["role"] != "user" {
				t.Errorf("%s: content/kind/role = %q/%v/%v, want unchanged", endpoint, row["content"], row["message_kind"], row["role"])
			}
		}
	}

	list := httptest.NewRequest(http.MethodGet, "/api/chat/sessions/"+sessionID+"/messages", nil)
	list.Header.Set("X-User-ID", testUserID)
	list = withChatTestWorkspaceCtx(t, withURLParam(list, "sessionId", sessionID))
	lw := httptest.NewRecorder()
	testHandler.ListChatMessages(lw, list)
	var listed []map[string]any
	if err := json.Unmarshal(lw.Body.Bytes(), &listed); err != nil || lw.Code != http.StatusOK {
		t.Fatalf("list: %d %s %v", lw.Code, lw.Body.String(), err)
	}
	check("list", listed)

	for name, query := range map[string]string{"page": "limit=10", "around": "limit=10&around=" + wakeupMessageID} {
		req := httptest.NewRequest(http.MethodGet, "/api/chat/sessions/"+sessionID+"/messages/page?"+query, nil)
		req.Header.Set("X-User-ID", testUserID)
		req = withChatTestWorkspaceCtx(t, withURLParam(req, "sessionId", sessionID))
		w := httptest.NewRecorder()
		testHandler.ListChatMessagesPage(w, req)
		var page struct {
			Messages []map[string]any `json:"messages"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil || w.Code != http.StatusOK {
			t.Fatalf("%s: %d %s %v", name, w.Code, w.Body.String(), err)
		}
		check(name, page.Messages)
	}
}

func TestChatWakeupResponseIncludesIssueWatch(t *testing.T) {
	issueID, actorID := uuid.NewString(), uuid.NewString()
	row := db.ChatWakeup{IssueID: util.MustParseUUID(issueID), EventType: "comment.created", FilterActorType: "agent", FilterActorID: util.MustParseUUID(actorID), OnTimeout: "wake"}
	out, err := json.Marshal(chatWakeupToResponse(row))
	for _, want := range []string{`"issue_id":"` + issueID + `"`, `"event":"comment.created"`, `"filter_actor_type":"agent"`, `"filter_actor_id":"` + actorID + `"`, `"on_timeout":"wake"`} {
		if err != nil || !strings.Contains(string(out), want) {
			t.Fatalf("response = %s, %v; want %s", out, err, want)
		}
	}
	plain, _ := json.Marshal(chatWakeupToResponse(db.ChatWakeup{Instruction: "x"}))
	if strings.Contains(string(plain), "issue_id") {
		t.Fatalf("timed wakeup response = %s, want no watch fields", plain)
	}
}

func TestCreateChatWakeupWatchRejectsUnknownIssue(t *testing.T) {
	req := newRequest(http.MethodPost, "/api/chat/wakeups", map[string]any{"issue": uuid.NewString(), "event": "comment.created", "instruction": "relay"})
	req.Header.Set("X-Actor-Source", "task_token")
	req.Header.Set("X-Task-ID", uuid.NewString())
	w := httptest.NewRecorder()
	testHandler.CreateChatWakeup(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d body = %s, want 404", w.Code, w.Body.String())
	}
}

// runningChatWakeupTask seeds a running chat task inside a fresh DM and returns its id.
func runningChatWakeupTask(t *testing.T) (taskID, sessionID string) {
	t.Helper()
	agentID := createHandlerTestAgent(t, "ChatWatchAgent", []byte("[]"))
	sessionID = createHandlerTestChatSession(t, agentID)
	if err := testPool.QueryRow(context.Background(), `
		INSERT INTO agent_task_queue (agent_id, chat_session_id, status, priority, context, runtime_id, initiator_user_id)
		VALUES ($1, $2, 'running', 2, '{}'::jsonb, $3, $4) RETURNING id`,
		agentID, sessionID, testRuntimeID, testUserID).Scan(&taskID); err != nil {
		t.Fatalf("seed running chat task: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM chat_wakeup WHERE chat_session_id = $1`, sessionID)
		testPool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE chat_session_id = $1`, sessionID)
	})
	return taskID, sessionID
}

func createChatWatch(taskID string, body map[string]any) *httptest.ResponseRecorder {
	req := newRequest(http.MethodPost, "/api/chat/wakeups", body)
	req.Header.Set("X-Actor-Source", "task_token")
	req.Header.Set("X-Task-ID", taskID)
	w := httptest.NewRecorder()
	testHandler.CreateChatWakeup(w, req)
	return w
}

// A watch has no clock trigger: after_seconds or at next to issue is a mistake
// to reject, not a field to drop silently.
func TestCreateChatWakeupWatchRejectsClockTrigger(t *testing.T) {
	taskID, _ := runningChatWakeupTask(t)
	issueID := createTestIssue(t, "watched "+t.Name(), "todo", "medium")
	for name, extra := range map[string]map[string]any{
		"after_seconds": {"after_seconds": 60},
		"at":            {"at": time.Now().Add(time.Hour).Format(time.RFC3339)},
	} {
		body := map[string]any{"issue": issueID, "event": "comment.created", "instruction": "relay"}
		for k, v := range extra {
			body[k] = v
		}
		if w := createChatWatch(taskID, body); w.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d body = %s, want 400", name, w.Code, w.Body.String())
		}
	}
}

// Event match through the HTTP surface: create the watch, list it, then a
// matching agent comment makes the next tick queue the DM turn quoting it.
func TestChatWatchCreatedOverHTTPFiresOnAgentComment(t *testing.T) {
	ctx := context.Background()
	taskID, sessionID := runningChatWakeupTask(t)
	issueID := createTestIssue(t, "watched "+t.Name(), "todo", "medium")
	var agentID string
	if err := testPool.QueryRow(ctx, `SELECT agent_id FROM chat_session WHERE id = $1`, sessionID).Scan(&agentID); err != nil {
		t.Fatal(err)
	}
	w := createChatWatch(taskID, map[string]any{
		"issue": issueID, "event": "comment.created", "filter_actor_type": "agent", "filter_actor_id": agentID, "instruction": "relay the verdict",
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("create: status = %d body = %s", w.Code, w.Body.String())
	}
	var created ChatWakeupResponse
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil || created.IssueID != issueID || created.Event != "comment.created" || created.ActorID != agentID {
		t.Fatalf("create response = %s, %v", w.Body.String(), err)
	}
	tick := func() {
		t.Helper()
		if err := testHandler.chatWakeupService().Tick(ctx); err != nil {
			t.Fatalf("Tick: %v", err)
		}
	}
	tick()
	var status string
	query := func() {
		testPool.QueryRow(ctx, `SELECT status FROM chat_wakeup WHERE id = $1`, created.ID).Scan(&status)
	}
	if query(); status != "pending" {
		t.Fatalf("before the comment: status = %q, want pending", status)
	}
	if _, err := testPool.Exec(ctx, `INSERT INTO comment (issue_id, workspace_id, author_type, author_id, content)
		SELECT id, workspace_id, 'agent', $2::uuid, 'Verdict PASS, SHA abc123' FROM issue WHERE id = $1`, issueID, agentID); err != nil {
		t.Fatal(err)
	}
	tick()
	if query(); status != "fired" {
		t.Fatalf("after the comment: status = %q, want fired", status)
	}
	var turn string
	if err := testPool.QueryRow(ctx, `
		SELECT m.content FROM chat_message m JOIN agent_task_queue t ON t.id = m.task_id
		WHERE m.chat_session_id = $1 AND t.status = 'queued'`, sessionID).Scan(&turn); err != nil || !strings.Contains(turn, "Verdict PASS, SHA abc123") || !strings.Contains(turn, "relay the verdict") {
		t.Fatalf("DM turn = %q, %v; want the instruction and the quoted comment", turn, err)
	}
}
