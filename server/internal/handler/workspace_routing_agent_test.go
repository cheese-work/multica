package handler

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	db "github.com/multica-ai/multica/server/pkg/db/generated"

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
	id := routingAgentFor(context.Background(), testHandler.Queries, ws, db.Project{}, status)
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
		dbfx.Exec(t, `UPDATE workspace SET settings = settings - 'routing_agent' - 'background_agents' WHERE id=$1`, testWorkspaceID)
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

// An override keyed "<workflow>:<status>" sends one step of one workflow to a
// different agent; another workflow's key, and the primary agent, do not apply.
func TestRoutingAgentOverrideIsPerWorkflowStep(t *testing.T) {
	seedTestCatalog(t)
	resetDefaultWorkflow(t)
	stepAgent := seededReadyAgentID(t)
	runtime := dbfx.Runtime(t, "ra-scoped", testutil.Cols{"workspace_id": testWorkspaceID})
	primary := dbfx.Agent(t, "ra-scoped-primary", runtime, testutil.Cols{"workspace_id": testWorkspaceID})
	scoped := dbfx.Agent(t, "ra-scoped-scoped", runtime, testutil.Cols{"workspace_id": testWorkspaceID})
	t.Cleanup(func() {
		dbfx.Exec(t, `UPDATE workspace SET settings = settings - 'routing_agent' - 'background_agents' WHERE id=$1`, testWorkspaceID)
	})
	projectID := createWorkflowTestProject(t, "Routing scoped project")
	setColumnHandoff(t, projectID, "todo", handToAgent(stepAgent, "Do it.")).Want(http.StatusOK)
	workflowID := projectWorkflowID(t, projectID)

	var n int
	handoff := func(overrides map[string]string) string {
		n++
		putRoutingAgent(t, testWorkspaceID, map[string]any{"agent_id": primary, "overrides": overrides}, nil).Want(http.StatusOK)
		issue := createIssueForTest(t, map[string]any{"title": fmt.Sprintf("scoped %d", n), "project_id": projectID})
		if issue.AssigneeID == nil {
			t.Fatal("todo handed to nobody")
		}
		return *issue.AssigneeID
	}
	if got := handoff(map[string]string{workflowID + ":todo": scoped}); got != scoped {
		t.Fatalf("this workflow's override: handed to %s, want %s", got, scoped)
	}
	otherWorkflowID := createTestWorkflow(t, "Routing other workflow").ID
	if got := handoff(map[string]string{otherWorkflowID + ":todo": scoped}); got != primary {
		t.Fatalf("another workflow's override: handed to %s, want primary %s", got, primary)
	}
}

func TestRoutingAgentRejectsOverrideForUnknownWorkflow(t *testing.T) {
	wsID, primary, _, other := routingFixture(t, "ra-unknown-wf")
	for name, key := range map[string]string{
		"workflow not in workspace": "00000000-0000-0000-0000-000000000001:todo",
		"not a workflow id":         "nope:todo",
		"empty status":              "nope:",
		"default is not a workflow": "default:todo",
	} {
		t.Run(name, func(t *testing.T) {
			putRoutingAgent(t, wsID, map[string]any{"agent_id": primary, "overrides": map[string]string{key: other}}, nil).Want(http.StatusBadRequest)
		})
	}
	putRoutingAgent(t, wsID, map[string]any{"agent_id": primary, "overrides": map[string]string{"todo": other}}, nil).Want(http.StatusOK)
}

// The fallback agent must take over the step, not observe it: after a provider
// failure the issue belongs to the fallback and its run claims the assignee's
// workflow brief, with the step instructions and status commands.
func TestRoutingFailoverGivesTheFallbackTheStepBrief(t *testing.T) {
	seedTestCatalog(t)
	resetDefaultWorkflow(t)
	ctx := context.Background()
	runtime := dbfx.Runtime(t, "ra-failover", testutil.Cols{"workspace_id": testWorkspaceID})
	routing := dbfx.Agent(t, "ra-failover-routing", runtime, testutil.Cols{"workspace_id": testWorkspaceID})
	fallback := dbfx.Agent(t, "ra-failover-fallback", runtime, testutil.Cols{"workspace_id": testWorkspaceID})
	t.Cleanup(func() {
		dbfx.Exec(t, `UPDATE workspace SET settings = settings - 'routing_agent' - 'background_agents' WHERE id=$1`, testWorkspaceID)
	})
	projectID := createWorkflowTestProject(t, "Routing failover project")
	setColumnHandoff(t, projectID, "todo", handToAgent(seededReadyAgentID(t), "Route it.")).Want(http.StatusOK)
	putRoutingAgent(t, testWorkspaceID, map[string]any{"agent_id": routing, "fallback_agent_id": fallback}, nil).Want(http.StatusOK)

	routed := createIssueForTest(t, map[string]any{"title": "routing failover", "project_id": projectID})
	if routed.AssigneeID == nil || *routed.AssigneeID != routing {
		t.Fatalf("todo handed to %v, want the routing agent", routed.AssigneeID)
	}
	var taskID string
	if err := testPool.QueryRow(ctx, `UPDATE agent_task_queue SET status='running', started_at=now()
		WHERE issue_id=$1 AND agent_id=$2 AND status IN ('queued','dispatched') RETURNING id`, routed.ID, routing).Scan(&taskID); err != nil {
		t.Fatalf("routing agent has no run to fail: %v", err)
	}
	if _, err := testHandler.TaskService.FailTask(ctx, util.MustParseUUID(taskID), "provider returned 500", "", "", "", "agent_error.provider_server_error", false, "", ""); err != nil {
		t.Fatal(err)
	}

	issue, err := testHandler.Queries.GetIssue(ctx, util.MustParseUUID(routed.ID))
	if err != nil {
		t.Fatal(err)
	}
	if got := util.UUIDToString(issue.AssigneeID); got != fallback {
		t.Fatalf("issue assigned to %s, want the fallback %s", got, fallback)
	}
	var queued db.AgentTaskQueue
	tasks, err := testHandler.Queries.ListActiveTasksByIssue(ctx, issue.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, task := range tasks {
		if util.UUIDToString(task.AgentID) == fallback {
			queued = task
		}
	}
	if !queued.ID.Valid {
		t.Fatal("no run queued for the fallback agent")
	}
	got := testHandler.claimProjectWorkflow(ctx, issue, &queued)
	if got == nil {
		t.Fatal("fallback claim carries no workflow")
	}
	if strings.Contains(got.Brief, "You are not its assignee") {
		t.Fatalf("fallback got the observer brief:\n%s", got.Brief)
	}
	if !strings.Contains(got.Brief, `"todo" step`) {
		t.Fatalf("brief does not name the step:\n%s", got.Brief)
	}
}
