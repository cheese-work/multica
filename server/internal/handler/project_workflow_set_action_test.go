package handler

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// CHE-10137: an agent run's switch of a project to a workflow that widens the
// run budget opens an owner card instead of the 403, and one card covers many
// projects. Approval switches all of them or none; the sealed bindings are
// recomputed under the locks and any difference ends the card as stale_base.

const projectWorkflowSetTestType = "project.workflow.set"

type projectSetFixture struct {
	escalateFixture
	// Both are budgeted at 25 runs; wide's window starts later, so narrow to wide
	// widens and wide to narrow does not.
	narrow, wide string
	hostIssue    string
	taskID       string
}

func newProjectSetFixture(t *testing.T) projectSetFixture {
	t.Helper()
	f := projectSetFixture{escalateFixture: newEscalateFixture(t)}
	steps := []string{"todo", "in_progress", "in_review"}
	f.narrow = createTestWorkflow(t, "narrow-"+workflowTestSuffix(), steps...).ID
	f.wide = createTestWorkflow(t, "wide-"+workflowTestSuffix(), steps...).ID
	dbfx.Exec(t, `UPDATE issue_workflow SET run_budget_since = now() - interval '1 hour', max_runs_per_issue = 25 WHERE id = $1`, f.narrow)
	dbfx.Exec(t, `UPDATE issue_workflow SET run_budget_since = now(), max_runs_per_issue = 25 WHERE id = $1`, f.wide)
	host := createWorkflowTestProject(t, "Set host project "+workflowTestSuffix())
	f.hostIssue = dbfx.Issue(t, "set host "+workflowTestSuffix(), testutil.Cols{"status": "in_progress", "project_id": host})
	f.taskID = createHandlerTestTaskForAgentOnIssue(t, seededReadyAgentID(t), f.hostIssue)
	return f
}

// narrowProject is a project on the budgeted workflow with one issue.
func (f projectSetFixture) narrowProject(t *testing.T) string {
	t.Helper()
	project := createWorkflowTestProject(t, "Set project "+workflowTestSuffix())
	setProjectWorkflow(t, project, map[string]any{"workflow_id": f.narrow}).Want(http.StatusOK)
	dbfx.Issue(t, "set issue "+workflowTestSuffix(), testutil.Cols{"status": "in_progress", "project_id": project})
	return project
}

func (f projectSetFixture) agentBatch(t *testing.T, source string, projects []string) *testutil.Response {
	t.Helper()
	req := withActorSource(workflowRequest("POST", "/api/projects/workflow", map[string]any{"project_ids": projects, "workflow_id": f.wide}), source)
	if source == "task_token" {
		req.Header.Set("X-Task-ID", f.taskID)
		req.Header.Set("X-Agent-ID", seededReadyAgentID(t))
	}
	return testutil.Call(t, testHandler.SetProjectsWorkflow, req)
}

func (f projectSetFixture) openBatchCard(t *testing.T, projects []string) openedWorkflowSetCard {
	t.Helper()
	var opened workflowSetOpened
	f.agentBatch(t, "task_token", projects).Want(http.StatusAccepted).JSON(&opened)
	if opened.ActionID == "" || opened.ApproveURL == "" {
		t.Fatalf("response %+v lacks the action id or the approve URL", opened)
	}
	return adoptCard(t, opened)
}

func TestAgentWideningProjectSwitchOpensTheCardAndChangesNothing(t *testing.T) {
	f := newProjectSetFixture(t)
	project := f.narrowProject(t)
	var opened workflowSetOpened
	agentSwitchProject(t, project, f.taskID, map[string]any{"workflow_id": f.wide}).Want(http.StatusAccepted).JSON(&opened)
	adoptCard(t, opened)
	if got := pendingActionsOfType(t, projectWorkflowSetTestType, f.hostIssue); got != 1 {
		t.Fatalf("pending %s actions = %d, want 1", projectWorkflowSetTestType, got)
	}
	if got := projectWorkflowOf(t, project); got != f.narrow {
		t.Fatalf("an unapproved agent switch changed the project workflow to %s", got)
	}
}

func TestApproveProjectWorkflowSetSwitchesEveryProjectWithoutReassigningOrStartingRuns(t *testing.T) {
	f := newProjectSetFixture(t)
	projects := []string{f.narrowProject(t), f.narrowProject(t), f.narrowProject(t)}
	assignees := func() int {
		return dbfx.Count(t, `SELECT count(*) FROM issue WHERE project_id = ANY($1::uuid[]) AND assignee_id IS NOT NULL`, projects)
	}
	queued := func() int {
		return dbfx.Count(t, `SELECT count(*) FROM agent_task_queue q JOIN issue i ON i.id = q.issue_id WHERE i.project_id = ANY($1::uuid[])`, projects)
	}
	audits := func() int {
		return dbfx.Count(t, `SELECT count(*) FROM workspace_audit_event WHERE kind = 'project.workflow.update' AND target_id = ANY($1::uuid[])`, projects)
	}
	assigneesBefore, queuedBefore, auditsBefore := assignees(), queued(), audits()

	card := f.openBatchCard(t, projects)
	if got := pendingActionsOfType(t, projectWorkflowSetTestType, f.hostIssue); got != 1 {
		t.Fatalf("one batch opened %d cards, want 1", got)
	}
	for _, project := range projects {
		if got := projectWorkflowOf(t, project); got != f.narrow {
			t.Fatalf("an unapproved batch changed project %s to %s", project, got)
		}
	}

	if out := card.approve(t); out.State != "executed" {
		t.Fatalf("outcome = %+v, want executed", out)
	}
	for _, project := range projects {
		if got := projectWorkflowOf(t, project); got != f.wide {
			t.Fatalf("project %s workflow = %s, want %s", project, got, f.wide)
		}
	}
	if got := assignees(); got != assigneesBefore {
		t.Fatalf("assigned issues %d -> %d: a switch must not reassign", assigneesBefore, got)
	}
	if got := queued(); got != queuedBefore {
		t.Fatalf("queued runs %d -> %d: a switch must not start runs", queuedBefore, got)
	}
	if got := audits() - auditsBefore; got != len(projects) {
		t.Fatalf("new project.workflow.update audit rows = %d, want %d", got, len(projects))
	}
}

func TestApproveProjectWorkflowSetFailsStaleBaseAndChangesNoProject(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(t *testing.T, f projectSetFixture, last string)
	}{
		{"a project switched workflow meanwhile", func(t *testing.T, f projectSetFixture, last string) {
			setProjectWorkflow(t, last, map[string]any{"workflow_id": f.other}).Want(http.StatusOK)
		}},
		{"the target workflow's budget changed", func(t *testing.T, f projectSetFixture, _ string) {
			dbfx.Exec(t, `UPDATE issue_workflow SET max_runs_per_issue = 9 WHERE id = $1`, f.narrow)
		}},
		{"an issue joined a project and needs a status move", func(t *testing.T, f projectSetFixture, last string) {
			createTestCustomStatus(t, offQuickStatus, "started")
			dbfx.Issue(t, "late "+workflowTestSuffix(), testutil.Cols{"status": offQuickStatus, "project_id": last})
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newProjectSetFixture(t)
			projects := []string{f.narrowProject(t), f.narrowProject(t)}
			card := f.openBatchCard(t, projects)
			c.mutate(t, f, projects[1])
			state := func() [2]string { return [2]string{projectWorkflowOf(t, projects[0]), projectWorkflowOf(t, projects[1])} }
			before := state()
			out := card.approve(t)
			if out.State != "failed" || out.ErrorCode != staleBaseCode {
				t.Fatalf("outcome = %+v, want failed/%s", out, staleBaseCode)
			}
			if got := state(); got != before {
				t.Fatalf("a stale approval changed the projects: %v -> %v", before, got)
			}
		})
	}
}

func TestProjectWorkflowSetRefusesOtherCredentialsAndNamesTheCardPath(t *testing.T) {
	f := newProjectSetFixture(t)
	a, b := f.narrowProject(t), f.narrowProject(t)
	for _, source := range []string{"pat", "cloud_pat", "cli_jwt"} {
		single := withActorSource(workflowRequest("POST", "/api/projects/"+a+"/workflow", map[string]any{"workflow_id": f.wide}), source)
		res := testutil.Call(t, testHandler.SetProjectWorkflow, withURLParam(single, "id", a))
		wantRebindRefused(t, res, "project workflow switch", source)
		if !strings.Contains(res.Text(), "multica project workflow set") {
			t.Fatalf("%s: 403 does not name the card path: %s", source, res.Text())
		}
		wantRebindRefused(t, f.agentBatch(t, source, []string{a, b}), "project workflow batch", source)
	}
	t.Run("a task token with no run to host the card", func(t *testing.T) {
		single := withActorSource(workflowRequest("POST", "/api/projects/"+a+"/workflow", map[string]any{"workflow_id": f.wide}), "task_token")
		wantRebindRefused(t, testutil.Call(t, testHandler.SetProjectWorkflow, withURLParam(single, "id", a)), "project workflow switch", "task_token")
	})
	for _, project := range []string{a, b} {
		if got := projectWorkflowOf(t, project); got != f.narrow {
			t.Fatalf("a refused switch changed project %s to %s", project, got)
		}
	}
}

func TestProjectWorkflowSetLeavesNarrowingAndQuickSwitchesAlone(t *testing.T) {
	// Each subtest builds its own fixture: both fixtures create the workflow
	// named Quick, which a workspace holds once.
	t.Run("a narrowing switch is a direct write for an agent", func(t *testing.T) {
		f := newProjectSetFixture(t)
		project := createWorkflowTestProject(t, "Narrowing project "+workflowTestSuffix())
		setProjectWorkflow(t, project, map[string]any{"workflow_id": f.wide}).Want(http.StatusOK)
		agentSwitchProject(t, project, f.taskID, map[string]any{"workflow_id": f.narrow}).Want(http.StatusOK)
		if got := projectWorkflowOf(t, project); got != f.narrow {
			t.Fatalf("project workflow = %s, want %s", got, f.narrow)
		}
		if got := pendingActionsOfType(t, projectWorkflowSetTestType, f.hostIssue); got != 0 {
			t.Fatalf("a narrowing switch opened %d cards", got)
		}
	})
	t.Run("a batch that needs no approval writes nothing and says so", func(t *testing.T) {
		f := newProjectSetFixture(t)
		a := createWorkflowTestProject(t, "Direct a "+workflowTestSuffix())
		b := createWorkflowTestProject(t, "Direct b "+workflowTestSuffix())
		for _, p := range []string{a, b} {
			setProjectWorkflow(t, p, map[string]any{"workflow_id": f.wide}).Want(http.StatusOK)
		}
		req := withActorSource(workflowRequest("POST", "/api/projects/workflow", map[string]any{"project_ids": []string{a, b}, "workflow_id": f.narrow}), "task_token")
		req.Header.Set("X-Task-ID", f.taskID)
		req.Header.Set("X-Agent-ID", seededReadyAgentID(t))
		res := testutil.Call(t, testHandler.SetProjectsWorkflow, req).Want(http.StatusConflict)
		if !strings.Contains(res.Text(), workflowBatchDirectCode) {
			t.Fatalf("409 without %s: %s", workflowBatchDirectCode, res.Text())
		}
	})
	t.Run("a switch into Quick still opens project.workflow.quick_set", func(t *testing.T) {
		q := newProjectSwitchFixture(t)
		q.openCard(t)
		if got := pendingActionsOfType(t, projectWorkflowQuickType, q.hostIssue); got != 1 {
			t.Fatalf("pending %s actions = %d, want 1", projectWorkflowQuickType, got)
		}
		if got := pendingActionsOfType(t, projectWorkflowSetTestType, q.hostIssue); got != 0 {
			t.Fatalf("a Quick switch opened %d %s cards", got, projectWorkflowSetTestType)
		}
	})
}

func TestProjectWorkflowSetBatchRejectsBadRequests(t *testing.T) {
	f := newProjectSetFixture(t)
	a := f.narrowProject(t)
	t.Run("a project listed twice", func(t *testing.T) { f.agentBatch(t, "task_token", []string{a, a}).Want(http.StatusBadRequest) })
	t.Run("no projects", func(t *testing.T) { f.agentBatch(t, "task_token", nil).Want(http.StatusBadRequest) })
	t.Run("more than 50 projects", func(t *testing.T) {
		ids := make([]string, 51)
		for i := range ids {
			ids[i] = a
		}
		f.agentBatch(t, "task_token", ids).Want(http.StatusBadRequest)
	})
	t.Run("a missing status mapping names the project", func(t *testing.T) {
		createTestCustomStatus(t, offQuickStatus, "started")
		b := f.narrowProject(t)
		dbfx.Issue(t, "off "+workflowTestSuffix(), testutil.Cols{"status": offQuickStatus, "project_id": b})
		res := f.agentBatch(t, "task_token", []string{a, b}).Want(http.StatusConflict)
		if !strings.Contains(res.Text(), "workflow_status_mapping_required") {
			t.Fatalf("409 without workflow_status_mapping_required: %s", res.Text())
		}
		if got := pendingActionsOfType(t, projectWorkflowSetTestType, f.hostIssue); got != 0 {
			t.Fatalf("a refused batch opened %d cards", got)
		}
	})
}

func TestProjectWorkflowSetCardPreviewListsEveryProject(t *testing.T) {
	f := newProjectSetFixture(t)
	projects := []string{f.narrowProject(t), f.narrowProject(t)}
	card := f.openBatchCard(t, projects)
	action := testHandler.decisionCardAction(context.Background(), parseUUID(testWorkspaceID), parseUUID(card.RequestID))
	fields := previewFields(t, action)
	assertPreviewDigest(t, action.Preview)
	if got := fields["Projects"]; got[1] != "2" {
		t.Fatalf("Projects = %v; want 2", got)
	}
	rows := 0
	for label, values := range fields {
		if strings.HasPrefix(label, "Project Set project ") {
			rows++
			if !strings.HasPrefix(values[0], "narrow-") || !strings.HasPrefix(values[1], "wide-") || !strings.Contains(values[0], "cap 25") || strings.Contains(values[1], "cap 25") {
				t.Fatalf("%s = %v; want the narrow workflow with cap 25, then the wide one without it", label, values)
			}
		}
	}
	if rows != 2 {
		t.Fatalf("preview has %d project rows, want 2: %v", rows, fields)
	}
}
