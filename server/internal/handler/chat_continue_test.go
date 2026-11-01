package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func continueChatRequest(t *testing.T, sessionID, summary, taskID string) *httptest.ResponseRecorder {
	t.Helper()
	req := newRequest(http.MethodPost, "/api/chat/sessions/"+sessionID+"/continue", map[string]any{"summary": summary})
	req = withChatTestWorkspaceCtx(t, withURLParam(req, "sessionId", sessionID))
	if taskID != "" {
		req.Header.Set("X-Actor-Source", "task_token")
		req.Header.Set("X-Task-ID", taskID)
	}
	response := httptest.NewRecorder()
	testHandler.ContinueChatSession(response, req)
	return response
}

func TestChatContinuationResponseLinks(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	agentID := createHandlerTestAgent(t, "continue-response-links", []byte("[]"))
	sourceID := dbfx.ChatSession(t, agentID, testutil.Cols{})
	dbfx.Insert(t, "chat_message", testutil.Cols{"chat_session_id": sourceID, "role": "user", "content": "Original goal."})
	response := continueChatRequest(t, sourceID, "Retained goal.", "")
	if response.Code != http.StatusCreated {
		t.Fatalf("continue: %d %s", response.Code, response.Body.String())
	}
	var result ChatContinueResponse
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	dbfx.Cleanup(t, `DELETE FROM chat_message WHERE chat_session_id = $1`, result.Session.ID)
	dbfx.Cleanup(t, `DELETE FROM chat_session WHERE id = $1`, result.Session.ID)
	var continuation map[string]json.RawMessage
	if err := json.Unmarshal(response.Body.Bytes(), &continuation); err != nil {
		t.Fatal(err)
	}
	assertLinks := func(raw json.RawMessage, from, into string) {
		t.Helper()
		var session map[string]any
		if err := json.Unmarshal(raw, &session); err != nil {
			t.Fatal(err)
		}
		for field, want := range map[string]string{"continued_from_session_id": from, "continued_into_session_id": into} {
			if want != "" && session[field] != want {
				t.Errorf("%s = %v, want %s", field, session[field], want)
			}
			if want == "" && session[field] != nil {
				t.Errorf("%s = %v, want omitted/null", field, session[field])
			}
		}
	}
	assertLinks(continuation["session"], sourceID, "")
	for _, sessionID := range []string{sourceID, result.Session.ID} {
		request := withChatTestWorkspaceCtx(t, withURLParam(newRequest(http.MethodGet, "/api/chat/sessions/"+sessionID, nil), "sessionId", sessionID))
		single := httptest.NewRecorder()
		testHandler.GetChatSession(single, request)
		if single.Code != http.StatusOK {
			t.Fatalf("get: %d %s", single.Code, single.Body.String())
		}
		if sessionID == sourceID {
			assertLinks(single.Body.Bytes(), "", result.Session.ID)
		} else {
			assertLinks(single.Body.Bytes(), sourceID, "")
		}
	}
	for _, status := range []string{"", "all"} {
		request := withChatTestWorkspaceCtx(t, newRequest(http.MethodGet, "/api/chat/sessions?status="+status, nil))
		list := httptest.NewRecorder()
		testHandler.ListChatSessions(list, request)
		if list.Code != http.StatusOK {
			t.Fatalf("list: %d %s", list.Code, list.Body.String())
		}
		var sessions []json.RawMessage
		if err := json.Unmarshal(list.Body.Bytes(), &sessions); err != nil {
			t.Fatal(err)
		}
		found := false
		for _, raw := range sessions {
			var session struct{ ID string }
			if err := json.Unmarshal(raw, &session); err != nil {
				t.Fatal(err)
			}
			if session.ID == result.Session.ID {
				found = true
				assertLinks(raw, sourceID, "")
			}
			if session.ID == sourceID {
				assertLinks(raw, "", result.Session.ID)
			}
		}
		if !found {
			t.Fatal("continued session missing from list")
		}
	}
}

func assertContinuedChatUnread(t *testing.T, sourceID, successorID string, wantUnread int) {
	t.Helper()
	for _, status := range []string{"active", "all"} {
		request := withChatTestWorkspaceCtx(t, newRequest(http.MethodGet, "/api/chat/sessions?status="+status, nil))
		var sessions []ChatSessionResponse
		testutil.Call(t, testHandler.ListChatSessions, request).Want(http.StatusOK).JSON(&sessions)
		foundSuccessor, foundSource := false, false
		for _, session := range sessions {
			switch session.ID {
			case successorID:
				foundSuccessor = true
				if session.UnreadCount != wantUnread || session.HasUnread != (wantUnread > 0) {
					t.Errorf("%s successor unread: count=%d has_unread=%v, want count=%d", status, session.UnreadCount, session.HasUnread, wantUnread)
				}
			case sourceID:
				foundSource = true
				if session.Status != "archived" || session.UnreadCount != 0 || session.HasUnread {
					t.Errorf("%s archived source unread: %+v", status, session)
				}
			}
		}
		if !foundSuccessor || foundSource != (status == "all") {
			t.Errorf("%s list: successor found=%v, source found=%v", status, foundSuccessor, foundSource)
		}
	}
}

func TestContinueChatSessionSummaryUnreadUntilRead(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	for _, taskScoped := range []bool{false, true} {
		name := "web continuation"
		if taskScoped {
			name = "multica chat continue"
		}
		t.Run(name, func(t *testing.T) {
			agentID := createHandlerTestAgent(t, "continue-unread", []byte("[]"))
			sourceID := dbfx.ChatSession(t, agentID, testutil.Cols{"last_read_at": "epoch"})
			dbfx.Insert(t, "chat_message", testutil.Cols{"chat_session_id": sourceID, "role": "assistant", "content": "Unread source reply"})
			path := "/api/chat/sessions/" + sourceID + "/continue"
			if taskScoped {
				path = "/api/chat/continue"
			}
			taskID := ""
			if taskScoped {
				taskID = dbfx.Task(t, agentID, testutil.Cols{"status": "running", "runtime_id": handlerTestRuntimeID(t), "chat_session_id": sourceID})
			}
			request := func() *http.Request {
				request := withChatTestWorkspaceCtx(t, newRequest(http.MethodPost, path, map[string]any{"summary": "Goal: retain the context."}))
				if taskScoped {
					request.Header.Set("X-Actor-Source", "task_token")
					request.Header.Set("X-Task-ID", taskID)
					request.Header.Set("X-Agent-ID", agentID)
				} else {
					request = withURLParam(request, "sessionId", sourceID)
				}
				return request
			}
			var result ChatContinueResponse
			testutil.Call(t, testHandler.ContinueChatSession, request()).Want(http.StatusCreated).JSON(&result)
			dbfx.Cleanup(t, `DELETE FROM chat_message WHERE chat_session_id = $1`, result.Session.ID)
			dbfx.Cleanup(t, `DELETE FROM chat_session WHERE id = $1`, result.Session.ID)
			assertContinuedChatUnread(t, sourceID, result.Session.ID, 1)
			var replay ChatContinueResponse
			testutil.Call(t, testHandler.ContinueChatSession, request()).Want(http.StatusOK).JSON(&replay)
			if replay.Session.ID != result.Session.ID {
				t.Fatalf("replay created another successor: %s", replay.Session.ID)
			}
			assertContinuedChatUnread(t, sourceID, result.Session.ID, 1)
			readRequest := withChatTestWorkspaceCtx(t, withURLParam(newRequest(http.MethodPost, "/api/chat/sessions/"+result.Session.ID+"/read", nil), "sessionId", result.Session.ID))
			testutil.Call(t, testHandler.MarkChatSessionRead, readRequest).Want(http.StatusNoContent)
			assertContinuedChatUnread(t, sourceID, result.Session.ID, 0)
			testutil.Call(t, testHandler.ContinueChatSession, request()).Want(http.StatusOK).JSON(&replay)
			assertContinuedChatUnread(t, sourceID, result.Session.ID, 0)
			if count := dbfx.Count(t, `SELECT count(*) FROM chat_message WHERE chat_session_id = $1`, result.Session.ID); count != 1 {
				t.Fatalf("replay duplicated the summary: %d", count)
			}
		})
	}
}

func TestContinueChatSessionArchivesAndSeedsFreshDM(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	agentID := createHandlerTestAgent(t, "continue-chat", []byte("[]"))
	sessionID := dbfx.ChatSession(t, agentID, testutil.Cols{"title": "Exact DM title", "session_id": "old-provider-session", "work_dir": "/tmp/old-chat"})
	dbfx.Insert(t, "chat_message", testutil.Cols{"chat_session_id": sessionID, "role": "user", "content": "original goal"})
	response := continueChatRequest(t, sessionID, "Goal: original goal. Evidence: retained.", "")
	if response.Code != http.StatusCreated {
		t.Fatalf("continue: %d %s", response.Code, response.Body.String())
	}
	var result ChatContinueResponse
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	dbfx.Cleanup(t, `DELETE FROM chat_message WHERE chat_session_id = $1`, result.Session.ID)
	dbfx.Cleanup(t, `DELETE FROM chat_session WHERE id = $1`, result.Session.ID)
	ctx := context.Background()
	old, err := testHandler.Queries.GetChatSession(ctx, parseUUID(sessionID))
	if err != nil || old.Status != "archived" {
		t.Fatalf("source not archived: %+v, %v", old, err)
	}
	next, err := testHandler.Queries.GetChatSession(ctx, parseUUID(result.Session.ID))
	if err != nil {
		t.Fatal(err)
	}
	if next.ID == old.ID || next.AgentID != old.AgentID || next.CreatorID != old.CreatorID || next.Title != old.Title || next.Status != "active" || next.SessionID.Valid || next.WorkDir.Valid {
		t.Fatalf("new DM does not preserve identity and start fresh: %+v", next)
	}
	messages, err := testHandler.Queries.ListChatMessages(ctx, next.ID)
	if err != nil || len(messages) != 1 {
		t.Fatalf("summary messages = %+v, %v", messages, err)
	}
	if messages[0].MessageKind != "continuation_summary" || !strings.Contains(messages[0].Content, "Goal: original goal.") || !strings.Contains(messages[0].Content, sessionID) {
		t.Fatalf("summary or source link missing: %+v", messages[0])
	}
	if count := dbfx.Count(t, `SELECT count(*) FROM chat_message WHERE chat_session_id = $1`, sessionID); count != 1 {
		t.Fatalf("source transcript changed: count=%d", count)
	}
	replay := continueChatRequest(t, sessionID, "Goal: original goal. Evidence: retained.", "")
	if replay.Code != http.StatusOK {
		t.Fatalf("replay: %d %s", replay.Code, replay.Body.String())
	}
	var repeated ChatContinueResponse
	if err := json.Unmarshal(replay.Body.Bytes(), &repeated); err != nil || repeated.Session.ID != result.Session.ID {
		t.Fatalf("retry created another DM: %+v %v", repeated, err)
	}
}

func TestContinueChatSessionPreservesPin(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	for _, pinned := range []bool{true, false} {
		name := "unpinned"
		if pinned {
			name = "pinned"
		}
		t.Run(name, func(t *testing.T) {
			agentID := createHandlerTestAgent(t, "continue-pin-"+name, []byte("[]"))
			columns := testutil.Cols{}
			if pinned {
				columns["pinned_at"] = time.Date(2025, time.January, 2, 3, 4, 5, 123456000, time.UTC)
			}
			sessionID := dbfx.ChatSession(t, agentID, columns)
			ctx := context.Background()
			source, err := testHandler.Queries.GetChatSession(ctx, parseUUID(sessionID))
			if err != nil {
				t.Fatal(err)
			}
			response := continueChatRequest(t, sessionID, "Retained goal and evidence.", "")
			if response.Code != http.StatusCreated {
				t.Fatalf("continue: %d %s", response.Code, response.Body.String())
			}
			var result ChatContinueResponse
			if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			dbfx.Cleanup(t, `DELETE FROM chat_message WHERE chat_session_id = $1`, result.Session.ID)
			dbfx.Cleanup(t, `DELETE FROM chat_session WHERE id = $1`, result.Session.ID)
			if result.Session.Pinned != pinned {
				t.Errorf("response pinned = %v, want %v", result.Session.Pinned, pinned)
			}
			for _, id := range []string{sessionID, result.Session.ID} {
				stored, err := testHandler.Queries.GetChatSession(ctx, parseUUID(id))
				if err != nil {
					t.Fatal(err)
				}
				// The new DM keeps the copied pin; the archived source is unpinned (Addendum A2).
				want := source.PinnedAt
				if id == sessionID {
					want = pgtype.Timestamptz{}
				}
				if stored.PinnedAt.Valid != want.Valid || !stored.PinnedAt.Time.Equal(want.Time) {
					t.Errorf("session %s pinned_at = %+v, want %+v", id, stored.PinnedAt, want)
				}
				if id == sessionID && stored.Status != "archived" {
					t.Errorf("source status = %s, want archived", stored.Status)
				}
			}
			request := withChatTestWorkspaceCtx(t, newRequest(http.MethodGet, "/api/chat/sessions?status=all", nil))
			listResponse := httptest.NewRecorder()
			testHandler.ListChatSessions(listResponse, request)
			if listResponse.Code != http.StatusOK {
				t.Fatalf("list: %d %s", listResponse.Code, listResponse.Body.String())
			}
			var sessions []ChatSessionResponse
			if err := json.Unmarshal(listResponse.Body.Bytes(), &sessions); err != nil {
				t.Fatal(err)
			}
			found := false
			for _, listed := range sessions {
				if listed.ID == result.Session.ID {
					found = true
					if listed.Pinned != pinned {
						t.Errorf("listed pinned = %v, want %v", listed.Pinned, pinned)
					}
				}
			}
			if !found {
				t.Fatal("continued session missing from refetched list")
			}
			replay := continueChatRequest(t, sessionID, "Retained goal and evidence.", "")
			if replay.Code != http.StatusOK {
				t.Fatalf("replay: %d %s", replay.Code, replay.Body.String())
			}
			var repeated ChatContinueResponse
			if err := json.Unmarshal(replay.Body.Bytes(), &repeated); err != nil {
				t.Fatal(err)
			}
			if repeated.Session.ID != result.Session.ID || repeated.Session.Pinned != pinned {
				t.Errorf("retry changed continuation: %+v", repeated.Session)
			}
		})
	}
}

func TestContinueChatSessionAuthorizationAndValidation(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	agentID := createHandlerTestAgent(t, "continue-auth", []byte("[]"))
	for _, testCase := range []struct {
		name  string
		actor string
		want  int
	}{
		{"session chat task", "same-task", http.StatusCreated},
		{"other chat task", "other-task", http.StatusForbidden},
		{"non-chat task", "issue-task", http.StatusForbidden},
		{"other human", "other-human", http.StatusForbidden},
		{"unscoped agent", "agent", http.StatusForbidden},
		{"empty summary", "empty", http.StatusBadRequest},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			sessionID := dbfx.ChatSession(t, agentID)
			req := newRequest(http.MethodPost, "/api/chat/sessions/"+sessionID+"/continue", map[string]any{"summary": "Goal: carry the context."})
			req = withChatTestWorkspaceCtx(t, withURLParam(req, "sessionId", sessionID))
			switch testCase.actor {
			case "same-task", "other-task", "issue-task":
				columns := testutil.Cols{"status": "running", "runtime_id": handlerTestRuntimeID(t)}
				if testCase.actor == "same-task" {
					columns["chat_session_id"] = sessionID
				} else if testCase.actor == "other-task" {
					columns["chat_session_id"] = dbfx.ChatSession(t, agentID)
				}
				taskID := dbfx.Task(t, agentID, columns)
				req.Header.Set("X-Actor-Source", "task_token")
				req.Header.Set("X-Task-ID", taskID)
				req.Header.Set("X-Agent-ID", agentID)
			case "other-human":
				req.Header.Set("X-User-ID", uuid.NewString())
			case "agent":
				req.Header.Set("X-Agent-ID", agentID)
			case "empty":
				req.Body = http.NoBody
			}
			response := httptest.NewRecorder()
			testHandler.ContinueChatSession(response, req)
			if response.Code != testCase.want {
				t.Fatalf("status=%d want=%d: %s", response.Code, testCase.want, response.Body.String())
			}
			if testCase.want == http.StatusCreated {
				var result ChatContinueResponse
				if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
					t.Fatal(err)
				}
				dbfx.Cleanup(t, `DELETE FROM chat_message WHERE chat_session_id = $1`, result.Session.ID)
				dbfx.Cleanup(t, `DELETE FROM chat_session WHERE id = $1`, result.Session.ID)
			} else if count := dbfx.Count(t, `SELECT count(*) FROM chat_session WHERE id = $1 AND status = 'active'`, sessionID); count != 1 {
				t.Fatal("rejected continuation archived the source")
			}
		})
	}
}

func TestCompactWebDMContinuesAndNextClaimUsesSummary(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	agentID, sessionID, runtimeID, daemonID := setupDirectChatSession(t, ctx, "Continued title")
	if err := testHandler.Queries.UpdateChatSessionSession(ctx, db.UpdateChatSessionSessionParams{
		ID: parseUUID(sessionID), RuntimeID: parseUUID(runtimeID),
		SessionID: pgtype.Text{String: "resumed-provider-session", Valid: true},
		WorkDir:   pgtype.Text{String: "/tmp/continuation-source", Valid: true},
	}); err != nil {
		t.Fatal(err)
	}
	taskID := sendDirectChat(t, ctx, agentID, sessionID, "/compact deployment evidence")
	claim := func() AgentTaskResponse {
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
	initial := claim()
	if initial.ID != taskID || initial.PriorSessionID != "resumed-provider-session" || !strings.Contains(initial.ChatMessage, "multica chat continue --summary-file") || !strings.Contains(initial.ChatMessage, "deployment evidence") {
		t.Fatalf("compact claim lost the resumed session or focus: %+v", initial)
	}
	markTaskRunning(t, ctx, taskID)
	response := continueChatRequest(t, sessionID, "Goal: deploy safely. Evidence: immutable artifact.", taskID)
	if response.Code != http.StatusCreated {
		t.Fatalf("continue: %d %s", response.Code, response.Body.String())
	}
	var continuation ChatContinueResponse
	if err := json.Unmarshal(response.Body.Bytes(), &continuation); err != nil {
		t.Fatal(err)
	}
	dbfx.Cleanup(t, `DELETE FROM chat_session WHERE id = $1`, continuation.Session.ID)
	if _, err := testHandler.TaskService.CompleteTask(ctx, parseUUID(taskID), []byte(`{"output":"Continued in the new DM."}`), "resumed-provider-session", "/tmp/continuation-source", "", false, "", ""); err != nil {
		t.Fatal(err)
	}
	if count := dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE id = $1 AND status = 'completed'`, taskID); count != 1 {
		t.Fatal("the source task did not complete after continuation")
	}
	// The agent-only summary and the run's final reply, now written into the successor.
	assertContinuedChatUnread(t, sessionID, continuation.Session.ID, 2)
	nextTaskID := sendDirectChat(t, ctx, agentID, continuation.Session.ID, "Take the next action")
	next := claim()
	if next.ID != nextTaskID || next.PriorSessionID != "" || next.PriorSessionResumeUnavailable || !strings.Contains(next.ChatMessage, "Goal: deploy safely.") || !strings.Contains(next.ChatMessage, "Take the next action") {
		t.Fatalf("next claim did not start fresh with the summary: %+v", next)
	}
	readRequest := newRequest(http.MethodGet, "/api/chat/sessions/"+sessionID, nil)
	readRequest = withChatTestWorkspaceCtx(t, withURLParam(readRequest, "sessionId", sessionID))
	testutil.Call(t, testHandler.GetChatSession, readRequest).Want(http.StatusOK)
}

type failContinuationSummaryStarter struct{ inner txStarter }

func (starter failContinuationSummaryStarter) Begin(ctx context.Context) (pgx.Tx, error) {
	transaction, err := starter.inner.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return failContinuationSummaryTx{Tx: transaction}, nil
}

type failContinuationSummaryTx struct{ pgx.Tx }

func (transaction failContinuationSummaryTx) QueryRow(ctx context.Context, query string, arguments ...any) pgx.Row {
	if strings.Contains(query, "-- name: CreateChatMessage") {
		return continuationFailureRow{}
	}
	return transaction.Tx.QueryRow(ctx, query, arguments...)
}

type continuationFailureRow struct{}

func (continuationFailureRow) Scan(...any) error { return errors.New("injected summary write failure") }

func TestContinueChatSessionRollsBackFailedSummary(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	agentID := createHandlerTestAgent(t, "continue-rollback", []byte("[]"))
	sessionID := dbfx.ChatSession(t, agentID)
	handler := *testHandler
	handler.TxStarter = failContinuationSummaryStarter{inner: testHandler.TxStarter}
	request := newRequest(http.MethodPost, "/api/chat/sessions/"+sessionID+"/continue", map[string]any{"summary": "Keep the goal."})
	request = withChatTestWorkspaceCtx(t, withURLParam(request, "sessionId", sessionID))
	testutil.Call(t, handler.ContinueChatSession, request).Want(http.StatusInternalServerError)
	if count := dbfx.Count(t, `SELECT count(*) FROM chat_session WHERE agent_id = $1`, agentID); count != 1 {
		t.Fatalf("failed continuation left a new session: %d", count)
	}
	if count := dbfx.Count(t, `SELECT count(*) FROM chat_session WHERE id = $1 AND status = 'active' AND continued_into_session_id IS NULL`, sessionID); count != 1 {
		t.Fatal("failed continuation archived or changed the source")
	}
}
