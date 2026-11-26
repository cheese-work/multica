package handler

import (
	"context"
	"net/http"
	"reflect"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// CHE-10000 (S2), claim level: the env a Claude run receives in the claim
// response (daemon.go, resp.Agent.CustomEnv) for each task kind and setting.
// The run cap is separate from the DM compaction threshold: a chat task gets
// max(dm_run_cap, DM threshold), any other task the built-in cap.
func TestClaimClaudeCompactEnvByTaskKind(t *testing.T) {
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

	const window, pct = "CLAUDE_CODE_AUTO_COMPACT_WINDOW", "CLAUDE_AUTOCOMPACT_PCT_OVERRIDE"
	const dmSettings = `{"auto_compact":{"threshold_tokens_1m":350000,"dm_run_cap_tokens_1m":430000}}`
	for _, tc := range []struct {
		name     string
		provider string
		model    string
		settings string
		agentEnv string
		chat     bool
		want     map[string]string
	}{
		{"chat 1m, saved 350k threshold and 430k cap", "claude", "claude-opus-5-5[1m]", dmSettings, `{}`, true, map[string]string{window: "430000"}},
		{"issue 1m, same settings", "claude", "claude-opus-5-5[1m]", dmSettings, `{}`, false, map[string]string{window: "400000"}},
		{"chat 1m, no saved keys", "claude", "claude-opus-5-5[1m]", `{}`, `{}`, true, map[string]string{window: "400000"}},
		{"issue 1m, no saved keys", "claude", "claude-opus-5-5[1m]", `{}`, `{}`, false, map[string]string{window: "400000"}},
		{"chat 1m, cap below threshold", "claude", "claude-opus-5-5[1m]", `{"auto_compact":{"threshold_tokens_1m":350000,"dm_run_cap_tokens_1m":300000}}`, `{}`, true, map[string]string{window: "350000"}},
		{"chat 1m, saved percent", "claude", "claude-opus-5-5[1m]", `{"auto_compact":{"threshold_pct":80}}`, `{}`, true, map[string]string{window: "400000"}},
		{"issue 1m, saved percent", "claude", "claude-opus-5-5[1m]", `{"auto_compact":{"threshold_pct":80}}`, `{}`, false, map[string]string{window: "400000"}},
		{"agent env key wins", "claude", "claude-opus-5-5[1m]", dmSettings, `{"CLAUDE_CODE_AUTO_COMPACT_WINDOW":"250000"}`, true, map[string]string{window: "250000"}},
		{"auto-compact disabled", "claude", "claude-opus-5-5[1m]", `{"auto_compact":{"enabled":false}}`, `{}`, true, map[string]string{}},
		{"codex provider", "codex", "gpt-5", dmSettings, `{}`, true, map[string]string{}},
		{"chat 200k model", "claude", "claude-sonnet-5-5", `{}`, `{}`, true, map[string]string{pct: "95"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dbfx.Exec(t, `UPDATE workspace SET settings = $1 WHERE id = $2`, []byte(tc.settings), testWorkspaceID)
			runtimeID := dbfx.Runtime(t, "Compact env runtime", testutil.Cols{"provider": tc.provider})
			agentID := dbfx.Agent(t, "Compact env agent", runtimeID, testutil.Cols{"model": tc.model, "custom_env": []byte(tc.agentEnv)})
			task := testutil.Cols{"runtime_id": runtimeID}
			if tc.chat {
				task["chat_session_id"] = dbfx.ChatSession(t, agentID)
			} else {
				_, issueID := createClaimReclaimAgentAndIssue(t, context.Background(), runtimeID, "Compact env issue")
				task["issue_id"] = issueID
			}
			dbfx.Task(t, agentID, task)

			req := withURLParam(newOwnerDaemonRequest("POST", "/api/daemon/runtimes/"+runtimeID+"/tasks/claim", nil), "runtimeId", runtimeID)
			var resp struct {
				Task *struct {
					Agent *struct {
						CustomEnv map[string]string `json:"custom_env"`
					} `json:"agent"`
				} `json:"task"`
			}
			testutil.Call(t, testHandler.ClaimTaskByRuntime, req).Want(http.StatusOK).JSON(&resp)
			if resp.Task == nil || resp.Task.Agent == nil {
				t.Fatalf("no task claimed: %+v", resp)
			}
			got := map[string]string{}
			for k, v := range resp.Task.Agent.CustomEnv {
				if k == window || k == pct {
					got[k] = v
				}
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("compaction env = %v, want %v", got, tc.want)
			}
		})
	}
}
