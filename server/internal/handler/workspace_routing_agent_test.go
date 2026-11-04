package handler

import (
	"context"
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
)

// Routing agent (CHE-3627): one workspace setting names the agent that handles
// routing-only workflow steps, a fallback, and per-status overrides.

func routingFixture(t *testing.T, slug string) (wsID, primary, fallback, other string) {
	t.Helper()
	wsID = pauseWorkspaceFixture(t, slug)
	runtime := dbfx.Runtime(t, slug, testutil.Cols{"workspace_id": wsID})
	agent := func(name string) string {
		return dbfx.Agent(t, slug+"-"+name, runtime, testutil.Cols{"workspace_id": wsID})
	}
	return wsID, agent("primary"), agent("fallback"), agent("other")
}

func putRoutingAgent(t *testing.T, wsID string, body map[string]any, tweak func(*http.Request)) *testutil.Response {
	t.Helper()
	req := withURLParam(newRequest("PUT", "/api/workspaces/"+wsID+"/routing-agent", body), "id", wsID)
	if tweak != nil {
		tweak(req)
	}
	return testutil.Call(t, testHandler.UpdateRoutingAgent, req)
}

func routedAgent(t *testing.T, wsID, status string) string {
	t.Helper()
	ws, err := util.ParseUUID(wsID)
	if err != nil {
		t.Fatal(err)
	}
	id := testHandler.routingAgentFor(context.Background(), ws, status)
	if !id.Valid {
		return ""
	}
	return uuidToString(id)
}

func TestRoutingAgentDefaultRoutesNothing(t *testing.T) {
	wsID, _, _, _ := routingFixture(t, "ra-default")
	for _, status := range defaultRoutingStatuses {
		if got := routedAgent(t, wsID, status); got != "" {
			t.Fatalf("unset workspace routed %s to %s", status, got)
		}
	}
}

func TestRoutingAgentRoutesOnlyRoutingStatuses(t *testing.T) {
	wsID, primary, fallback, other := routingFixture(t, "ra-routes")
	putRoutingAgent(t, wsID, map[string]any{
		"agent_id": primary, "fallback_agent_id": fallback,
		"overrides": map[string]string{"need_decision": other},
	}, nil).Want(http.StatusOK)

	for _, status := range []string{"todo", "blocked", "merged"} {
		if got := routedAgent(t, wsID, status); got != primary {
			t.Fatalf("%s routed to %q, want primary %q", status, got, primary)
		}
	}
	if got := routedAgent(t, wsID, "need_decision"); got != other {
		t.Fatalf("override routed to %q, want %q", got, other)
	}
	if got := routedAgent(t, wsID, "in_progress"); got != "" {
		t.Fatalf("in_progress is not a routing step, routed to %q", got)
	}
}

func TestRoutingAgentFallsBackWhenPrimaryArchived(t *testing.T) {
	wsID, primary, fallback, _ := routingFixture(t, "ra-archived")
	putRoutingAgent(t, wsID, map[string]any{"agent_id": primary, "fallback_agent_id": fallback}, nil).Want(http.StatusOK)
	dbfx.Exec(t, `UPDATE agent SET archived_at=now() WHERE id=$1`, primary)
	if got := routedAgent(t, wsID, "todo"); got != fallback {
		t.Fatalf("archived primary routed to %q, want fallback %q", got, fallback)
	}
	dbfx.Exec(t, `UPDATE agent SET archived_at=now() WHERE id=$1`, fallback)
	if got := routedAgent(t, wsID, "todo"); got != "" {
		t.Fatalf("both archived: routed to %q, want the step's own handler", got)
	}
}

func TestRoutingAgentCustomStatuses(t *testing.T) {
	wsID, primary, _, _ := routingFixture(t, "ra-statuses")
	putRoutingAgent(t, wsID, map[string]any{"agent_id": primary, "statuses": []string{"backlog"}}, nil).Want(http.StatusOK)
	if got := routedAgent(t, wsID, "backlog"); got != primary {
		t.Fatalf("backlog routed to %q", got)
	}
	if got := routedAgent(t, wsID, "todo"); got != "" {
		t.Fatalf("todo left the list but routed to %q", got)
	}
}

func TestRoutingAgentHumanOwnerOnlyAndAudited(t *testing.T) {
	wsID, primary, fallback, _ := routingFixture(t, "ra-audit")
	for _, source := range []string{"task_token", "cloud_pat"} {
		putRoutingAgent(t, wsID, map[string]any{"agent_id": primary}, func(r *http.Request) { r.Header.Set("X-Actor-Source", source) }).Want(http.StatusForbidden)
	}
	if got := routedAgent(t, wsID, "todo"); got != "" {
		t.Fatal("a refused write changed the setting")
	}
	putRoutingAgent(t, wsID, map[string]any{"agent_id": primary}, nil).Want(http.StatusOK)
	putRoutingAgent(t, wsID, map[string]any{"agent_id": primary, "fallback_agent_id": fallback}, nil).Want(http.StatusOK)
	var n int
	dbfx.QueryRow(t, `SELECT count(*) FROM activity_log WHERE workspace_id=$1 AND action='routing_agent_changed' AND actor_type='member' AND actor_id=$2`, wsID, testUserID).Scan(&n)
	if n != 2 {
		t.Fatalf("audit records = %d, want 2", n)
	}
	dbfx.Exec(t, `UPDATE member SET role='member' WHERE workspace_id=$1 AND user_id=$2`, wsID, testUserID)
	putRoutingAgent(t, wsID, map[string]any{"agent_id": primary}, nil).Want(http.StatusForbidden)
}

func TestRoutingAgentGenericSettingsSaveCannotChange(t *testing.T) {
	wsID, primary, _, _ := routingFixture(t, "ra-generic")
	saveWorkspaceSettings(t, wsID, map[string]any{routingAgentKey: map[string]any{"agent_id": primary}})
	if got := routedAgent(t, wsID, "todo"); got != "" {
		t.Fatal("generic settings save set the routing agent")
	}
	putRoutingAgent(t, wsID, map[string]any{"agent_id": primary}, nil).Want(http.StatusOK)
	saveWorkspaceSettings(t, wsID, map[string]any{routingAgentKey: map[string]any{}})
	if got := routedAgent(t, wsID, "todo"); got != primary {
		t.Fatal("generic settings save cleared the routing agent")
	}
}

func TestRoutingAgentRejectsInvalidBodies(t *testing.T) {
	wsID, primary, _, _ := routingFixture(t, "ra-invalid")
	for name, body := range map[string]map[string]any{
		"not a uuid":      {"agent_id": "nope"},
		"foreign agent":   {"agent_id": "00000000-0000-0000-0000-000000000001"},
		"fallback alone":  {"fallback_agent_id": primary},
		"same agent":      {"agent_id": primary, "fallback_agent_id": primary},
		"unknown key":     {"agent_id": primary, "extra": true},
		"bad status":      {"agent_id": primary, "statuses": []string{""}},
		"foreign overide": {"agent_id": primary, "overrides": map[string]string{"todo": "00000000-0000-0000-0000-000000000001"}},
	} {
		t.Run(name, func(t *testing.T) { putRoutingAgent(t, wsID, body, nil).Want(http.StatusBadRequest) })
	}
}

func TestRoutingAgentTakesTheHandoffOfARoutingStep(t *testing.T) {
	seedTestCatalog(t)
	resetDefaultWorkflow(t)
	stepAgent := seededReadyAgentID(t)
	runtime := dbfx.Runtime(t, "ra-handoff", testutil.Cols{"workspace_id": testWorkspaceID})
	routing := dbfx.Agent(t, "ra-handoff-routing", runtime, testutil.Cols{"workspace_id": testWorkspaceID})
	t.Cleanup(func() {
		dbfx.Exec(t, `UPDATE workspace SET settings = settings - 'routing_agent' WHERE id=$1`, testWorkspaceID)
	})
	projectID := createWorkflowTestProject(t, "Routing handoff project")
	for _, status := range []string{"todo", "in_progress"} {
		setColumnHandoff(t, projectID, status, handToAgent(stepAgent, "Do it.")).Want(http.StatusOK)
	}

	putRoutingAgent(t, testWorkspaceID, map[string]any{"agent_id": routing}, nil).Want(http.StatusOK)

	routed := createIssueForTest(t, map[string]any{"title": "routing step", "project_id": projectID})
	if routed.AssigneeID == nil || *routed.AssigneeID != routing {
		t.Fatalf("todo handed to %v, want the routing agent", routed.AssigneeID)
	}
	updateIssueForTest(t, routed.ID, map[string]any{"status": "in_progress"}).Want(http.StatusOK)
	if _, _, assignee := issueStatusAndAssignee(t, routed.ID); assignee != stepAgent {
		t.Fatalf("in_progress handed to %s, want its own handler %s", assignee, stepAgent)
	}
}
