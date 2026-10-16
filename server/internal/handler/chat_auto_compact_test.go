package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// CHE-1807 (A3): a web DM compacts itself after a completed run whose last
// prompt reached the threshold. Every assertion goes through the public
// handlers and raw SQL, so the file builds against a tree without the feature.

// The five states that count as "a task is active" for a chat session
// (chat.sql HasActiveChatTaskForSession). Compaction guards must cover all of
// them, not just queued and running (CHE-1804 P1).
var activeTaskStates = []string{"queued", "dispatched", "running", "waiting_local_directory", "deferred"}

type compactionTask struct {
	ID             string
	Status         string
	Kind           *string
	Reason         *string
	TriggerTokens  *int64
	ChatInputOwner *string
}

type autoCompactDM struct {
	agentID, sessionID, runtimeID, daemonID string
}

func newAutoCompactDM(t *testing.T, settings, agentConfig string) autoCompactDM {
	t.Helper()
	ctx := context.Background()
	workspace, err := testHandler.Queries.GetWorkspace(ctx, parseUUID(testWorkspaceID))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		dbfx.Exec(t, `UPDATE workspace SET settings = $1 WHERE id = $2`, workspace.Settings, testWorkspaceID)
	})
	agentID, sessionID, runtimeID, daemonID := setupDirectChatSession(t, ctx, "Auto-compact "+t.Name())
	dbfx.Exec(t, `UPDATE workspace SET settings = $1 WHERE id = $2`, []byte(settings), testWorkspaceID)
	dbfx.Exec(t, `UPDATE agent SET runtime_config = $1 WHERE id = $2`, []byte(agentConfig), agentID)
	t.Cleanup(func() {
		testPool.Exec(ctx, `DELETE FROM agent_task_queue WHERE chat_session_id = $1`, sessionID)
		testPool.Exec(ctx, `DELETE FROM chat_message WHERE chat_session_id = $1`, sessionID)
	})
	return autoCompactDM{agentID, sessionID, runtimeID, daemonID}
}

// sendRunning sends one user turn and puts its task in the running state.
func (dm autoCompactDM) sendRunning(t *testing.T, content string) string {
	t.Helper()
	taskID := sendDirectChat(t, context.Background(), dm.agentID, dm.sessionID, content)
	markTaskRunning(t, context.Background(), taskID)
	return taskID
}

// complete reports a successful run through the real /complete handler. Zero
// tokens or window omits the context object, as an old daemon would.
func (dm autoCompactDM) complete(t *testing.T, taskID string, tokens, window int64) {
	t.Helper()
	body := map[string]any{"output": "done"}
	if tokens > 0 || window > 0 {
		body["context"] = map[string]any{"tokens": tokens, "window": window}
	}
	req := newDaemonTokenRequest(http.MethodPost, "/api/daemon/tasks/"+taskID+"/complete", body, testWorkspaceID, dm.daemonID)
	testutil.Call(t, testHandler.CompleteTask, withURLParam(req, "taskId", taskID)).Want(http.StatusOK)
}

func (dm autoCompactDM) fail(t *testing.T, taskID string) {
	t.Helper()
	req := newDaemonTokenRequest(http.MethodPost, "/api/daemon/tasks/"+taskID+"/fail", map[string]any{"error": "boom"}, testWorkspaceID, dm.daemonID)
	testutil.Call(t, testHandler.FailTask, withURLParam(req, "taskId", taskID)).Want(http.StatusOK)
}

func (dm autoCompactDM) compactionTasks(t *testing.T) []compactionTask {
	t.Helper()
	rows, err := testPool.Query(context.Background(), `
		SELECT id::text, status, task_kind, compact_reason, compact_trigger_tokens, chat_input_task_id::text
		FROM agent_task_queue WHERE chat_session_id = $1 AND task_kind = 'compact' ORDER BY created_at`, dm.sessionID)
	if err != nil {
		t.Fatalf("read compaction tasks: %v", err)
	}
	defer rows.Close()
	var out []compactionTask
	for rows.Next() {
		var c compactionTask
		if err := rows.Scan(&c.ID, &c.Status, &c.Kind, &c.Reason, &c.TriggerTokens, &c.ChatInputOwner); err != nil {
			t.Fatal(err)
		}
		out = append(out, c)
	}
	return out
}

func (dm autoCompactDM) messages(t *testing.T, kind string) []string {
	t.Helper()
	rows, err := testPool.Query(context.Background(),
		`SELECT content FROM chat_message WHERE chat_session_id = $1 AND message_kind = $2 ORDER BY created_at`, dm.sessionID, kind)
	if err != nil {
		t.Fatalf("read messages: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var content string
		if err := rows.Scan(&content); err != nil {
			t.Fatal(err)
		}
		out = append(out, content)
	}
	return out
}

func (dm autoCompactDM) send(t *testing.T, content string) *httptest.ResponseRecorder {
	t.Helper()
	req := withChatTestWorkspaceCtx(t, withURLParam(newRequest(http.MethodPost, "/api/chat/sessions/"+dm.sessionID+"/messages", map[string]any{"content": content}), "sessionId", dm.sessionID))
	w := httptest.NewRecorder()
	testHandler.SendChatMessage(w, req)
	return w
}

func (dm autoCompactDM) userMessageCount(t *testing.T) int {
	t.Helper()
	return dbfx.Count(t, `SELECT count(*) FROM chat_message WHERE chat_session_id = $1 AND role = 'user'`, dm.sessionID)
}

func TestAutoCompactFiresOnlyAtThePercentThreshold(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	for _, tc := range []struct {
		name           string
		tokens, window int64
		want           bool
	}{
		{"200k below", 159_999, 200_000, false},
		{"200k at", 160_000, 200_000, true},
		{"200k above", 170_000, 200_000, true},
		{"1M below", 799_999, 1_000_000, false},
		{"1M at", 800_000, 1_000_000, true},
		{"old daemon sends no context", 0, 0, false},
		{"unknown window", 164_000, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dm := newAutoCompactDM(t, `{}`, `{}`)
			dm.complete(t, dm.sendRunning(t, "hello"), tc.tokens, tc.window)
			got := len(dm.compactionTasks(t))
			if (got == 1) != tc.want || got > 1 {
				t.Fatalf("%d compaction tasks, want fire=%v", got, tc.want)
			}
		})
	}
}

func TestAutoCompactEnqueuesATaskWithAServerBuiltReason(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	dm := newAutoCompactDM(t, `{}`, `{}`)
	dm.complete(t, dm.sendRunning(t, "hello"), 164_000, 200_000)

	tasks := dm.compactionTasks(t)
	if len(tasks) != 1 {
		t.Fatalf("%d compaction tasks, want 1", len(tasks))
	}
	task := tasks[0]
	wantReason := "Auto-compacted: context 164k of 200k tokens (82%) reached the 80% threshold."
	if task.Status != "queued" || task.Reason == nil || *task.Reason != wantReason {
		t.Fatalf("compaction task = %+v, want queued with reason %q", task, wantReason)
	}
	if task.TriggerTokens == nil || *task.TriggerTokens != 164_000 {
		t.Fatalf("compact_trigger_tokens = %v, want 164000", task.TriggerTokens)
	}
	// No user message feeds it: the task owns an empty input batch.
	if task.ChatInputOwner == nil || *task.ChatInputOwner != task.ID {
		t.Fatalf("chat_input_task_id = %v, want the task itself", task.ChatInputOwner)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM chat_message WHERE task_id = $1 AND role = 'user'`, task.ID); n != 0 {
		t.Fatalf("compaction task owns %d user messages, want 0", n)
	}
	notices := dm.messages(t, "compact_notice")
	if len(notices) != 1 || notices[0] != wantReason+" Continuing in a new DM." {
		t.Fatalf("compact_notice rows = %q, want one %q", notices, wantReason+" Continuing in a new DM.")
	}
	if role := dbfx.Count(t, `SELECT count(*) FROM chat_message WHERE chat_session_id = $1 AND message_kind = 'compact_notice' AND role = 'assistant'`, dm.sessionID); role != 1 {
		t.Fatalf("the notice must be an assistant message")
	}
}

func TestAutoCompactSettingsDecideTheTrigger(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	for _, tc := range []struct {
		name, settings, agentConfig string
		tokens, window              int64
		want                        bool
	}{
		{"workspace off", `{"auto_compact":{"enabled":false}}`, `{}`, 190_000, 200_000, false},
		{"agent turns it off", `{}`, `{"auto_compact":{"enabled":false}}`, 190_000, 200_000, false},
		{"agent turns it back on", `{"auto_compact":{"enabled":false}}`, `{"auto_compact":{"enabled":true}}`, 190_000, 200_000, true},
		{"workspace threshold 90 holds at 82%", `{"auto_compact":{"threshold_pct":90}}`, `{}`, 164_000, 200_000, false},
		{"workspace threshold 50 fires at 55%", `{"auto_compact":{"threshold_pct":50}}`, `{}`, 110_000, 200_000, true},
		{"agent threshold wins over workspace", `{"auto_compact":{"threshold_pct":50}}`, `{"auto_compact":{"threshold_pct":90}}`, 110_000, 200_000, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dm := newAutoCompactDM(t, tc.settings, tc.agentConfig)
			dm.complete(t, dm.sendRunning(t, "hello"), tc.tokens, tc.window)
			if got := len(dm.compactionTasks(t)); (got == 1) != tc.want || got > 1 {
				t.Fatalf("%d compaction tasks, want fire=%v", got, tc.want)
			}
		})
	}
}

func TestAutoCompactIgnoresRunsThatCannotTrigger(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	t.Run("a failed run", func(t *testing.T) {
		dm := newAutoCompactDM(t, `{}`, `{}`)
		dm.fail(t, dm.sendRunning(t, "hello"))
		if n := len(dm.compactionTasks(t)); n != 0 {
			t.Fatalf("%d compaction tasks after a failed run, want 0", n)
		}
	})
	t.Run("an already continued DM", func(t *testing.T) {
		dm := newAutoCompactDM(t, `{}`, `{}`)
		taskID := dm.sendRunning(t, "hello")
		next := dbfx.ChatSession(t, dm.agentID, testutil.Cols{})
		dbfx.Exec(t, `UPDATE chat_session SET continued_into_session_id = $1 WHERE id = $2`, next, dm.sessionID)
		dm.complete(t, taskID, 190_000, 200_000)
		if n := len(dm.compactionTasks(t)); n != 0 {
			t.Fatalf("%d compaction tasks for a continued DM, want 0", n)
		}
	})
	t.Run("a compaction task itself", func(t *testing.T) {
		dm := newAutoCompactDM(t, `{}`, `{}`)
		dm.complete(t, dm.sendRunning(t, "/compact"), 190_000, 200_000)
		for _, c := range dm.compactionTasks(t) {
			dbfx.Exec(t, `UPDATE agent_task_queue SET status = 'running', started_at = now() WHERE id = $1`, c.ID)
			dm.complete(t, c.ID, 195_000, 200_000)
		}
		if n := len(dm.compactionTasks(t)); n != 1 {
			t.Fatalf("%d compaction tasks, want only the manual one", n)
		}
	})
}

func TestAutoCompactQueuesAtMostOneCompactionInEveryActiveState(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	for _, state := range activeTaskStates {
		t.Run(state, func(t *testing.T) {
			dm := newAutoCompactDM(t, `{}`, `{}`)
			first := dm.sendRunning(t, "one")
			second := dm.sendRunning(t, "two")
			dm.complete(t, first, 170_000, 200_000)
			tasks := dm.compactionTasks(t)
			if len(tasks) != 1 {
				t.Fatalf("%d compaction tasks after the first run, want 1", len(tasks))
			}
			dbfx.Exec(t, `UPDATE agent_task_queue SET status = $2 WHERE id = $1`, tasks[0].ID, state)
			dm.complete(t, second, 175_000, 200_000)
			if n := len(dm.compactionTasks(t)); n != 1 {
				t.Fatalf("%d compaction tasks with the first in %q, want still 1", n, state)
			}
		})
	}
}

func TestSendIsRefusedWhileACompactionIsActiveInEveryActiveState(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	for _, state := range activeTaskStates {
		t.Run(state, func(t *testing.T) {
			dm := newAutoCompactDM(t, `{}`, `{}`)
			dm.complete(t, dm.sendRunning(t, "hello"), 170_000, 200_000)
			tasks := dm.compactionTasks(t)
			if len(tasks) != 1 {
				t.Fatalf("%d compaction tasks, want 1", len(tasks))
			}
			dbfx.Exec(t, `UPDATE agent_task_queue SET status = $2 WHERE id = $1`, tasks[0].ID, state)
			before := dm.userMessageCount(t)

			w := dm.send(t, "are you there?")
			if w.Code != http.StatusConflict {
				t.Fatalf("send while compaction is %q: %d %s, want 409", state, w.Code, w.Body.String())
			}
			var body struct{ Code string }
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body.Code != "chat_compacting" {
				t.Fatalf("409 body = %s, want code chat_compacting", w.Body.String())
			}
			if after := dm.userMessageCount(t); after != before {
				t.Fatalf("a refused send stored %d new user messages", after-before)
			}
		})
	}
}

func TestCancellingTheCompactionUnlocksSends(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	dm := newAutoCompactDM(t, `{}`, `{}`)
	dm.complete(t, dm.sendRunning(t, "hello"), 170_000, 200_000)
	tasks := dm.compactionTasks(t)
	if len(tasks) != 1 {
		t.Fatalf("%d compaction tasks, want 1", len(tasks))
	}
	if w := dm.send(t, "blocked"); w.Code != http.StatusConflict {
		t.Fatalf("send during compaction: %d, want 409", w.Code)
	}
	if _, err := testHandler.TaskService.CancelTask(context.Background(), parseUUID(tasks[0].ID)); err != nil {
		t.Fatalf("cancel compaction: %v", err)
	}
	if w := dm.send(t, "now allowed"); w.Code != http.StatusCreated && w.Code != http.StatusOK {
		t.Fatalf("send after cancel: %d %s, want success", w.Code, w.Body.String())
	}
}

func TestManualCompactIsMarkedAtEnqueueAndLocksSends(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	dm := newAutoCompactDM(t, `{}`, `{}`)
	manual := sendDirectChat(t, context.Background(), dm.agentID, dm.sessionID, "/compact keep SHAs")
	tasks := dm.compactionTasks(t)
	if len(tasks) != 1 || tasks[0].ID != manual {
		t.Fatalf("compaction tasks = %+v, want the manual task %s", tasks, manual)
	}
	if tasks[0].Reason != nil || tasks[0].TriggerTokens != nil {
		t.Fatalf("a manual compaction carries no reason: %+v", tasks[0])
	}
	if w := dm.send(t, "meanwhile"); w.Code != http.StatusConflict {
		t.Fatalf("send during a manual compaction: %d, want 409", w.Code)
	}
	// Not a compaction: the command word must be the first word of the message.
	t.Run("a message that only mentions /compact", func(t *testing.T) {
		other := newAutoCompactDM(t, `{}`, `{}`)
		sendDirectChat(t, context.Background(), other.agentID, other.sessionID, "please /compact later")
		if n := len(other.compactionTasks(t)); n != 0 {
			t.Fatalf("a message that only mentions /compact created %d compaction tasks", n)
		}
	})
}

func TestClaimedAutoCompactionRunsTheCompactPromptWithoutUserInput(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	dm := newAutoCompactDM(t, `{"compact_prompt":"Keep exact candidate SHAs."}`, `{}`)
	dm.complete(t, dm.sendRunning(t, "hello"), 170_000, 200_000)
	tasks := dm.compactionTasks(t)
	if len(tasks) != 1 {
		t.Fatalf("%d compaction tasks, want 1", len(tasks))
	}

	request := withURLParam(newDaemonTokenRequest(http.MethodPost, "/api/daemon/runtimes/"+dm.runtimeID+"/claim", nil, testWorkspaceID, dm.daemonID), "runtimeId", dm.runtimeID)
	var response struct {
		Task *AgentTaskResponse `json:"task"`
	}
	testutil.Call(t, testHandler.ClaimTaskByRuntime, request).Want(http.StatusOK).JSON(&response)
	if response.Task == nil || response.Task.ID != tasks[0].ID {
		t.Fatalf("claim = %+v, want the compaction task %s", response.Task, tasks[0].ID)
	}
	for _, required := range []string{"multica chat continue --summary-file", "Keep exact candidate SHAs.", "Do not invent facts"} {
		if !strings.Contains(response.Task.ChatMessage, required) {
			t.Errorf("prompt does not contain %q: %s", required, response.Task.ChatMessage)
		}
	}
	var status string
	dbfx.QueryRow(t, `SELECT status FROM agent_task_queue WHERE id = $1`, tasks[0].ID).Scan(&status)
	if status == "cancelled" {
		t.Fatal("the claim cancelled the compaction task for having no user input")
	}
}

func TestAutoCompactReasonLeadsTheNewDMSummary(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	dm := newAutoCompactDM(t, `{}`, `{}`)
	dm.complete(t, dm.sendRunning(t, "hello"), 164_000, 200_000)
	tasks := dm.compactionTasks(t)
	if len(tasks) != 1 {
		t.Fatalf("%d compaction tasks, want 1", len(tasks))
	}
	dbfx.Exec(t, `UPDATE agent_task_queue SET status = 'running', started_at = now() WHERE id = $1`, tasks[0].ID)

	response := continueChatRequest(t, dm.sessionID, "Retained goal.", tasks[0].ID)
	if response.Code != http.StatusCreated {
		t.Fatalf("continue: %d %s", response.Code, response.Body.String())
	}
	var result ChatContinueResponse
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	dbfx.Cleanup(t, `DELETE FROM chat_message WHERE chat_session_id = $1`, result.Session.ID)
	dbfx.Cleanup(t, `DELETE FROM chat_session WHERE id = $1`, result.Session.ID)

	var summary string
	dbfx.QueryRow(t, `SELECT content FROM chat_message WHERE chat_session_id = $1 AND message_kind = 'continuation_summary'`, result.Session.ID).Scan(&summary)
	wantFirstLine := "Auto-compacted: context 164k of 200k tokens (82%) reached the 80% threshold."
	if first, _, _ := strings.Cut(summary, "\n"); first != wantFirstLine {
		t.Fatalf("first line of the new DM summary = %q, want %q", first, wantFirstLine)
	}
	if !strings.Contains(summary, "Retained goal.") {
		t.Fatalf("the agent's summary is missing from %q", summary)
	}

	// The compaction succeeded: finishing it must not post a failure notice.
	dm.complete(t, tasks[0].ID, 0, 0)
	for _, content := range dm.messages(t, "compact_notice") {
		if strings.Contains(content, "failed") {
			t.Fatalf("a successful compaction posted %q", content)
		}
	}
}

func TestAutoCompactFailureNoticeOnEveryTerminalPath(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	for _, tc := range []struct {
		name, status string
		finish       func(t *testing.T, dm autoCompactDM, taskID string)
	}{
		{"completed without continue", "completed", func(t *testing.T, dm autoCompactDM, id string) { dm.complete(t, id, 0, 0) }},
		{"failed", "failed", func(t *testing.T, dm autoCompactDM, id string) { dm.fail(t, id) }},
		{"cancelled", "cancelled", func(t *testing.T, dm autoCompactDM, id string) {
			if _, err := testHandler.TaskService.CancelTask(context.Background(), parseUUID(id)); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dm := newAutoCompactDM(t, `{}`, `{}`)
			dm.complete(t, dm.sendRunning(t, "hello"), 170_000, 200_000)
			tasks := dm.compactionTasks(t)
			if len(tasks) != 1 {
				t.Fatalf("%d compaction tasks, want 1", len(tasks))
			}
			dbfx.Exec(t, `UPDATE agent_task_queue SET status = 'running', started_at = now() WHERE id = $1`, tasks[0].ID)
			tc.finish(t, dm, tasks[0].ID)

			want := fmt.Sprintf("Auto-compact failed (%s). Type /compact to retry.", tc.status)
			var found bool
			for _, content := range dm.messages(t, "compact_notice") {
				found = found || content == want
			}
			if !found {
				t.Fatalf("compact_notice rows = %q, want one %q", dm.messages(t, "compact_notice"), want)
			}
		})
	}

	t.Run("a failed manual compaction posts no notice", func(t *testing.T) {
		dm := newAutoCompactDM(t, `{}`, `{}`)
		manual := sendDirectChat(t, context.Background(), dm.agentID, dm.sessionID, "/compact")
		markTaskRunning(t, context.Background(), manual)
		dm.fail(t, manual)
		if notices := dm.messages(t, "compact_notice"); len(notices) != 0 {
			t.Fatalf("a manual compaction posted %q", notices)
		}
	})
}

func TestAutoCompactDoesNotLoopAfterAFailureUntilTheContextGrows(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	dm := newAutoCompactDM(t, `{}`, `{}`)
	dm.complete(t, dm.sendRunning(t, "one"), 170_000, 200_000)
	failed := dm.compactionTasks(t)[0]
	dbfx.Exec(t, `UPDATE agent_task_queue SET status = 'running', started_at = now() WHERE id = $1`, failed.ID)
	dm.fail(t, failed.ID)

	// 1.1 x 170000 = 187000: below it nothing re-arms, at it the next run does.
	dm.complete(t, dm.sendRunning(t, "two"), 186_999, 200_000)
	if n := len(dm.compactionTasks(t)); n != 1 {
		t.Fatalf("%d compaction tasks below the re-arm point, want 1", n)
	}
	dm.complete(t, dm.sendRunning(t, "three"), 187_000, 200_000)
	if n := len(dm.compactionTasks(t)); n != 2 {
		t.Fatalf("%d compaction tasks at the re-arm point, want 2", n)
	}
}

func TestFailedManualCompactionDoesNotSuppressAutoCompact(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	dm := newAutoCompactDM(t, `{}`, `{}`)
	manual := sendDirectChat(t, context.Background(), dm.agentID, dm.sessionID, "/compact")
	markTaskRunning(t, context.Background(), manual)
	dm.fail(t, manual)
	dm.complete(t, dm.sendRunning(t, "next"), 170_000, 200_000)
	if n := len(dm.compactionTasks(t)); n != 2 {
		t.Fatalf("%d compaction tasks, want the failed manual one plus an automatic one", n)
	}
}

func TestChatSessionReportsCompacting(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	dm := newAutoCompactDM(t, `{}`, `{}`)
	compacting := func() bool {
		t.Helper()
		request := withChatTestWorkspaceCtx(t, withURLParam(newRequest(http.MethodGet, "/api/chat/sessions/"+dm.sessionID, nil), "sessionId", dm.sessionID))
		w := httptest.NewRecorder()
		testHandler.GetChatSession(w, request)
		var session struct {
			Compacting bool `json:"compacting"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &session); err != nil {
			t.Fatalf("decode %s: %v", w.Body.String(), err)
		}
		if !strings.Contains(w.Body.String(), `"compacting"`) {
			t.Fatalf("session response has no compacting field: %s", w.Body.String())
		}
		return session.Compacting
	}
	if compacting() {
		t.Fatal("a fresh DM reports compacting")
	}
	dm.complete(t, dm.sendRunning(t, "hello"), 170_000, 200_000)
	if !compacting() {
		t.Fatal("a DM with a queued compaction does not report compacting")
	}
	tasks := dm.compactionTasks(t)
	dbfx.Exec(t, `UPDATE agent_task_queue SET status = 'cancelled', completed_at = now() WHERE id = $1`, tasks[0].ID)
	if compacting() {
		t.Fatal("a DM whose compaction ended still reports compacting")
	}
}
