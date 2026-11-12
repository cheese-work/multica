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

func TestContinueChatSessionCompactsInPlace(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	for _, pinned := range []bool{true, false} {
		name := map[bool]string{true: "pinned", false: "unpinned"}[pinned]
		t.Run(name, func(t *testing.T) {
			agentID := createHandlerTestAgent(t, "continue-in-place-"+name, []byte("[]"))
			columns := testutil.Cols{"title": "Exact DM title", "session_id": "old-provider-session", "work_dir": "/tmp/old-chat"}
			if pinned {
				columns["pinned_at"] = time.Date(2025, time.January, 2, 3, 4, 5, 123456000, time.UTC)
			}
			sessionID := dbfx.ChatSession(t, agentID, columns)
			dbfx.Insert(t, "chat_message", testutil.Cols{"chat_session_id": sessionID, "role": "user", "content": "original goal"})

			var result ChatContinueResponse
			response := continueChatRequest(t, sessionID, "Goal: original goal. Evidence: retained.", "")
			if response.Code != http.StatusOK {
				t.Fatalf("continue: %d %s", response.Code, response.Body.String())
			}
			if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if result.Session.ID != sessionID || result.PreviousSessionID != sessionID || result.Session.Pinned != pinned {
				t.Fatalf("response must name the same DM with its pin: %+v", result)
			}
			stored, err := testHandler.Queries.GetChatSession(context.Background(), parseUUID(sessionID))
			if err != nil {
				t.Fatal(err)
			}
			if stored.Status != "active" || stored.ContinuedIntoSessionID.Valid || stored.CompactionEpoch != 1 || stored.SessionID.Valid || stored.PinnedAt.Valid != pinned || stored.Title != "Exact DM title" {
				t.Fatalf("DM was not compacted in place: %+v", stored)
			}
			if count := dbfx.Count(t, `SELECT count(*) FROM chat_session WHERE agent_id = $1`, agentID); count != 1 {
				t.Fatalf("continue created another DM: %d", count)
			}
			var kind string
			var taskID *string
			var content string
			dbfx.QueryRow(t, `SELECT message_kind, task_id::text, content FROM chat_message WHERE chat_session_id = $1 AND message_kind = 'compaction_summary'`, sessionID).Scan(&kind, &taskID, &content)
			if taskID != nil || !strings.Contains(content, "Goal: original goal.") {
				t.Fatalf("human divider must carry the summary and no task: task=%v content=%q", taskID, content)
			}
			if count := dbfx.Count(t, `SELECT count(*) FROM chat_message WHERE chat_session_id = $1`, sessionID); count != 2 {
				t.Fatalf("transcript must keep the original message plus the divider: %d", count)
			}
		})
	}
}

// (p) The human's compact waits for a claimed or running reply, but a turn that
// is only queued does not block it: it is restamped when it is claimed.
func TestContinueChatSessionRespectsActiveTasks(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	for _, status := range []string{"dispatched", "waiting_local_directory", "running", "queued"} {
		t.Run(status, func(t *testing.T) {
			agentID, sessionID, _, _ := setupDirectChatSession(t, context.Background(), "continue-active-"+status)
			taskID := sendDirectChat(t, context.Background(), agentID, sessionID, "a turn")
			dbfx.Exec(t, `UPDATE agent_task_queue SET status = $2 WHERE id = $1`, taskID, status)
			response := continueChatRequest(t, sessionID, "Goal: carry on.", "")
			wantStatus, wantEpoch := http.StatusConflict, 0
			if status == "queued" {
				wantStatus, wantEpoch = http.StatusOK, 1
			}
			if response.Code != wantStatus {
				t.Fatalf("status = %d, want %d: %s", response.Code, wantStatus, response.Body.String())
			}
			if count := dbfx.Count(t, `SELECT count(*) FROM chat_session WHERE id = $1 AND compaction_epoch = $2`, sessionID, wantEpoch); count != 1 {
				t.Fatalf("epoch is not %d after a %s turn", wantEpoch, status)
			}
		})
	}
}

// A DM archived by an earlier continuation still answers with its successor.
func TestContinueChatSessionReturnsLegacySuccessor(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	agentID := createHandlerTestAgent(t, "continue-legacy", []byte("[]"))
	successorID := dbfx.ChatSession(t, agentID)
	sourceID := dbfx.ChatSession(t, agentID, testutil.Cols{"status": "archived", "continued_into_session_id": successorID})
	var result ChatContinueResponse
	testutil.Call(t, testHandler.ContinueChatSession, withChatTestWorkspaceCtx(t, withURLParam(
		newRequest(http.MethodPost, "/api/chat/sessions/"+sourceID+"/continue", map[string]any{"summary": "Goal."}), "sessionId", sourceID,
	))).Want(http.StatusOK).JSON(&result)
	if result.Session.ID != successorID || result.PreviousSessionID != sourceID {
		t.Fatalf("legacy replay = %+v, want successor %s", result, successorID)
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
		{"session chat task", "same-task", http.StatusOK},
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

func TestCompactWebDMContinuesAndNextClaimStartsFresh(t *testing.T) {
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
	if response.Code != http.StatusOK {
		t.Fatalf("continue: %d %s", response.Code, response.Body.String())
	}
	if _, err := testHandler.TaskService.CompleteTask(ctx, parseUUID(taskID), []byte(`{"output":"Continued in the new DM."}`), "resumed-provider-session", "/tmp/continuation-source", "", false, "", ""); err != nil {
		t.Fatal(err)
	}
	if count := dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE id = $1 AND status = 'completed'`, taskID); count != 1 {
		t.Fatal("the source task did not complete after continuation")
	}
	nextTaskID := sendDirectChat(t, ctx, agentID, sessionID, "Take the next action")
	next := claim()
	// Seeding the fresh session from the summary is the floor-column work (L1b).
	if next.ID != nextTaskID || next.PriorSessionID != "" || next.PriorSessionResumeUnavailable || !strings.Contains(next.ChatMessage, "Take the next action") {
		t.Fatalf("next claim did not start a fresh session: id=%s prior=%q unavailable=%v message=%q", next.ID, next.PriorSessionID, next.PriorSessionResumeUnavailable, next.ChatMessage)
	}
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
	if strings.Contains(query, "-- name: CreateChatCompactionDivider") {
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
	if count := dbfx.Count(t, `SELECT count(*) FROM chat_session WHERE id = $1 AND status = 'active' AND compaction_epoch = 0`, sessionID); count != 1 {
		t.Fatal("failed compaction opened an epoch or changed the DM")
	}
	if count := dbfx.Count(t, `SELECT count(*) FROM chat_message WHERE chat_session_id = $1`, sessionID); count != 0 {
		t.Fatalf("failed compaction left %d messages", count)
	}
}
