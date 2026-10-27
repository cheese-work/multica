package handler

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/middleware"
	"github.com/multica-ai/multica/server/internal/testutil"
)

func chatRenameRequest(test *testing.T, sessionID, agentID, taskID, title string) *http.Request {
	test.Helper()
	request := withChatTestWorkspaceCtx(test, newRequest(http.MethodPatch, "/api/chat/sessions/"+sessionID, map[string]string{"title": title}))
	request = withURLParam(request, "sessionId", sessionID)
	request.Header.Set("X-Actor-Source", "task_token")
	request.Header.Set("X-Agent-ID", agentID)
	request.Header.Set("X-Task-ID", taskID)
	return request
}

func TestChatRenameWorkspaceAgent(test *testing.T) {
	caller := createHandlerTestAgent(test, "rename-caller", []byte("[]"))
	target := createHandlerTestAgent(test, "rename-private-target", []byte("[]"))
	owner := createSecondWorkspaceMember(test)
	sessionID := createHandlerTestChatSession(test, target)
	dbfx.Exec(test, `UPDATE chat_session SET creator_id = $2, title = 'Old lane', session_id = 'private-resume', work_dir = '/private' WHERE id = $1`, sessionID, owner)
	dbfx.Insert(test, "chat_message", testutil.Cols{"chat_session_id": sessionID, "role": "user", "content": "private transcript"})
	credentials := provenanceCredentials(test, caller)
	request := chatRenameRequest(test, sessionID, caller, "", " Decision lane ")
	request.Header.Set("Authorization", "Bearer "+credentials["task_token"])
	protected := middleware.Auth(testHandler.Queries, nil, nil, nil)(http.HandlerFunc(testHandler.UpdateChatSession))
	var response map[string]any
	testutil.Call(test, protected.ServeHTTP, request).Want(http.StatusOK).JSON(&response)
	if response["id"] != sessionID || response["title"] != "Decision lane" || response["previous_title"] != "Old lane" || response["message_id"] == "" || response["message_id"] == nil || len(response) != 4 {
		test.Fatalf("rename must return only rename metadata: %+v", response)
	}
	var content, sentVia string
	if err := testPool.QueryRow(context.Background(), `SELECT content, sent_via FROM chat_message WHERE id = $1 AND chat_session_id = $2`, response["message_id"], sessionID).Scan(&content, &sentVia); err != nil {
		test.Fatal(err)
	}
	if !strings.Contains(content, "Old lane → Decision lane") || !strings.Contains(content, caller) || !strings.Contains(content, "rename-caller") || sentVia != "task_token" {
		test.Fatalf("missing title transition or authenticated actor: %q, via %q", content, sentVia)
	}
	dbfx.Cleanup(test, `DELETE FROM chat_message WHERE id = $1`, response["message_id"])
	for _, endpoint := range []http.HandlerFunc{testHandler.GetChatSession, testHandler.ListChatMessages} {
		read := chatRenameRequest(test, sessionID, caller, "", "")
		read.Header.Set("Authorization", "Bearer "+credentials["task_token"])
		guarded := middleware.Auth(testHandler.Queries, nil, nil, nil)(endpoint)
		testutil.Call(test, guarded.ServeHTTP, read).Want(http.StatusForbidden)
	}
}

func TestChatRenameValidationAndAuthorization(test *testing.T) {
	agentID := createHandlerTestAgent(test, "rename-validation", []byte("[]"))
	sessionID := createHandlerTestChatSession(test, agentID)
	taskID := dbfx.Task(test, agentID, testutil.Cols{"status": "running", "runtime_id": handlerTestRuntimeID(test)})
	for _, title := range []string{" ", strings.Repeat("界", chatSessionTitleMaxLen+1), "forged\nannouncement", "bad\x00title", "line\u2028separator"} {
		test.Run(fmt.Sprintf("invalid_%q", title), func(test *testing.T) {
			testutil.Call(test, testHandler.UpdateChatSession, chatRenameRequest(test, sessionID, agentID, taskID, title)).Want(http.StatusBadRequest)
		})
	}
	for _, change := range []struct {
		name, statement string
		value           any
	}{
		{"completed_task", `UPDATE agent_task_queue SET status = $2 WHERE id = $1`, "completed"},
		{"other_agent_task", `UPDATE agent_task_queue SET agent_id = $2 WHERE id = $1`, createHandlerTestAgent(test, "rename-other", []byte("[]"))},
	} {
		test.Run(change.name, func(test *testing.T) {
			dbfx.Exec(test, change.statement, taskID, change.value)
			test.Cleanup(func() {
				dbfx.Exec(test, `UPDATE agent_task_queue SET status = 'running', agent_id = $2 WHERE id = $1`, taskID, agentID)
			})
			testutil.Call(test, testHandler.UpdateChatSession, chatRenameRequest(test, sessionID, agentID, taskID, "Denied")).Want(http.StatusForbidden)
		})
	}
	foreignWorkspace := createOtherTestWorkspace(test)
	dbfx.Exec(test, `UPDATE chat_session SET workspace_id = $2 WHERE id = $1`, sessionID, foreignWorkspace)
	testutil.Call(test, testHandler.UpdateChatSession, chatRenameRequest(test, sessionID, agentID, taskID, "Denied")).Want(http.StatusNotFound)
	if count := dbfx.Count(test, `SELECT count(*) FROM chat_message WHERE chat_session_id = $1`, sessionID); count != 0 {
		test.Fatalf("rejected renames wrote %d announcements", count)
	}
}

func TestChatRenameForgedCredentials(test *testing.T) {
	agentID := createHandlerTestAgent(test, "rename-forgery", []byte("[]"))
	owner := createSecondWorkspaceMember(test)
	sessionID := createHandlerTestChatSession(test, agentID)
	dbfx.Exec(test, `UPDATE chat_session SET creator_id = $2 WHERE id = $1`, sessionID, owner)
	credentials := provenanceCredentials(test, agentID)
	protected := middleware.Auth(testHandler.Queries, nil, provenanceCloudVerifier(test), nil)(http.HandlerFunc(testHandler.UpdateChatSession))
	for source, token := range credentials {
		if source == "task_token" {
			continue
		}
		test.Run(source, func(test *testing.T) {
			request := chatRenameRequest(test, sessionID, agentID, "forged-task", "Denied")
			request.Header.Set("Authorization", "Bearer "+token)
			testutil.Call(test, protected.ServeHTTP, request).Want(http.StatusForbidden)
		})
	}
	if count := dbfx.Count(test, `SELECT count(*) FROM chat_message WHERE chat_session_id = $1`, sessionID); count != 0 {
		test.Fatalf("forged actor wrote %d announcements", count)
	}
}

func TestChatRenameUnicodeBoundary(test *testing.T) {
	agentID := createHandlerTestAgent(test, "rename-unicode", []byte("[]"))
	sessionID := createHandlerTestChatSession(test, agentID)
	taskID := dbfx.Task(test, agentID, testutil.Cols{"status": "running", "runtime_id": handlerTestRuntimeID(test)})
	test.Cleanup(func() { dbfx.Exec(test, `DELETE FROM chat_message WHERE chat_session_id = $1`, sessionID) })
	title := strings.Repeat("界", chatSessionTitleMaxLen)
	testutil.Call(test, testHandler.UpdateChatSession, chatRenameRequest(test, sessionID, agentID, taskID, title)).Want(http.StatusOK)
	if actual := chatSessionTitleFromDB(test, parseUUID(sessionID)); actual != title {
		test.Fatalf("valid Unicode boundary title changed: %q", actual)
	}
}

func TestChatRenameAtomicAnnouncement(test *testing.T) {
	agentID := createHandlerTestAgent(test, "rename-atomic", []byte("[]"))
	sessionID := createHandlerTestChatSession(test, agentID)
	constraint := "che2090_reject_notice"
	dbfx.Exec(test, fmt.Sprintf(`ALTER TABLE chat_message ADD CONSTRAINT %s CHECK (chat_session_id <> '%s'::uuid) NOT VALID`, constraint, sessionID))
	test.Cleanup(func() { dbfx.Exec(test, "ALTER TABLE chat_message DROP CONSTRAINT "+constraint) })
	request := withChatTestWorkspaceCtx(test, withURLParam(newRequest(http.MethodPatch, "/", map[string]string{"title": "Must roll back"}), "sessionId", sessionID))
	testutil.Call(test, testHandler.UpdateChatSession, request).Want(http.StatusInternalServerError)
	if title := chatSessionTitleFromDB(test, parseUUID(sessionID)); title != "Handler Test Chat Session" {
		test.Fatalf("title committed without announcement: %q", title)
	}
}

func TestChatRenameTitleGenerationRaces(test *testing.T) {
	withStubLLM(test, stubLLMCompletion(test, http.StatusOK, "Generated title"))
	for _, titles := range [][]string{{"Original"}, {"Temporary", "Original"}} {
		test.Run(strings.Join(titles, "/"), func(test *testing.T) {
			agentID := createHandlerTestAgent(test, "rename-race", []byte("[]"))
			sessionID := createHandlerTestChatSession(test, agentID)
			dbfx.Exec(test, `UPDATE chat_session SET title = 'Original' WHERE id = $1`, sessionID)
			test.Cleanup(func() { dbfx.Exec(test, `DELETE FROM chat_message WHERE chat_session_id = $1`, sessionID) })
			for _, title := range titles {
				request := withChatTestWorkspaceCtx(test, withURLParam(newRequest(http.MethodPatch, "/", map[string]string{"title": title}), "sessionId", sessionID))
				testutil.Call(test, testHandler.UpdateChatSession, request).Want(http.StatusOK)
			}
			_, applied, err := testHandler.generateChatSessionTitle(context.Background(), parseUUID(sessionID), "Original", "Opening message")
			if err != nil || applied || chatSessionTitleFromDB(test, parseUUID(sessionID)) != "Original" {
				test.Fatalf("generation overwrote manual rename: applied=%v, error=%v", applied, err)
			}
		})
	}
}

func TestChatRenameDuringTitleGeneration(test *testing.T) {
	agentID := createHandlerTestAgent(test, "rename-inflight", []byte("[]"))
	sessionID := createHandlerTestChatSession(test, agentID)
	dbfx.Exec(test, `UPDATE chat_session SET title = 'Original' WHERE id = $1`, sessionID)
	test.Cleanup(func() { dbfx.Exec(test, `DELETE FROM chat_message WHERE chat_session_id = $1`, sessionID) })
	renameRequest := withChatTestWorkspaceCtx(test, withURLParam(newRequest(http.MethodPatch, "/", map[string]string{"title": "Manual title"}), "sessionId", sessionID))
	status := make(chan int, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		response := httptest.NewRecorder()
		testHandler.UpdateChatSession(response, renameRequest)
		status <- response.Code
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, `{"choices":[{"message":{"role":"assistant","content":"Generated title"},"finish_reason":"stop"}]}`)
	}))
	test.Cleanup(server.Close)
	withStubLLM(test, server)
	_, applied, err := testHandler.generateChatSessionTitle(context.Background(), parseUUID(sessionID), "Original", "Opening message")
	if renameStatus := <-status; renameStatus != http.StatusOK || err != nil || applied || chatSessionTitleFromDB(test, parseUUID(sessionID)) != "Manual title" {
		test.Fatalf("in-flight title generation clobbered rename: status=%d applied=%v error=%v", renameStatus, applied, err)
	}
}

func TestChatRenameConcurrentAnnouncements(test *testing.T) {
	agentID := createHandlerTestAgent(test, "rename-concurrent", []byte("[]"))
	sessionID := createHandlerTestChatSession(test, agentID)
	dbfx.Exec(test, `UPDATE chat_session SET title = 'Original' WHERE id = $1`, sessionID)
	test.Cleanup(func() { dbfx.Exec(test, `DELETE FROM chat_message WHERE chat_session_id = $1`, sessionID) })
	responses := make(chan *httptest.ResponseRecorder, 2)
	for _, title := range []string{"First", "Second"} {
		request := withChatTestWorkspaceCtx(test, withURLParam(newRequest(http.MethodPatch, "/", map[string]string{"title": title}), "sessionId", sessionID))
		go func() {
			response := httptest.NewRecorder()
			testHandler.UpdateChatSession(response, request)
			responses <- response
		}()
	}
	first, second := <-responses, <-responses
	for _, response := range []*httptest.ResponseRecorder{first, second} {
		if response.Code != http.StatusOK {
			test.Fatalf("concurrent rename failed: %d %s", response.Code, response.Body.String())
		}
	}
	if count := dbfx.Count(test, `SELECT count(*) FROM chat_message WHERE chat_session_id = $1`, sessionID); count != 2 {
		test.Fatalf("expected two atomic announcements, got %d", count)
	}
	final := chatSessionTitleFromDB(test, parseUUID(sessionID))
	other := "First"
	if final == "First" {
		other = "Second"
	}
	for _, transition := range []string{"Original → " + other, other + " → " + final} {
		if count := dbfx.Count(test, `SELECT count(*) FROM chat_message WHERE chat_session_id = $1 AND strpos(content, $2) > 0`, sessionID, transition); count != 1 {
			test.Fatalf("missing serialized transition %q", transition)
		}
	}
}
