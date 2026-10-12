package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

func TestChatAutoCompactIdleThreshold(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	for _, testCase := range []struct {
		name, setting           string
		input, cached, cliLimit int64
		queued                  bool
		want                    int
	}{
		{"default off", "", 170000, 10000, 190000, false, 0},
		{"below threshold despite task sum", "threshold:80", 4000, 4000, 190000, false, 0},
		{"cache counts toward threshold", "threshold:80", 10000, 160000, 190000, false, 1},
		{"queued human input", "threshold:80", 170000, 10000, 190000, true, 0},
		{"CLI compacts before threshold", "threshold:80", 170000, 10000, 150000, false, 0},
		{"unknown CLI point", "threshold:80", 170000, 10000, 0, false, 0},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			ctx := context.Background()
			agentID, sessionID, runtimeID, daemonID := setupDirectChatSession(t, ctx, "Auto compact")
			config, _ := json.Marshal(map[string]string{"auto_compact": testCase.setting})
			dbfx.Exec(t, `UPDATE agent SET runtime_config = $1 WHERE id = $2`, config, agentID)
			taskID := sendDirectChat(t, ctx, agentID, sessionID, "Finish this turn")
			claimAutoCompactTestTask(t, runtimeID, daemonID)
			markTaskRunning(t, ctx, taskID)
			last := map[string]any{"provider": "claude", "model": "claude-sonnet-4-6", "input_tokens": testCase.input, "cache_read_tokens": testCase.cached, "context_window": 200000, "cli_compaction_tokens": testCase.cliLimit}
			usageRequest := newDaemonTokenRequest(http.MethodPost, "/api/daemon/tasks/"+taskID+"/usage", map[string]any{
				"usage":              []map[string]any{{"provider": "claude", "model": "claude-sonnet-4-6", "input_tokens": 900000}},
				"last_model_request": last,
			}, testWorkspaceID, daemonID)
			usageRequest = withURLParam(usageRequest, "taskId", taskID)
			testutil.Call(t, testHandler.ReportTaskUsage, usageRequest).Want(http.StatusOK)
			if count := dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE id = $1 AND last_model_request->>'input_tokens' = $2`, taskID, strconv.FormatInt(testCase.input, 10)); count != 1 {
				t.Fatal("last request was not recorded separately from task totals")
			}
			if count := dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE chat_session_id = $1 AND chat_compaction`, sessionID); count != 0 {
				t.Fatal("auto compact cut off a running turn")
			}
			if testCase.queued {
				sendDirectChat(t, ctx, agentID, sessionID, "Queued human message")
			}
			completeAutoCompactTestTask(t, taskID, daemonID)
			if count := dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE chat_session_id = $1 AND chat_compaction`, sessionID); count != testCase.want {
				t.Fatalf("auto compact tasks = %d, want %d", count, testCase.want)
			}
			completeAutoCompactTestTask(t, taskID, daemonID)
			if count := dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE chat_session_id = $1 AND chat_compaction`, sessionID); count != testCase.want {
				t.Fatal("completion replay duplicated auto compact")
			}
			if testCase.want == 1 {
				compact := claimAutoCompactTestTask(t, runtimeID, daemonID)
				if compact.PriorSessionID != "auto-source-provider" || !strings.Contains(compact.ChatMessage, "multica chat continue --summary-file") {
					t.Fatalf("auto compact did not resume with the summary instruction: %+v", compact)
				}
				markTaskRunning(t, ctx, compact.ID)
				completeAutoCompactTestTask(t, compact.ID, daemonID)
				if count := dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE chat_session_id = $1 AND chat_compaction`, sessionID); count != 1 {
					t.Fatal("auto completion created a compaction loop")
				}
				dbfx.Exec(t, `UPDATE agent_task_queue SET status = 'cancelled' WHERE id = $1`, compact.ID)
				completeAutoCompactTestTask(t, taskID, daemonID)
				if count := dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE chat_session_id = $1 AND chat_compaction`, sessionID); count != 1 {
					t.Fatal("completion replay retried a cancelled compaction")
				}
			}
		})
	}
}

func claimAutoCompactTestTask(t *testing.T, runtimeID, daemonID string) AgentTaskResponse {
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

func completeAutoCompactTestTask(t *testing.T, taskID, daemonID string) {
	t.Helper()
	request := newDaemonTokenRequest(http.MethodPost, "/api/daemon/tasks/"+taskID+"/complete", map[string]string{"output": "Turn finished.", "session_id": "auto-source-provider", "work_dir": "/tmp/auto-source"}, testWorkspaceID, daemonID)
	request = withURLParam(request, "taskId", taskID)
	testutil.Call(t, testHandler.CompleteTask, request).Want(http.StatusOK)
}

func TestChatCompactionRoutesNewHumanMessages(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	agentID, sessionID, runtimeID, daemonID := setupDirectChatSession(t, ctx, "Auto compact race")
	taskID := sendDirectChat(t, ctx, agentID, sessionID, "Finish the source turn")
	claimAutoCompactTestTask(t, runtimeID, daemonID)
	markTaskRunning(t, ctx, taskID)
	dbfx.Exec(t, `UPDATE agent_task_queue SET chat_compaction = true WHERE id = $1`, taskID)
	queuedID := sendDirectChat(t, ctx, agentID, sessionID, "Arrived during compaction")
	response := continueChatRequest(t, sessionID, "Goal: keep the newly arrived message.", taskID)
	if response.Code != http.StatusCreated {
		t.Fatalf("continue: %d %s", response.Code, response.Body.String())
	}
	var result ChatContinueResponse
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	dbfx.Cleanup(t, `DELETE FROM chat_message WHERE chat_session_id = $1`, result.Session.ID)
	dbfx.Cleanup(t, `DELETE FROM agent_task_queue WHERE chat_session_id = $1`, result.Session.ID)
	dbfx.Cleanup(t, `DELETE FROM chat_session WHERE id = $1`, result.Session.ID)
	if count := dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE id = $1 AND chat_session_id = $2`, queuedID, result.Session.ID); count != 1 {
		t.Fatal("queued input stayed in the archived source")
	}
	if count := dbfx.Count(t, `SELECT count(*) FROM chat_message WHERE task_id = $1 AND chat_session_id = $2 AND content = 'Arrived during compaction'`, queuedID, result.Session.ID); count != 1 {
		t.Fatal("new input did not follow its owning task")
	}
	messages, err := testHandler.Queries.ListChatMessages(ctx, parseUUID(result.Session.ID))
	if err != nil || len(messages) < 2 || messages[0].MessageKind != "continuation_summary" {
		t.Fatalf("summary was not first: %+v %v", messages, err)
	}
	completeAutoCompactTestTask(t, taskID, daemonID)
	claim := claimAutoCompactTestTask(t, runtimeID, daemonID)
	if claim.ID != queuedID || claim.PriorSessionID != "" || !strings.Contains(claim.ChatMessage, "keep the newly arrived message") || !strings.Contains(claim.ChatMessage, "Arrived during compaction") {
		t.Fatalf("fresh successor claim: %+v", claim)
	}
	lateRequest := newRequest(http.MethodPost, "/api/chat/sessions/"+sessionID+"/messages", map[string]string{"content": "Arrived just after archive"})
	lateRequest = withChatTestWorkspaceCtx(t, withURLParam(lateRequest, "sessionId", sessionID))
	var sent SendChatMessageResponse
	testutil.Call(t, testHandler.SendChatMessage, lateRequest).Want(http.StatusCreated).JSON(&sent)
	if count := dbfx.Count(t, `SELECT count(*) FROM chat_message WHERE id = $1 AND chat_session_id = $2`, sent.MessageID, result.Session.ID); count != 1 {
		t.Fatal("stale-tab send did not follow the successor")
	}
}
