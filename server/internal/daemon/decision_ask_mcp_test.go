package daemon

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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
func TestDecisionAskReachableFollowsMcpSupport(t *testing.T) {
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
		// Droid and Amp refuse any Multica-managed MCP config, so decision_ask
		// (which would put one on every issue task) stays off them.
		{"droid", "droid", "1.0.0", true, false},
		{"amp", "amp", "1.0.0", true, false},
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

// The tool description names options[].design and says frame_updated_at is the server's (CHE-7064 S10).
func TestDecisionAskDescriptionNamesTheDesignOptionField(t *testing.T) {
	for _, want := range []string{"design {frame_url, attestation_id}", "frame_updated_at", "design_version_server_owned"} {
		if !strings.Contains(decisionAskDescription, want) {
			t.Errorf("decision_ask description does not mention %q", want)
		}
	}
}

// G4 (CHE-7064 S12): options[].design passes through the tool verbatim, and a
// refusal of the design guard comes back to the agent as a tool error that
// names the status and the code.
func TestDecisionAskToolPassesDesignThroughAndShowsAGuardRefusal(t *testing.T) {
	var gotBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"code":"design_attestation_stale","error":"the page changed after the attestation"}`))
	}))
	t.Cleanup(server.Close)
	task := Task{ID: "task-1", IssueID: "issue-1", AuthToken: "mat_task", RemoteMCPDaemonToken: "daemon-secret"}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	_, set, err := startTaskPluginHookMCP(ctx, task.ID, taskHookTools(task, true), taskHookInvoker(task, NewClient(server.URL), nil), nil)
	if err != nil || set == nil {
		t.Fatalf("start hook MCP: set=%v err=%v", set, err)
	}
	t.Cleanup(set.Close)
	mcp := set.server.Handler.(*pluginHookMCPServer)

	call := `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"decision_ask","arguments":{"ask":{"options":[{"label":"A","design":{"frame_url":"https://doop.example.com/c/cv1?frame=f1","attestation_id":"run-1"}}]},"escalation_class":"product_direction"}}}`
	result := callMCP(t, mcp, mcp.path, call)["result"].(map[string]any)
	if result["isError"] != true {
		t.Fatalf("result = %v, want a tool error", result)
	}
	text := result["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "409") || !strings.Contains(text, "design_attestation_stale") {
		t.Fatalf("tool error = %q, want the 409 status and the design_attestation_stale code", text)
	}
	var sent struct {
		Ask struct {
			Options []struct {
				Design map[string]any `json:"design"`
			} `json:"options"`
		} `json:"ask"`
	}
	if err := json.Unmarshal([]byte(gotBody), &sent); err != nil || len(sent.Ask.Options) != 1 ||
		sent.Ask.Options[0].Design["frame_url"] != "https://doop.example.com/c/cv1?frame=f1" || sent.Ask.Options[0].Design["attestation_id"] != "run-1" {
		t.Fatalf("server saw %s (err=%v), want options[].design verbatim", gotBody, err)
	}
}
