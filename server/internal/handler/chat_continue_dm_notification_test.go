package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// CHE-1793 (N2): a Long-DM rollover started by the agent's own chat task
// (X-Actor-Source: task_token) archives the DM the run was replying in, so its
// reply cannot notify. The summary written into the successor is the agent's
// message and must notify there. A continue the DM creator submits must not.

func continueAsAgentTask(t *testing.T, h *Handler, sessionID, taskID, agentID string) *httptest.ResponseRecorder {
	t.Helper()
	req := newRequest(http.MethodPost, "/api/chat/sessions/"+sessionID+"/continue", map[string]any{"summary": "Goal: keep the context."})
	req = withChatTestWorkspaceCtx(t, withURLParam(req, "sessionId", sessionID))
	req.Header.Set("X-Actor-Source", "task_token")
	req.Header.Set("X-Task-ID", taskID)
	req.Header.Set("X-Agent-ID", agentID)
	response := httptest.NewRecorder()
	h.ContinueChatSession(response, req)
	return response
}

func runningChatTask(t *testing.T, agentID, sessionID string) string {
	t.Helper()
	ctx := context.Background()
	taskID := sendDirectChat(t, ctx, agentID, sessionID, "ping")
	markTaskRunning(t, ctx, taskID)
	return taskID
}

func cleanupSuccessor(t *testing.T, sourceID string) {
	t.Helper()
	t.Cleanup(func() {
		ctx := context.Background()
		testPool.Exec(ctx, `DELETE FROM chat_message WHERE chat_session_id IN (SELECT continued_into_session_id FROM chat_session WHERE id = $1)`, sourceID)
		testPool.Exec(ctx, `DELETE FROM chat_session WHERE continued_from_session_id = $1`, sourceID)
	})
}

func TestAgentRolloverNotifiesInTheNewDM(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	resetDMInbox(t)
	agentID, sessionID, _, _ := setupDirectChatSession(t, context.Background(), "rollover")
	cleanupSuccessor(t, sessionID)
	taskID := runningChatTask(t, agentID, sessionID)

	var result ChatContinueResponse
	response := continueAsAgentTask(t, testHandler, sessionID, taskID, agentID)
	if response.Code != http.StatusCreated {
		t.Fatalf("continue: %d %s", response.Code, response.Body.String())
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}

	records := dmRecords(t)
	if len(records) != 1 {
		t.Fatalf("dm_reply records = %d, want 1 for the new DM: %+v", len(records), records)
	}
	record := records[0]
	if record.Read || record.ChatSessionID == nil || *record.ChatSessionID != result.Session.ID {
		t.Fatalf("record must be unread and link the new DM %s: %+v", result.Session.ID, record)
	}
	if record.Body != nil {
		t.Fatalf("record body must not carry the summary text: %q", *record.Body)
	}
}

func TestCreatorContinueDoesNotNotify(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	resetDMInbox(t)
	agentID, sessionID, _, _ := setupDirectChatSession(t, context.Background(), "creator continue")
	cleanupSuccessor(t, sessionID)

	if response := continueChatRequest(t, sessionID, "Goal: keep the context.", ""); response.Code != http.StatusCreated {
		t.Fatalf("continue: %d %s", response.Code, response.Body.String())
	}
	if records := dmRecords(t); len(records) != 0 {
		t.Fatalf("a continue the creator submitted must not notify: %+v", records)
	}
	// Control: the same agent's ordinary reply still notifies, so the empty
	// result above is not just a disabled feature.
	controlDM(t, agentID)
	if records := dmRecords(t); len(records) != 1 {
		t.Fatalf("control reply must notify once: %+v", records)
	}
}

type failDMRecordStarter struct{ inner txStarter }

func (starter failDMRecordStarter) Begin(ctx context.Context) (pgx.Tx, error) {
	transaction, err := starter.inner.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return failDMRecordTx{Tx: transaction}, nil
}

type failDMRecordTx struct{ pgx.Tx }

func (transaction failDMRecordTx) QueryRow(ctx context.Context, query string, arguments ...any) pgx.Row {
	if strings.Contains(query, "-- name: UpsertDMReplyInboxItem") {
		return continuationFailureRow{}
	}
	return transaction.Tx.QueryRow(ctx, query, arguments...)
}

func TestAgentRolloverNotificationFailureRollsBackTheContinuation(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	resetDMInbox(t)
	agentID, sessionID, _, _ := setupDirectChatSession(t, context.Background(), "rollover rollback")
	cleanupSuccessor(t, sessionID)
	taskID := runningChatTask(t, agentID, sessionID)

	handler := *testHandler
	handler.TxStarter = failDMRecordStarter{inner: testHandler.TxStarter}
	response := continueAsAgentTask(t, &handler, sessionID, taskID, agentID)
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("continue with a failing notification: %d, want 500 (%s)", response.Code, response.Body.String())
	}
	if count := dbfx.Count(t, `SELECT count(*) FROM chat_session WHERE continued_from_session_id = $1`, sessionID); count != 0 {
		t.Fatalf("failed continuation left %d successor sessions", count)
	}
	if count := dbfx.Count(t, `SELECT count(*) FROM chat_session WHERE id = $1 AND status = 'active' AND continued_into_session_id IS NULL`, sessionID); count != 1 {
		t.Fatal("failed continuation archived or changed the source")
	}
	if count := dmRecordCountInDB(t); count != 0 {
		t.Fatalf("failed continuation left %d dm_reply records", count)
	}
}
