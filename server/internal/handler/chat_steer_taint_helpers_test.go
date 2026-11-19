package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/multica-ai/multica/server/internal/daemon"
)

// The taint tests (CHE-3702 round 20) need a chat turn that has been steered
// and a daemon client that claims the steer. The fork's own steer fixtures went
// with the fork steer code (CHE-7278 steer pick: upstream wins); these build the
// same shape on upstream's chat_task_supplement path.

type chatSteerFixture struct {
	sessionID, runningID, messageID string
}

// steeredFixture starts a negotiated chat turn and steers one message into it.
func steeredFixture(t *testing.T) chatSteerFixture {
	t.Helper()
	sessionID, taskID, _ := newChatSupplementFixture(t, true)
	sent := sendChatGuidance(t, sessionID, map[string]any{
		"steer": true, "content": "steer me", "submission_id": uuid.NewString(),
	})
	if !sent.Steered || sent.TaskID != taskID {
		t.Fatalf("steer not accepted: %+v", sent)
	}
	return chatSteerFixture{sessionID: sessionID, runningID: taskID, messageID: sent.MessageID}
}

// newChatSupplementDaemonClient serves the real claim and ack handlers to the
// real daemon client, authenticated as the runtime owner.
func newChatSupplementDaemonClient(t *testing.T) *daemon.Client {
	t.Helper()
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			req.Header.Set("X-User-ID", testUserID)
			req.Header.Set("X-Workspace-ID", testWorkspaceID)
			next.ServeHTTP(w, req)
		})
	})
	r.Post("/api/daemon/tasks/{taskId}/supplements/claim", testHandler.ClaimTaskSupplement)
	r.Post("/api/daemon/tasks/{taskId}/supplements/{commentId}/ack", testHandler.AckTaskSupplement)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return daemon.NewClient(srv.URL)
}

func claimSteer(t *testing.T, client *daemon.Client, f chatSteerFixture) *daemon.TaskSupplement {
	t.Helper()
	sup, err := client.ClaimTaskSupplement(context.Background(), f.runningID)
	if err != nil || sup == nil {
		t.Fatalf("claim = %+v, %v", sup, err)
	}
	return sup
}
