package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/internal/middleware"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func forkChatRequest(test *testing.T, sessionID, key string) *http.Request {
	test.Helper()
	request := newRequest(http.MethodPost, "/api/chat/sessions/"+sessionID+"/fork", map[string]string{
		"summary": "Goal: explore another approach. Evidence: retained.", "idempotency_key": key,
	})
	return withChatTestWorkspaceCtx(test, withURLParam(request, "sessionId", sessionID))
}

func cleanupChatFork(test *testing.T, childID string) {
	test.Helper()
	dbfx.Cleanup(test, `DELETE FROM chat_session WHERE id = $1`, childID)
	dbfx.Cleanup(test, `DELETE FROM chat_message WHERE chat_session_id = $1`, childID)
}

func TestForkChatSessionPreservesSourceAndStartsFresh(test *testing.T) {
	agentID := createHandlerTestAgent(test, "fork-chat", []byte("[]"))
	runtimeID := handlerTestRuntimeID(test)
	projectID := dbfx.Project(test, "Fork project")
	dbfx.Exec(test, `UPDATE agent SET runtime_id = $2 WHERE id = $1`, agentID, runtimeID)
	sourceID := dbfx.ChatSession(test, agentID, testutil.Cols{
		"title": "Explore", "project_id": projectID, "session_id": "provider-source", "work_dir": "/tmp/fork-source",
		"runtime_id": runtimeID,
	})
	dbfx.Insert(test, "chat_message", testutil.Cols{"chat_session_id": sourceID, "role": "user", "content": "Source goal"})
	before, err := testHandler.Queries.GetChatSession(context.Background(), parseUUID(sourceID))
	if err != nil {
		test.Fatal(err)
	}
	var sourceSnapshot string
	dbfx.QueryRow(test, `SELECT row_to_json(session)::text FROM chat_session session WHERE id = $1`, sourceID).Scan(&sourceSnapshot)
	var result ChatContinueResponse
	testutil.Call(test, testHandler.ForkChatSession, forkChatRequest(test, sourceID, "operation-one")).Want(http.StatusCreated).JSON(&result)
	cleanupChatFork(test, result.Session.ID)
	after, err := testHandler.Queries.GetChatSession(context.Background(), parseUUID(sourceID))
	if err != nil || before != after {
		test.Fatalf("fork changed the source DM: before=%+v after=%+v error=%v", before, after, err)
	}
	var sourceAfter string
	dbfx.QueryRow(test, `SELECT row_to_json(session)::text FROM chat_session session WHERE id = $1`, sourceID).Scan(&sourceAfter)
	if sourceSnapshot != sourceAfter {
		test.Fatal("fork changed a source column outside the session projection")
	}
	child, err := testHandler.Queries.GetChatSession(context.Background(), parseUUID(result.Session.ID))
	if err != nil {
		test.Fatal(err)
	}
	if child.ID == before.ID || child.AgentID != before.AgentID || child.CreatorID != before.CreatorID || child.ProjectID != before.ProjectID || child.Title != "Explore (fork)" || child.Status != "active" || child.SessionID.Valid || child.WorkDir.Valid || child.RuntimeID.Valid || child.ContinuedFromSessionID.Valid || child.ContinuedIntoSessionID.Valid {
		test.Fatalf("fork did not start fresh: %+v", child)
	}
	messages, err := testHandler.Queries.ListChatMessages(context.Background(), child.ID)
	if err != nil || len(messages) != 1 || messages[0].MessageKind != "continuation_summary" || !strings.Contains(messages[0].Content, "Goal: explore another approach.") || !strings.Contains(messages[0].Content, sourceID) || messages[0].TaskID.Valid {
		test.Fatalf("fork summary = %+v, error=%v", messages, err)
	}
	if result.PreviousSessionID != sourceID || !strings.Contains(result.URL, result.Session.ID) {
		test.Fatalf("fork response missing source or destination: %+v", result)
	}
	if child.ForkedFromSessionID != before.ID || result.Session.ForkedFromSessionID == nil || *result.Session.ForkedFromSessionID != sourceID {
		test.Fatalf("fork source link missing: %+v", result.Session)
	}
	for _, status := range []string{"active", "all"} {
		var sessions []ChatSessionResponse
		request := withChatTestWorkspaceCtx(test, newRequest(http.MethodGet, "/?status="+status, nil))
		testutil.Call(test, testHandler.ListChatSessions, request).Want(http.StatusOK).JSON(&sessions)
		foundSource, foundChild := false, false
		for _, session := range sessions {
			if session.ID == sourceID {
				foundSource = true
			}
			if session.ID == result.Session.ID {
				foundChild = true
				if session.ForkedFromSessionID == nil || *session.ForkedFromSessionID != sourceID || session.UnreadCount != 1 {
					test.Fatalf("list dropped fork source or unread summary: %+v", session)
				}
			}
		}
		if !foundSource || !foundChild {
			test.Fatalf("%s list lost source or child: source=%v child=%v", status, foundSource, foundChild)
		}
	}
	var fetched ChatSessionResponse
	request := withChatTestWorkspaceCtx(test, withURLParam(newRequest(http.MethodGet, "/", nil), "sessionId", result.Session.ID))
	testutil.Call(test, testHandler.GetChatSession, request).Want(http.StatusOK).JSON(&fetched)
	if fetched.ForkedFromSessionID == nil || *fetched.ForkedFromSessionID != sourceID {
		test.Fatalf("get dropped fork source: %+v", fetched)
	}
}

func TestForkChatSessionTaskDefaultKey(test *testing.T) {
	agentID := createHandlerTestAgent(test, "fork-task-key", []byte("[]"))
	sourceID := dbfx.ChatSession(test, agentID)
	var children []string
	for _, status := range []string{"running", "dispatched"} {
		taskID := dbfx.Task(test, agentID, testutil.Cols{"chat_session_id": sourceID, "status": status, "runtime_id": handlerTestRuntimeID(test)})
		request := func() *http.Request {
			request := forkChatRequest(test, "", "")
			request.Header.Set("X-Actor-Source", "task_token")
			request.Header.Set("X-Task-ID", taskID)
			return request
		}
		var child, repeated ChatContinueResponse
		testutil.Call(test, testHandler.ForkChatSession, request()).Want(http.StatusCreated).JSON(&child)
		cleanupChatFork(test, child.Session.ID)
		testutil.Call(test, testHandler.ForkChatSession, request()).Want(http.StatusOK).JSON(&repeated)
		if repeated.Session.ID != child.Session.ID {
			test.Fatalf("task retry changed identity: %+v", repeated)
		}
		children = append(children, child.Session.ID)
	}
	if children[0] == children[1] {
		test.Fatal("different tasks reused a single-successor fork")
	}
}

func TestForkChatSessionAuthorizationAndValidation(test *testing.T) {
	agentID := createHandlerTestAgent(test, "fork-auth", []byte("[]"))
	for _, name := range []string{"other-human", "agent", "other-chat", "issue-task", "completed-task", "wrong-agent", "wrong-workspace", "malformed-task", "missing-key", "long-key", "nul-key", "blank-summary", "long-summary", "nul-summary", "archived", "channel"} {
		test.Run(name, func(test *testing.T) {
			sourceID := dbfx.ChatSession(test, agentID)
			request := forkChatRequest(test, sourceID, "operation")
			want := http.StatusForbidden
			switch name {
			case "other-human":
				request.Header.Set("X-User-ID", uuid.NewString())
			case "agent":
				request.Header.Set("X-Agent-ID", agentID)
			case "other-chat", "issue-task", "completed-task", "wrong-agent", "wrong-workspace":
				columns := testutil.Cols{"status": "running", "chat_session_id": sourceID, "runtime_id": handlerTestRuntimeID(test)}
				taskAgentID := agentID
				if name == "other-chat" {
					columns["chat_session_id"] = dbfx.ChatSession(test, agentID)
				} else if name == "issue-task" {
					delete(columns, "chat_session_id")
				} else if name == "completed-task" {
					columns["status"] = "completed"
				} else if name == "wrong-agent" {
					taskAgentID = createHandlerTestAgent(test, "fork-wrong-agent", []byte("[]"))
				} else {
					request = request.WithContext(middleware.SetMemberContext(request.Context(), uuid.NewString(), db.Member{}))
				}
				taskID := dbfx.Task(test, taskAgentID, columns)
				request.Header.Set("X-Actor-Source", "task_token")
				request.Header.Set("X-Task-ID", taskID)
			case "malformed-task":
				request.Header.Set("X-Actor-Source", "task_token")
				request.Header.Set("X-Task-ID", "not-a-uuid")
			case "missing-key", "long-key", "nul-key":
				key := map[string]string{"missing-key": "", "long-key": strings.Repeat("x", 129), "nul-key": "key\x00"}[name]
				request = forkChatRequest(test, sourceID, key)
				want = http.StatusBadRequest
			case "blank-summary", "long-summary", "nul-summary":
				summary := map[string]string{"blank-summary": " \n", "long-summary": strings.Repeat("x", 65537), "nul-summary": "summary\x00"}[name]
				request = withChatTestWorkspaceCtx(test, withURLParam(newRequest(http.MethodPost, "/fork", map[string]string{"summary": summary, "idempotency_key": "operation"}), "sessionId", sourceID))
				want = http.StatusBadRequest
			case "archived":
				dbfx.Exec(test, `UPDATE chat_session SET status = 'archived' WHERE id = $1`, sourceID)
				want = http.StatusConflict
			case "channel":
				dbfx.Exec(test, `UPDATE chat_session SET channel_origin_at = now() WHERE id = $1`, sourceID)
				want = http.StatusConflict
			}
			before, err := testHandler.Queries.GetChatSession(context.Background(), parseUUID(sourceID))
			if err != nil {
				test.Fatal(err)
			}
			testutil.Call(test, testHandler.ForkChatSession, request).Want(want)
			after, err := testHandler.Queries.GetChatSession(context.Background(), parseUUID(sourceID))
			if err != nil || before != after || dbfx.Count(test, `SELECT count(*) FROM chat_session WHERE forked_from_session_id = $1`, sourceID) != 0 {
				test.Fatalf("rejected fork changed source or created a child: %+v, %v", after, err)
			}
		})
	}
}

func TestForkChatSessionLeavesDecisionAndTaskOwnership(test *testing.T) {
	fixture := newRequestCoreFixture(test, false)
	decision, err := testHandler.createDecisionRequest(fixture.request(), fixture.issue, fixture.input())
	if err != nil {
		test.Fatal(err)
	}
	taskID := dbfx.Task(test, fixture.agentID, testutil.Cols{"chat_session_id": fixture.sessionID, "status": "running", "runtime_id": handlerTestRuntimeID(test)})
	answerID := dbfx.Insert(test, "decision_answer", testutil.Cols{
		"decision_request_id": decision.Request.ID, "answered_by_task_id": taskID, "source_chat_session_id": fixture.sessionID,
		"chat_message_ids": []string{uuidToString(decision.Message.ID)}, "content_snapshot": "Approved", "effective_at": testutil.Raw("now()"),
	})
	tokenID := dbfx.Insert(test, "task_token", testutil.Cols{
		"token_hash": "fork-fixture-" + uuid.NewString(), "task_id": taskID, "agent_id": fixture.agentID,
		"workspace_id": testWorkspaceID, "user_id": fixture.humanID, "expires_at": testutil.Raw("now() + interval '1 hour'"),
	})
	var answerBefore, tokenBefore string
	dbfx.QueryRow(test, `SELECT row_to_json(answer)::text FROM decision_answer answer WHERE id = $1`, answerID).Scan(&answerBefore)
	dbfx.QueryRow(test, `SELECT row_to_json(token)::text FROM task_token token WHERE id = $1`, tokenID).Scan(&tokenBefore)
	before, err := testHandler.Queries.GetAgentTask(context.Background(), parseUUID(taskID))
	if err != nil {
		test.Fatal(err)
	}
	request := forkChatRequest(test, fixture.sessionID, "decision-fork")
	request.Header.Set("X-Actor-Source", "task_token")
	request.Header.Set("X-Task-ID", taskID)
	var child ChatContinueResponse
	testutil.Call(test, testHandler.ForkChatSession, request).Want(http.StatusCreated).JSON(&child)
	cleanupChatFork(test, child.Session.ID)
	stored, err := testHandler.Queries.GetDecisionRequest(context.Background(), db.GetDecisionRequestParams{WorkspaceID: fixture.issue.WorkspaceID, ID: decision.Request.ID})
	if err != nil || stored != decision.Request {
		test.Fatalf("fork transferred or changed the decision: %+v, %v", stored, err)
	}
	after, err := testHandler.Queries.GetAgentTask(context.Background(), parseUUID(taskID))
	if err != nil || !reflect.DeepEqual(before, after) {
		test.Fatalf("fork changed the source task: %+v, %v", after, err)
	}
	var answerAfter, tokenAfter string
	dbfx.QueryRow(test, `SELECT row_to_json(answer)::text FROM decision_answer answer WHERE id = $1`, answerID).Scan(&answerAfter)
	dbfx.QueryRow(test, `SELECT row_to_json(token)::text FROM task_token token WHERE id = $1`, tokenID).Scan(&tokenAfter)
	if answerBefore != answerAfter || tokenBefore != tokenAfter {
		test.Fatal("fork changed the source approval or task token")
	}
	for label, query := range map[string]string{
		"decisions": `SELECT count(*) FROM decision_request WHERE chat_session_id = $1`,
		"tasks":     `SELECT count(*) FROM agent_task_queue WHERE chat_session_id = $1`,
		"approvals": `SELECT count(*) FROM decision_answer WHERE source_chat_session_id = $1`,
		"tokens":    `SELECT count(*) FROM task_token token JOIN agent_task_queue task ON task.id = token.task_id WHERE task.chat_session_id = $1`,
	} {
		if count := dbfx.Count(test, query, child.Session.ID); count != 0 {
			test.Fatalf("child inherited %s: %d", label, count)
		}
	}
}

func TestForkChatSessionConcurrentRetry(test *testing.T) {
	agentID := createHandlerTestAgent(test, "fork-concurrent", []byte("[]"))
	sourceID := dbfx.ChatSession(test, agentID)
	requests := []*http.Request{forkChatRequest(test, sourceID, "same"), forkChatRequest(test, sourceID, "same"), forkChatRequest(test, sourceID, "different")}
	responses := make(chan *httptest.ResponseRecorder, len(requests))
	for _, request := range requests {
		go func(request *http.Request) {
			response := httptest.NewRecorder()
			testHandler.ForkChatSession(response, request)
			responses <- response
		}(request)
	}
	children := map[string]bool{}
	created, replayed := 0, 0
	for range requests {
		response := <-responses
		if response.Code == http.StatusCreated {
			created++
		} else if response.Code == http.StatusOK {
			replayed++
		} else {
			test.Errorf("concurrent fork: %d %s", response.Code, response.Body.String())
			continue
		}
		var child ChatContinueResponse
		if err := json.Unmarshal(response.Body.Bytes(), &child); err != nil {
			test.Error(err)
			continue
		}
		if !children[child.Session.ID] {
			cleanupChatFork(test, child.Session.ID)
			children[child.Session.ID] = true
		}
	}
	if created != 2 || replayed != 1 || len(children) != 2 || dbfx.Count(test, `SELECT count(*) FROM chat_session WHERE forked_from_session_id = $1`, sourceID) != 2 {
		test.Fatalf("concurrent operation counts: created=%d replayed=%d children=%d", created, replayed, len(children))
	}
}

func TestForkChatSessionFirstClaimUsesSummary(test *testing.T) {
	ctx := context.Background()
	agentID, sourceID, runtimeID, daemonID := setupDirectChatSession(test, ctx, "Fork claim")
	dbfx.Exec(test, `UPDATE chat_session SET session_id = 'provider-source', work_dir = '/tmp/fork-source' WHERE id = $1`, sourceID)
	var child ChatContinueResponse
	testutil.Call(test, testHandler.ForkChatSession, forkChatRequest(test, sourceID, "claim-operation")).Want(http.StatusCreated).JSON(&child)
	cleanupChatFork(test, child.Session.ID)
	taskID := sendDirectChat(test, ctx, agentID, child.Session.ID, "Take the next action")
	request := newDaemonTokenRequest(http.MethodPost, "/api/daemon/runtimes/"+runtimeID+"/claim", nil, testWorkspaceID, daemonID)
	request = withURLParam(request, "runtimeId", runtimeID)
	var response struct {
		Task *AgentTaskResponse `json:"task"`
	}
	testutil.Call(test, testHandler.ClaimTaskByRuntime, request).Want(http.StatusOK).JSON(&response)
	if response.Task == nil {
		test.Fatal("no fork task was claimed")
	}
	if response.Task.ID != taskID || response.Task.PriorSessionID != "" || response.Task.PriorWorkDir != "" || !strings.Contains(response.Task.ChatMessage, "Goal: explore another approach.") || !strings.Contains(response.Task.ChatMessage, "Take the next action") {
		test.Fatalf("fork claim did not start fresh with the summary: task=%s prior_session=%q prior_work_dir=%q message=%q", response.Task.ID, response.Task.PriorSessionID, response.Task.PriorWorkDir, response.Task.ChatMessage)
	}
}

func TestForkChatSessionRollsBackFailedSummary(test *testing.T) {
	agentID := createHandlerTestAgent(test, "fork-rollback", []byte("[]"))
	sourceID := dbfx.ChatSession(test, agentID)
	handler := *testHandler
	handler.TxStarter = failContinuationSummaryStarter{inner: testHandler.TxStarter}
	testutil.Call(test, handler.ForkChatSession, forkChatRequest(test, sourceID, "failed-operation")).Want(http.StatusInternalServerError)
	if count := dbfx.Count(test, `SELECT count(*) FROM chat_session WHERE agent_id = $1`, agentID); count != 1 {
		test.Fatalf("failed fork left a child: %d", count)
	}
	if count := dbfx.Count(test, `SELECT count(*) FROM chat_session WHERE id = $1 AND status = 'active' AND continued_into_session_id IS NULL`, sourceID); count != 1 {
		test.Fatal("failed fork changed the source")
	}
}

func TestForkChatSessionOperationRetryAndMultipleChildren(test *testing.T) {
	agentID := createHandlerTestAgent(test, "fork-retry", []byte("[]"))
	sourceID := dbfx.ChatSession(test, agentID)
	var first, retry, second ChatContinueResponse
	testutil.Call(test, testHandler.ForkChatSession, forkChatRequest(test, sourceID, "first")).Want(http.StatusCreated).JSON(&first)
	cleanupChatFork(test, first.Session.ID)
	testutil.Call(test, testHandler.ForkChatSession, forkChatRequest(test, sourceID, "first")).Want(http.StatusOK).JSON(&retry)
	testutil.Call(test, testHandler.ForkChatSession, forkChatRequest(test, sourceID, "second")).Want(http.StatusCreated).JSON(&second)
	cleanupChatFork(test, second.Session.ID)
	if retry.Session.ID != first.Session.ID || second.Session.ID == first.Session.ID {
		test.Fatalf("per-operation identity violated: first=%s retry=%s second=%s", first.Session.ID, retry.Session.ID, second.Session.ID)
	}
	if count := dbfx.Count(test, `SELECT count(*) FROM chat_message WHERE chat_session_id IN ($1, $2)`, first.Session.ID, second.Session.ID); count != 2 {
		test.Fatalf("fork retries duplicated summaries: %d", count)
	}
}
