package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// CHE-1793 (N2): a compaction started by the agent's own chat task
// (X-Actor-Source: task_token) happens in place. The agent-only summary never
// notifies, and neither does a continue the DM creator submits.

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

func TestAgentRolloverSummaryDoesNotNotify(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	resetDMInbox(t)
	agentID, sessionID, _, _ := setupDirectChatSession(t, context.Background(), "rollover")
	taskID := runningChatTask(t, agentID, sessionID)

	if response := continueAsAgentTask(t, testHandler, sessionID, taskID, agentID); response.Code != http.StatusOK {
		t.Fatalf("continue: %d %s", response.Code, response.Body.String())
	}
	if records := dmRecords(t); len(records) != 0 {
		t.Fatalf("the summary must not notify, only the run's final reply does: %+v", records)
	}
}

func TestCreatorContinueDoesNotNotify(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	resetDMInbox(t)
	agentID, sessionID, _, _ := setupDirectChatSession(t, context.Background(), "creator continue")

	if response := continueChatRequest(t, sessionID, "Goal: keep the context.", ""); response.Code != http.StatusOK {
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
