package handler

import (
	"net/http"
	"strings"
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
			if endpoint.list && (caller.source == "pat" || caller.source == "cli_jwt") {
				caller.want = http.StatusOK
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
	for _, endpoint := range []struct {
		name, method string
		handler      http.HandlerFunc
	}{
		{"list", http.MethodGet, testHandler.ListChatSessions},
		{"get", http.MethodGet, testHandler.GetChatSession},
		{"messages", http.MethodGet, testHandler.ListChatMessages},
		{"page", http.MethodGet, testHandler.ListChatMessagesPage},
	} {
		for caller, token := range credentials {
			test.Run(endpoint.name+"/"+caller, func(test *testing.T) {
				req := withChatTestWorkspaceCtx(test, newRequest(endpoint.method, "/api/chat/sessions", nil))
				req = withURLParam(req, "sessionId", sessionID)
				req.Header.Set("Authorization", "Bearer "+token)
				req.Header.Set("X-Actor-Source", "interactive_jwt")
				req.Header.Set("X-Task-ID", taskID)
				req.Header.Set("X-Agent-ID", agentID)
				want := http.StatusForbidden
				if caller == "interactive_jwt" || caller == "own_chat" {
					want = http.StatusOK
				}
				if endpoint.name == "list" && listsOwnChats(caller) {
					want = http.StatusOK
				}
				protected := middleware.Auth(testHandler.Queries, nil, verifier, nil)(endpoint.handler)
				response := testutil.Call(test, protected.ServeHTTP, req).Want(want)
				if endpoint.name != "list" && (caller == "cli_jwt" || caller == "legacy_jwt" || caller == "unknown_jwt") {
					var refusal map[string]string
					response.JSON(&refusal)
					if refusal["code"] != "interactive_login_required" {
						test.Fatalf("chat refusal = %v, want interactive_login_required", refusal)
					}
				}
				if caller == "own_chat" && endpoint.name != "list" {
					req = withURLParam(req, "sessionId", otherID)
					testutil.Call(test, protected.ServeHTTP, req).Want(http.StatusForbidden)
				}
			})
		}
	}
}

// listsOwnChats names the user credentials that may list their own DMs
// (CHE-3950); task and cloud-node credentials stay refused.
func listsOwnChats(caller string) bool {
	switch caller {
	case "pat", "cli_jwt", "legacy_jwt", "unknown_jwt":
		return true
	}
	return false
}

func TestChatSessionsListOwnForUserCredentials(test *testing.T) {
	agentID := createHandlerTestAgent(test, "own-list", []byte("[]"))
	ownID := createHandlerTestChatSession(test, agentID)
	archivedID := dbfx.Insert(test, "chat_session", testutil.Cols{"workspace_id": testWorkspaceID, "agent_id": agentID, "creator_id": testUserID, "title": "archived", "status": "archived"})
	otherUserID := dbfx.Insert(test, "user", testutil.Cols{"name": "Own List Other", "email": "own-list-other@multica.ai"})
	otherSessionID := dbfx.Insert(test, "chat_session", testutil.Cols{"workspace_id": testWorkspaceID, "agent_id": agentID, "creator_id": otherUserID, "title": "not yours"})
	dbfx.Insert(test, "chat_message", testutil.Cols{"chat_session_id": ownID, "role": "assistant", "content": "unread reply", "created_at": time.Now().Add(time.Minute)})
	dbfx.Insert(test, "chat_message", testutil.Cols{"chat_session_id": archivedID, "role": "assistant", "content": "archived reply", "created_at": time.Now().Add(time.Minute)})
	for _, source := range []string{"pat", "cli_jwt"} {
		test.Run(source, func(test *testing.T) {
			for _, path := range []string{"/api/chat/sessions", "/api/chat/sessions?status=all"} {
				request := withChatTestWorkspaceCtx(test, newRequest(http.MethodGet, path, nil))
				request.Header.Set("X-Actor-Source", source)
				var sessions []ChatSessionResponse
				testutil.Call(test, testHandler.ListChatSessions, request).Want(http.StatusOK).JSON(&sessions)
				found, archivedFound := false, false
				for _, session := range sessions {
					if session.ID == archivedID {
						archivedFound = true
					}
					if session.ID == otherSessionID {
						test.Fatalf("%s: listed another user's session: %+v", path, session)
					}
					if session.LastMessage != nil {
						test.Fatalf("%s: session %s exposes last_message %+v", path, session.ID, session.LastMessage)
					}
					if session.ID == ownID {
						found = true
						if session.UnreadCount != 1 || !session.HasUnread {
							test.Fatalf("%s: own session unread = %d/%v, want 1/true", path, session.UnreadCount, session.HasUnread)
						}
					}
				}
				if !found {
					test.Fatalf("%s: own session %s missing from %+v", path, ownID, sessions)
				}
				if strings.Contains(path, "status=all") && !archivedFound {
					test.Fatalf("%s: archived session %s missing, redaction not exercised", path, archivedID)
				}
			}
		})
	}
}
