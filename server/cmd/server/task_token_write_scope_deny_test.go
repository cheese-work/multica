package main

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/analytics"
	"github.com/multica-ai/multica/server/internal/auth"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/realtime"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// A5b: routes that change other issues or tasks without naming them answer 403
// to a run's task token (mode on), and the rows they would change stay as they
// were. Each case hits the real router with a minted task token.
func TestTaskTokenDeniedOnCrossIssueRoutes(t *testing.T) {
	t.Setenv("MULTICA_TASK_WRITE_SCOPE", "on")
	server := httptest.NewServer(NewRouter(testPool, realtime.NewHub(), events.New(), analytics.NoopClient{}, nil))
	defer server.Close()

	fixture := workspaceFixture(t)
	runtimeID := fixture.Runtime(t, "Deny run runtime")
	runAgent := fixture.Agent(t, "Deny run agent", runtimeID)
	ownIssue := fixture.Issue(t, "Deny own issue", testutil.Cols{"assignee_type": "agent", "assignee_id": runAgent})
	runTask := fixture.Task(t, runAgent, testutil.Cols{"issue_id": ownIssue, "runtime_id": runtimeID, "status": "running"})
	token := mintAgentTaskToken(t, runAgent, runTask, testUserID)

	foreignRuntime := fixture.Runtime(t, "Deny foreign runtime")
	foreignAgent := fixture.Agent(t, "Deny foreign agent", foreignRuntime)
	foreignIssue := fixture.Issue(t, "Deny foreign issue", testutil.Cols{"assignee_type": "agent", "assignee_id": foreignAgent})
	foreignTask := fixture.Task(t, foreignAgent, testutil.Cols{"issue_id": foreignIssue, "runtime_id": foreignRuntime, "status": "queued"})
	project := fixture.Project(t, "Deny foreign project")
	fixture.Exec(t, `UPDATE issue SET project_id = $1 WHERE id = $2`, project, foreignIssue)
	label := fixture.Insert(t, "issue_label", testutil.Cols{"workspace_id": testWorkspaceID, "name": "deny-label", "color": "#000000"})
	fixture.Exec(t, `INSERT INTO issue_to_label (issue_id, label_id) VALUES ($1, $2)`, foreignIssue, label)
	fixture.Cleanup(t, `DELETE FROM issue_to_label WHERE issue_id = $1`, foreignIssue)
	squad := fixture.Squad(t, "Deny squad", foreignAgent)
	// explicitly_created_at makes the session visible to the chat handlers (else 404).
	chat := fixture.ChatSession(t, foreignAgent, testutil.Cols{"explicitly_created_at": time.Now()})
	fixture.Task(t, foreignAgent, testutil.Cols{"chat_session_id": chat, "runtime_id": foreignRuntime, "status": "running"})
	chatTask := fixture.Task(t, foreignAgent, testutil.Cols{"chat_session_id": chat, "runtime_id": foreignRuntime, "status": "queued"})
	// Steering needs an agent the caller may invoke, else the handler's own 403 hides the scope gap.
	steerAgent := fixture.Agent(t, "Deny steer agent", foreignRuntime, testutil.Cols{"permission_mode": "public_to", "visibility": "workspace"})
	fixture.Insert(t, "agent_invocation_target", testutil.Cols{"agent_id": steerAgent, "target_type": "workspace", "target_id": testWorkspaceID})
	steerChat := fixture.ChatSession(t, steerAgent, testutil.Cols{"explicitly_created_at": time.Now()})
	fixture.Task(t, steerAgent, testutil.Cols{"chat_session_id": steerChat, "runtime_id": foreignRuntime, "status": "running"})
	steerTask := fixture.Task(t, steerAgent, testutil.Cols{"chat_session_id": steerChat, "runtime_id": foreignRuntime, "status": "queued"})
	pausedProject := fixture.Project(t, "Deny paused project")
	pausedIssue := fixture.Issue(t, "Deny paused project issue", testutil.Cols{"assignee_type": "agent", "assignee_id": foreignAgent, "project_id": pausedProject})
	pausedTask := fixture.Task(t, foreignAgent, testutil.Cols{"issue_id": pausedIssue, "runtime_id": foreignRuntime, "status": "queued"})
	deletedProject := fixture.Project(t, "Deny deleted project")
	// One runtime per runtime route: the first to run would delete the rest.
	runtimeWithTask := func(name string) (runtimeID, taskID string) {
		runtimeID = fixture.Runtime(t, name)
		agentID := fixture.Agent(t, name+" agent", runtimeID)
		return runtimeID, fixture.Task(t, agentID, testutil.Cols{"runtime_id": runtimeID, "status": "queued"})
	}
	unbindRuntime, unbindTask := runtimeWithTask("Deny unbind runtime")
	archiveRuntime, archiveTask := runtimeWithTask("Deny archive runtime")
	revokeRuntime, revokeTask := runtimeWithTask("Deny revoke runtime")
	fixture.Exec(t, `UPDATE agent_runtime SET visibility = 'public' WHERE id = $1`, revokeRuntime)

	// Workspace routes take the workspace from the path, but the token is bound
	// to the workspace of its run, so these run in a second workspace.
	otherWorkspace := fixture.Workspace(t, "Deny other workspace", "deny-other-ws")
	fixture.Member(t, otherWorkspace, testUserID, "owner")
	otherUser := fixture.User(t, "Deny other member", "deny-other-member@example.com")
	otherMember := fixture.Member(t, otherWorkspace, otherUser, "member")
	other := testutil.New(testPool, otherWorkspace, testUserID)
	otherRuntime := other.Runtime(t, "Deny other runtime")
	otherAgent := other.Agent(t, "Deny other agent", otherRuntime)
	otherIssue := other.Issue(t, "Deny other issue", testutil.Cols{"assignee_type": "agent", "assignee_id": otherAgent})
	otherTask := other.Task(t, otherAgent, testutil.Cols{"issue_id": otherIssue, "runtime_id": otherRuntime, "status": "running"})
	otherToken := mintWorkspaceTaskToken(t, otherWorkspace, otherAgent, otherTask, testUserID)

	cases := []struct {
		name, method, path, body string
		unchanged                string // SQL that must still count 1 row
		args                     []any
		token, workspace         string // the run's token; defaults to the first run's
	}{
		{"cancel agent tasks", "POST", "/api/agents/" + foreignAgent + "/cancel-tasks", ``,
			`SELECT count(*) FROM agent_task_queue WHERE id = $1 AND status = 'queued'`, []any{foreignTask}, "", ""},
		{"archive agent", "POST", "/api/agents/" + foreignAgent + "/archive", ``,
			`SELECT count(*) FROM agent WHERE id = $1 AND archived_at IS NULL`, []any{foreignAgent}, "", ""},
		{"set project workflow", "POST", "/api/projects/" + project + "/workflow", `{"workflow_id":null}`,
			`SELECT count(*) FROM issue WHERE id = $1 AND status = 'todo' AND project_id = $2`, []any{foreignIssue, project}, "", ""},
		{"pause project", "PUT", "/api/projects/" + pausedProject + "/", `{"status":"paused"}`,
			`SELECT count(*) FROM agent_task_queue WHERE id = $1 AND status = 'queued'`, []any{pausedTask}, "", ""},
		{"delete project", "DELETE", "/api/projects/" + deletedProject + "/", ``,
			`SELECT count(*) FROM project WHERE id = $1`, []any{deletedProject}, "", ""},
		{"clear queued chat tasks", "DELETE", "/api/chat/sessions/" + chat + "/queued-tasks", ``,
			`SELECT count(*) FROM agent_task_queue WHERE id = $1 AND status = 'queued'`, []any{chatTask}, "", ""},
		{"prioritize queued chat task", "POST", "/api/chat/sessions/" + chat + "/queued-tasks/" + chatTask + "/prioritize", ``,
			`SELECT count(*) FROM agent_task_queue WHERE id = $1 AND status = 'queued' AND priority = 0`, []any{chatTask}, "", ""},
		{"steer queued chat task", "POST", "/api/chat/sessions/" + steerChat + "/tasks/" + steerTask + "/steer", `{"content":"steer"}`,
			`SELECT count(*) FROM agent_task_queue WHERE id = $1 AND status = 'queued'`, []any{steerTask}, "", ""},
		{"approve github revision", "POST", "/api/github/intake/revisions/00000000-0000-0000-0000-000000000001/approve", `{"snapshot_hash":"x"}`,
			`SELECT count(*) FROM workspace WHERE id = $1`, []any{testWorkspaceID}, "", ""},
		{"delete label", "DELETE", "/api/labels/" + label, ``,
			`SELECT count(*) FROM issue_to_label WHERE issue_id = $1 AND label_id = $2`, []any{foreignIssue, label}, "", ""},
		{"delete squad", "DELETE", "/api/squads/" + squad, ``,
			`SELECT count(*) FROM squad WHERE id = $1`, []any{squad}, "", ""},
		{"delete runtime", "DELETE", "/api/runtimes/" + foreignRuntime, ``,
			`SELECT count(*) FROM agent_runtime WHERE id = $1`, []any{foreignRuntime}, "", ""},
		{"unbind agents and delete runtime", "POST", "/api/runtimes/" + unbindRuntime + "/unbind-agents-and-delete", `{"expected_active_agent_ids":[]}`,
			`SELECT count(*) FROM agent_task_queue WHERE id = $1 AND status = 'queued'`, []any{unbindTask}, "", ""},
		{"archive agents and delete runtime", "POST", "/api/runtimes/" + archiveRuntime + "/archive-agents-and-delete", `{"expected_active_agent_ids":[]}`,
			`SELECT count(*) FROM agent_task_queue WHERE id = $1 AND status = 'queued'`, []any{archiveTask}, "", ""},
		{"revoke runtime workspace access", "POST", "/api/runtimes/" + revokeRuntime + "/revoke-workspace-access", `{}`,
			`SELECT count(*) FROM agent_task_queue WHERE id = $1 AND status = 'queued'`, []any{revokeTask}, "", ""},
		{"archive chat session", "PATCH", "/api/chat/sessions/" + chat + "/archive", `{"archived":true}`,
			`SELECT count(*) FROM chat_session WHERE id = $1 AND status = 'active'`, []any{chat}, "", ""},
		{"delete chat session", "DELETE", "/api/chat/sessions/" + chat, ``,
			`SELECT count(*) FROM chat_session WHERE id = $1`, []any{chat}, "", ""},
		{"remove workspace member", "DELETE", "/api/workspaces/" + otherWorkspace + "/members/" + otherMember, ``,
			`SELECT count(*) FROM member WHERE id = $1`, []any{otherMember}, otherToken, otherWorkspace},
		{"leave workspace", "POST", "/api/workspaces/" + otherWorkspace + "/leave", ``,
			`SELECT count(*) FROM member WHERE workspace_id = $1 AND user_id = $2`, []any{otherWorkspace, testUserID}, otherToken, otherWorkspace},
		{"delete workspace", "DELETE", "/api/workspaces/" + otherWorkspace, ``,
			`SELECT count(*) FROM workspace WHERE id = $1`, []any{otherWorkspace}, otherToken, otherWorkspace},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			request, err := http.NewRequest(tc.method, server.URL+tc.path, bytes.NewReader([]byte(tc.body)))
			if err != nil {
				t.Fatal(err)
			}
			use, workspace := token, testWorkspaceID
			if tc.token != "" {
				use, workspace = tc.token, tc.workspace
			}
			request.Header.Set("Authorization", "Bearer "+use)
			request.Header.Set("X-Workspace-ID", workspace)
			request.Header.Set("Content-Type", "application/json")
			response, err := server.Client().Do(request)
			if err != nil {
				t.Fatal(err)
			}
			response.Body.Close()
			if response.StatusCode != http.StatusForbidden {
				t.Errorf("%s %s = %d, want 403", tc.method, tc.path, response.StatusCode)
			}
			if n := fixture.Count(t, tc.unchanged, tc.args...); n != 1 {
				t.Errorf("foreign row changed (%d match): %s", n, tc.unchanged)
			}
		})
	}
}

// mintWorkspaceTaskToken is mintAgentTaskToken for a run in another workspace.
func mintWorkspaceTaskToken(t *testing.T, workspaceID, agentID, taskID, boundUserID string) string {
	t.Helper()
	raw := fmt.Sprintf("mat_test_%s_%d", strings.ReplaceAll(agentID, "-", ""), time.Now().UnixNano())
	if _, err := testPool.Exec(context.Background(), `
		INSERT INTO task_token (token_hash, task_id, agent_id, workspace_id, user_id, expires_at)
		VALUES ($1, $2, $3, $4, $5, now() + interval '1 hour')
	`, auth.HashToken(raw), taskID, agentID, workspaceID, boundUserID); err != nil {
		t.Fatalf("mint task token: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM task_token WHERE token_hash = $1`, auth.HashToken(raw))
	})
	return raw
}
