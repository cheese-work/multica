package handler

import (
	"context"
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/featureflags"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
)

// Per-issue workflow override (CHE-1986).

func setIssueOverride(t *testing.T, issueID, workflowID string) {
	t.Helper()
	dbfx.Exec(t, `UPDATE issue SET workflow_id = $2 WHERE id = $1`, issueID, workflowID)
}

func issueOverride(t *testing.T, issueID string) string {
	t.Helper()
	var id *string
	dbfx.QueryRow(t, `SELECT workflow_id::text FROM issue WHERE id = $1`, issueID).Scan(&id)
	if id == nil {
		return ""
	}
	return *id
}

// putIssueWorkflow sets (id != "") or clears (id == "") an issue's override.
func putIssueWorkflow(t *testing.T, issueID, workflowID string) *testutil.Response {
	t.Helper()
	body := map[string]any{"workflow_id": nil}
	if workflowID != "" {
		body["workflow_id"] = workflowID
	}
	req := withURLParam(newRequest("PUT", "/api/issues/"+issueID+"/workflow", body), "id", issueID)
	return testutil.Call(t, testHandler.SetIssueWorkflow, req)
}

// taskInState queues a task for the issue in the given state; live states
// need the agent's runtime.
func taskInState(t *testing.T, agentID, issueID, state string) {
	t.Helper()
	dbfx.Task(t, agentID, testutil.Cols{
		"issue_id": issueID, "status": state,
		"runtime_id": testutil.Raw("(SELECT runtime_id FROM agent WHERE id='" + agentID + "')"),
	})
}

func workflowHandoffPreview(t *testing.T, issueID, status string) *testutil.Response {
	t.Helper()
	req := withURLParam(newRequest("GET", "/api/issues/"+issueID+"/workflow-handoff?status="+status, nil), "id", issueID)
	return testutil.Call(t, testHandler.PreviewWorkflowHandoff, req)
}

// Two issues of one project can follow different workflows: the preview and
// the batch write judge each against its own.
func TestMixedOverridesInOneProjectPreviewAndBatch(t *testing.T) {
	seedTestCatalog(t)
	resetDefaultWorkflow(t)
	limited := createTestWorkflow(t, "Limited "+workflowTestSuffix())
	wide := createTestWorkflow(t, "Wide "+workflowTestSuffix(), "in_progress")
	projectID := createWorkflowTestProject(t, "Override mixed project")
	setProjectWorkflow(t, projectID, map[string]any{"workflow_id": limited.ID}).Want(http.StatusOK)
	inherits := dbfx.Issue(t, "inherits", testutil.Cols{"project_id": projectID, "status": "todo"})
	overridden := dbfx.Issue(t, "overridden", testutil.Cols{"project_id": projectID, "status": "todo"})
	setIssueOverride(t, overridden, wide.ID)

	workflowHandoffPreview(t, inherits, "in_progress").Want(http.StatusBadRequest)
	workflowHandoffPreview(t, overridden, "in_progress").Want(http.StatusOK)

	testutil.Call(t, testHandler.BatchUpdateIssues, newRequest("POST", "/api/issues/batch?workspace_id="+testWorkspaceID, map[string]any{
		"issue_ids": []string{inherits, overridden},
		"updates":   map[string]any{"status": "in_progress"},
	})).Want(http.StatusOK)
	wantIssueStatus(t, overridden, "in_progress")
	wantIssueStatus(t, inherits, "todo")
}

// A project workflow change moves only the issues that follow the project.
func TestProjectWorkflowRemapSkipsOverriddenIssues(t *testing.T) {
	seedTestCatalog(t)
	resetDefaultWorkflow(t)
	from := createTestWorkflow(t, "Remap from "+workflowTestSuffix(), "in_progress")
	to := createTestWorkflow(t, "Remap to "+workflowTestSuffix())
	projectID := createWorkflowTestProject(t, "Override remap project")
	setProjectWorkflow(t, projectID, map[string]any{"workflow_id": from.ID}).Want(http.StatusOK)
	inherits := dbfx.Issue(t, "remap inherits", testutil.Cols{"project_id": projectID, "status": "in_progress"})
	overridden := dbfx.Issue(t, "remap overridden", testutil.Cols{"project_id": projectID, "status": "in_progress"})
	setIssueOverride(t, overridden, from.ID)

	setProjectWorkflow(t, projectID, map[string]any{
		"workflow_id": to.ID, "status_mapping": map[string]string{"in_progress": "todo"},
	}).Want(http.StatusOK)

	wantIssueStatus(t, inherits, "todo")
	wantIssueStatus(t, overridden, "in_progress")
}

func TestDeleteWorkflowRefusedWhileAnIssueOverridesToIt(t *testing.T) {
	seedTestCatalog(t)
	wf := createTestWorkflow(t, "Referenced "+workflowTestSuffix())
	issueID := dbfx.Issue(t, "references", testutil.Cols{"status": "todo"})
	setIssueOverride(t, issueID, wf.ID)

	del := func() *testutil.Response {
		return testutil.Call(t, testHandler.DeleteIssueWorkflow, withURLParam(workflowRequest("DELETE", "/api/issue-workflows/"+wf.ID, nil), "id", wf.ID))
	}
	if code := del().Want(http.StatusConflict).Map()["code"]; code != "workflow_in_use" {
		t.Fatalf("code = %v, want workflow_in_use", code)
	}
	if issueOverride(t, issueID) != wf.ID {
		t.Fatal("the refused delete changed the override")
	}
	dbfx.Exec(t, `UPDATE issue SET workflow_id = NULL WHERE id = $1`, issueID)
	del().Want(http.StatusNoContent)
}

func TestIssueOutsideAProjectHasNoWorkflowWithoutAnOverride(t *testing.T) {
	seedTestCatalog(t)
	issueID := dbfx.Issue(t, "no project", testutil.Cols{"status": "todo"})
	issue, err := testHandler.Queries.GetIssue(context.Background(), util.MustParseUUID(issueID))
	if err != nil {
		t.Fatal(err)
	}
	if got := testHandler.claimProjectWorkflow(context.Background(), issue, nil); got != nil {
		t.Fatalf("an issue with no project and no override got a workflow brief:\n%s", got.Brief)
	}
	updateIssueForTest(t, issueID, map[string]any{"status": "in_progress"}).Want(http.StatusOK)
}

// Outside a project an override still applies its status list, but a handler
// relative to the project (the lead) has nobody to resolve to.
func TestOverrideOutsideAProjectResolvesNoRelativeHandler(t *testing.T) {
	seedTestCatalog(t)
	wf := createTestWorkflow(t, "Lead handoff "+workflowTestSuffix(), "in_review")
	setWorkflowHandoff(t, wf.ID, "in_review", map[string]any{"handler": map[string]any{"type": "project_lead"}}).Want(http.StatusOK)
	issueID := dbfx.Issue(t, "override no project", testutil.Cols{"status": "todo"})
	setIssueOverride(t, issueID, wf.ID)

	body := updateIssueForTest(t, issueID, map[string]any{"status": "in_progress"}).Want(http.StatusBadRequest).Map()
	if body["code"] != "status_not_in_workflow" {
		t.Fatalf("code = %v, want status_not_in_workflow: the override lists no in_progress", body["code"])
	}
	updateIssueForTest(t, issueID, map[string]any{"status": "in_review"}).Want(http.StatusOK)
	if _, at, _ := issueStatusAndAssignee(t, issueID); at != "" {
		t.Fatalf("assignee type = %q, want none: the project lead resolves to nobody", at)
	}
	tasks, err := testHandler.Queries.ListTasksByIssue(context.Background(), util.MustParseUUID(issueID))
	if err != nil || len(tasks) != 0 {
		t.Fatalf("tasks = %d (%v), want no handoff run", len(tasks), err)
	}
}

// An effective-workflow switch is refused while the issue has a task in any
// of the five live states: a deferred retry keeps its old workflow step.
func TestSwitchRefusedWhileTheIssueHasALiveTask(t *testing.T) {
	seedTestCatalog(t)
	agentID := seededReadyAgentID(t)
	wf := createTestWorkflow(t, "Switch live "+workflowTestSuffix(), "in_progress")
	for _, state := range []string{"queued", "dispatched", "waiting_local_directory", "running", "deferred"} {
		t.Run(state, func(t *testing.T) {
			issueID := dbfx.Issue(t, "live "+state, testutil.Cols{"status": "todo"})
			taskInState(t, agentID, issueID, state)

			putIssueWorkflow(t, issueID, wf.ID).Want(http.StatusConflict)

			if got := issueOverride(t, issueID); got != "" {
				t.Fatalf("override = %q after a refused switch, want none", got)
			}
			wantIssueStatus(t, issueID, "todo")
		})
	}
}

func TestSwitchAllowedWithOnlyTerminalOrNoTasks(t *testing.T) {
	seedTestCatalog(t)
	agentID := seededReadyAgentID(t)
	wf := createTestWorkflow(t, "Switch idle "+workflowTestSuffix(), "in_progress")
	for _, state := range []string{"completed", "failed", "cancelled", ""} {
		name := state
		if name == "" {
			name = "no tasks"
		}
		t.Run(name, func(t *testing.T) {
			issueID := dbfx.Issue(t, "idle "+name, testutil.Cols{"status": "todo"})
			if state != "" {
				taskInState(t, agentID, issueID, state)
			}
			putIssueWorkflow(t, issueID, wf.ID).Want(http.StatusOK)
			if got := issueOverride(t, issueID); got != wf.ID {
				t.Fatalf("override = %q, want %q", got, wf.ID)
			}
			// An explicit null returns the issue to inheritance.
			putIssueWorkflow(t, issueID, "").Want(http.StatusOK)
			if got := issueOverride(t, issueID); got != "" {
				t.Fatalf("override = %q after reset, want none", got)
			}
		})
	}
}

func TestSwitchRefusedWhenTheWorkflowLacksTheCurrentStatus(t *testing.T) {
	seedTestCatalog(t)
	limited := createTestWorkflow(t, "Switch lacking "+workflowTestSuffix())
	issueID := dbfx.Issue(t, "needs in_progress", testutil.Cols{"status": "in_progress"})

	putIssueWorkflow(t, issueID, limited.ID).Want(http.StatusBadRequest)

	if got := issueOverride(t, issueID); got != "" {
		t.Fatalf("override = %q after a refused switch, want none", got)
	}
	wantIssueStatus(t, issueID, "in_progress")
}

func TestSwitchChangesNoAssigneeAndStartsNoRun(t *testing.T) {
	seedTestCatalog(t)
	agentID := seededReadyAgentID(t)
	wf := createTestWorkflow(t, "Switch admin "+workflowTestSuffix(), "in_progress")
	setWorkflowHandoff(t, wf.ID, "in_progress", handToAgent(agentID, "do it")).Want(http.StatusOK)
	issueID := dbfx.Issue(t, "admin switch", testutil.Cols{"status": "in_progress"})

	putIssueWorkflow(t, issueID, wf.ID).Want(http.StatusOK)

	if _, at, _ := issueStatusAndAssignee(t, issueID); at != "" {
		t.Fatalf("assignee type = %q, want none: a switch is administrative", at)
	}
	tasks, err := testHandler.Queries.ListTasksByIssue(context.Background(), util.MustParseUUID(issueID))
	if err != nil || len(tasks) != 0 {
		t.Fatalf("tasks = %d (%v), want none", len(tasks), err)
	}
}

func TestSwitchValidatesWorkflowAndCaller(t *testing.T) {
	seedTestCatalog(t)
	withFeatureFlag(t, testHandler, featureflags.ProjectWorkflowsV1, true)
	issueID := dbfx.Issue(t, "validate", testutil.Cols{"status": "todo"})
	putIssueWorkflow(t, issueID, "00000000-0000-0000-0000-000000000001").Want(http.StatusBadRequest)

	suffix := workflowTestSuffix()
	userID := dbfx.User(t, "Override member "+suffix, "override-member-"+suffix+"@example.com")
	dbfx.Member(t, testWorkspaceID, userID, "member")
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM member WHERE user_id = $1`, userID)
		testPool.Exec(context.Background(), `DELETE FROM "user" WHERE id = $1`, userID)
	})
	wf := createTestWorkflow(t, "Switch caller "+suffix)
	req := withURLParam(newRequest("PUT", "/api/issues/"+issueID+"/workflow", map[string]any{"workflow_id": wf.ID}), "id", issueID)
	req.Header.Set("X-User-ID", userID)
	testutil.Call(t, testHandler.SetIssueWorkflow, req).Want(http.StatusForbidden)
}

func TestCreateIssueWithAWorkflowOverride(t *testing.T) {
	seedTestCatalog(t)
	wf := createTestWorkflow(t, "Create override "+workflowTestSuffix(), "in_progress")
	create := func(body map[string]any) *testutil.Response {
		return testutil.Call(t, testHandler.CreateIssue, newRequest("POST", "/api/issues?workspace_id="+testWorkspaceID, body))
	}

	var created struct {
		ID         string  `json:"id"`
		WorkflowID *string `json:"workflow_id"`
	}
	create(map[string]any{"title": "override create " + workflowTestSuffix(), "status": "in_progress", "workflow_id": wf.ID}).
		Want(http.StatusCreated).JSON(&created)
	t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM issue WHERE id = $1`, created.ID) })
	if created.WorkflowID == nil || *created.WorkflowID != wf.ID {
		t.Fatalf("response workflow_id = %v, want %s", created.WorkflowID, wf.ID)
	}
	if got := issueOverride(t, created.ID); got != wf.ID {
		t.Fatalf("stored override = %q, want %q", got, wf.ID)
	}

	// done is a fixed step, so it is listed; an unlisted status is rejected.
	limited := createTestWorkflow(t, "Create limited "+workflowTestSuffix())
	create(map[string]any{"title": "override reject " + workflowTestSuffix(), "status": "in_progress", "workflow_id": limited.ID}).
		Want(http.StatusBadRequest)
	create(map[string]any{"title": "override missing " + workflowTestSuffix(), "workflow_id": "00000000-0000-0000-0000-000000000001"}).
		Want(http.StatusBadRequest)
}

// Issues that override to a workflow are not remapped, so an edit cannot drop
// a status one of them holds.
func TestEditWorkflowKeepsStatusesHeldByOverriddenIssues(t *testing.T) {
	seedTestCatalog(t)
	wf := createTestWorkflow(t, "Edit held "+workflowTestSuffix(), "in_progress")
	issueID := dbfx.Issue(t, "holds in_progress", testutil.Cols{"status": "in_progress"})
	setIssueOverride(t, issueID, wf.ID)

	patch := withURLParam(workflowRequest("PATCH", "/api/issue-workflows/"+wf.ID, map[string]any{"steps": statusSteps("backlog", "blocked")}), "id", wf.ID)
	if code := testutil.Call(t, testHandler.UpdateIssueWorkflow, patch).Want(http.StatusConflict).Map()["code"]; code != "workflow_in_use" {
		t.Fatalf("code = %v, want workflow_in_use", code)
	}
}

// Only an explicit null resets the override: a body without the field is a
// malformed request and leaves the issue as it was.
func TestSwitchRequiresTheWorkflowIDField(t *testing.T) {
	seedTestCatalog(t)
	withFeatureFlag(t, testHandler, featureflags.ProjectWorkflowsV1, true)
	wf := createTestWorkflow(t, "Switch field "+workflowTestSuffix())
	issueID := dbfx.Issue(t, "keeps override", testutil.Cols{"status": "todo"})
	setIssueOverride(t, issueID, wf.ID)

	for name, body := range map[string]map[string]any{"empty body": {}, "other fields only": {"title": "x"}} {
		t.Run(name, func(t *testing.T) {
			req := withURLParam(newRequest("PUT", "/api/issues/"+issueID+"/workflow", body), "id", issueID)
			testutil.Call(t, testHandler.SetIssueWorkflow, req).Want(http.StatusBadRequest)
			if got := issueOverride(t, issueID); got != wf.ID {
				t.Fatalf("override = %q after a body without workflow_id, want it kept", got)
			}
		})
	}
	// An empty string is a value, not a reset.
	req := withURLParam(newRequest("PUT", "/api/issues/"+issueID+"/workflow", map[string]any{"workflow_id": ""}), "id", issueID)
	testutil.Call(t, testHandler.SetIssueWorkflow, req).Want(http.StatusBadRequest)
	if got := issueOverride(t, issueID); got != wf.ID {
		t.Fatalf("override = %q after an empty workflow_id, want it kept", got)
	}
	putIssueWorkflow(t, issueID, "").Want(http.StatusOK)
	if got := issueOverride(t, issueID); got != "" {
		t.Fatalf("override = %q after explicit null, want none", got)
	}
}

// A switch bumps the issue revision, so it honours the caller's expected
// revision like any other issue write.
func TestSwitchHonoursExpectedRevision(t *testing.T) {
	seedTestCatalog(t)
	wf := createTestWorkflow(t, "Switch revision "+workflowTestSuffix())
	issueID := dbfx.Issue(t, "revision", testutil.Cols{"status": "todo"})
	var revision int64
	dbfx.QueryRow(t, `SELECT revision FROM issue WHERE id = $1`, issueID).Scan(&revision)
	put := func(expected int64) *testutil.Response {
		req := withURLParam(newRequest("PUT", "/api/issues/"+issueID+"/workflow",
			map[string]any{"workflow_id": wf.ID, "expected_revision": expected}), "id", issueID)
		return testutil.Call(t, testHandler.SetIssueWorkflow, req)
	}

	if code := put(revision + 7).Want(http.StatusConflict).Map()["code"]; code != "revision_conflict" {
		t.Fatalf("code = %v, want revision_conflict", code)
	}
	if got := issueOverride(t, issueID); got != "" {
		t.Fatalf("override = %q after a stale revision, want none", got)
	}
	var got struct {
		Revision int64 `json:"revision"`
	}
	put(revision).Want(http.StatusOK).JSON(&got)
	if got.Revision != revision+1 {
		t.Fatalf("response revision = %d, want %d: the caller chains the next write on it", got.Revision, revision+1)
	}
}

// A green CI head advances an issue by the workflow the issue follows, so an
// override with the flag moves an issue whose project's workflow has none.
func TestCIGreenAdvancesByTheIssuesOverrideWorkflow(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	seedTestCatalog(t)
	const installationID int64 = 1986001
	const prNumber int32 = 198601
	fx := newPRMergeWebhookFixture(t, installationID)
	wf := createTestWorkflowSteps(t, "CI override "+workflowTestSuffix(), []map[string]any{
		{"status_key": "backlog"},
		{"status_key": "in_progress", "advance_on_ci_green": true},
		{"status_key": "in_review"},
		{"status_key": "blocked"},
	})
	projectID := createWorkflowTestProject(t, "CI override project")
	if _, err := testPool.Exec(context.Background(), `UPDATE issue SET project_id=$2, workflow_id=$3, status='in_progress', assignee_type=NULL, assignee_id=NULL WHERE id=$1`, fx.child.ID, projectID, wf.ID); err != nil {
		t.Fatal(err)
	}
	linkedPRForWakeup(t, fx, installationID, prNumber, "green-ovr-a")
	setWorkspacePRWakeSetting(t, "github_enabled", true)
	snap := newGreenSnapshot("green-ovr-a")
	refresh := startSnapshotRefresh(t, installationID, prNumber, snap.get)

	snap.set(t, prNumber, "green-ovr-a", "SUCCESS")
	refresh()
	wantIssueStatus(t, fx.child.ID, "in_review")
}
