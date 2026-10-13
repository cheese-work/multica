package handler

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/auth"
	"github.com/multica-ai/multica/server/internal/middleware"
	"github.com/multica-ai/multica/server/internal/testutil"
)

func TestChatLaneReadAccess(test *testing.T) {
	agentID := createHandlerTestAgent(test, "lane-read-access", []byte("[]"))
	sessionID := createHandlerTestChatSession(test, agentID)
	otherSessionID := createHandlerTestChatSession(test, agentID)
	runtimeID := handlerTestRuntimeID(test)
	chatTaskID := dbfx.Task(test, agentID, testutil.Cols{"chat_session_id": sessionID, "status": "running", "runtime_id": runtimeID})
	issueTaskID := dbfx.Task(test, agentID, testutil.Cols{"status": "running", "runtime_id": runtimeID})
	for _, endpoint := range []struct {
		name    string
		handler http.HandlerFunc
		list    bool
	}{
		{"list", testHandler.ListChatSessions, true},
		{"get", testHandler.GetChatSession, false},
		{"messages", testHandler.ListChatMessages, false},
		{"page", testHandler.ListChatMessagesPage, false},
	} {
		for _, caller := range []struct {
			name, source, taskID, target string
			want                         int
		}{
			{"interactive", "interactive_jwt", "", sessionID, http.StatusOK},
			{"pat", "pat", "", sessionID, http.StatusForbidden},
			{"cloud_pat", "cloud_pat", "", sessionID, http.StatusForbidden},
			{"cli", "cli_jwt", "", sessionID, http.StatusForbidden},
			{"unstamped", "", "", sessionID, http.StatusForbidden},
			{"issue_task", "task_token", issueTaskID, sessionID, http.StatusForbidden},
			{"own_chat", "task_token", chatTaskID, sessionID, http.StatusOK},
			{"other_chat", "task_token", chatTaskID, otherSessionID, http.StatusForbidden},
		} {
			if endpoint.list && caller.name == "other_chat" {
				continue
			}
			test.Run(endpoint.name+"/"+caller.name, func(test *testing.T) {
				req := newRequest(http.MethodGet, "/api/chat/sessions", nil)
				req = withChatTestWorkspaceCtx(test, req)
				req = withURLParam(req, "sessionId", caller.target)
				req.Header.Set("X-Actor-Source", caller.source)
				req.Header.Set("X-Task-ID", caller.taskID)
				response := testutil.Call(test, endpoint.handler, req).Want(caller.want)
				if endpoint.list && caller.name == "own_chat" {
					var sessions []ChatSessionResponse
					response.JSON(&sessions)
					if len(sessions) != 1 || sessions[0].ID != sessionID {
						test.Fatalf("chat task enumerated other lanes: %+v", sessions)
					}
				}
			})
		}
	}
}

func TestChatLaneAuthCannotBeForged(test *testing.T) {
	agentID := createHandlerTestAgent(test, "lane-auth", []byte("[]"))
	sessionID := createHandlerTestChatSession(test, agentID)
	otherID := createHandlerTestChatSession(test, agentID)
	credentials := provenanceCredentials(test, agentID)
	taskID := dbfx.Task(test, agentID, testutil.Cols{"chat_session_id": sessionID, "runtime_id": handlerTestRuntimeID(test), "status": "running"})
	chatToken := "mat_lane_" + taskID
	dbfx.Insert(test, "task_token", testutil.Cols{
		"token_hash": auth.HashToken(chatToken), "task_id": taskID, "agent_id": agentID,
		"workspace_id": testWorkspaceID, "user_id": testUserID, "expires_at": time.Now().Add(time.Hour),
	})
	credentials["own_chat"] = chatToken
	verifier := provenanceCloudVerifier(test)
	dbfx.Exec(test, `UPDATE chat_session SET lane_summary = 'private lane' WHERE id = $1`, sessionID)
	for _, endpoint := range []struct {
		name, method string
		handler      http.HandlerFunc
	}{
		{"list", http.MethodGet, testHandler.ListChatSessions},
		{"get", http.MethodGet, testHandler.GetChatSession},
		{"messages", http.MethodGet, testHandler.ListChatMessages},
		{"page", http.MethodGet, testHandler.ListChatMessagesPage},
		{"summary", http.MethodPut, testHandler.SetChatLaneSummary},
	} {
		for caller, token := range credentials {
			test.Run(endpoint.name+"/"+caller, func(test *testing.T) {
				req := withChatTestWorkspaceCtx(test, newRequest(endpoint.method, "/api/chat/sessions", map[string]string{"lane_summary": "allowed"}))
				req = withURLParam(req, "sessionId", sessionID)
				req.Header.Set("Authorization", "Bearer "+token)
				req.Header.Set("X-Actor-Source", "interactive_jwt")
				req.Header.Set("X-Task-ID", taskID)
				req.Header.Set("X-Agent-ID", agentID)
				want := http.StatusForbidden
				if caller == "interactive_jwt" || caller == "own_chat" {
					want = http.StatusOK
				}
				protected := middleware.Auth(testHandler.Queries, nil, verifier, nil)(endpoint.handler)
				response := testutil.Call(test, protected.ServeHTTP, req).Want(want)
				if endpoint.name != "summary" && (caller == "cli_jwt" || caller == "legacy_jwt" || caller == "unknown_jwt") {
					var refusal map[string]string
					response.JSON(&refusal)
					if refusal["code"] != "interactive_login_required" {
						test.Fatalf("chat refusal = %v, want interactive_login_required", refusal)
					}
				}
				if caller == "own_chat" && endpoint.name == "get" {
					var session ChatSessionResponse
					response.JSON(&session)
					if session.LaneSummary != "" || session.LaneSummaryUpdatedAt != nil {
						test.Fatal("lane summary leaked outside its dedicated agent projection")
					}
				}
				if caller == "own_chat" && endpoint.name != "list" {
					req = withURLParam(req, "sessionId", otherID)
					testutil.Call(test, protected.ServeHTTP, req).Want(http.StatusForbidden)
				}
			})
		}
	}
	stored, err := testHandler.Queries.GetChatSession(context.Background(), parseUUID(otherID))
	if err != nil || stored.LaneSummary != "" {
		test.Fatalf("forged caller wrote another lane: %+v, %v", stored, err)
	}
}
