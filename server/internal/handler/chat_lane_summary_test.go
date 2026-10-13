package handler

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/internal/middleware"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func TestSetChatLaneSummary(test *testing.T) {
	agentID := createHandlerTestAgent(test, "lane-summary", []byte("[]"))
	sessionID := createHandlerTestChatSession(test, agentID)
	otherID := createHandlerTestChatSession(test, agentID)
	runtimeID := handlerTestRuntimeID(test)
	chatTaskID := dbfx.Task(test, agentID, testutil.Cols{"chat_session_id": sessionID, "runtime_id": runtimeID, "status": "running"})
	issueTaskID := dbfx.Task(test, agentID, testutil.Cols{"runtime_id": runtimeID, "status": "running"})
	for _, caller := range []struct {
		name, source, taskID, target string
		want                         int
	}{
		{"creator", "interactive_jwt", "", sessionID, http.StatusOK},
		{"own_chat", "task_token", chatTaskID, sessionID, http.StatusOK},
		{"current_chat", "task_token", chatTaskID, "", http.StatusOK},
		{"other_chat", "task_token", chatTaskID, otherID, http.StatusForbidden},
		{"issue_task", "task_token", issueTaskID, sessionID, http.StatusForbidden},
		{"pat", "pat", "", sessionID, http.StatusForbidden},
		{"cloud_pat", "cloud_pat", "", sessionID, http.StatusForbidden},
		{"cli", "cli_jwt", "", sessionID, http.StatusForbidden},
		{"unstamped", "", "", sessionID, http.StatusForbidden},
	} {
		test.Run(caller.name, func(test *testing.T) {
			value := "Lane for " + caller.name
			req := withChatTestWorkspaceCtx(test, newRequest(http.MethodPut, "/api/chat/summary", map[string]any{"lane_summary": value}))
			req = withURLParam(req, "sessionId", caller.target)
			req.Header.Set("X-Actor-Source", caller.source)
			req.Header.Set("X-Task-ID", caller.taskID)
			response := testutil.Call(test, testHandler.SetChatLaneSummary, req).Want(caller.want)
			if caller.want == http.StatusOK {
				var session ChatSessionResponse
				response.JSON(&session)
				if session.LaneSummary != value || session.LaneSummaryUpdatedAt == nil {
					test.Fatalf("summary not persisted: %+v", session)
				}
				stored, err := testHandler.Queries.GetChatSession(context.Background(), parseUUID(sessionID))
				if err != nil || stored.LaneSummary != value || !stored.LaneSummaryUpdatedAt.Valid {
					test.Fatalf("stored summary = %+v, err = %v", stored, err)
				}
			}
		})
	}
	for _, body := range []any{
		map[string]any{}, map[string]any{"lane_summary": nil}, map[string]any{"lane_summary": 42},
		map[string]any{"lane_summary": strings.Repeat("🙂", 601)},
	} {
		req := withChatTestWorkspaceCtx(test, newRequest(http.MethodPut, "/", body))
		req = withURLParam(req, "sessionId", sessionID)
		testutil.Call(test, testHandler.SetChatLaneSummary, req).Want(http.StatusBadRequest)
	}
	for _, value := range []string{strings.Repeat("🙂", 600), strings.Repeat("e\u0301", 300), ""} {
		req := withChatTestWorkspaceCtx(test, newRequest(http.MethodPut, "/", map[string]any{"lane_summary": value}))
		req = withURLParam(req, "sessionId", sessionID)
		var session ChatSessionResponse
		testutil.Call(test, testHandler.SetChatLaneSummary, req).Want(http.StatusOK).JSON(&session)
		if session.LaneSummary != value {
			test.Fatalf("Unicode summary changed: %q", session.LaneSummary)
		}
	}
}

func TestChatLaneSummaryScopeAndLifecycle(test *testing.T) {
	agentID := createHandlerTestAgent(test, "lane-summary-scope", []byte("[]"))
	sessionID := createHandlerTestChatSession(test, agentID)
	taskID := dbfx.Task(test, agentID, testutil.Cols{"chat_session_id": sessionID, "runtime_id": handlerTestRuntimeID(test), "status": "running"})
	for _, scenario := range []string{"other_human", "other_workspace", "terminal_task", "wrong_agent", "archived"} {
		test.Run(scenario, func(test *testing.T) {
			req := withChatTestWorkspaceCtx(test, newRequest(http.MethodPut, "/", map[string]string{"lane_summary": "must not write"}))
			req = withURLParam(req, "sessionId", sessionID)
			want := http.StatusForbidden
			switch scenario {
			case "other_human":
				req.Header.Set("X-User-ID", uuid.NewString())
			case "other_workspace":
				req.Header.Set("X-Actor-Source", "task_token")
				req.Header.Set("X-Task-ID", taskID)
				req = req.WithContext(middleware.SetMemberContext(req.Context(), uuid.NewString(), db.Member{}))
			case "terminal_task":
				req.Header.Set("X-Actor-Source", "task_token")
				req.Header.Set("X-Task-ID", taskID)
				dbfx.Exec(test, `UPDATE agent_task_queue SET status = 'completed' WHERE id = $1`, taskID)
			case "wrong_agent":
				otherAgentID := createHandlerTestAgent(test, "lane-summary-wrong-agent", []byte("[]"))
				otherTaskID := dbfx.Task(test, otherAgentID, testutil.Cols{"chat_session_id": sessionID, "runtime_id": handlerTestRuntimeID(test), "status": "running"})
				req.Header.Set("X-Actor-Source", "task_token")
				req.Header.Set("X-Task-ID", otherTaskID)
			case "archived":
				dbfx.Exec(test, `UPDATE chat_session SET status = 'archived' WHERE id = $1`, sessionID)
				want = http.StatusConflict
			}
			testutil.Call(test, testHandler.SetChatLaneSummary, req).Want(want)
			stored, err := testHandler.Queries.GetChatSession(context.Background(), parseUUID(sessionID))
			if err != nil || stored.LaneSummary != "" || stored.LaneSummaryUpdatedAt.Valid {
				test.Fatalf("denied caller wrote summary: %+v, %v", stored, err)
			}
		})
	}
}

func TestClaimTaskLaneSummary(test *testing.T) {
	runtimeID := handlerTestRuntimeID(test)
	agentID := createHandlerTestAgent(test, "claim-lane-summary", []byte("[]"))
	sessionID := createHandlerTestChatSession(test, agentID)
	taskID := dbfx.Task(test, agentID, testutil.Cols{"chat_session_id": sessionID, "runtime_id": runtimeID, "status": "running"})
	dbfx.Exec(test, `UPDATE chat_session SET lane_summary = 'Build and release', lane_summary_updated_at = now() - interval '1 hour' WHERE id = $1`, sessionID)
	dbfx.Exec(test, `INSERT INTO chat_message (chat_session_id, role, content, created_at) VALUES ($1, 'user', 'old', now() - interval '2 hours')`, sessionID)
	dbfx.Exec(test, `INSERT INTO chat_message (chat_session_id, role, content) SELECT $1, 'user', 'new' FROM generate_series(1, 20)`, sessionID)
	dbfx.Exec(test, `INSERT INTO chat_message (chat_session_id, role, content, message_kind) VALUES ($1, 'user', 'command', 'channel_command')`, sessionID)
	task, err := testHandler.Queries.GetAgentTask(context.Background(), parseUUID(taskID))
	if err != nil {
		test.Fatal(err)
	}
	runtime, err := testHandler.Queries.GetAgentRuntime(context.Background(), parseUUID(runtimeID))
	if err != nil {
		test.Fatal(err)
	}
	claim := func() AgentTaskResponse {
		test.Helper()
		req := newDaemonTokenRequest(http.MethodPost, "/claim", nil, testWorkspaceID, "lane-summary-claim")
		response, _, _, _, _, failure := testHandler.buildClaimedTaskResponse(req, &task, runtime, runtimeID, testWorkspaceID)
		if failure != nil {
			test.Fatalf("claim failed: %+v", failure)
		}
		return response
	}
	response := claim()
	if !response.ChatLaneSummaryEnabled || response.ChatLaneSummary != "Build and release" || response.ChatLaneSummaryUpdatedAt == nil || response.ChatLaneSummaryMessageCount != 20 {
		test.Fatalf("claim lost summary or cadence: %+v", response)
	}
	dbfx.Exec(test, `UPDATE chat_session SET lane_summary_updated_at = now() WHERE id = $1`, sessionID)
	if response = claim(); response.ChatLaneSummaryMessageCount != 0 {
		test.Fatalf("upkeep did not reset cadence: %d", response.ChatLaneSummaryMessageCount)
	}
	dbfx.Exec(test, `UPDATE chat_session SET lane_summary = '', lane_summary_updated_at = NULL WHERE id = $1`, sessionID)
	if response = claim(); response.ChatLaneSummary != "" || response.ChatLaneSummaryUpdatedAt != nil || response.ChatLaneSummaryMessageCount != 21 {
		test.Fatalf("empty lane summary claim: %+v", response)
	}
	dbfx.Exec(test, `UPDATE chat_session SET channel_origin_at = now() WHERE id = $1`, sessionID)
	if response = claim(); response.ChatLaneSummaryEnabled || response.ChatLaneSummaryMessageCount != 0 {
		test.Fatalf("historical channel treated as a web DM: %+v", response)
	}
}

func TestChatLaneSummaryResponsesAndDatabaseBound(test *testing.T) {
	agentID := createHandlerTestAgent(test, "lane-summary-responses", []byte("[]"))
	sessionID := createHandlerTestChatSession(test, agentID)
	dbfx.Exec(test, `UPDATE chat_session SET lane_summary = $2, lane_summary_updated_at = now() WHERE id = $1`, sessionID, strings.Repeat("界", 600))
	if _, err := testPool.Exec(context.Background(), `UPDATE chat_session SET lane_summary = $2 WHERE id = $1`, sessionID, strings.Repeat("界", 601)); err == nil {
		test.Fatal("database accepted a summary above 600 Unicode characters")
	}
	for _, status := range []string{"active", "all"} {
		req := withChatTestWorkspaceCtx(test, newRequest(http.MethodGet, "/?status="+status, nil))
		var sessions []ChatSessionResponse
		testutil.Call(test, testHandler.ListChatSessions, req).Want(http.StatusOK).JSON(&sessions)
		found := false
		for _, session := range sessions {
			if session.ID == sessionID {
				found = true
				if session.LaneSummary != strings.Repeat("界", 600) || session.LaneSummaryUpdatedAt == nil {
					test.Fatalf("list dropped summary: %+v", session)
				}
			}
		}
		if !found {
			test.Fatal("session missing from list")
		}
	}
	req := withChatTestWorkspaceCtx(test, newRequest(http.MethodGet, "/", nil))
	req = withURLParam(req, "sessionId", sessionID)
	var session ChatSessionResponse
	testutil.Call(test, testHandler.GetChatSession, req).Want(http.StatusOK).JSON(&session)
	if session.LaneSummary != strings.Repeat("界", 600) || session.LaneSummaryUpdatedAt == nil {
		test.Fatalf("get dropped summary: %+v", session)
	}
}

func TestChatLaneSummaryMutationResponseDoesNotBypassReadGate(test *testing.T) {
	agentID := createHandlerTestAgent(test, "lane-summary-mutation", []byte("[]"))
	sessionID := createHandlerTestChatSession(test, agentID)
	dbfx.Exec(test, `UPDATE chat_session SET lane_summary = 'private lane', lane_summary_updated_at = now() WHERE id = $1`, sessionID)
	for _, source := range []string{"pat", "cli_jwt", "interactive_jwt"} {
		req := withChatTestWorkspaceCtx(test, newRequest(http.MethodPatch, "/", map[string]string{"title": "Renamed"}))
		req = withURLParam(req, "sessionId", sessionID)
		req.Header.Set("X-Actor-Source", source)
		var response ChatSessionResponse
		testutil.Call(test, testHandler.UpdateChatSession, req).Want(http.StatusOK).JSON(&response)
		if source == "interactive_jwt" {
			if response.LaneSummary != "private lane" || response.LaneSummaryUpdatedAt == nil {
				test.Fatal("interactive mutation dropped summary")
			}
		} else if response.LaneSummary != "" || response.LaneSummaryUpdatedAt != nil {
			test.Fatalf("%s read a summary through a mutation: %+v", source, response)
		}
	}
	for _, source := range []string{"pat", "cloud_pat", "cli_jwt", "interactive_jwt", "task_token"} {
		test.Run("continue_"+source, func(test *testing.T) {
			sessionID := createHandlerTestChatSession(test, agentID)
			taskID := ""
			if source == "task_token" {
				taskID = dbfx.Task(test, agentID, testutil.Cols{"chat_session_id": sessionID, "runtime_id": handlerTestRuntimeID(test), "status": "running"})
			}
			request := func() *http.Request {
				req := withChatTestWorkspaceCtx(test, newRequest(http.MethodPost, "/", map[string]string{"summary": "Continue this DM"}))
				req = withURLParam(req, "sessionId", sessionID)
				req.Header.Set("X-Actor-Source", source)
				req.Header.Set("X-Task-ID", taskID)
				return req
			}
			var created ChatContinueResponse
			testutil.Call(test, testHandler.ContinueChatSession, request()).Want(http.StatusCreated).JSON(&created)
			dbfx.Cleanup(test, `DELETE FROM chat_message WHERE chat_session_id = $1`, created.Session.ID)
			dbfx.Cleanup(test, `DELETE FROM chat_session WHERE id = $1`, created.Session.ID)
			if created.Session.LaneSummary != "" || created.Session.LaneSummaryUpdatedAt != nil {
				test.Fatalf("new continuation must start without a lane summary: %+v", created.Session)
			}
			dbfx.Exec(test, `UPDATE chat_session SET lane_summary = 'private continuation', lane_summary_updated_at = now() WHERE id = $1`, created.Session.ID)
			var repeated ChatContinueResponse
			testutil.Call(test, testHandler.ContinueChatSession, request()).Want(http.StatusOK).JSON(&repeated)
			if repeated.Session.ID != created.Session.ID || repeated.PreviousSessionID != sessionID {
				test.Fatalf("continuation identity changed: %+v", repeated)
			}
			if source == "interactive_jwt" {
				if repeated.Session.LaneSummary != "private continuation" || repeated.Session.LaneSummaryUpdatedAt == nil {
					test.Fatal("interactive continuation dropped summary")
				}
			} else if repeated.Session.LaneSummary != "" || repeated.Session.LaneSummaryUpdatedAt != nil {
				test.Fatalf("%s read a successor summary through continuation: %+v", source, repeated.Session)
			}
		})
	}
}
