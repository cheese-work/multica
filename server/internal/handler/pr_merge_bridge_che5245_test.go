package handler

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// CHE-5245: while pr_merge_status is not none, a stored pr.merged edge does not
// move an issue by itself: it calls the PR merge decision (the bridge), which
// is the one writer. With none, the edge moves the issue as before.

var bridgeSteps = []map[string]any{
	{"status_key": "backlog"},
	{"status_key": "todo"},
	{"status_key": "in_progress"},
	{"status_key": "in_review", "edges": []map[string]any{
		{"to": "merged", "guard": "pr.merged", "timeout": "336h", "on_timeout": "blocked"},
		{"to": "in_progress", "guard": "pr.checks_red", "timeout": "336h", "on_timeout": "blocked"},
	}},
	{"status_key": "merged"},
	{"status_key": "blocked"},
}

// bridgeIssue is an issue in_review on bridgeSteps whose own PR is open. The
// merge is set later by mergeOwnPR, so no GitHub event reaches the decision.
func bridgeIssue(t *testing.T, installationID int64, prNumber int32, secret string) (issueID, agentID string) {
	t.Helper()
	giveBackRunBudgetHooks()
	seedTestCatalog(t)
	// A new pr.merged edge is refused while a status is chosen; the test picks
	// one after the workflow exists.
	setWorkspacePRMergeStatusForTest(t, "none")
	if dbfx.Count(t, `SELECT count(*) FROM issue_status WHERE workspace_id=$1 AND key='merged'`, testWorkspaceID) == 0 {
		createTestCustomStatus(t, "merged", "started")
	}
	agentID = seededReadyAgentID(t)
	wf := createTestWorkflowSteps(t, "Bridge "+workflowTestSuffix(), bridgeSteps)
	setWorkflowHandoff(t, wf.ID, "merged", handToAgent(agentID, "Verify landing.")).Want(http.StatusOK)
	projectID := createWorkflowTestProject(t, "Bridge project")
	setProjectWorkflow(t, projectID, map[string]any{"workflow_id": wf.ID}).Want(http.StatusOK)
	issue := prAutoCompleteTestIssue(t, "bridge", installationID)
	firePRWebhook(t, secret, installationID, prNumber, issue.Identifier+": work", "", "feat/bridge", "opened")
	if _, err := testPool.Exec(context.Background(),
		`UPDATE issue SET project_id=$2, status='in_review', assignee_type=NULL, assignee_id=NULL WHERE id=$1`, issue.ID, projectID); err != nil {
		t.Fatal(err)
	}
	return issue.ID, agentID
}

// mergeOwnPR marks the issue's PRs merged without a webhook, as a PR that
// merged before the issue entered the step.
func mergeOwnPR(t *testing.T, issueID string) {
	t.Helper()
	if _, err := testPool.Exec(context.Background(),
		`UPDATE github_pull_request SET state='merged', merged_at=now(), merge_commit_sha='bridge-merge'
		 WHERE id IN (SELECT pull_request_id FROM issue_pull_request WHERE issue_id=$1)`, issueID); err != nil {
		t.Fatal(err)
	}
}

func wakeEdgeNow(t *testing.T, issueID string) {
	t.Helper()
	if _, err := testPool.Exec(context.Background(),
		`UPDATE issue_wakeup SET next_fire_at=now() WHERE issue_id=$1 AND system_rule='workflow_edges'`, issueID); err != nil {
		t.Fatal(err)
	}
}

func lastStatusSource(t *testing.T, issueID string) any {
	t.Helper()
	rows := statusRows(t, issueID)
	if len(rows) == 0 {
		t.Fatal("no status_changed row")
	}
	return rows[len(rows)-1]["source"]
}

func TestPRMergeBridgeMovesThroughDecisionWithOneRun(t *testing.T) {
	if testHandler == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	const installationID int64 = 5245001
	secret := "che5245-secret"
	t.Setenv("GITHUB_WEBHOOK_SECRET", secret)
	issueID, agentID := bridgeIssue(t, installationID, 1, secret)
	setWorkspacePRMergeStatusForTest(t, "merged")
	mergeOwnPR(t, issueID)

	tickEdges(t)

	wantHandedOff(t, issueID, agentID)
	if got := lastStatusSource(t, issueID); got != "pr_automation" {
		t.Fatalf("status change source = %v, want pr_automation (the decision, not the edge)", got)
	}
	tickEdges(t)
	if n := taskCountFor(t, issueID, agentID); n != 1 {
		t.Fatalf("handler runs after a second pass = %d, want 1", n)
	}
}

// A decision that fails writes nothing; the watch looks again and the next pass
// moves the issue once.
func TestPRMergeBridgeErrorReschedulesThenMovesOnce(t *testing.T) {
	if testHandler == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	const installationID int64 = 5245002
	secret := "che5245-secret"
	t.Setenv("GITHUB_WEBHOOK_SECRET", secret)
	issueID, agentID := bridgeIssue(t, installationID, 2, secret)
	setWorkspacePRMergeStatusForTest(t, "merged")
	mergeOwnPR(t, issueID)
	ctx := context.Background()
	for _, stmt := range []string{
		`CREATE OR REPLACE FUNCTION che5245_refuse() RETURNS trigger AS $$ BEGIN RAISE EXCEPTION 'che5245 refuse'; END $$ LANGUAGE plpgsql`,
		`CREATE TRIGGER che5245_refuse BEFORE UPDATE ON issue FOR EACH ROW WHEN (NEW.status = 'merged') EXECUTE FUNCTION che5245_refuse()`,
	} {
		if _, err := testPool.Exec(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	dropped := false
	drop := func() {
		if dropped {
			return
		}
		dropped = true
		testPool.Exec(ctx, `DROP TRIGGER IF EXISTS che5245_refuse ON issue`)
		testPool.Exec(ctx, `DROP FUNCTION IF EXISTS che5245_refuse()`)
	}
	t.Cleanup(drop)

	// The scheduler reports the failed pass and keeps the watch.
	err := (&service.IssueWakeupService{Tasks: testHandler.TaskService}).TickWorkspaces(ctx, parseUUID(testWorkspaceID))
	if err == nil || !strings.Contains(err.Error(), "che5245 refuse") {
		t.Fatalf("tick error = %v, want the refused write", err)
	}

	if got := issueStatusForTest(t, issueID); got != "in_review" {
		t.Fatalf("a failed decision left the issue at %q, want in_review", got)
	}
	var due time.Time
	if err := testPool.QueryRow(ctx, `SELECT next_fire_at FROM issue_wakeup WHERE issue_id=$1 AND system_rule='workflow_edges'`, issueID).Scan(&due); err != nil {
		t.Fatalf("the watch is gone after a failed decision: %v", err)
	}
	if !due.After(time.Now()) {
		t.Fatalf("the watch is due at %s after a failed decision, want a later pass", due)
	}
	drop()
	wakeEdgeNow(t, issueID)
	tickEdges(t)
	wantHandedOff(t, issueID, agentID)
	if got := lastStatusSource(t, issueID); got != "pr_automation" {
		t.Fatalf("status change source = %v, want pr_automation", got)
	}
	if rows := statusRows(t, issueID); len(rows) != 1 {
		t.Fatalf("status_changed rows = %d, want 1", len(rows))
	}
}

// Rollback: the settings PATCH back to none hands the edge its moves again.
func TestPRMergeBridgeRollbackRestoresEdgeMoves(t *testing.T) {
	if testHandler == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	const installationID int64 = 5245003
	secret := "che5245-secret"
	t.Setenv("GITHUB_WEBHOOK_SECRET", secret)
	first, agentID := bridgeIssue(t, installationID, 3, secret)
	setWorkspacePRMergeStatusForTest(t, "merged")
	mergeOwnPR(t, first)
	tickEdges(t)
	wantHandedOff(t, first, agentID)

	second, agentID := bridgeIssue(t, installationID, 4, secret)
	patch := withURLParam(newRequest("PATCH", "/api/workspaces/"+testWorkspaceID, map[string]any{"settings": map[string]any{"pr_merge_status": "none"}}), "id", testWorkspaceID)
	testutil.Call(t, testHandler.UpdateWorkspace, patch).Want(http.StatusOK)
	mergeOwnPR(t, second)

	tickEdges(t)

	wantHandedOff(t, second, agentID)
	if got := lastStatusSource(t, second); got == "pr_automation" {
		t.Fatalf("status change source = %v, want the edge to move the issue after the rollback", got)
	}
}

// With a status chosen, the merge event moves the issue directly: the step's
// pr.merged edge no longer makes the decision stand down (workflow_owned).
func TestPRMergeEventMovesIssueDespitePRMergedEdge(t *testing.T) {
	if testHandler == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	const installationID int64 = 5245004
	secret := "che5245-secret"
	t.Setenv("GITHUB_WEBHOOK_SECRET", secret)
	issueID, agentID := bridgeIssue(t, installationID, 5, secret)
	setWorkspacePRMergeStatusForTest(t, "merged")

	firePRWebhook(t, secret, installationID, 5, "bridge", "", "feat/bridge", "merged")

	wantHandedOff(t, issueID, agentID)
	if got := prAutoCompleteStateForTest(t, issueID).State; got == "workflow_owned" {
		t.Fatalf("auto_complete state = %q, want it removed", got)
	}
}

// Other edges and the deadline keep working while the bridge is on.
func TestPRMergeBridgeKeepsChecksRedAndDeadline(t *testing.T) {
	if testHandler == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	const installationID int64 = 5245005
	secret := "che5245-secret"
	t.Setenv("GITHUB_WEBHOOK_SECRET", secret)
	ctx := context.Background()

	t.Run("checks red", func(t *testing.T) {
		issueID, _ := bridgeIssue(t, installationID, 6, secret)
		setWorkspacePRMergeStatusForTest(t, "merged")
		const prNum int32 = 6
		if _, err := testPool.Exec(ctx, `UPDATE github_pull_request SET checks_rollup_state='FAILURE', head_sha='bridge-head', snapshot_head_sha='bridge-head' WHERE workspace_id=$1 AND pr_number=$2`, testWorkspaceID, prNum); err != nil {
			t.Fatal(err)
		}
		if _, err := testPool.Exec(ctx, `INSERT INTO github_pull_request_check_run (pr_id, head_sha, ordinal, name, status, conclusion, red_cause)
			SELECT id, head_sha, 0, 'backend-tests', 'completed', 'failure', 'pr' FROM github_pull_request WHERE workspace_id=$1 AND pr_number=$2`, testWorkspaceID, prNum); err != nil {
			t.Fatal(err)
		}
		wakeEdgeNow(t, issueID)
		tickEdges(t)
		if got := issueStatusForTest(t, issueID); got != "in_progress" {
			t.Fatalf("a PR-caused red left the issue at %q, want in_progress", got)
		}
	})

	t.Run("deadline", func(t *testing.T) {
		issueID, _ := bridgeIssue(t, installationID+1, 7, secret)
		setWorkspacePRMergeStatusForTest(t, "merged")
		if _, err := testPool.Exec(ctx, `UPDATE issue_wakeup SET created_at=now()-interval '400 hours', next_fire_at=now() WHERE issue_id=$1 AND system_rule='workflow_edges'`, issueID); err != nil {
			t.Fatal(err)
		}
		tickEdges(t)
		if got := issueStatusForTest(t, issueID); got != "blocked" {
			t.Fatalf("an open PR past 336h left the issue at %q, want blocked", got)
		}
	})
}

func saveBridgeWorkflow(t *testing.T, id string, steps []map[string]any) *testutil.Response {
	t.Helper()
	req := withURLParam(workflowRequest("PUT", "/api/issue-workflows/"+id, map[string]any{"steps": steps}), "id", id)
	req.Header.Set("X-Actor-Source", "interactive_jwt")
	return testutil.Call(t, testHandler.UpdateIssueWorkflow, req)
}

var bridgeStepsNoEdge = []map[string]any{
	{"status_key": "backlog"}, {"status_key": "todo"}, {"status_key": "in_progress"},
	{"status_key": "in_review"}, {"status_key": "merged"}, {"status_key": "blocked"},
}

func TestPRMergedEdgeSaveFollowsSetting(t *testing.T) {
	if testHandler == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	seedTestCatalog(t)
	createTestCustomStatus(t, "merged", "started")

	t.Run("refused while a status is chosen", func(t *testing.T) {
		setWorkspacePRMergeStatusForTest(t, "merged")
		wf := createTestWorkflowSteps(t, "Refuse "+workflowTestSuffix(), bridgeStepsNoEdge)
		res := saveBridgeWorkflow(t, wf.ID, bridgeSteps).Want(http.StatusBadRequest)
		if body := res.Body.String(); !strings.Contains(body, "Settings → Code") || !strings.Contains(body, "pr.merged") {
			t.Fatalf("refusal = %s, want it to name pr.merged and Settings → Code", body)
		}
	})

	t.Run("accepted with none", func(t *testing.T) {
		setWorkspacePRMergeStatusForTest(t, "none")
		wf := createTestWorkflowSteps(t, "Accept "+workflowTestSuffix(), bridgeStepsNoEdge)
		saveBridgeWorkflow(t, wf.ID, bridgeSteps).Want(http.StatusOK)
	})

	t.Run("a stored edge loads and saves in both cases", func(t *testing.T) {
		setWorkspacePRMergeStatusForTest(t, "none")
		wf := createTestWorkflowSteps(t, "Stored "+workflowTestSuffix(), bridgeSteps)
		setWorkspacePRMergeStatusForTest(t, "merged")
		saveBridgeWorkflow(t, wf.ID, bridgeSteps).Want(http.StatusOK)
		req := withURLParam(workflowRequest("PUT", "/api/issue-workflows/"+wf.ID, map[string]any{"name": "Renamed " + workflowTestSuffix()}), "id", wf.ID)
		req.Header.Set("X-Actor-Source", "interactive_jwt")
		testutil.Call(t, testHandler.UpdateIssueWorkflow, req).Want(http.StatusOK)
	})
}
