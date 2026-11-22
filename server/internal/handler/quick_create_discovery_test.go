package handler

import (
	"context"
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/featureflags"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// CHE-9075 (C1, R12): with workspace setting quick_create_discovery on, a
// quick-create run that asks for `investigating` gets it only when the workflow
// the server picks for the issue lists the step; otherwise the issue starts as
// it does today. With the setting off, nothing changes.
func TestQuickCreateDiscoveryRouting(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	withFeatureFlag(t, testHandler, featureflags.ProjectWorkflowsV1, true)

	var agentID, runtimeID string
	if err := testPool.QueryRow(ctx,
		`SELECT id, runtime_id FROM agent WHERE workspace_id = $1 AND name = $2`,
		testWorkspaceID, "Handler Test Agent",
	).Scan(&agentID, &runtimeID); err != nil {
		t.Fatalf("find test agent: %v", err)
	}
	var taskID string
	if err := testPool.QueryRow(ctx,
		`INSERT INTO agent_task_queue (agent_id, runtime_id, status, priority, originator_user_id, accountable_user_id, context)
		 VALUES ($1, $2, 'running', 0, $3, $3, jsonb_build_object('type', 'quick_create', 'workspace_id', $4::text)) RETURNING id`,
		agentID, runtimeID, testUserID, testWorkspaceID,
	).Scan(&taskID); err != nil {
		t.Fatalf("seed quick-create task: %v", err)
	}
	t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE id = $1`, taskID) })

	// Discovery is a custom status of the workspace catalog; a workflow may list it.
	insertCustomStatus(t, "investigating", "started", 900, false)
	withDiscovery := createTestWorkflow(t, "Discovery on "+workflowTestSuffix(), "todo", "investigating")
	withoutDiscovery := createTestWorkflow(t, "Discovery off "+workflowTestSuffix(), "todo")
	projectWith := createWorkflowTestProject(t, "Discovery project with")
	projectWithout := createWorkflowTestProject(t, "Discovery project without")
	for project, wf := range map[string]string{projectWith: withDiscovery.ID, projectWithout: withoutDiscovery.ID} {
		setProjectWorkflow(t, project, map[string]any{"workflow_id": wf}).Want(http.StatusOK)
	}

	var previous []byte
	dbfx.QueryRow(t, `SELECT settings FROM workspace WHERE id=$1`, testWorkspaceID).Scan(&previous)
	t.Cleanup(func() {
		dbfx.Exec(t, `UPDATE workspace SET settings=$2::jsonb WHERE id=$1`, testWorkspaceID, string(previous))
	})
	setSettings := func(discovery string, byType string) {
		dbfx.Exec(t, `UPDATE workspace SET settings=$2::jsonb WHERE id=$1`, testWorkspaceID,
			`{"quick_create_discovery":"`+discovery+`","workflow_by_type":`+byType+`}`)
	}

	create := func(body map[string]any, asQuickCreate bool) *testutil.Response {
		if asQuickCreate {
			body["origin_type"], body["origin_id"] = "quick_create", taskID
		}
		req := workflowRequest("POST", "/api/issues", body)
		req.Header.Set("X-Agent-ID", agentID)
		req.Header.Set("X-Task-ID", taskID)
		return testutil.Call(t, testHandler.CreateIssue, req)
	}
	status := func(r *testutil.Response) string {
		t.Helper()
		var got IssueResponse
		r.Want(http.StatusCreated).JSON(&got)
		t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM issue WHERE id = $1`, got.ID) })
		return got.Status
	}
	unclear := func(project string, title string) map[string]any {
		return map[string]any{"title": title + " " + workflowTestSuffix(), "project_id": project, "status": "investigating"}
	}

	toDiscovery := `{"design":"` + withDiscovery.ID + `"}`
	toNoDiscovery := `{"design":"` + withoutDiscovery.ID + `"}`

	t.Run("on: workflow with the step keeps an unclear request in Discovery", func(t *testing.T) {
		setSettings("on", `{}`)
		if got := status(create(unclear(projectWith, "feat(x): vague"), true)); got != "investigating" {
			t.Fatalf("status = %q, want investigating", got)
		}
	})
	t.Run("on: workflow without the step starts the issue as today", func(t *testing.T) {
		setSettings("on", `{}`)
		if got := status(create(unclear(projectWithout, "feat(x): vague"), true)); got != "todo" {
			t.Fatalf("status = %q, want todo", got)
		}
	})
	t.Run("on: a clear request keeps the default status", func(t *testing.T) {
		setSettings("on", `{}`)
		body := unclear(projectWith, "feat(x): clear")
		delete(body, "status")
		if got := status(create(body, true)); got == "investigating" {
			t.Fatalf("clear request was filed in Discovery")
		}
	})
	t.Run("on: mixed title types judge the workflow the title picks", func(t *testing.T) {
		// Project workflow lacks the step, the title-type workflow has it.
		setSettings("on", toDiscovery)
		if got := status(create(unclear(projectWithout, "design(x): vague"), true)); got != "investigating" {
			t.Fatalf("routed to a workflow with the step: status = %q, want investigating", got)
		}
		// Project workflow has the step, the title-type workflow lacks it.
		setSettings("on", toNoDiscovery)
		if got := status(create(unclear(projectWith, "design(x): vague"), true)); got != "todo" {
			t.Fatalf("routed to a workflow without the step: status = %q, want todo", got)
		}
		// An unrouted title in the same workspace still follows its project.
		if got := status(create(unclear(projectWith, "feat(x): vague"), true)); got != "investigating" {
			t.Fatalf("unrouted title: status = %q, want investigating", got)
		}
	})
	t.Run("off: nothing changes", func(t *testing.T) {
		setSettings("off", `{}`)
		if got := status(create(unclear(projectWith, "feat(x): vague"), true)); got != "investigating" {
			t.Fatalf("explicit status on a workflow with the step: status = %q, want investigating", got)
		}
		create(unclear(projectWithout, "feat(x): vague"), true).Want(http.StatusBadRequest)
	})
	t.Run("on: only a quick-create origin is downgraded", func(t *testing.T) {
		setSettings("on", `{}`)
		create(unclear(projectWithout, "feat(x): vague"), false).Want(http.StatusBadRequest)
	})
}
