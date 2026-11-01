package handler

import (
	"net/http"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

func silenceAlertRequest(t *testing.T, taskID string, body map[string]any) *http.Request {
	t.Helper()
	req := testutil.JSONRequest(http.MethodPost,
		"/api/daemon/tasks/"+taskID+"/silence-alert", body)
	req = testutil.WithURLParams(req, "taskId", taskID)
	req.Header.Set("X-User-ID", testUserID)
	return req
}

func countSystemComments(t *testing.T, taskID string) int {
	t.Helper()
	var n int
	dbfx.QueryRow(t, `SELECT count(*) FROM comment WHERE author_type='system' AND source_task_id=$1`, taskID).Scan(&n)
	return n
}

func TestSilenceAlertIsIdempotentPerSegment(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	taskID := seedBatchTask(t, "silence-idem")
	body := map[string]any{"segment_id": "seg-1", "silent_for_seconds": 300}

	testutil.Call(t, testHandler.ReportSilenceAlert, silenceAlertRequest(t, taskID, body)).Want(http.StatusOK)
	if got := countSystemComments(t, taskID); got != 1 {
		t.Fatalf("after first alert: %d system comments, want 1", got)
	}
	testutil.Call(t, testHandler.ReportSilenceAlert, silenceAlertRequest(t, taskID, body)).Want(http.StatusOK)
	if got := countSystemComments(t, taskID); got != 1 {
		t.Fatalf("after replay: %d system comments, want 1", got)
	}
	// A different segment is a new silence and alerts again.
	body["segment_id"] = "seg-2"
	testutil.Call(t, testHandler.ReportSilenceAlert, silenceAlertRequest(t, taskID, body)).Want(http.StatusOK)
	if got := countSystemComments(t, taskID); got != 2 {
		t.Fatalf("after new segment: %d system comments, want 2", got)
	}
}

func TestSilenceAlertRedactsText(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	taskID := seedBatchTask(t, "silence-redact")
	// Built from parts so the literal never sits in source as a token.
	ghToken := "ghp_" + strings.Repeat("a1B2c3D4e5", 3) + "a1B2c3"

	testutil.Call(t, testHandler.ReportSilenceAlert, silenceAlertRequest(t, taskID, map[string]any{
		"segment_id": "seg-" + ghToken, "silent_for_seconds": 120,
	})).Want(http.StatusOK)

	var content string
	dbfx.QueryRow(t, `SELECT content FROM comment WHERE author_type='system' AND source_task_id=$1`, taskID).Scan(&content)
	if content == "" {
		t.Fatal("no comment stored")
	}
	if strings.Contains(content, ghToken) {
		t.Errorf("comment leaked token: %q", content)
	}
}

func TestSilenceAlertSkipsChatAndFinishedTasks(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	body := map[string]any{"segment_id": "seg-1", "silent_for_seconds": 300}

	done := seedBatchTask(t, "silence-done")
	dbfx.Exec(t, `UPDATE agent_task_queue SET status='completed', completed_at=now() WHERE id=$1`, done)
	testutil.Call(t, testHandler.ReportSilenceAlert, silenceAlertRequest(t, done, body)).Want(http.StatusOK)
	if got := countSystemComments(t, done); got != 0 {
		t.Errorf("finished task got %d comments, want 0", got)
	}

	agentID := dbfx.Agent(t, "silence-chat agent", handlerTestRuntimeID(t), testutil.Cols{
		"instructions": "",
		"custom_env":   testutil.Raw("'{}'::jsonb"),
		"custom_args":  testutil.Raw("'[]'::jsonb"),
	})
	chatTask := dbfx.Task(t, agentID, testutil.Cols{
		"runtime_id":      handlerTestRuntimeID(t),
		"chat_session_id": dbfx.ChatSession(t, agentID),
		"status":          "running",
		"started_at":      testutil.Raw("now()"),
	})
	testutil.Call(t, testHandler.ReportSilenceAlert, silenceAlertRequest(t, chatTask, body)).Want(http.StatusOK)
	if got := countSystemComments(t, chatTask); got != 0 {
		t.Errorf("chat task got %d comments, want 0", got)
	}
}

func TestReportTaskMessagesStoresSilenceMarkers(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	taskID := seedBatchTask(t, "silence-markers")
	testutil.Call(t, testHandler.ReportTaskMessages, batchMessagesRequest(t, taskID, []any{
		map[string]any{"seq": 1, "type": "silence_warn", "content": "no output for 120s"},
		map[string]any{"seq": 2, "type": "tool_stalled", "tool": "Bash", "content": "Bash running for 300s"},
	})).Want(http.StatusOK)

	for seq, want := range map[int]string{1: "silence_warn", 2: "tool_stalled"} {
		var typ, content string
		dbfx.QueryRow(t, `SELECT type, COALESCE(content,'') FROM task_message WHERE task_id=$1 AND seq=$2`, taskID, seq).Scan(&typ, &content)
		if typ != want || content == "" {
			t.Errorf("seq %d stored as %q/%q, want type %q with content", seq, typ, content, want)
		}
	}
}
