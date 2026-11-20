package handler

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
)

// roleRow is one workflow_role activity row.
type roleRow struct {
	TaskID, AgentID, Role, WorkflowID, WorkflowVersion, Status string
}

func workflowRoleRows(t *testing.T, issueID string) []roleRow {
	t.Helper()
	rows, err := testPool.Query(context.Background(), `
		SELECT COALESCE(details->>'task_id', ''), COALESCE(details->>'agent_id', ''), COALESCE(details->>'role', ''),
		       COALESCE(details->>'workflow_id', ''), COALESCE(details->>'workflow_version', ''), COALESCE(details->>'status', '')
		FROM activity_log WHERE issue_id = $1 AND action = 'workflow_role'
		ORDER BY created_at, id`, issueID)
	if err != nil {
		t.Fatalf("list workflow_role rows: %v", err)
	}
	defer rows.Close()
	var out []roleRow
	for rows.Next() {
		var r roleRow
		if err := rows.Scan(&r.TaskID, &r.AgentID, &r.Role, &r.WorkflowID, &r.WorkflowVersion, &r.Status); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, r)
	}
	return out
}

func issueAuthorsForTest(t *testing.T, issueID string, lab func(provider, model string) string) []service.IssueAuthor {
	t.Helper()
	if lab == nil {
		lab = voteLab
	}
	authors, err := service.IssueAuthors(context.Background(), testHandler.Queries, util.MustParseUUID(issueID), lab)
	if err != nil {
		t.Fatalf("IssueAuthors: %v", err)
	}
	return authors
}

func authorTaskIDs(authors []service.IssueAuthor) map[string]bool {
	out := map[string]bool{}
	for _, a := range authors {
		out[a.TaskID] = true
	}
	return out
}

// seedRoleIssue is a delivery project's issue moved to in_progress, so the
// handoff queued one run for the agent.
func seedRoleIssue(t *testing.T) (projectID, issueID, agentID, taskID string) {
	t.Helper()
	seedTestCatalog(t)
	agentID = seededReadyAgentID(t)
	projectID = deliveryProject(t, "Workflow role project", agentID)
	issueID = dbfx.Issue(t, "workflow role", testutil.Cols{"project_id": projectID, "status": "todo"})
	updateIssueForTest(t, issueID, map[string]any{"status": "in_progress"}).Want(http.StatusOK)
	dbfx.QueryRow(t, `SELECT id::text FROM agent_task_queue WHERE issue_id = $1 AND agent_id = $2`, issueID, agentID).Scan(&taskID)
	return projectID, issueID, agentID, taskID
}

// seedReviewerTask is a task of agentID on the issue whose role row says
// reviewer, as a reviewer handler would leave it.
func seedReviewerTask(t *testing.T, issueID, agentID string) string {
	t.Helper()
	var runtimeID string
	dbfx.QueryRow(t, `SELECT runtime_id::text FROM agent WHERE id = $1`, agentID).Scan(&runtimeID)
	taskID := dbfx.Task(t, agentID, testutil.Cols{"issue_id": issueID, "runtime_id": runtimeID, "status": "completed", "workflow_step": "agent_review"})
	dbfx.Exec(t, `INSERT INTO activity_log (workspace_id, issue_id, actor_type, actor_id, action, details)
		VALUES ($1, $2, 'agent', $3::text::uuid, 'workflow_role', jsonb_build_object('task_id', $4::text, 'agent_id', $3::text, 'role', 'reviewer', 'status', 'agent_review'))`,
		testWorkspaceID, issueID, agentID, taskID)
	return taskID
}

// An enqueue writes one row: the task, its agent, the step handler's type, the
// workflow and its version, and the status the task was queued at. Moving the
// issue along from the run itself (a chained advance) adds a row for the same
// task at the new step.
func TestWorkflowRoleRowOnEnqueueAndChainAdvance(t *testing.T) {
	projectID, issueID, agentID, taskID := seedRoleIssue(t)
	workflowID := projectWorkflowID(t, projectID)

	rows := workflowRoleRows(t, issueID)
	if len(rows) != 1 {
		t.Fatalf("rows after the enqueue = %+v, want one", rows)
	}
	first := rows[0]
	if first.TaskID != taskID || first.AgentID != agentID || first.Role != "agent" || first.Status != "in_progress" ||
		first.WorkflowID != workflowID || first.WorkflowVersion == "" {
		t.Fatalf("enqueue row = %+v, want task %s agent %s role agent status in_progress workflow %s with a version", first, taskID, agentID, workflowID)
	}

	dbfx.Exec(t, `UPDATE agent_task_queue SET status = 'running', started_at = now() WHERE id = $1`, taskID)
	agentUpdateIssue(t, issueID, agentID, taskID, map[string]any{"status": "in_review"}).Want(http.StatusOK)

	rows = workflowRoleRows(t, issueID)
	if len(rows) < 2 {
		t.Fatalf("rows after the chained advance = %+v, want a new row", rows)
	}
	var advance *roleRow
	for i := range rows {
		if rows[i].TaskID == taskID && rows[i].Status == "in_review" {
			advance = &rows[i]
		}
	}
	if advance == nil || advance.AgentID != agentID || advance.WorkflowID != workflowID {
		t.Fatalf("no advance row for task %s at in_review in %+v", taskID, rows)
	}
}

// A task's role is what the step's handler was when it was queued. A workflow
// edit after that does not rewrite the row, and the authors query reads rows,
// never the live workflow.
func TestWorkflowRoleSurvivesWorkflowEdit(t *testing.T) {
	projectID, issueID, agentID, taskID := seedRoleIssue(t)
	before := workflowRoleRows(t, issueID)
	if len(before) != 1 {
		t.Fatalf("rows after the enqueue = %+v, want one", before)
	}

	// Take the step away from the agent, then give it back.
	setColumnHandoff(t, projectID, "in_progress", map[string]any{"handler": map[string]any{"type": "none"}}).Want(http.StatusOK)
	if after := workflowRoleRows(t, issueID); len(after) != len(before) || after[0] != before[0] {
		t.Fatalf("a workflow edit changed the rows: before %+v, after %+v", before, after)
	}

	// A reviewer's row stays a reviewer's even when the step now hands to an agent.
	reviewerTask := seedReviewerTask(t, issueID, agentID)
	setColumnHandoff(t, projectID, "in_review", handToAgent(agentID, "Review it.")).Want(http.StatusOK)
	got := authorTaskIDs(issueAuthorsForTest(t, issueID, nil))
	if !got[taskID] {
		t.Fatalf("the author task %s is missing from %v", taskID, got)
	}
	if got[reviewerTask] {
		t.Fatalf("the reviewer task %s counts as authorship after a workflow edit", reviewerTask)
	}
}

// Rebinding the issue to another workflow and back does not change who its
// authors are: the evidence belongs to the issue's tasks, not its workflow.
func TestWorkflowRoleAuthorsSurviveRebind(t *testing.T) {
	_, issueID, _, taskID := seedRoleIssue(t)
	other := createTestWorkflow(t, "Rebind candidate")
	before := authorTaskIDs(issueAuthorsForTest(t, issueID, nil))
	if !before[taskID] {
		t.Fatalf("authors before the rebind = %v, want task %s", before, taskID)
	}

	dbfx.Exec(t, `UPDATE issue SET workflow_id = $2 WHERE id = $1`, issueID, other.ID)
	dbfx.Exec(t, `UPDATE issue SET workflow_id = NULL WHERE id = $1`, issueID)
	after := authorTaskIDs(issueAuthorsForTest(t, issueID, nil))
	if len(after) != len(before) || !after[taskID] {
		t.Fatalf("authors after the rebind = %v, want %v", after, before)
	}
}

// An issue whose history has no role rows, such as every task before this
// change, counts every agent task as authorship. Only an explicit reviewer row
// takes a task out.
func TestWorkflowRoleHistoryWithNoRowsCountsAsAuthors(t *testing.T) {
	seedTestCatalog(t)
	agentID := seededReadyAgentID(t)
	issueID := dbfx.Issue(t, "no role rows", testutil.Cols{})
	var runtimeID string
	dbfx.QueryRow(t, `SELECT runtime_id::text FROM agent WHERE id = $1`, agentID).Scan(&runtimeID)
	legacy := dbfx.Task(t, agentID, testutil.Cols{"issue_id": issueID, "runtime_id": runtimeID, "status": "completed", "workflow_step": "agent_review"})
	reviewer := seedReviewerTask(t, issueID, agentID)

	got := authorTaskIDs(issueAuthorsForTest(t, issueID, nil))
	if !got[legacy] {
		t.Fatalf("a task with no role row must count as an author: %v", got)
	}
	if got[reviewer] {
		t.Fatalf("a task whose row says reviewer must not count as an author: %v", got)
	}
}

// A task that has any row other than reviewer counts: a run that was queued as
// a reviewer and later chained into other work authored something.
func TestWorkflowRoleMixedRowsCountAsAuthor(t *testing.T) {
	seedTestCatalog(t)
	agentID := seededReadyAgentID(t)
	issueID := dbfx.Issue(t, "mixed rows", testutil.Cols{})
	taskID := seedReviewerTask(t, issueID, agentID)
	dbfx.Exec(t, `INSERT INTO activity_log (workspace_id, issue_id, actor_type, actor_id, action, details)
		VALUES ($1, $2, 'agent', $3::text::uuid, 'workflow_role', jsonb_build_object('task_id', $4::text, 'agent_id', $3::text, 'role', 'agent', 'status', 'in_progress'))`,
		testWorkspaceID, issueID, agentID, taskID)
	if got := authorTaskIDs(issueAuthorsForTest(t, issueID, nil)); !got[taskID] {
		t.Fatalf("a task with a reviewer row and an agent row must count as an author: %v", got)
	}
}

// The lab comes from the task row: the model it applied and its runtime's
// provider. A pair the registry does not list is "unknown", never guessed.
func TestWorkflowRoleUnknownLab(t *testing.T) {
	seedTestCatalog(t)
	agentID := seededReadyAgentID(t)
	issueID := dbfx.Issue(t, "lab evidence", testutil.Cols{})
	var runtimeID, provider string
	dbfx.QueryRow(t, `SELECT a.runtime_id::text, r.provider FROM agent a JOIN agent_runtime r ON r.id = a.runtime_id WHERE a.id = $1`, agentID).Scan(&runtimeID, &provider)
	known := dbfx.Task(t, agentID, testutil.Cols{"issue_id": issueID, "runtime_id": runtimeID, "status": "completed", "applied_model": "known-model", "started_at": time.Now()})
	unlisted := dbfx.Task(t, agentID, testutil.Cols{"issue_id": issueID, "runtime_id": runtimeID, "status": "completed", "applied_model": "unlisted-model", "started_at": time.Now()})
	unrecorded := dbfx.Task(t, agentID, testutil.Cols{"issue_id": issueID, "runtime_id": runtimeID, "status": "queued"})
	t.Setenv("MULTICA_VOTE_GATE_LABS", `{"`+provider+`/known-model":"openai"}`)

	labs := map[string]string{}
	for _, a := range issueAuthorsForTest(t, issueID, nil) {
		labs[a.TaskID] = a.Lab
	}
	if labs[known] != "openai" || labs[unlisted] != service.LabUnknown || labs[unrecorded] != service.LabUnknown {
		t.Fatalf("labs = %v, want known=openai, unlisted and unrecorded=%s", labs, service.LabUnknown)
	}
}

// The role rows are evidence, not issue history: the timeline does not list them.
func TestWorkflowRoleRowsAreNotInTheTimeline(t *testing.T) {
	_, issueID, _, _ := seedRoleIssue(t)
	if len(workflowRoleRows(t, issueID)) == 0 {
		t.Fatal("the enqueue wrote no role row")
	}
	var timeline []TimelineEntry
	testutil.Call(t, testHandler.ListTimeline, withURLParam(newRequest(http.MethodGet, "/api/issues/"+issueID+"/timeline", nil), "id", issueID)).Want(http.StatusOK).JSON(&timeline)
	for _, e := range timeline {
		if e.Action != nil && *e.Action == service.ActivityWorkflowRole {
			t.Fatalf("the timeline lists a role row: %+v", e)
		}
	}
}

// A chained advance and its role row are one write. When the row cannot be
// written, the run does not move into the new step: a reviewer run with a
// reviewer row must never reach author work with no row saying so (RGS-1,
// fail closed).
func TestWorkflowRoleChainAdvanceFailsClosedWhenRowCannotBeWritten(t *testing.T) {
	_, issueID, agentID, taskID := seedRoleIssue(t)
	// The run was queued as a reviewer: its only row says so.
	dbfx.Exec(t, `UPDATE activity_log SET details = jsonb_set(details, '{role}', '"reviewer"') WHERE issue_id = $1 AND action = 'workflow_role'`, issueID)
	dbfx.Exec(t, `UPDATE agent_task_queue SET status = 'running', started_at = now() WHERE id = $1`, taskID)
	dbfx.Exec(t, `CREATE OR REPLACE FUNCTION che7676_fail_role() RETURNS trigger AS $$
		BEGIN RAISE EXCEPTION 'injected workflow_role write failure'; END $$ LANGUAGE plpgsql`)
	// Only the run's own advance row fails; the next step's enqueue still writes.
	dbfx.Exec(t, `CREATE TRIGGER che7676_fail_role BEFORE INSERT ON activity_log
		FOR EACH ROW WHEN (NEW.action = 'workflow_role' AND NEW.details->>'task_id' = '`+taskID+`') EXECUTE FUNCTION che7676_fail_role()`)
	t.Cleanup(func() {
		dbfx.Exec(t, `DROP TRIGGER IF EXISTS che7676_fail_role ON activity_log`)
		dbfx.Exec(t, `DROP FUNCTION IF EXISTS che7676_fail_role()`)
	})

	before := taskWorkflowStep(t, taskID)
	agentUpdateIssue(t, issueID, agentID, taskID, map[string]any{"status": "in_review"}).Want(http.StatusOK)

	if got := taskWorkflowStep(t, taskID); got != before {
		t.Fatalf("run step = %q after a failed role write, want it kept at %q", got, before)
	}
	// The issue change is committed, so the run must not stay alive: it would
	// work as an author with only its reviewer row.
	var status, reason string
	dbfx.QueryRow(t, `SELECT status, COALESCE(failure_reason, '') FROM agent_task_queue WHERE id = $1`, taskID).Scan(&status, &reason)
	if status != "cancelled" || reason != "workflow_role_unrecorded" {
		t.Fatalf("run status = %q reason = %q after a failed role write, want cancelled / workflow_role_unrecorded", status, reason)
	}
	var issueStatus string
	dbfx.QueryRow(t, `SELECT status FROM issue WHERE id = $1`, issueID).Scan(&issueStatus)
	if issueStatus != "in_review" {
		t.Fatalf("issue status = %q, want in_review (the committed transition)", issueStatus)
	}
	for _, r := range workflowRoleRows(t, issueID) {
		if r.TaskID == taskID && r.Role != "reviewer" {
			t.Fatalf("a non-reviewer row exists for the task: %+v", r)
		}
	}
}
