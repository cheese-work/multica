package handler

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// CHE-7734 (slice of CHE-7658): an agent's move of an issue into a project that
// follows Quick, and its switch of a project to Quick, open an owner card
// instead of the budget_rebinding_requires_interactive refusal. The card seals
// the bindings; the executor recomputes them under the locks and ends the
// action as stale_base on any difference. A member still moves at once.

const (
	issueProjectQuickType    = "issue.project.quick_set"
	projectWorkflowQuickType = "project.workflow.quick_set"
)

type quickMoveFixture struct {
	escalateFixture
	quickProj, fullProj string
}

func newQuickMoveFixture(t *testing.T) quickMoveFixture {
	t.Helper()
	f := quickMoveFixture{escalateFixture: newEscalateFixture(t)}
	// Equal budgets: no move below widens one, so only the Quick gate can ask.
	dbfx.Exec(t, `UPDATE issue_workflow SET run_budget_since = now() - interval '1 hour', max_runs_per_issue = 10 WHERE id = ANY($1::uuid[])`, []string{f.full, f.quick})
	f.quickProj = createWorkflowTestProject(t, "Quick project "+workflowTestSuffix())
	setProjectWorkflow(t, f.quickProj, map[string]any{"workflow_id": f.quick}).Want(http.StatusOK)
	f.fullProj = createWorkflowTestProject(t, "Full project "+workflowTestSuffix())
	setProjectWorkflow(t, f.fullProj, map[string]any{"workflow_id": f.full}).Want(http.StatusOK)
	return f
}

func issueProjectOf(t *testing.T, id string) string {
	t.Helper()
	var p *string
	dbfx.QueryRow(t, `SELECT project_id::text FROM issue WHERE id = $1`, id).Scan(&p)
	if p == nil {
		return ""
	}
	return *p
}

func pendingActionsOfType(t *testing.T, actionType, issueID string) int {
	t.Helper()
	return dbfx.Count(t, `SELECT count(*) FROM decision_action a JOIN decision_request r ON r.id = a.decision_request_id
		WHERE a.action_type = $1 AND a.state = 'pending' AND r.issue_id = $2`, actionType, issueID)
}

// agentMoveIssue is `multica issue update <id> --project <project>` from an agent run.
func agentMoveIssue(t *testing.T, id string, body map[string]any, source string) *testutil.Response {
	t.Helper()
	req := withURLParam(withActorSource(newRequest("PATCH", "/api/issues/"+id, body), source), "id", id)
	return testutil.Call(t, testHandler.UpdateIssue, req)
}

// agentSwitchProject is a project workflow switch from the agent run taskID.
func agentSwitchProject(t *testing.T, project, taskID string, body map[string]any) *testutil.Response {
	t.Helper()
	req := withActorSource(workflowRequest("POST", "/api/projects/"+project+"/workflow", body), "task_token")
	if taskID != "" {
		req.Header.Set("X-Task-ID", taskID)
		req.Header.Set("X-Agent-ID", seededReadyAgentID(t))
	}
	return testutil.Call(t, testHandler.SetProjectWorkflow, withURLParam(req, "id", project))
}

func (f quickMoveFixture) openIssueMoveCard(t *testing.T, issueID string) openedWorkflowSetCard {
	t.Helper()
	var opened workflowSetOpened
	agentMoveIssue(t, issueID, map[string]any{"project_id": f.quickProj}, "task_token").Want(http.StatusAccepted).JSON(&opened)
	if opened.ActionID == "" || opened.ApproveURL == "" {
		t.Fatalf("response %+v lacks the action id or the approve URL", opened)
	}
	return adoptCard(t, opened)
}

func TestAgentIssueMoveIntoQuickProjectOpensTheOwnerCard(t *testing.T) {
	f := newQuickMoveFixture(t)
	id := dbfx.Issue(t, "into quick "+workflowTestSuffix(), testutil.Cols{"status": "in_progress", "project_id": f.fullProj})

	card := f.openIssueMoveCard(t, id)
	if got := pendingActionsOfType(t, issueProjectQuickType, id); got != 1 {
		t.Fatalf("pending %s actions = %d, want 1", issueProjectQuickType, got)
	}
	if got := issueProjectOf(t, id); got != f.fullProj {
		t.Fatalf("an unapproved agent move changed the project to %s", got)
	}

	var again workflowSetOpened
	agentMoveIssue(t, id, map[string]any{"project_id": f.quickProj}, "task_token").Want(http.StatusAccepted).JSON(&again)
	if again.ActionID != card.ActionID {
		t.Fatalf("a repeat ask opened action %s, want the open one %s", again.ActionID, card.ActionID)
	}
}

func TestApproveQuickIssueMoveAppliesTheMove(t *testing.T) {
	f := newQuickMoveFixture(t)
	id := dbfx.Issue(t, "approve into quick "+workflowTestSuffix(), testutil.Cols{"status": "in_progress", "project_id": f.fullProj})
	card := f.openIssueMoveCard(t, id)

	out := card.approve(t)
	if out.State != "executed" {
		t.Fatalf("outcome = %+v, want executed", out)
	}
	if got := issueProjectOf(t, id); got != f.quickProj {
		t.Fatalf("project = %s, want the Quick project %s", got, f.quickProj)
	}
	if got := len(escalateActivities(t, id, "project_changed")); got != 1 {
		t.Fatalf("project_changed activities = %d, want 1", got)
	}
}

// Each change the card did not seal fails the approval with stale_base and
// moves nothing.
func TestApproveQuickIssueMoveFailsStaleBase(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(t *testing.T, f quickMoveFixture, issueID string)
	}{
		{"target project switched off Quick", func(t *testing.T, f quickMoveFixture, _ string) {
			setProjectWorkflow(t, f.quickProj, map[string]any{"workflow_id": f.other}).Want(http.StatusOK)
		}},
		{"target workflow cap edited", func(t *testing.T, f quickMoveFixture, _ string) {
			dbfx.Exec(t, `UPDATE issue_workflow SET max_runs_per_issue = 9 WHERE id = $1`, f.quick)
		}},
		{"issue moved to another project", func(t *testing.T, f quickMoveFixture, issueID string) {
			other := createWorkflowTestProject(t, "Other project "+workflowTestSuffix())
			dbfx.Exec(t, `UPDATE issue SET project_id = $2 WHERE id = $1`, issueID, other)
		}},
		{"issue got its own workflow", func(t *testing.T, f quickMoveFixture, issueID string) {
			setIssueOverride(t, issueID, f.other)
		}},
		{"issue's budget override raised", func(t *testing.T, f quickMoveFixture, issueID string) {
			dbfx.Exec(t, `UPDATE issue SET max_runs_override = 40 WHERE id = $1`, issueID)
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newQuickMoveFixture(t)
			id := dbfx.Issue(t, "stale "+workflowTestSuffix(), testutil.Cols{"status": "in_progress", "project_id": f.fullProj})
			card := f.openIssueMoveCard(t, id)
			c.mutate(t, f, id)
			before := issueProjectOf(t, id)
			out := card.approve(t)
			if out.State != "failed" || out.ErrorCode != staleBaseCode {
				t.Fatalf("outcome = %+v, want failed/%s", out, staleBaseCode)
			}
			if got := issueProjectOf(t, id); got != before {
				t.Fatalf("a stale approval moved the issue to %s", got)
			}
		})
	}
}

func TestQuickIssueMoveStaysImmediateForMembersAndRefusedForOtherAgentCredentials(t *testing.T) {
	f := newQuickMoveFixture(t)

	t.Run("member move applies at once", func(t *testing.T) {
		id := dbfx.Issue(t, "member into quick "+workflowTestSuffix(), testutil.Cols{"status": "in_progress", "project_id": f.fullProj})
		agentMoveIssue(t, id, map[string]any{"project_id": f.quickProj}, "interactive_jwt").Want(http.StatusOK)
		if got := issueProjectOf(t, id); got != f.quickProj {
			t.Fatalf("project = %s, want %s", got, f.quickProj)
		}
		if got := pendingActionsOfType(t, issueProjectQuickType, id); got != 0 {
			t.Fatalf("a member move opened %d cards", got)
		}
	})
	t.Run("a PAT or CLI session is still refused", func(t *testing.T) {
		for _, source := range []string{"pat", "cli_jwt"} {
			id := dbfx.Issue(t, "refused "+workflowTestSuffix(), testutil.Cols{"status": "in_progress", "project_id": f.fullProj})
			wantRebindRefused(t, agentMoveIssue(t, id, map[string]any{"project_id": f.quickProj}, source), "issue project move", source)
			if got := issueProjectOf(t, id); got != f.fullProj {
				t.Fatalf("%s: a refused move changed the project to %s", source, got)
			}
		}
	})
	t.Run("an agent move that is not into Quick needs no card", func(t *testing.T) {
		id := dbfx.Issue(t, "out of quick "+workflowTestSuffix(), testutil.Cols{"status": "in_progress", "project_id": f.quickProj})
		agentMoveIssue(t, id, map[string]any{"project_id": f.fullProj}, "task_token").Want(http.StatusOK)
	})
	t.Run("other fields in the same request are not dropped silently", func(t *testing.T) {
		id := dbfx.Issue(t, "alone "+workflowTestSuffix(), testutil.Cols{"status": "in_progress", "project_id": f.fullProj})
		res := agentMoveIssue(t, id, map[string]any{"project_id": f.quickProj, "title": "renamed"}, "task_token").Want(http.StatusConflict)
		if !strings.Contains(res.Text(), "quick_move_alone") {
			t.Fatalf("409 without the quick_move_alone code: %s", res.Text())
		}
		if got := pendingActionsOfType(t, issueProjectQuickType, id); got != 0 {
			t.Fatalf("a refused request opened %d cards", got)
		}
	})
}

func TestIssueMoveCardPreviewShowsProjectAndWorkflowBeforeAndAfter(t *testing.T) {
	f := newQuickMoveFixture(t)
	id := dbfx.Issue(t, "preview "+workflowTestSuffix(), testutil.Cols{"status": "in_progress", "project_id": f.fullProj})
	card := f.openIssueMoveCard(t, id)
	action := testHandler.decisionCardAction(context.Background(), parseUUID(testWorkspaceID), parseUUID(card.RequestID))
	fields := previewFields(t, action)
	assertPreviewDigest(t, action.Preview)
	if got := fields["Workflow"]; got[0] == "" || got[1] != "Quick" {
		t.Fatalf("Workflow = %v; want the full workflow, then Quick", got)
	}
	if got := fields["Project"]; got[0] == "" || got[1] == "" || got[0] == got[1] {
		t.Fatalf("Project = %v; want two different names", got)
	}
}

// projectSwitchFixture is a Default project with two issues and the agent run
// that asks for the switch, which hosts the owner card.
type projectSwitchFixture struct {
	quickMoveFixture
	project, taskID, hostIssue string
}

func newProjectSwitchFixture(t *testing.T) projectSwitchFixture {
	t.Helper()
	f := projectSwitchFixture{quickMoveFixture: newQuickMoveFixture(t)}
	f.project = createWorkflowTestProject(t, "Switch project "+workflowTestSuffix())
	f.hostIssue = dbfx.Issue(t, "host "+workflowTestSuffix(), testutil.Cols{"status": "in_progress", "project_id": f.project})
	dbfx.Issue(t, "backlog issue "+workflowTestSuffix(), testutil.Cols{"status": "backlog", "project_id": f.project})
	f.taskID = createHandlerTestTaskForAgentOnIssue(t, seededReadyAgentID(t), f.hostIssue)
	return f
}

func (f projectSwitchFixture) body() map[string]any {
	return map[string]any{"workflow_id": f.quick, "status_mapping": map[string]string{"backlog": "todo"}}
}

func (f projectSwitchFixture) openCard(t *testing.T) openedWorkflowSetCard {
	t.Helper()
	var opened workflowSetOpened
	agentSwitchProject(t, f.project, f.taskID, f.body()).Want(http.StatusAccepted).JSON(&opened)
	if opened.ActionID == "" || opened.ApproveURL == "" {
		t.Fatalf("response %+v lacks the action id or the approve URL", opened)
	}
	return adoptCard(t, opened)
}

func projectWorkflowOf(t *testing.T, project string) string {
	t.Helper()
	var got string
	dbfx.QueryRow(t, `SELECT COALESCE(workflow_id::text, '-') FROM project WHERE id = $1`, project).Scan(&got)
	return got
}

func TestAgentProjectSwitchToQuickOpensTheOwnerCard(t *testing.T) {
	f := newProjectSwitchFixture(t)
	f.openCard(t)
	if got := pendingActionsOfType(t, projectWorkflowQuickType, f.hostIssue); got != 1 {
		t.Fatalf("pending %s actions = %d, want 1", projectWorkflowQuickType, got)
	}
	if got := projectWorkflowOf(t, f.project); got != "-" {
		t.Fatalf("an unapproved agent switch changed the project workflow to %s", got)
	}
	if got := dbfx.Count(t, `SELECT count(*) FROM issue WHERE project_id = $1 AND status = 'backlog'`, f.project); got != 1 {
		t.Fatalf("backlog issues = %d after an unapproved switch, want the one still there", got)
	}
}

func TestApproveQuickProjectSwitchAppliesTheSwitchAndTheRemap(t *testing.T) {
	f := newProjectSwitchFixture(t)
	card := f.openCard(t)
	out := card.approve(t)
	if out.State != "executed" {
		t.Fatalf("outcome = %+v, want executed", out)
	}
	if got := projectWorkflowOf(t, f.project); got != f.quick {
		t.Fatalf("project workflow = %s, want Quick %s", got, f.quick)
	}
	if got := dbfx.Count(t, `SELECT count(*) FROM issue WHERE project_id = $1 AND status = 'backlog'`, f.project); got != 0 {
		t.Fatalf("%d issues still on backlog; the mapping sends them to todo", got)
	}
	if got := dbfx.Count(t, `SELECT count(*) FROM workspace_audit_event WHERE kind = 'project.workflow.update' AND target_id = $1`, f.project); got != 1 {
		t.Fatalf("project.workflow.update audit rows = %d, want 1", got)
	}
}

func TestApproveQuickProjectSwitchFailsStaleBase(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(t *testing.T, f projectSwitchFixture)
	}{
		{"project switched meanwhile", func(t *testing.T, f projectSwitchFixture) {
			setProjectWorkflow(t, f.project, map[string]any{"workflow_id": f.other}).Want(http.StatusOK)
		}},
		{"target workflow cap edited", func(t *testing.T, f projectSwitchFixture) {
			dbfx.Exec(t, `UPDATE issue_workflow SET max_runs_per_issue = 9 WHERE id = $1`, f.quick)
		}},
		{"an issue joined the project", func(t *testing.T, f projectSwitchFixture) {
			dbfx.Issue(t, "late "+workflowTestSuffix(), testutil.Cols{"status": "backlog", "project_id": f.project})
		}},
		{"an issue left the project", func(t *testing.T, f projectSwitchFixture) {
			dbfx.Exec(t, `UPDATE issue SET project_id = NULL WHERE project_id = $1 AND status = 'backlog'`, f.project)
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newProjectSwitchFixture(t)
			card := f.openCard(t)
			c.mutate(t, f)
			before := projectWorkflowOf(t, f.project)
			out := card.approve(t)
			if out.State != "failed" || out.ErrorCode != staleBaseCode {
				t.Fatalf("outcome = %+v, want failed/%s", out, staleBaseCode)
			}
			if got := projectWorkflowOf(t, f.project); got != before {
				t.Fatalf("a stale approval switched the project to %s", got)
			}
		})
	}
}

func TestQuickProjectSwitchStaysImmediateForMembersAndAsksForAMapping(t *testing.T) {
	t.Run("member switch applies at once", func(t *testing.T) {
		f := newProjectSwitchFixture(t)
		setProjectWorkflow(t, f.project, f.body()).Want(http.StatusOK)
		if got := projectWorkflowOf(t, f.project); got != f.quick {
			t.Fatalf("project workflow = %s, want %s", got, f.quick)
		}
		if got := pendingActionsOfType(t, projectWorkflowQuickType, f.hostIssue); got != 0 {
			t.Fatalf("a member switch opened %d cards", got)
		}
	})
	t.Run("an agent switch without the mapping asks for it, no card", func(t *testing.T) {
		f := newProjectSwitchFixture(t)
		res := agentSwitchProject(t, f.project, f.taskID, map[string]any{"workflow_id": f.quick}).Want(http.StatusConflict)
		if !strings.Contains(res.Text(), "workflow_status_mapping_required") {
			t.Fatalf("409 without workflow_status_mapping_required: %s", res.Text())
		}
		if got := pendingActionsOfType(t, projectWorkflowQuickType, f.hostIssue); got != 0 {
			t.Fatalf("a refused switch opened %d cards", got)
		}
	})
	t.Run("an agent call with no run to host the card is refused", func(t *testing.T) {
		f := newProjectSwitchFixture(t)
		wantRebindRefused(t, agentSwitchProject(t, f.project, "", f.body()), "project workflow switch", "task_token")
		if got := projectWorkflowOf(t, f.project); got != "-" {
			t.Fatalf("a refused switch changed the project workflow to %s", got)
		}
	})
	t.Run("a PAT switch is still refused", func(t *testing.T) {
		f := newProjectSwitchFixture(t)
		req := withActorSource(workflowRequest("POST", "/api/projects/"+f.project+"/workflow", f.body()), "pat")
		wantRebindRefused(t, testutil.Call(t, testHandler.SetProjectWorkflow, withURLParam(req, "id", f.project)), "project workflow switch", "pat")
	})
	t.Run("an agent switch that is not to Quick needs no card", func(t *testing.T) {
		f := newProjectSwitchFixture(t)
		agentSwitchProject(t, f.project, f.taskID, map[string]any{"workflow_id": f.full, "status_mapping": map[string]string{"backlog": "todo"}}).Want(http.StatusOK)
	})
}

func TestProjectSwitchCardPreviewShowsTheSwitch(t *testing.T) {
	f := newProjectSwitchFixture(t)
	card := f.openCard(t)
	action := testHandler.decisionCardAction(context.Background(), parseUUID(testWorkspaceID), parseUUID(card.RequestID))
	fields := previewFields(t, action)
	assertPreviewDigest(t, action.Preview)
	if got := fields["Workflow"]; got[1] != "Quick" {
		t.Fatalf("Workflow = %v; want it to end at Quick", got)
	}
	if got := fields["Issues moved to another status"]; got[1] != "1" {
		t.Fatalf("Issues moved to another status = %v; want 1", got)
	}
	if _, found := fields["Project"]; !found {
		t.Fatalf("preview lacks the project: %v", fields)
	}
}
