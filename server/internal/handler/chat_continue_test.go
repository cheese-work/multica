package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
