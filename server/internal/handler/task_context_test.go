package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
)

// The optional top-level `context: {tokens, window}` object on /complete lands
// in agent_task_queue.context_tokens / context_window. Anything the server
// cannot trust leaves both NULL and never fails the completion.
func TestCompleteTaskStoresOptionalContext(t *testing.T) {
	known := func(n int64) pgtype.Int8 { return pgtype.Int8{Int64: n, Valid: true} }
	for _, tc := range []struct {
		name       string
		body       map[string]any
		wantTokens pgtype.Int8
		wantWindow pgtype.Int8
	}{
		{"present", map[string]any{"output": "ok", "context": map[string]any{"tokens": 164000, "window": 200000}}, known(164000), known(200000)},
		{"absent_from_old_daemon", map[string]any{"output": "ok"}, pgtype.Int8{}, pgtype.Int8{}},
		{"zero_window", map[string]any{"output": "ok", "context": map[string]any{"tokens": 164000, "window": 0}}, pgtype.Int8{}, pgtype.Int8{}},
		{"negative_tokens", map[string]any{"output": "ok", "context": map[string]any{"tokens": -1, "window": 200000}}, pgtype.Int8{}, pgtype.Int8{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, taskID := seedNULTask(t, "ctx-"+tc.name)

			w := httptest.NewRecorder()
			testHandler.CompleteTask(w, daemonTaskRequest(t, "/api/daemon/tasks/"+taskID+"/complete", taskID, tc.body))
			if w.Code != http.StatusOK {
				t.Fatalf("CompleteTask returned %d, want 200: %s", w.Code, w.Body.String())
			}

			var tokens, window pgtype.Int8
			if err := testPool.QueryRow(context.Background(),
				`SELECT context_tokens, context_window FROM agent_task_queue WHERE id = $1`, taskID,
			).Scan(&tokens, &window); err != nil {
				t.Fatalf("read context columns: %v", err)
			}
			if tokens != tc.wantTokens || window != tc.wantWindow {
				t.Fatalf("context = %+v of %+v, want %+v of %+v", tokens, window, tc.wantTokens, tc.wantWindow)
			}
		})
	}
}
