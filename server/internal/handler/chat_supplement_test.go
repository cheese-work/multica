package handler

import (
	"context"
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// chatSteerFixture is a DM session with a head turn and one queued follow-up,
// both created through the real direct-send path.
type chatSteerFixture struct {
	agentID, sessionID, runtimeID, daemonID string
	runningID, queuedID, messageID          string
}

type chatErrorBody struct {
	Code string `json:"code"`
}

// newChatSteerFixture builds the head turn in headState:
//   - capable: started by a daemon that offered task-supplement-chat-v1
//   - unsupported: started by an old daemon (task-supplement-v1 only)
//   - starting: dispatched, not started
//   - ended: started, then completed after the follow-up was sent
//   - none: no head turn; the follow-up is the queue head
func newChatSteerFixture(t *testing.T, headState string) chatSteerFixture {
	t.Helper()
	if testHandler == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	var f chatSteerFixture
	f.agentID, f.sessionID, f.runtimeID, f.daemonID = setupDirectChatSession(t, ctx, "steer "+headState)
	dbfx.Exec(t, `UPDATE agent_runtime SET provider = 'codex' WHERE id = $1`, f.runtimeID)
	dbfx.Cleanup(t, `DELETE FROM chat_message WHERE chat_session_id = $1`, f.sessionID)
	dbfx.Cleanup(t, `DELETE FROM agent_task_queue WHERE chat_session_id = $1`, f.sessionID)
	dbfx.Cleanup(t, `DELETE FROM task_supplement_capability WHERE chat_session_id = $1`, f.sessionID)
	dbfx.Cleanup(t, `DELETE FROM chat_task_supplement WHERE chat_session_id = $1`, f.sessionID)
	if headState != "none" {
		f.runningID = sendDirectChat(t, ctx, f.agentID, f.sessionID, "first turn")
		dbfx.Exec(t, `UPDATE agent_task_queue SET status = 'dispatched', dispatched_at = now() WHERE id = $1`, f.runningID)
		switch headState {
		case "capable", "ended":
			startChatTask(t, f.runningID, protocol.DaemonCapabilityTaskSupplementV1, protocol.DaemonCapabilityTaskSupplementChatV1)
		case "unsupported":
			startChatTask(t, f.runningID, protocol.DaemonCapabilityTaskSupplementV1)
		}
	}
	f.queuedID = sendDirectChat(t, ctx, f.agentID, f.sessionID, "steer me")
	if headState == "ended" {
		dbfx.Exec(t, `UPDATE agent_task_queue SET status = 'completed', completed_at = now() WHERE id = $1`, f.runningID)
	}
	dbfx.QueryRow(t, `SELECT id FROM chat_message WHERE task_id = $1 AND role = 'user'`, f.queuedID).Scan(&f.messageID)
	return f
}

func startChatTask(t *testing.T, taskID string, capabilities ...string) AgentTaskResponse {
	t.Helper()
	req := withURLParam(newRequest(http.MethodPost, "/start", map[string]any{"capabilities": capabilities}), "taskId", taskID)
	var started AgentTaskResponse
	testutil.Call(t, testHandler.StartTask, req).Want(http.StatusOK).JSON(&started)
	return started
}

func steerChat(t *testing.T, sessionID, taskID, userID string) *testutil.Response {
	t.Helper()
	req := newRequestAs(userID, http.MethodPost, "/api/chat/sessions/"+sessionID+"/tasks/"+taskID+"/steer", nil)
	req = withURLParams(req, "sessionId", sessionID, "taskId", taskID)
	return testutil.Call(t, testHandler.SteerChatTask, chatPendingCtxAs(t, req, userID))
}

func chatTaskStatus(t *testing.T, taskID string) string {
	t.Helper()
	var status string
	dbfx.QueryRow(t, `SELECT status FROM agent_task_queue WHERE id = $1`, taskID).Scan(&status)
	return status
}

func TestChatSupplementStartNegotiation(t *testing.T) {
	t.Run("capable", func(t *testing.T) {
		f := newChatSteerFixture(t, "starting")
		started := startChatTask(t, f.runningID, protocol.DaemonCapabilityTaskSupplementV1, protocol.DaemonCapabilityTaskSupplementChatV1)
		if started.Status != "running" || started.SupplementCapability != protocol.DaemonCapabilityTaskSupplementChatV1 {
			t.Fatalf("chat start = status %q capability %q", started.Status, started.SupplementCapability)
		}
		if n := dbfx.Count(t, `
			SELECT count(*) FROM task_supplement_capability
			WHERE task_id = $1 AND issue_id IS NULL AND chat_session_id = $2 AND capability = $3
		`, f.runningID, f.sessionID, protocol.DaemonCapabilityTaskSupplementChatV1); n != 1 {
			t.Fatalf("chat capability rows = %d, want 1", n)
		}
	})
	t.Run("old daemon", func(t *testing.T) {
		f := newChatSteerFixture(t, "starting")
		if started := startChatTask(t, f.runningID, protocol.DaemonCapabilityTaskSupplementV1); started.SupplementCapability != "" {
			t.Fatalf("old daemon negotiated %q", started.SupplementCapability)
		}
		if n := dbfx.Count(t, `SELECT count(*) FROM task_supplement_capability WHERE task_id = $1`, f.runningID); n != 0 {
			t.Fatalf("old daemon wrote %d capability rows", n)
		}
	})
	t.Run("compaction run", func(t *testing.T) {
		f := newChatSteerFixture(t, "starting")
		dbfx.Exec(t, `UPDATE agent_task_queue SET task_kind = 'compact' WHERE id = $1`, f.runningID)
		startChatTask(t, f.runningID, protocol.DaemonCapabilityTaskSupplementV1, protocol.DaemonCapabilityTaskSupplementChatV1)
		if n := dbfx.Count(t, `SELECT count(*) FROM task_supplement_capability WHERE task_id = $1`, f.runningID); n != 0 {
			t.Fatalf("compaction run wrote %d capability rows", n)
		}
	})
	t.Run("unsupported provider", func(t *testing.T) {
		f := newChatSteerFixture(t, "starting")
		dbfx.Exec(t, `UPDATE agent_runtime SET provider = 'kimi' WHERE id = $1`, f.runtimeID)
		startChatTask(t, f.runningID, protocol.DaemonCapabilityTaskSupplementV1, protocol.DaemonCapabilityTaskSupplementChatV1)
		if n := dbfx.Count(t, `SELECT count(*) FROM task_supplement_capability WHERE task_id = $1`, f.runningID); n != 0 {
			t.Fatalf("unsupported provider wrote %d capability rows", n)
		}
	})
}

func TestSteerChatTaskAddsMessageToRunningTask(t *testing.T) {
	f := newChatSteerFixture(t, "capable")

	var receipt ChatSupplementResponse
	steerChat(t, f.sessionID, f.queuedID, testUserID).Want(http.StatusOK).JSON(&receipt)
	if receipt.Status != "pending" || receipt.TaskID != f.runningID || receipt.MessageID != f.messageID || receipt.ChatSessionID != f.sessionID {
		t.Fatalf("receipt = %+v", receipt)
	}
	if got := chatTaskStatus(t, f.runningID); got != "running" {
		t.Fatalf("running task status = %q", got)
	}
	if got := chatTaskStatus(t, f.queuedID); got != "cancelled" {
		t.Fatalf("queued follow-up status = %q, want cancelled", got)
	}
	var owner string
	dbfx.QueryRow(t, `SELECT task_id FROM chat_message WHERE id = $1`, f.messageID).Scan(&owner)
	if owner != f.queuedID {
		t.Fatalf("steered message owner = %q, want the cancelled follow-up %q", owner, f.queuedID)
	}

	var replay ChatSupplementResponse
	steerChat(t, f.sessionID, f.queuedID, testUserID).Want(http.StatusOK).JSON(&replay)
	if replay.TaskID != receipt.TaskID || replay.Status != "pending" {
		t.Fatalf("replayed receipt = %+v", replay)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM chat_task_supplement WHERE message_id = $1`, f.messageID); n != 1 {
		t.Fatalf("receipts = %d, want 1", n)
	}

	// The transcript lists the steered message once, in send order, with its receipt.
	req := withURLParam(newRequest(http.MethodGet, "/api/chat/sessions/"+f.sessionID+"/messages", nil), "sessionId", f.sessionID)
	var messages []ChatMessageResponse
	testutil.Call(t, testHandler.ListChatMessages, chatPendingCtxAs(t, req, testUserID)).Want(http.StatusOK).JSON(&messages)
	var contents []string
	var steered *ChatMessageResponse
	for i, m := range messages {
		contents = append(contents, m.Content)
		if m.ID == f.messageID {
			steered = &messages[i]
		}
	}
	if len(messages) != 2 || contents[0] != "first turn" || contents[1] != "steer me" {
		t.Fatalf("transcript = %q", contents)
	}
	if steered == nil || steered.Supplement == nil || steered.Supplement.Status != "pending" || steered.Supplement.TaskID != f.runningID {
		t.Fatalf("steered message supplement = %+v", steered)
	}
}

func TestSteerChatTaskRefusalsLeaveFollowUpQueued(t *testing.T) {
	for _, tc := range []struct{ head, code string }{
		{"unsupported", "task_supplement_unsupported"},
		{"starting", "task_supplement_starting"},
		{"ended", "task_supplement_turn_ended"},
		{"none", "no_active_run"},
	} {
		t.Run(tc.head, func(t *testing.T) {
			f := newChatSteerFixture(t, tc.head)
			var body chatErrorBody
			steerChat(t, f.sessionID, f.queuedID, testUserID).Want(http.StatusConflict).JSON(&body)
			if body.Code != tc.code {
				t.Fatalf("code = %q, want %q", body.Code, tc.code)
			}
			if got := chatTaskStatus(t, f.queuedID); got != "queued" {
				t.Fatalf("follow-up status = %q, want queued", got)
			}
			if n := dbfx.Count(t, `SELECT count(*) FROM chat_task_supplement WHERE chat_session_id = $1`, f.sessionID); n != 0 {
				t.Fatalf("refused steer wrote %d receipts", n)
			}
		})
	}
}

func TestSteerChatTaskAccess(t *testing.T) {
	f := newChatSteerFixture(t, "capable")
	privateAgentID, _, memberID := privateAgentTestFixture(t)

	// The existing creator gate answers a non-creator with 403.
	steerChat(t, f.sessionID, f.queuedID, memberID).Want(http.StatusForbidden)
	if got := chatTaskStatus(t, f.queuedID); got != "queued" {
		t.Fatalf("non-creator steer changed the follow-up to %q", got)
	}

	// The member created this session but has lost access to the private agent.
	sessionID := insertChatSessionAs(t, privateAgentID, memberID)
	dbfx.Cleanup(t, `DELETE FROM chat_task_supplement WHERE chat_session_id = $1`, sessionID)
	runningID := insertPendingChatTask(t, privateAgentID, sessionID, "running")
	dbfx.Exec(t, `
		INSERT INTO task_supplement_capability (task_id, workspace_id, chat_session_id, capability)
		VALUES ($1, $2, $3, $4)
	`, runningID, testWorkspaceID, sessionID, protocol.DaemonCapabilityTaskSupplementChatV1)
	dbfx.Cleanup(t, `DELETE FROM task_supplement_capability WHERE task_id = $1`, runningID)
	queuedID, _, _ := insertQueuedChatInputWithAttachment(t, privateAgentID, sessionID, "private steer")
	steerChat(t, sessionID, queuedID, memberID).Want(http.StatusForbidden)
	if got := chatTaskStatus(t, queuedID); got != "queued" {
		t.Fatalf("private-agent steer changed the follow-up to %q", got)
	}
}

func TestGetPendingChatTaskReportsHeadSupplement(t *testing.T) {
	for _, tc := range []struct{ head, want string }{
		{"capable", HeadSupplementCapable},
		{"unsupported", HeadSupplementUnsupported},
		{"starting", HeadSupplementStarting},
		{"none", HeadSupplementStarting},
	} {
		t.Run(tc.head, func(t *testing.T) {
			f := newChatSteerFixture(t, tc.head)
			req := withURLParam(newRequest(http.MethodGet, "/api/chat/sessions/"+f.sessionID+"/pending-task", nil), "sessionId", f.sessionID)
			var resp PendingChatTaskResponse
			testutil.Call(t, testHandler.GetPendingChatTask, chatPendingCtxAs(t, req, testUserID)).Want(http.StatusOK).JSON(&resp)
			if resp.HeadSupplement != tc.want {
				t.Fatalf("head_supplement = %q, want %q", resp.HeadSupplement, tc.want)
			}
		})
	}
}
