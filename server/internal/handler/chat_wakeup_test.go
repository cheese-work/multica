package handler

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
)

func TestCreateChatWakeupRejectsOverflowingDelay(t *testing.T) {
	for name, afterSeconds := range map[string]int64{
		"just over 24h":    86401,
		"overflows 64-bit": 18446744075,
		"max int64":        1<<63 - 1,
	} {
		req := newRequest(http.MethodPost, "/api/chat/wakeups", map[string]any{"after_seconds": afterSeconds, "instruction": "refresh"})
		req.Header.Set("X-Actor-Source", "task_token")
		req.Header.Set("X-Task-ID", uuid.NewString())
		w := httptest.NewRecorder()
		testHandler.CreateChatWakeup(w, req)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400 (%s)", name, w.Code, w.Body.String())
		}
	}
}
