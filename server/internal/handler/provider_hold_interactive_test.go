package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestChatSendRefusesProviderBreakerBeforeWriting(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	seedProviderBreaker(t, "openai", "half_open")
	agentID := createHandlerTestAgent(t, "HeldChat", []byte("[]"))
	if _, err := testPool.Exec(context.Background(), `UPDATE agent SET model='gpt-5.6-sol' WHERE id=$1`, agentID); err != nil {
		t.Fatal(err)
	}
	sessionID := createHandlerTestChatSession(t, agentID)
	req := withChatTestWorkspaceCtx(t, withURLParam(newRequest("POST", "/api/chat-sessions/"+sessionID+"/messages", map[string]any{"content": "unsent prompt"}), "sessionId", sessionID))
	w := httptest.NewRecorder()
	testHandler.SendChatMessage(w, req)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "provider_breaker") {
		t.Fatalf("chat send: %d %s; want provider_breaker refusal", w.Code, w.Body.String())
	}
	var messages, tasks int
	if err := testPool.QueryRow(context.Background(), `SELECT count(*) FROM chat_message WHERE chat_session_id=$1`, sessionID).Scan(&messages); err != nil {
		t.Fatal(err)
	}
	if err := testPool.QueryRow(context.Background(), `SELECT count(*) FROM agent_task_queue WHERE chat_session_id=$1`, sessionID).Scan(&tasks); err != nil {
		t.Fatal(err)
	}
	if messages != 0 || tasks != 0 {
		t.Fatalf("refusal persisted %d messages, %d tasks", messages, tasks)
	}
}

func TestProviderHoldSettingValidation(t *testing.T) {
	for _, raw := range []string{"0", "-1", "1.5", `"bad"`, "2147483648"} {
		if err := validateSettingsPatchValue("provider_breaker_max_hold_seconds", json.RawMessage(raw)); err == nil {
			t.Errorf("accepted invalid hold deadline %s", raw)
		}
	}
	if err := validateSettingsPatchValue("provider_breaker_max_hold_seconds", json.RawMessage("120")); err != nil {
		t.Fatal(err)
	}
}
