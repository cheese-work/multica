package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// claimLeaderSessionProbe decodes the resume fields off a claim response.
type claimLeaderSessionProbe struct {
	Task *struct {
		PriorSessionID string `json:"prior_session_id"`
		PriorWorkDir   string `json:"prior_work_dir"`
	} `json:"task"`
}

// claimIssueFollowUp claims a queued follow-up on an issue whose earlier run
// holds a resumable session, with the follow-up flagged leader or not.
func claimIssueFollowUp(t *testing.T, name string, leader bool) claimLeaderSessionProbe {
	t.Helper()
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()

	agentID := createHandlerTestAgent(t, name, []byte("[]"))
	runtimeID := handlerTestRuntimeID(t)

	var issueID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO issue (workspace_id, title, status, priority, creator_type, creator_id, assignee_type, assignee_id, number)
		VALUES ($1, 'leader fresh session issue', 'todo', 'none', 'member', $2, 'agent', $3,
		        (SELECT COALESCE(MAX(number), 0) + 1 FROM issue WHERE workspace_id = $1))
		RETURNING id
	`, testWorkspaceID, testUserID, agentID).Scan(&issueID); err != nil {
		t.Fatalf("create issue: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE issue_id = $1`, issueID)
		testPool.Exec(context.Background(), `DELETE FROM issue WHERE id = $1`, issueID)
	})

	if _, err := testPool.Exec(ctx, `
		INSERT INTO agent_task_queue (agent_id, runtime_id, issue_id, status, priority, started_at, completed_at, session_id, work_dir)
		VALUES ($1, $2, $3, 'completed', 0, now() - interval '2 minutes', now() - interval '1 minute', 'PRIOR-ISSUE-SESSION', '/tmp/prior-issue-workdir')
	`, agentID, runtimeID, issueID); err != nil {
		t.Fatalf("insert prior task: %v", err)
	}
	if _, err := testPool.Exec(ctx, `
		INSERT INTO agent_task_queue (agent_id, runtime_id, issue_id, status, priority, is_leader_task)
		VALUES ($1, $2, $3, 'queued', 1000, $4)
	`, agentID, runtimeID, issueID, leader); err != nil {
		t.Fatalf("insert follow-up task: %v", err)
	}

	w := httptest.NewRecorder()
	req := newDaemonTokenRequest(http.MethodPost, "/api/daemon/runtimes/"+runtimeID+"/claim", nil, testWorkspaceID, "leader-fresh-session-test")
	req = withURLParam(req, "runtimeId", runtimeID)
	testHandler.ClaimTaskByRuntime(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("ClaimTaskByRuntime: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var probe claimLeaderSessionProbe
	if err := json.NewDecoder(w.Body).Decode(&probe); err != nil {
		t.Fatalf("decode claim response: %v", err)
	}
	if probe.Task == nil {
		t.Fatal("expected a claimed task in the response")
	}
	return probe
}

// A leader's resumed session re-reads its whole history on every model call
// (CHE-1931). The leader starts a fresh session but keeps the issue workdir.
func TestClaimTaskByRuntime_LeaderIssueRunStartsFreshSession(t *testing.T) {
	probe := claimIssueFollowUp(t, "LeaderFreshSessionAgent", true)
	if probe.Task.PriorSessionID != "" {
		t.Fatalf("leader claim resumed session %q, want a fresh session", probe.Task.PriorSessionID)
	}
	if probe.Task.PriorWorkDir != "/tmp/prior-issue-workdir" {
		t.Fatalf("leader claim prior_work_dir = %q, want the prior issue workdir", probe.Task.PriorWorkDir)
	}
}

func TestClaimTaskByRuntime_NonLeaderIssueRunResumesSession(t *testing.T) {
	probe := claimIssueFollowUp(t, "WorkerResumeAgent", false)
	if probe.Task.PriorSessionID != "PRIOR-ISSUE-SESSION" {
		t.Fatalf("worker claim prior_session_id = %q, want PRIOR-ISSUE-SESSION", probe.Task.PriorSessionID)
	}
}
