package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

func TestCompactWebDMSavedPrompt(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	workspace, err := testHandler.Queries.GetWorkspace(ctx, parseUUID(testWorkspaceID))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		dbfx.Exec(t, `UPDATE workspace SET settings = $1 WHERE id = $2`, workspace.Settings, testWorkspaceID)
	})
	for _, testCase := range []struct {
		name, settings, config, want string
	}{
		{"builtin", `{}`, `{}`, "goal, decisions, evidence, open items"},
		{"workspace", `{"compact_prompt":"Keep test receipts and blockers."}`, `{}`, "Keep test receipts and blockers."},
		{"agent override", `{"compact_prompt":"Workspace criteria."}`, `{"compact_prompt":"Keep exact candidate SHAs."}`, "Keep exact candidate SHAs."},
		{"empty override inherits", `{"compact_prompt":"Workspace criteria."}`, `{"compact_prompt":"  "}`, "Workspace criteria."},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			agentID, sessionID, runtimeID, daemonID := setupDirectChatSession(t, ctx, "Saved compact prompt")
			dbfx.Exec(t, `UPDATE workspace SET settings = $1 WHERE id = $2`, []byte(testCase.settings), testWorkspaceID)
			dbfx.Exec(t, `UPDATE agent SET runtime_config = $1 WHERE id = $2`, []byte(testCase.config), agentID)
			sendDirectChat(t, ctx, agentID, sessionID, "/compact retain IDs")
			request := newDaemonTokenRequest(http.MethodPost, "/api/daemon/runtimes/"+runtimeID+"/claim", nil, testWorkspaceID, daemonID)
			request = withURLParam(request, "runtimeId", runtimeID)
			var response struct {
				Task *AgentTaskResponse `json:"task"`
			}
			testutil.Call(t, testHandler.ClaimTaskByRuntime, request).Want(http.StatusOK).JSON(&response)
			if response.Task == nil {
				t.Fatal("compact task was not claimed")
			}
			for _, required := range []string{testCase.want, "retain IDs", "multica chat continue --summary-file", "Do not invent facts"} {
				if !strings.Contains(response.Task.ChatMessage, required) {
					t.Errorf("compact prompt does not contain %q: %s", required, response.Task.ChatMessage)
				}
			}
			if testCase.name == "agent override" && strings.Contains(response.Task.ChatMessage, "Workspace criteria.") {
				t.Fatal("agent override did not replace the workspace criteria")
			}
		})
	}
}

func TestCompactChatPrompt(t *testing.T) {
	for _, content := range []string{"/compact", "/compact focus on evidence", "\n /compact focus\nnext action"} {
		prompt := compactChatPrompt(content, nil, nil)
		for _, required := range []string{"multica chat continue --summary-file", "goal, decisions, evidence, open items", "archives this DM", "Do not invent facts"} {
			if !strings.Contains(prompt, required) {
				t.Errorf("prompt for %q does not contain %q", content, required)
			}
		}
	}
	if !strings.Contains(compactChatPrompt("/compact retain deployment IDs", nil, nil), "retain deployment IDs") {
		t.Fatal("focus was not preserved")
	}
	for _, content := range []string{"hello", "please /compact", "first line\n/compact", "/compacted"} {
		if prompt := compactChatPrompt(content, nil, nil); prompt != content {
			t.Errorf("literal input %q was rewritten as %q", content, prompt)
		}
	}
}

func TestSavedCompactPromptValidation(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	workspace, err := testHandler.Queries.GetWorkspace(context.Background(), parseUUID(testWorkspaceID))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		dbfx.Exec(t, `UPDATE workspace SET settings = $1 WHERE id = $2`, workspace.Settings, testWorkspaceID)
	})
	agentID := createHandlerTestAgent(t, "compact-settings-validation", []byte("[]"))
	for _, value := range []any{42, strings.Repeat("x", 16385)} {
		settings := map[string]any{"compact_prompt": value}
		workspaceRequest := newRequest(http.MethodPatch, "/api/workspaces/"+testWorkspaceID, map[string]any{"settings": settings})
		workspaceRequest = withURLParam(workspaceRequest, "id", testWorkspaceID)
		testutil.Call(t, testHandler.UpdateWorkspace, workspaceRequest).Want(http.StatusBadRequest)
		agentRequest := newRequest(http.MethodPatch, "/api/agents/"+agentID, map[string]any{"runtime_config": settings})
		agentRequest = withChatTestWorkspaceCtx(t, withURLParam(agentRequest, "id", agentID))
		testutil.Call(t, testHandler.UpdateAgent, agentRequest).Want(http.StatusBadRequest)
	}
}

// CHE-1612: the dedicated compact_prompt field patches one settings key.
func TestWorkspaceCompactPromptField(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	workspace, err := testHandler.Queries.GetWorkspace(ctx, parseUUID(testWorkspaceID))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		dbfx.Exec(t, `UPDATE workspace SET settings = $1 WHERE id = $2`, workspace.Settings, testWorkspaceID)
	})
	others := map[string]any{"co_authored_by_enabled": false, "nested": map[string]any{"keep": "me"}}
	seeded, _ := json.Marshal(others)
	dbfx.Exec(t, `UPDATE workspace SET settings = $1 WHERE id = $2`, seeded, testWorkspaceID)

	patch := func(body map[string]any, want int) map[string]any {
		t.Helper()
		request := withURLParam(newRequest(http.MethodPatch, "/api/workspaces/"+testWorkspaceID, body), "id", testWorkspaceID)
		testutil.Call(t, testHandler.UpdateWorkspace, request).Want(want)
		stored, err := testHandler.Queries.GetWorkspace(ctx, parseUUID(testWorkspaceID))
		if err != nil {
			t.Fatal(err)
		}
		var settings map[string]any
		if err := json.Unmarshal(stored.Settings, &settings); err != nil {
			t.Fatal(err)
		}
		return settings
	}
	keepsOthers := func(settings map[string]any) {
		t.Helper()
		for key, value := range others {
			if !reflect.DeepEqual(settings[key], value) {
				t.Errorf("settings[%q] = %v, want %v (full settings %v)", key, settings[key], value, settings)
			}
		}
	}

	settings := patch(map[string]any{"compact_prompt": "Keep candidate SHAs."}, http.StatusOK)
	if settings["compact_prompt"] != "Keep candidate SHAs." {
		t.Fatalf("compact_prompt was not saved: %v", settings)
	}
	keepsOthers(settings)

	agentID, sessionID, runtimeID, daemonID := setupDirectChatSession(t, ctx, "Settings compact prompt")
	dbfx.Exec(t, `UPDATE agent SET runtime_config = '{}' WHERE id = $1`, agentID)
	sendDirectChat(t, ctx, agentID, sessionID, "/compact")
	request := withURLParam(newDaemonTokenRequest(http.MethodPost, "/api/daemon/runtimes/"+runtimeID+"/claim", nil, testWorkspaceID, daemonID), "runtimeId", runtimeID)
	var response struct {
		Task *AgentTaskResponse `json:"task"`
	}
	testutil.Call(t, testHandler.ClaimTaskByRuntime, request).Want(http.StatusOK).JSON(&response)
	if response.Task == nil || !strings.Contains(response.Task.ChatMessage, "Keep candidate SHAs.") {
		t.Fatalf("/compact did not use the saved workspace prompt: %+v", response.Task)
	}

	// 1e400 overflows float64; it must be rejected, not read as null (review).
	for _, invalid := range []any{42, strings.Repeat("x", 16385), json.RawMessage("1e400")} {
		settings := patch(map[string]any{"compact_prompt": invalid}, http.StatusBadRequest)
		keepsOthers(settings)
		if settings["compact_prompt"] != "Keep candidate SHAs." {
			t.Fatalf("invalid %v changed the saved prompt: %v", invalid, settings)
		}
	}

	settings = patch(map[string]any{"compact_prompt": nil}, http.StatusOK)
	if _, present := settings["compact_prompt"]; present {
		t.Fatalf("null did not reset compact_prompt: %v", settings)
	}
	keepsOthers(settings)
}
