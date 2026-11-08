package daemon

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// decision_ask rides the plugin hook MCP server, but its call goes to the
// decision route with the agent's task token, never a daemon credential.
func TestDecisionAskToolReachesServerWithTheTaskTokenOnly(t *testing.T) {
	var gotPath, gotAuth, gotBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotPath, gotAuth, gotBody = r.URL.Path, r.Header.Get("Authorization"), string(body)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"decision-1"}`))
	}))
	t.Cleanup(server.Close)
	client := NewClient(server.URL)

	task := Task{ID: "task-1", IssueID: "issue-1", AuthToken: "mat_task", RemoteMCPDaemonToken: "daemon-secret"}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	_, set, err := startTaskPluginHookMCP(ctx, task.ID, taskHookTools(task, true), taskHookInvoker(task, client, nil), nil)
	if err != nil || set == nil {
		t.Fatalf("start hook MCP: set=%v err=%v", set, err)
	}
	t.Cleanup(set.Close)
	mcp := set.server.Handler.(*pluginHookMCPServer)

	listed := callMCP(t, mcp, mcp.path, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)["result"].(map[string]any)["tools"].([]any)
	if len(listed) != 1 || listed[0].(map[string]any)["name"] != "decision_ask" {
		t.Fatalf("tools = %v, want decision_ask", listed)
	}

	response := callMCP(t, mcp, mcp.path, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"decision_ask","arguments":{"ask":{"decision":"Ship?"},"escalation_class":"reversible"}}}`)
	if result := response["result"].(map[string]any); result["isError"] == true {
		t.Fatalf("call failed: %v", response)
	}
	if gotPath != "/api/decisions/ask" || gotAuth != "Bearer mat_task" {
		t.Fatalf("server saw %s with %q, want /api/decisions/ask with the task token", gotPath, gotAuth)
	}
	var sent map[string]any
	if err := json.Unmarshal([]byte(gotBody), &sent); err != nil || sent["escalation_class"] != "reversible" || len(sent) != 2 {
		t.Fatalf("body = %s, want the tool arguments verbatim (err=%v)", gotBody, err)
	}
}

func TestDecisionAskToolIsAbsentFromChatTasks(t *testing.T) {
	if tools := taskHookTools(Task{ID: "task-1", ChatSessionID: "chat-1"}, true); len(tools) != 0 {
		t.Fatalf("chat task tools = %v, want none", tools)
	}
}

// Pi refuses to start with any MCP config below its MCP floor, so a tool every
// issue task carries would stop those tasks on old pi. Pi at or above the floor
// loads it, and a custom profile command only promises pi's protocol.
func TestDecisionAskReachableFollowsPiMcpFloor(t *testing.T) {
	cases := []struct {
		name, provider, version string
		builtin, want           bool
	}{
		{"claude", "claude", "2.1.0", true, true},
		{"pi at the floor", "pi", "0.99.0", true, true},
		{"pi above the floor", "pi", "1.4.2", true, true},
		{"pi below the floor", "pi", "0.98.9", true, false},
		{"pi version unreadable", "pi", "", true, false},
		{"pi custom profile", "pi", "", false, true},
	}
	for _, c := range cases {
		if got := decisionAskReachable(c.provider, c.version, c.builtin); got != c.want {
			t.Errorf("%s: reachable = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestDecisionAskToolIsAbsentWhenRuntimeCannotLoadIt(t *testing.T) {
	if tools := taskHookTools(Task{ID: "task-1", IssueID: "issue-1"}, false); len(tools) != 0 {
		t.Fatalf("tools = %v, want none", tools)
	}
}
