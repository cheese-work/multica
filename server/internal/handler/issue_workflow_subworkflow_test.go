package handler

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/featureflags"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// Sub-workflow step (CHE-2031): entering the step spawns one child on the
// target workflow in the step's stage; the step's children_done edge joins on
// that stage only.

func subWorkflowSteps(targetID string, stage int) []map[string]any {
	return []map[string]any{
		{"status_key": "backlog"},
		{"status_key": "todo"},
		{"status_key": "in_progress",
			"sub_workflow": map[string]any{"workflow_id": targetID, "stage": stage},
			"edges":        []map[string]any{{"to": "in_review", "guard": "children_done"}}},
		{"status_key": "in_review"},
		{"status_key": "blocked"},
	}
}

// newSubWorkflowCase puts one issue on a project whose workflow has a
// sub-workflow step on in_progress; the issue waits at backlog.
func newSubWorkflowCase(t *testing.T, stage int) (c edgeCase, targetID string) {
	t.Helper()
	return newSubWorkflowCaseTo(t, stage, statusSteps("todo", "in_progress", "in_review", "blocked"))
}

// newSubWorkflowCaseTo is newSubWorkflowCase with the target workflow's steps given.
func newSubWorkflowCaseTo(t *testing.T, stage int, targetSteps []map[string]any) (c edgeCase, targetID string) {
	t.Helper()
	seedTestCatalog(t)
	target := createTestWorkflowSteps(t, "Sub target "+workflowTestSuffix(), targetSteps)
	wf := createTestWorkflowSteps(t, "Sub parent "+workflowTestSuffix(), subWorkflowSteps(target.ID, stage))
	projectID := createWorkflowTestProject(t, "Sub project")
	setProjectWorkflow(t, projectID, map[string]any{"workflow_id": wf.ID}).Want(http.StatusOK)
	issueID := mustCreateIssue(t, "sub-workflow parent", "backlog")
	if _, err := testPool.Exec(context.Background(),
		`UPDATE issue SET project_id=$2, assignee_type=NULL, assignee_id=NULL WHERE id=$1`, issueID, projectID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM issue_wakeup WHERE issue_id IN (SELECT id FROM issue WHERE parent_issue_id=$1) OR issue_id=$1`, issueID)
		testPool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE issue_id IN (SELECT id FROM issue WHERE parent_issue_id=$1)`, issueID)
		testPool.Exec(context.Background(), `DELETE FROM issue WHERE parent_issue_id=$1`, issueID)
	})
	return edgeCase{workflowID: wf.ID, issueID: uuidToString(issueID)}, target.ID
}

func (c edgeCase) children(t *testing.T) int {
	t.Helper()
	return dbfx.Count(t, `SELECT count(*) FROM issue WHERE parent_issue_id=$1`, c.issueID)
}

func TestSubWorkflowSpawnsOneChildOnEntry(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	c, targetID := newSubWorkflowCase(t, 2)
	c.enter(t, "in_progress")
	tickEdges(t)
	if got := c.children(t); got != 1 {
		t.Fatalf("entering the sub-workflow step spawned %d children, want 1", got)
	}
	var workflowID string
	var stage int
	dbfx.QueryRow(t, `SELECT workflow_id::text, stage FROM issue WHERE parent_issue_id=$1`, c.issueID).Scan(&workflowID, &stage)
	if workflowID != targetID || stage != 2 {
		t.Fatalf("child is on workflow %s stage %d, want %s stage 2", workflowID, stage, targetID)
	}
	// The watch looks again: the same entry must not spawn a second child.
	for range 2 {
		testPool.Exec(context.Background(), `UPDATE issue_wakeup SET next_fire_at = now() WHERE issue_id=$1 AND system_rule='workflow_edges'`, c.issueID)
		tickEdges(t)
	}
	if got := c.children(t); got != 1 {
		t.Fatalf("re-evaluating the step left %d children, want 1", got)
	}
}

func TestSubWorkflowJoinCountsOnlyItsStage(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	c, targetID := newSubWorkflowCase(t, 2)
	// The step's own child already exists (so nothing spawns), plus a done child
	// in another stage and an open unstaged one: only stage 2 is the join.
	own := mustCreateIssue(t, "stage 2 child", "todo")
	other := mustCreateIssue(t, "other stage child", "done")
	unstaged := mustCreateIssue(t, "unstaged child", "todo")
	for _, row := range []struct {
		id    any
		stage string
		wf    string
	}{{own, "2", targetID}, {other, "1", "NULL"}, {unstaged, "NULL", "NULL"}} {
		q := `UPDATE issue SET parent_issue_id=$2, stage=` + row.stage + `, workflow_id=` + map[bool]string{true: "$3::uuid", false: "NULL"}[row.wf != "NULL"] + ` WHERE id=$1`
		args := []any{row.id, c.issueID}
		if row.wf != "NULL" {
			args = append(args, row.wf)
		}
		if _, err := testPool.Exec(context.Background(), q, args...); err != nil {
			t.Fatal(err)
		}
	}
	c.enter(t, "in_progress")
	tickEdges(t)
	if got := c.status(t); got != "in_progress" {
		t.Fatalf("stage 2 still open: issue at %s, want in_progress", got)
	}
	if got := c.children(t); got != 3 {
		t.Fatalf("a child already on the step's workflow and stage left %d children, want 3", got)
	}
	if _, err := testPool.Exec(context.Background(), `UPDATE issue SET status='done' WHERE id=$1`, own); err != nil {
		t.Fatal(err)
	}
	testPool.Exec(context.Background(), `UPDATE issue_wakeup SET next_fire_at = now() WHERE issue_id=$1 AND system_rule='workflow_edges'`, c.issueID)
	tickEdges(t)
	if got := c.status(t); got != "in_review" {
		t.Fatalf("stage 2 done, an open unstaged child ignored: issue at %s, want in_review", got)
	}
}

func TestSubWorkflowSaveRejectsOtherWorkspaceTarget(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	seedTestCatalog(t)
	withFeatureFlag(t, testHandler, featureflags.ProjectWorkflowsV1, true)
	foreign := foreignWorkspaceWorkflow(t)
	for name, target := range map[string]string{
		"other workspace": foreign,
		"unknown id":      "00000000-0000-4000-8000-000000000001",
	} {
		t.Run(name, func(t *testing.T) {
			var errBody struct {
				Code string `json:"code"`
			}
			testutil.Call(t, testHandler.CreateIssueWorkflow, workflowRequest("POST", "/api/issue-workflows", map[string]any{
				"name":  "Sub bad " + workflowTestSuffix(),
				"steps": subWorkflowSteps(target, 1),
			})).Want(http.StatusBadRequest).JSON(&errBody)
			if errBody.Code != "invalid_workflow" {
				t.Fatalf("code = %q, want invalid_workflow", errBody.Code)
			}
		})
	}
}

func TestSubWorkflowSpawnRefusesOtherWorkspaceTarget(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	c, _ := newSubWorkflowCase(t, 1)
	foreign := foreignWorkspaceWorkflow(t)
	// A stored definition can predate the save-time check: rewrite it directly.
	if _, err := testPool.Exec(context.Background(),
		`UPDATE issue_workflow SET steps = replace(steps::text, (steps->2->'sub_workflow'->>'workflow_id'), $2)::jsonb WHERE id=$1`, c.workflowID, foreign); err != nil {
		t.Fatal(err)
	}
	c.enter(t, "in_progress")
	tickEdges(t)
	if got := c.children(t); got != 0 {
		t.Fatalf("spawned %d children onto another workspace's workflow, want 0", got)
	}
}

// Containment (CHE-2031, option A): a spawned child is never auto-assigned, so
// no run starts for it. Unattended child-agent execution is deferred until the
// spawn carries trusted provenance. The child still exists and the parent's join
// still closes once the child is done. Both a start step with an agent handler
// and one with a human handler leave the child unassigned.
func TestSubWorkflowChildIsLeftUnassigned(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	agentID := seededReadyAgentID(t)
	for name, handler := range map[string]map[string]any{
		"agent": {"type": "agent", "id": agentID},
		"human": {"type": "human_by_role"},
	} {
		t.Run(name, func(t *testing.T) {
			c, _ := newSubWorkflowCaseTo(t, 1, []map[string]any{
				{"status_key": "todo", "handler": handler, "instructions": "Do the sub-task."},
				{"status_key": "in_progress"}, {"status_key": "in_review"}, {"status_key": "blocked"},
			})
			c.enter(t, "in_progress")
			tickEdges(t)
			if got := c.children(t); got != 1 {
				t.Fatalf("spawned %d children, want 1", got)
			}
			var childID, assigneeType, assigneeID string
			dbfx.QueryRow(t, `SELECT id::text, COALESCE(assignee_type, ''), COALESCE(assignee_id::text, '') FROM issue WHERE parent_issue_id=$1`, c.issueID).Scan(&childID, &assigneeType, &assigneeID)
			if assigneeType != "" || assigneeID != "" {
				t.Fatalf("child assignee = %q %q, want none", assigneeType, assigneeID)
			}
			if steps := pendingStepRuns(t, childID, agentID); len(steps) != 0 {
				t.Fatalf("runs queued for the child = %v, want none", steps)
			}
			if _, err := testPool.Exec(context.Background(), `UPDATE issue SET status='done' WHERE id=$1`, childID); err != nil {
				t.Fatal(err)
			}
			testPool.Exec(context.Background(), `UPDATE issue_wakeup SET next_fire_at = now() WHERE issue_id=$1 AND system_rule='workflow_edges'`, c.issueID)
			tickEdges(t)
			if got := c.status(t); got != "in_review" {
				t.Fatalf("child done: parent at %s, want in_review", got)
			}
		})
	}
}

// updateWorkflowSteps saves new steps on a workflow through the API.
func updateWorkflowSteps(t *testing.T, id string, steps []map[string]any) *testutil.Response {
	t.Helper()
	req := withURLParam(workflowRequest("PUT", "/api/issue-workflows/"+id, map[string]any{"steps": steps}), "id", id)
	return testutil.Call(t, testHandler.UpdateIssueWorkflow, req)
}

// A workflow may not run itself as its own sub-workflow, directly (A -> A) or
// through others (A -> B -> A): the spawn would never end.
func TestSubWorkflowSaveRejectsCycles(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	seedTestCatalog(t)
	a := createTestWorkflowSteps(t, "Cycle A "+workflowTestSuffix(), statusSteps("backlog", "todo", "in_progress", "in_review", "blocked"))
	b := createTestWorkflowSteps(t, "Cycle B "+workflowTestSuffix(), statusSteps("backlog", "todo", "in_progress", "in_review", "blocked"))

	t.Run("A to A", func(t *testing.T) {
		var errBody struct {
			Code string `json:"code"`
		}
		updateWorkflowSteps(t, a.ID, subWorkflowSteps(a.ID, 1)).Want(http.StatusBadRequest).JSON(&errBody)
		if errBody.Code != "invalid_workflow" {
			t.Fatalf("code = %q, want invalid_workflow", errBody.Code)
		}
	})
	t.Run("A to B to A", func(t *testing.T) {
		updateWorkflowSteps(t, a.ID, subWorkflowSteps(b.ID, 1)).Want(http.StatusOK)
		var errBody struct {
			Code string `json:"code"`
		}
		updateWorkflowSteps(t, b.ID, subWorkflowSteps(a.ID, 1)).Want(http.StatusBadRequest).JSON(&errBody)
		if errBody.Code != "invalid_workflow" {
			t.Fatalf("code = %q, want invalid_workflow", errBody.Code)
		}
	})
	t.Run("a chain without a cycle saves", func(t *testing.T) {
		c := createTestWorkflowSteps(t, "Cycle C "+workflowTestSuffix(), statusSteps("backlog", "todo", "in_progress", "in_review", "blocked"))
		updateWorkflowSteps(t, b.ID, subWorkflowSteps(c.ID, 1)).Want(http.StatusOK)
	})
}

// A stored definition can predate the save-time check (or be written around
// it): the spawn stops once the issue already sits MaxSubWorkflowDepth
// sub-workflow children deep.
func TestSubWorkflowSpawnStopsAtDepthBound(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	for depth, want := range map[int]int{maxSubWorkflowDepth - 1: 1, maxSubWorkflowDepth: 0} {
		c, targetID := newSubWorkflowCase(t, 1)
		parent := c.issueID
		for level := range depth {
			child := uuidToString(mustCreateIssue(t, fmt.Sprintf("ancestor %d-%d", depth, level), "in_progress"))
			if _, err := testPool.Exec(context.Background(), `UPDATE issue SET workflow_id=$2::uuid, stage=1 WHERE id=$1`, child, targetID); err != nil {
				t.Fatal(err)
			}
			if _, err := testPool.Exec(context.Background(), `UPDATE issue SET parent_issue_id=$2 WHERE id=$1`, parent, child); err != nil {
				t.Fatal(err)
			}
			parent = child
		}
		c.enter(t, "in_progress")
		tickEdges(t)
		if got := c.children(t); got != want {
			t.Fatalf("issue %d sub-workflow levels deep spawned %d children, want %d", depth, got, want)
		}
	}
}

// foreignWorkspaceWorkflow is a workflow that lives in a second workspace.
func foreignWorkspaceWorkflow(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	testPool.Exec(ctx, `DELETE FROM workspace WHERE slug = 'sub-wf-foreign-test'`)
	var wsID, wfID string
	if err := testPool.QueryRow(ctx, `INSERT INTO workspace (name, slug, description, issue_prefix) VALUES ('Sub foreign', 'sub-wf-foreign-test', '', 'SWF') RETURNING id`).Scan(&wsID); err != nil {
		t.Fatalf("create foreign workspace: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM issue_workflow WHERE workspace_id = $1`, wsID)
		testPool.Exec(context.Background(), `DELETE FROM workspace WHERE id = $1`, wsID)
	})
	if err := testPool.QueryRow(ctx, `INSERT INTO issue_workflow (workspace_id, name, description, initial_status_key, steps, owner_id) VALUES ($1, 'Foreign', '', 'todo', '[]'::jsonb, $2) RETURNING id`, wsID, testUserID).Scan(&wfID); err != nil {
		t.Fatalf("create foreign workflow: %v", err)
	}
	return wfID
}
