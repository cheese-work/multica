package handler

import (
	"net/http"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// Option R (CHE-2587): agent prose keeps keyword labels and only loses
// provider-shaped tokens; machine output (tool input/output) still gets the
// generic keyword rule.
func TestReportTaskMessagesRedactsBySurface(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	taskID := seedBatchTask(t, "redaction-surface")
	const cell = "| Token: input, output, cache read, cache write, chi phí | Có. |"
	// Built from parts so the literal never sits in source as a token.
	ghToken := "ghp_" + strings.Repeat("a1B2c3D4e5", 3) + "a1B2c3"

	testutil.Call(t, testHandler.ReportTaskMessages, batchMessagesRequest(t, taskID, []any{
		map[string]any{"seq": 1, "type": "text", "content": cell + " " + ghToken},
		map[string]any{"seq": 2, "type": "thinking", "content": cell + " " + ghToken},
		map[string]any{"seq": 3, "type": "tool_result", "tool": "Bash", "output": "DB_PASSWORD=hunter2 " + ghToken},
		map[string]any{"seq": 4, "type": "tool_use", "tool": "Bash", "input": map[string]any{"cmd": "export DB_PASSWORD=hunter2 " + ghToken}},
		map[string]any{"seq": 5, "type": "error", "content": "failed: api_key=hunter2"},
	})).Want(http.StatusOK)

	read := func(seq int, column string) string {
		var v string
		dbfx.QueryRow(t, `SELECT COALESCE(`+column+`::text,'') FROM task_message WHERE task_id=$1 AND seq=$2`, taskID, seq).Scan(&v)
		return v
	}
	for _, seq := range []int{1, 2} {
		got := read(seq, "content")
		if !strings.Contains(got, cell) {
			t.Errorf("seq %d prose lost its keyword label: %q", seq, got)
		}
		if strings.Contains(got, ghToken) {
			t.Errorf("seq %d prose leaked the provider token: %q", seq, got)
		}
	}
	if got := read(3, "output"); strings.Contains(got, "hunter2") || strings.Contains(got, ghToken) {
		t.Errorf("tool output leaked: %q", got)
	}
	if got := read(4, "input"); strings.Contains(got, "hunter2") || strings.Contains(got, ghToken) {
		t.Errorf("tool input leaked: %q", got)
	}
	if got := read(5, "content"); strings.Contains(got, "hunter2") {
		t.Errorf("error message leaked: %q", got)
	}
}
