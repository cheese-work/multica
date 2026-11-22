package handler

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

const projectSetAction = "issue.project.set"

// projectMoveFixture is a pending move of the fixture's issue from the project
// at fromCap to the project at toCap.
type projectMoveFixture struct {
	pendingAction
	fromProject, toProject   string
	fromWorkflow, toWorkflow string
}

// workflowProject creates a project whose workflow caps an issue at cap runs
// and lists the statuses the fixture issues use.
func workflowProject(test *testing.T, title string, cap int) (projectID, workflowID string) {
	test.Helper()
	return workflowProjectWithStatuses(test, title, cap, "backlog", "todo", "in_progress", "done")
}

// workflowProjectWithStatuses is workflowProject for a workflow that lists
// only the given statuses.
func workflowProjectWithStatuses(test *testing.T, title string, cap int, statuses ...string) (projectID, workflowID string) {
	test.Helper()
	steps := make([]map[string]any, 0, len(statuses))
	for _, status := range statuses {
		steps = append(steps, map[string]any{"status_key": status, "handler": map[string]any{"type": "none"}})
	}
	encoded, err := json.Marshal(steps)
	if err != nil {
		test.Fatal(err)
	}
	dbfx.QueryRow(test, `INSERT INTO issue_workflow (workspace_id, name, initial_status_key, steps, max_runs_per_issue, run_budget_since) VALUES ($1, $2, 'todo', $3::jsonb, $4, '2026-01-01T00:00:00Z') RETURNING id`, testWorkspaceID, title+" wf", string(encoded), cap).Scan(&workflowID)
	dbfx.Cleanup(test, `DELETE FROM issue_workflow WHERE id = $1`, workflowID)
	projectID = dbfx.Project(test, title)
	dbfx.Exec(test, `UPDATE project SET workflow_id = $1 WHERE id = $2`, workflowID, projectID)
	return projectID, workflowID
}

func (fixture singleCallFixture) projectSetBody(projectID string, withIssueID bool) map[string]any {
	params := map[string]any{"project_id": projectID}
	if withIssueID {
		params["issue_id"] = fixture.issueID
	}
	return fixture.withAction(map[string]any{"type": projectSetAction, "version": 1, "params": params})
}

func newPendingProjectMove(test *testing.T, fromCap, toCap int) projectMoveFixture {
	test.Helper()
	fixture := newActionFixture(test)
	move := projectMoveFixture{}
	move.fromProject, move.fromWorkflow = workflowProject(test, "Move from", fromCap)
	move.toProject, move.toWorkflow = workflowProject(test, "Move to", toCap)
	dbfx.Exec(test, `UPDATE issue SET project_id = $1 WHERE id = $2`, move.fromProject, fixture.issueID)
	dbfx.Cleanup(test, `UPDATE issue SET project_id = NULL WHERE id = $1`, fixture.issueID)
	dbfx.Cleanup(test, `DELETE FROM activity_log WHERE issue_id = $1`, fixture.issueID)
	var created actionResponse
	fixture.respond(test, fixture.projectSetBody(move.toProject, true), http.StatusCreated, &created)
	dbfx.Cleanup(test, `DELETE FROM decision_answer WHERE decision_request_id = $1`, created.ID)
	move.pendingAction = pendingAction{fixture, created.ID, created.Action.ID}
	return move
}

func (move projectMoveFixture) issueProject(test *testing.T) string {
	test.Helper()
	var projectID string
	dbfx.QueryRow(test, `SELECT COALESCE(project_id::text, '') FROM issue WHERE id = $1`, move.issueID).Scan(&projectID)
	return projectID
}

func (move projectMoveFixture) escalationClass(test *testing.T) string {
	test.Helper()
	var class string
	dbfx.QueryRow(test, `SELECT escalation_class FROM decision_request WHERE id = $1`, move.requestID).Scan(&class)
	return class
}

func (move projectMoveFixture) movedActivities(test *testing.T) int {
	test.Helper()
	return dbfx.Count(test, `SELECT count(*) FROM activity_log WHERE issue_id = $1 AND action = 'project_changed'`, move.issueID)
}

func TestProjectSetActionMovesTheIssueAndRecordsTheApprover(test *testing.T) {
	move := newPendingProjectMove(test, 10, 50)
	out := move.approveAccepted(test)
	if out.State != "executed" {
		test.Fatalf("outcome = %+v; want executed", out)
	}
	if got := move.issueProject(test); got != move.toProject {
		test.Fatalf("issue project = %s; want %s", got, move.toProject)
	}
	var result struct {
		ProjectID  string `json:"project_id"`
		WorkflowID string `json:"workflow_id"`
	}
	if err := json.Unmarshal(out.Result, &result); err != nil || result.ProjectID != move.toProject || result.WorkflowID != move.toWorkflow {
		test.Fatalf("result %s (%v); want project %s workflow %s", out.Result, err, move.toProject, move.toWorkflow)
	}
	var details string
	dbfx.QueryRow(test, `SELECT details::text FROM activity_log WHERE issue_id = $1 AND action = 'project_changed' AND actor_type = 'member' AND actor_id = $2`, move.issueID, move.humanID).Scan(&details)
	var recorded struct {
		From     string `json:"from_project_id"`
		To       string `json:"to_project_id"`
		ActionID string `json:"action_id"`
	}
	if err := json.Unmarshal([]byte(details), &recorded); err != nil || recorded.From != move.fromProject || recorded.To != move.toProject || recorded.ActionID != move.actionID {
		test.Fatalf("activity details %s (%v); want %s -> %s action %s", details, err, move.fromProject, move.toProject, move.actionID)
	}
}

func TestProjectSetActionRejectLeavesTheIssueUnchanged(test *testing.T) {
	move := newPendingProjectMove(test, 10, 50)
	move.reject(test, withVerifier(acceptingVerifier), http.StatusOK)
	if got := move.issueProject(test); got != move.fromProject {
		test.Fatalf("issue project = %s after reject; want %s", got, move.fromProject)
	}
	if move.movedActivities(test) != 0 {
		test.Fatal("a rejected move left an activity row")
	}
}

func TestProjectSetActionRefusesAStaleCard(test *testing.T) {
	move := newPendingProjectMove(test, 10, 50)
	other, _ := workflowProject(test, "Moved elsewhere", 10)
	dbfx.Exec(test, `UPDATE issue SET project_id = $1 WHERE id = $2`, other, move.issueID)
	out := move.approveAccepted(test)
	if out.State != "failed" || out.ErrorCode != "stale_project" {
		test.Fatalf("outcome = %+v; want failed/stale_project", out)
	}
	if got := move.issueProject(test); got != other {
		test.Fatalf("issue project = %s; want the newer %s", got, other)
	}
	if move.movedActivities(test) != 0 {
		test.Fatal("a stale move left an activity row")
	}
}

func TestProjectSetActionRefusesAWidenedBudgetAfterTheRequest(test *testing.T) {
	move := newPendingProjectMove(test, 10, 10)
	if class := move.escalationClass(test); class != "reversible" {
		test.Fatalf("class = %s; want reversible", class)
	}
	// The target workflow grows after the card was asked as reversible.
	dbfx.Exec(test, `UPDATE issue_workflow SET max_runs_per_issue = 80 WHERE id = $1`, move.toWorkflow)
	out := move.approveAccepted(test)
	if out.State != "failed" || out.ErrorCode != "stale_budget" {
		test.Fatalf("outcome = %+v; want failed/stale_budget", out)
	}
	if got := move.issueProject(test); got != move.fromProject {
		test.Fatalf("issue project = %s; want %s", got, move.fromProject)
	}
}

func TestProjectSetActionNeedsAnOwner(test *testing.T) {
	move := newPendingProjectMove(test, 10, 10)
	dbfx.Exec(test, `UPDATE member SET role = 'member' WHERE workspace_id = $1 AND user_id = $2`, testWorkspaceID, move.humanID)
	body := move.approve(test, withVerifier(acceptingVerifier), move.claim(test, map[string]string{}), http.StatusForbidden)
	if !strings.Contains(body, "class_needs_owner") {
		test.Fatalf("body %q; want class_needs_owner", body)
	}
	if got := move.issueProject(test); got != move.fromProject {
		test.Fatalf("issue project = %s; want %s", got, move.fromProject)
	}
	dbfx.Exec(test, `UPDATE member SET role = 'owner' WHERE workspace_id = $1 AND user_id = $2`, testWorkspaceID, move.humanID)
}

func TestProjectSetActionClassFollowsTheRunBudget(test *testing.T) {
	for _, tc := range []struct {
		name      string
		from, to  int
		wantClass string
	}{
		{"larger target cap widens", 10, 50, "spend"},
		{"smaller target cap does not", 50, 10, "reversible"},
		{"equal cap does not", 10, 10, "reversible"},
	} {
		test.Run(tc.name, func(test *testing.T) {
			move := newPendingProjectMove(test, tc.from, tc.to)
			if got := move.escalationClass(test); got != tc.wantClass {
				test.Fatalf("class = %s; want %s", got, tc.wantClass)
			}
		})
	}
}

// The CLI sends only project_id; the server fills in the issue from the request.
func TestProjectSetActionProposalWithoutAnIssueIDInParams(test *testing.T) {
	fixture := newActionFixture(test)
	from, _ := workflowProject(test, "CLI from", 10)
	to, _ := workflowProject(test, "CLI to", 10)
	dbfx.Exec(test, `UPDATE issue SET project_id = $1 WHERE id = $2`, from, fixture.issueID)
	dbfx.Cleanup(test, `UPDATE issue SET project_id = NULL WHERE id = $1`, fixture.issueID)
	var created actionResponse
	fixture.respond(test, fixture.projectSetBody(to, false), http.StatusCreated, &created)
	dbfx.Cleanup(test, `DELETE FROM decision_answer WHERE decision_request_id = $1`, created.ID)
	if created.Action == nil || created.Action.ActionType != projectSetAction {
		test.Fatalf("created = %+v; want an %s action", created, projectSetAction)
	}
}

func TestProjectSetActionCardPreviewShowsBeforeAndAfter(test *testing.T) {
	move := newPendingProjectMove(test, 10, 50)
	card := move.cardAction(test)
	fields := previewFields(test, card)
	assertPreviewDigest(test, card.Preview)
	if got := fields["Project"]; got[0] != "Move from" || got[1] != "Move to" {
		test.Fatalf("Project = %v; want Move from -> Move to", got)
	}
	if got := fields["Workflow"]; got[0] != "Move from wf" || got[1] != "Move to wf" {
		test.Fatalf("Workflow = %v; want Move from wf -> Move to wf", got)
	}
	if got := fields["Run cap"]; got != [2]string{"10", "50"} {
		test.Fatalf("Run cap = %v; want [10 50]", got)
	}
	if _, found := fields["Runs used"]; !found {
		test.Fatalf("preview lacks runs used: %v", fields)
	}
	if got := fields["Run budget"][1]; !strings.Contains(got, "widens") {
		test.Fatalf("Run budget = %q; want it to say the move widens", got)
	}
}

func TestProjectSetActionCardPreviewSaysWhenTheBudgetDoesNotWiden(test *testing.T) {
	move := newPendingProjectMove(test, 50, 10)
	fields := previewFields(test, move.cardAction(test))
	if got := fields["Run budget"][1]; !strings.Contains(got, "does not widen") {
		test.Fatalf("Run budget = %q; want it to say the move does not widen", got)
	}
}

// newPendingStatusMove is a pending move of an issue sitting in status from the
// default-listing project into a project whose workflow lists only toStatuses.
func newPendingStatusMove(test *testing.T, status string, toStatuses ...string) projectMoveFixture {
	test.Helper()
	fixture := newActionFixture(test)
	move := projectMoveFixture{}
	move.fromProject, move.fromWorkflow = workflowProject(test, "Status from", 10)
	move.toProject, move.toWorkflow = workflowProjectWithStatuses(test, "Status to", 10, toStatuses...)
	dbfx.Exec(test, `UPDATE issue SET project_id = $1, status = $2 WHERE id = $3`, move.fromProject, status, fixture.issueID)
	dbfx.Cleanup(test, `UPDATE issue SET project_id = NULL, status = 'todo' WHERE id = $1`, fixture.issueID)
	dbfx.Cleanup(test, `DELETE FROM activity_log WHERE issue_id = $1`, fixture.issueID)
	var created actionResponse
	fixture.respond(test, fixture.projectSetBody(move.toProject, true), http.StatusCreated, &created)
	dbfx.Cleanup(test, `DELETE FROM decision_answer WHERE decision_request_id = $1`, created.ID)
	move.pendingAction = pendingAction{fixture, created.ID, created.Action.ID}
	return move
}

func (move projectMoveFixture) issueStatus(test *testing.T) string {
	test.Helper()
	var status string
	dbfx.QueryRow(test, `SELECT status FROM issue WHERE id = $1`, move.issueID).Scan(&status)
	return status
}

func TestProjectSetActionMapsAStatusTheTargetWorkflowLacks(test *testing.T) {
	// backlog and todo share a lifecycle category; the target lists todo only.
	move := newPendingStatusMove(test, "backlog", "todo", "in_progress", "done")
	fields := previewFields(test, move.cardAction(test))
	if got := fields["Status"]; got != [2]string{"backlog", "todo"} {
		test.Fatalf("Status = %v; want [backlog todo]", got)
	}
	if out := move.approveAccepted(test); out.State != "executed" {
		test.Fatalf("outcome = %+v; want executed", out)
	}
	if got := move.issueStatus(test); got != "todo" {
		test.Fatalf("issue status = %s; want the mapped todo", got)
	}
}

func TestProjectSetActionKeepsAStatusTheTargetWorkflowLists(test *testing.T) {
	move := newPendingStatusMove(test, "in_progress", "todo", "in_progress", "done")
	if got := previewFields(test, move.cardAction(test))["Status"]; got != [2]string{"in_progress", "in_progress"} {
		test.Fatalf("Status = %v; want in_progress on both sides", got)
	}
	if out := move.approveAccepted(test); out.State != "executed" {
		test.Fatalf("outcome = %+v; want executed", out)
	}
	if got := move.issueStatus(test); got != "in_progress" {
		test.Fatalf("issue status = %s; want in_progress kept", got)
	}
}

func TestProjectSetActionRefusesAMoveThatLeavesNoValidStatus(test *testing.T) {
	fixture := newActionFixture(test)
	from, _ := workflowProject(test, "No status from", 10)
	to, _ := workflowProjectWithStatuses(test, "No status to", 10, "done")
	dbfx.Exec(test, `UPDATE issue SET project_id = $1 WHERE id = $2`, from, fixture.issueID)
	dbfx.Cleanup(test, `UPDATE issue SET project_id = NULL WHERE id = $1`, fixture.issueID)
	body := fixture.respond(test, fixture.projectSetBody(to, true), http.StatusBadRequest, nil)
	if !strings.Contains(string(body), "invalid_status") {
		test.Fatalf("body %s; want invalid_status", body)
	}
}

func TestProjectSetActionPublishesAnIssueUpdatedEvent(test *testing.T) {
	move := newPendingStatusMove(test, "backlog", "todo", "in_progress", "done")
	var active atomic.Bool
	active.Store(true)
	test.Cleanup(func() { active.Store(false) })
	updates := make(chan map[string]any, 8)
	testHandler.Bus.SubscribeAll(func(event events.Event) {
		payload, ok := event.Payload.(map[string]any)
		if !active.Load() || event.Type != protocol.EventIssueUpdated || !ok {
			return
		}
		if issue, ok := payload["issue"].(IssueResponse); ok && issue.ID == move.issueID {
			updates <- payload
		}
	})
	if out := move.approveAccepted(test); out.State != "executed" {
		test.Fatalf("outcome = %+v; want executed", out)
	}
	select {
	case payload := <-updates:
		issue := payload["issue"].(IssueResponse)
		if payload["project_changed"] != true || payload["status_changed"] != true || issue.ProjectID == nil || *issue.ProjectID != move.toProject || issue.Status != "todo" {
			test.Fatalf("payload %+v; want project_changed and status_changed with the new project and status", payload)
		}
		if payload["prev_status"] != "backlog" {
			test.Fatalf("prev_status = %v; want backlog", payload["prev_status"])
		}
	default:
		test.Fatal("no issue:updated event after the move")
	}
}
