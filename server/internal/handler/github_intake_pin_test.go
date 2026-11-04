package handler

import (
	"context"
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// Intake-run pinning (CHE-2216 plan 3.6, slice B6; T11 intake part).

// dispatchedIntakeTask runs the intake worker once and returns the task it dispatched.
func dispatchedIntakeTask(t *testing.T, fixture githubReviewFixture, suffix string) string {
	t.Helper()
	postIntakeForWorker(t, fixture, suffix)
	if _, err := intakeWorkerForTest(t, fixture).runOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	var taskID string
	dbfx.QueryRow(t, `SELECT task_id FROM github_webhook_dispatch WHERE issue_id=$1 AND state='dispatched'`, fixture.issueID).Scan(&taskID)
	return taskID
}

func TestIntakePinForTask(t *testing.T) {
	ctx := context.Background()
	fixture := newGitHubIntakeFixture(t)
	taskID := dispatchedIntakeTask(t, fixture, "-pin")
	pullID := func() string {
		var id string
		dbfx.QueryRow(t, `SELECT pull_request_id FROM github_webhook_revision WHERE workspace_id=$1 AND pull_request_id IS NOT NULL ORDER BY created_at DESC LIMIT 1`, testWorkspaceID).Scan(&id)
		return id
	}()
	setPR := func(branch string, sameRepo bool) {
		dbfx.Exec(t, `UPDATE github_pull_request SET branch=$1, head_repo_same=$2 WHERE id=$3`, branch, sameRepo, pullID)
	}

	var wantRepo string
	dbfx.QueryRow(t, `SELECT lower(split_part(split_part(html_url, '://', 2), '/', 1) || '/' || repo_owner || '/' || repo_name) FROM github_pull_request WHERE id=$1`, pullID).Scan(&wantRepo)
	if wantRepo == "" {
		t.Fatal("fixture PR has no repository")
	}

	setPR("feature/pr-head", true)
	if intake, branch, repo := testHandler.intakePinForTask(ctx, parseUUID(testWorkspaceID), parseUUID(taskID)); !intake || branch != "feature/pr-head" || repo != wantRepo {
		t.Fatalf("same-repo PR: intake=%v branch=%q repo=%q, want true feature/pr-head %s", intake, branch, repo, wantRepo)
	}
	// A fork PR is still an intake run, but nothing is pinned: no ref is allowed.
	setPR("feature/pr-head", false)
	if intake, branch, repo := testHandler.intakePinForTask(ctx, parseUUID(testWorkspaceID), parseUUID(taskID)); !intake || branch != "" || repo != "" {
		t.Fatalf("fork PR: intake=%v branch=%q repo=%q, want true and empty", intake, branch, repo)
	}
	setPR("", true)
	if intake, branch, repo := testHandler.intakePinForTask(ctx, parseUUID(testWorkspaceID), parseUUID(taskID)); !intake || branch != "" || repo != "" {
		t.Fatalf("PR without head ref: intake=%v branch=%q repo=%q, want true and empty", intake, branch, repo)
	}

	// A task no intake item dispatched is not an intake run.
	other := dbfx.Task(t, fixture.author, testutil.Cols{"issue_id": fixture.issueID, "runtime_id": handlerTestRuntimeID(t), "status": "completed"})
	if intake, branch, repo := testHandler.intakePinForTask(ctx, parseUUID(testWorkspaceID), parseUUID(other)); intake || branch != "" || repo != "" {
		t.Fatalf("ordinary task: intake=%v branch=%q repo=%q, want false and empty", intake, branch, repo)
	}
	// Another workspace never reads this workspace's receipt.
	foreignWS := dbfx.Workspace(t, "Pin Foreign", "pin-foreign-ws")
	if intake, _, _ := testHandler.intakePinForTask(ctx, parseUUID(foreignWS), parseUUID(taskID)); intake {
		t.Fatal("cross-workspace receipt read")
	}
}

func TestClaimCarriesIntakePin(t *testing.T) {
	ctx := context.Background()
	fixture := newGitHubIntakeFixture(t)
	taskID := dispatchedIntakeTask(t, fixture, "-claim")
	dbfx.Exec(t, `UPDATE github_pull_request SET branch='feature/claimed', head_repo_same=true
		WHERE id=(SELECT pull_request_id FROM github_webhook_revision WHERE workspace_id=$1 AND pull_request_id IS NOT NULL ORDER BY created_at DESC LIMIT 1)`, testWorkspaceID)
	runtimeID := createClaimReclaimRuntime(t, ctx, "Pin claim runtime")
	dbfx.Exec(t, `UPDATE agent_task_queue SET runtime_id=$1 WHERE id=$2`, runtimeID, taskID)
	dbfx.Exec(t, `UPDATE agent SET runtime_id=$1 WHERE id=(SELECT agent_id FROM agent_task_queue WHERE id=$2)`, runtimeID, taskID)

	req := withURLParam(newOwnerDaemonRequest("POST", "/api/daemon/runtimes/"+runtimeID+"/tasks/claim", nil), "runtimeId", runtimeID)
	var resp struct {
		Task *struct {
			ID           string `json:"id"`
			IntakeRun    bool   `json:"intake_run"`
			PinnedBranch string `json:"pinned_branch"`
			PinnedRepo   string `json:"pinned_repo"`
		} `json:"task"`
	}
	testutil.Call(t, testHandler.ClaimTaskByRuntime, req).Want(http.StatusOK).JSON(&resp)
	if resp.Task == nil || resp.Task.ID != taskID || !resp.Task.IntakeRun || resp.Task.PinnedBranch != "feature/claimed" || resp.Task.PinnedRepo == "" {
		t.Fatalf("claim = %+v, want intake run pinned to feature/claimed", resp.Task)
	}
}
