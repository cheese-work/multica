package handler

import (
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// CHE-5334: the pause dialog reads what a pause leaves waiting before it asks.

type schedulingImpactBody struct {
	Running    int `json:"running_tasks"`
	Queued     int `json:"queued_tasks"`
	OpenIssues int `json:"open_issues"`
	Wakeups    int `json:"enabled_wakeups"`
}

func callImpact(t *testing.T, id string, tweak func(*http.Request)) *testutil.Response {
	t.Helper()
	req := withURLParam(newRequest("GET", "/api/agents/"+id+"/scheduling-impact", nil), "id", id)
	if tweak != nil {
		tweak(req)
	}
	return testutil.Call(t, testHandler.GetAgentSchedulingImpact, req)
}

func TestAgentSchedulingImpactCountsHeldWork(t *testing.T) {
	agentID := createHandlerTestAgent(t, "ImpactTarget", []byte("[]"))
	createHandlerTestTaskForAgent(t, agentID) // one running task

	var got schedulingImpactBody
	callImpact(t, agentID, nil).Want(http.StatusOK).JSON(&got)
	if got.Running != 1 || got.Queued != 0 || got.OpenIssues != 0 || got.Wakeups != 0 {
		t.Fatalf("impact = %+v, want 1 running and nothing else", got)
	}
}

func TestAgentSchedulingImpactIsForOwnersAndAdminsOnly(t *testing.T) {
	memberID := dbfx.User(t, "Impact Member", "impact-member@example.com")
	dbfx.Member(t, testWorkspaceID, memberID, "member")
	agentID := createHandlerTestAgent(t, "ImpactGuarded", []byte("[]"))

	callImpact(t, agentID, asPauseUser(memberID)).Want(http.StatusForbidden)
	callImpact(t, agentID, func(r *http.Request) { r.Header.Set("X-Actor-Source", "task_token") }).Want(http.StatusForbidden)
}
