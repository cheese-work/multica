package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

const proseCell = "| Token: input, output, cache read, cache write, chi phí | Có. |"

// Built from parts so the literal never sits in source as a token.
func proseGitHubToken() string {
	return "ghp_" + strings.Repeat("a1B2c3D4e5", 3) + "a1B2c3"
}

func requireProseKept(t *testing.T, surface, got string) {
	t.Helper()
	if !strings.Contains(got, proseCell) {
		t.Errorf("%s lost its keyword label: %q", surface, got)
	}
	if strings.Contains(got, proseGitHubToken()) {
		t.Errorf("%s leaked the provider token: %q", surface, got)
	}
}

func TestCompleteChatTaskKeepsProseLabelsAndMasksTokens(t *testing.T) {
	if testPool == nil {
		t.Skip("test database not available")
	}
	agentID := createHandlerTestAgent(t, "ProseRedactionAgent", []byte("[]"))
	sessionID := createHandlerTestChatSession(t, agentID)
	taskID := seedRunningChatTask(t, agentID, sessionID)

	output := proseCell + " " + proseGitHubToken()
	task, err := testHandler.TaskService.CompleteTask(context.Background(),
		parseUUID(taskID), []byte(mustJSON(t, map[string]any{"output": output})), "", "", "", false, "", "")
	if err != nil {
		t.Fatalf("CompleteTask: %v", err)
	}
	_, content, ok := assistantMessageForTask(t, taskID)
	if !ok {
		t.Fatal("expected an assistant message")
	}
	requireProseKept(t, "chat reply", content)

	// The suggestion pass is prose too: a label like "Rotate token: X" survives,
	// a provider token does not.
	raw, _ := json.Marshal(map[string]any{"actions": []map[string]any{
		{"label": "Token: input", "prompt": "explain " + proseGitHubToken()},
	}})
	if err := testHandler.TaskService.SupplementChatQuickActions(context.Background(), *task, string(raw), false); err != nil {
		t.Fatalf("SupplementChatQuickActions: %v", err)
	}
	var stored []byte
	if err := testPool.QueryRow(context.Background(),
		`SELECT quick_actions FROM chat_message WHERE task_id = $1 AND role = 'assistant'`, taskID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	var actions []protocol.ChatQuickAction
	if err := json.Unmarshal(stored, &actions); err != nil || len(actions) != 1 {
		t.Fatalf("decode quick actions %s: %v", stored, err)
	}
	if actions[0].Label != "Token: input" {
		t.Errorf("quick-action label mangled: %q", actions[0].Label)
	}
	if strings.Contains(actions[0].Prompt, proseGitHubToken()) {
		t.Errorf("quick-action prompt leaked the provider token: %q", actions[0].Prompt)
	}
}

func TestCompleteTaskFallbackCommentKeepsProseLabelsAndMasksTokens(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	var agentID, runtimeID string
	dbfx.QueryRow(t, `SELECT a.id, a.runtime_id FROM agent a WHERE a.workspace_id = $1 LIMIT 1`, testWorkspaceID).Scan(&agentID, &runtimeID)
	issueID := dbfx.Issue(t, "prose redaction fallback", testutil.Cols{"status": "in_progress"})
	triggerCommentID := dbfx.Comment(t, issueID, "please report")
	taskID := dbfx.Task(t, agentID, testutil.Cols{
		"runtime_id": runtimeID, "issue_id": issueID, "trigger_comment_id": triggerCommentID,
		"status": "running", "started_at": testutil.Raw("now()"),
	})

	w := httptest.NewRecorder()
	req := newOwnerDaemonRequest("POST", "/api/daemon/tasks/"+taskID+"/complete",
		map[string]any{"output": proseCell + " " + proseGitHubToken()})
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("taskId", taskID)
	testHandler.CompleteTask(w, req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx)))
	if w.Code != http.StatusOK {
		t.Fatalf("CompleteTask: %d %s", w.Code, w.Body.String())
	}
	var content string
	dbfx.QueryRow(t, `SELECT content FROM comment WHERE issue_id = $1 AND author_type = 'agent' AND author_id = $2`, issueID, agentID).Scan(&content)
	requireProseKept(t, "synthesized fallback comment", content)
}

func TestCompactSummaryContentKeepsProseLabelsAndMasksTokens(t *testing.T) {
	requireProseKept(t, "continued-DM summary",
		compactSummaryContent("", proseCell+" "+proseGitHubToken(), "https://example.test/chat/1"))
}
