package handler

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/integrations/ghrun"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const (
	workflowSecret   = "che2155-workflow-secret"
	cdDeployRunID    = int64(40343069164)
	cdDeployDeployID = int64(121642870734)
)

// workflowRunEnvelope wraps the real REST run object of the cd-deploy run.
func workflowRunEnvelope(t *testing.T, installationID int64, action, status string) map[string]any {
	t.Helper()
	var run map[string]any
	readWorkflowFixture(t, "run-40343069164.json", &run)
	run["status"] = status
	return map[string]any{
		"action": action, "workflow_run": run,
		"repository":   map[string]any{"name": "multica", "owner": map[string]any{"login": "cheese-work"}},
		"installation": map[string]any{"id": installationID},
	}
}

// workflowJobEnvelope wraps the real REST `deploy` job of the cd-deploy run.
func workflowJobEnvelope(t *testing.T, installationID int64, action string) map[string]any {
	t.Helper()
	var jobs []map[string]any
	readWorkflowFixture(t, "jobs-40343069164.json", &jobs)
	return map[string]any{
		"action": action, "workflow_job": jobs[5],
		"repository":   map[string]any{"name": "multica", "owner": map[string]any{"login": "cheese-work"}},
		"installation": map[string]any{"id": installationID},
	}
}

func readWorkflowFixture(t *testing.T, name string, into any) {
	t.Helper()
	raw, err := os.ReadFile("../integrations/ghrun/testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, into); err != nil {
		t.Fatal(err)
	}
}

// postWorkflowEvent signs and posts one delivery, returning the HTTP status.
func postWorkflowEvent(t *testing.T, event string, body any) int {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	mac := hmac.New(sha256.New, []byte(workflowSecret))
	mac.Write(raw)
	req := httptest.NewRequest(http.MethodPost, "/api/webhooks/github", bytes.NewReader(raw))
	req.Header.Set("X-GitHub-Event", event)
	req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	rec := httptest.NewRecorder()
	testHandler.HandleGitHubWebhook(rec, req)
	return rec.Code
}

func workflowRunRow(t *testing.T, workspaceID pgtype.UUID) (db.GithubWorkflowRun, bool) {
	t.Helper()
	run, err := testHandler.Queries.GetGitHubWorkflowRun(context.Background(), db.GetGitHubWorkflowRunParams{
		WorkspaceID: workspaceID, RepoOwner: "cheese-work", RepoName: "multica", RunID: cdDeployRunID,
	})
	return run, err == nil
}

func workflowJobPhase(t *testing.T, workspaceID pgtype.UUID) int32 {
	t.Helper()
	jobs, err := testHandler.Queries.ListGitHubWorkflowJobsByRunAttempt(context.Background(), db.ListGitHubWorkflowJobsByRunAttemptParams{
		WorkspaceID: workspaceID, RepoOwner: "cheese-work", RepoName: "multica", RunID: cdDeployRunID, RunAttempt: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, job := range jobs {
		if job.JobID == cdDeployDeployID {
			return job.Phase
		}
	}
	return 0
}

// workflowFanoutFixture binds one installation to the test workspace and a
// second throwaway workspace.
func workflowFanoutFixture(t *testing.T, installationID int64, slug string) (first, second pgtype.UUID) {
	t.Helper()
	if testHandler == nil || testPool == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	t.Setenv("GITHUB_WEBHOOK_SECRET", workflowSecret)
	second = bindSecondWorkspaceForTest(t, slug, "WFB", installationID)
	first = parseUUID(testWorkspaceID)
	if _, err := testHandler.Queries.CreateGitHubInstallation(context.Background(), db.CreateGitHubInstallationParams{
		WorkspaceID: first, InstallationID: installationID, AccountLogin: "cheese-work", AccountType: "Organization",
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM github_workflow_run WHERE repo_owner = 'cheese-work' AND repo_name = 'multica' AND run_id = $1`, cdDeployRunID)
		testPool.Exec(context.Background(), `DELETE FROM github_workflow_job WHERE repo_owner = 'cheese-work' AND repo_name = 'multica' AND run_id = $1`, cdDeployRunID)
	})
	return first, second
}

// failingWorkflowWriter fails every write for one workspace.
type failingWorkflowWriter struct {
	next workflowWriter
	fail pgtype.UUID
}

func (w failingWorkflowWriter) ApplyRunEvent(ctx context.Context, workspaceID pgtype.UUID, event ghrun.RunEvent) error {
	if workspaceID == w.fail {
		return errors.New("synthetic storage failure")
	}
	return w.next.ApplyRunEvent(ctx, workspaceID, event)
}

func (w failingWorkflowWriter) ApplyJobEvent(ctx context.Context, workspaceID pgtype.UUID, event ghrun.JobEvent) error {
	if workspaceID == w.fail {
		return errors.New("synthetic storage failure")
	}
	return w.next.ApplyJobEvent(ctx, workspaceID, event)
}

// Test 10: one installation bound to two workspaces writes one row each.
func TestWorkflowWebhookFansOutToEveryBoundWorkspace(t *testing.T) {
	const installationID = int64(215500001)
	first, second := workflowFanoutFixture(t, installationID, "wf-fanout-b")
	if code := postWorkflowEvent(t, "workflow_run", workflowRunEnvelope(t, installationID, "completed", "completed")); code != http.StatusAccepted {
		t.Fatalf("workflow_run status = %d", code)
	}
	if code := postWorkflowEvent(t, "workflow_job", workflowJobEnvelope(t, installationID, "completed")); code != http.StatusAccepted {
		t.Fatalf("workflow_job status = %d", code)
	}
	for name, workspaceID := range map[string]pgtype.UUID{"first": first, "second": second} {
		run, ok := workflowRunRow(t, workspaceID)
		if !ok || run.StatusRank != ghrun.RankCompleted || run.Conclusion != "success" || run.InstallationID != installationID {
			t.Fatalf("%s workspace run = %+v ok=%v", name, run, ok)
		}
		if got := workflowJobPhase(t, workspaceID); got != ghrun.PhaseCompleted {
			t.Fatalf("%s workspace job phase = %d", name, got)
		}
	}
}

func TestWorkflowWebhookSkipsGitHubDisabledWorkspace(t *testing.T) {
	const installationID = int64(215500002)
	first, second := workflowFanoutFixture(t, installationID, "wf-disabled-b")
	if _, err := testPool.Exec(context.Background(), `UPDATE workspace SET settings = '{"github_enabled": false}' WHERE id = $1`, second); err != nil {
		t.Fatal(err)
	}
	postWorkflowEvent(t, "workflow_run", workflowRunEnvelope(t, installationID, "completed", "completed"))
	postWorkflowEvent(t, "workflow_job", workflowJobEnvelope(t, installationID, "completed"))
	if _, ok := workflowRunRow(t, first); !ok {
		t.Fatal("enabled workspace has no run row")
	}
	if _, ok := workflowRunRow(t, second); ok {
		t.Fatal("GitHub-disabled workspace got a run row")
	}
	if got := workflowJobPhase(t, second); got != 0 {
		t.Fatalf("GitHub-disabled workspace got a job row, phase %d", got)
	}
}

func TestWorkflowWebhookPartialFailureReturns5xxAndReplaysCleanly(t *testing.T) {
	const installationID = int64(215500003)
	first, second := workflowFanoutFixture(t, installationID, "wf-partial-b")
	real := testHandler.workflowWriter
	t.Cleanup(func() { testHandler.workflowWriter = real })
	testHandler.workflowWriter = failingWorkflowWriter{next: testHandler.defaultWorkflowWriter(), fail: second}

	body := workflowRunEnvelope(t, installationID, "completed", "completed")
	if code := postWorkflowEvent(t, "workflow_run", body); code != http.StatusInternalServerError {
		t.Fatalf("partial failure status = %d, want 500", code)
	}
	if _, ok := workflowRunRow(t, first); !ok {
		t.Fatal("healthy workspace row was not written")
	}
	if _, ok := workflowRunRow(t, second); ok {
		t.Fatal("failing workspace has a row")
	}

	testHandler.workflowWriter = real
	if code := postWorkflowEvent(t, "workflow_run", body); code != http.StatusAccepted {
		t.Fatalf("replay status = %d, want 202", code)
	}
	// A late in_progress of the same attempt must not undo the replayed completed.
	if code := postWorkflowEvent(t, "workflow_run", workflowRunEnvelope(t, installationID, "in_progress", "in_progress")); code != http.StatusAccepted {
		t.Fatalf("late event status = %d", code)
	}
	for name, workspaceID := range map[string]pgtype.UUID{"first": first, "second": second} {
		run, ok := workflowRunRow(t, workspaceID)
		if !ok || run.StatusRank != ghrun.RankCompleted {
			t.Fatalf("%s workspace after replay = %+v ok=%v", name, run, ok)
		}
	}
	var rows int
	if err := testPool.QueryRow(context.Background(), `SELECT count(*) FROM github_workflow_run WHERE run_id = $1`, cdDeployRunID).Scan(&rows); err != nil || rows != 2 {
		t.Fatalf("run rows = %d (err %v), want 2", rows, err)
	}
}

func TestWorkflowWebhookDropsUnboundInstallationAndBadPayload(t *testing.T) {
	const installationID = int64(215500004)
	first, _ := workflowFanoutFixture(t, installationID, "wf-drop-b")
	if code := postWorkflowEvent(t, "workflow_run", workflowRunEnvelope(t, 215500999, "completed", "completed")); code != http.StatusAccepted {
		t.Fatalf("unbound installation status = %d, want 202", code)
	}
	for _, event := range []string{"workflow_run", "workflow_job"} {
		if code := postWorkflowEvent(t, event, map[string]any{"action": "completed", "installation": map[string]any{"id": installationID}}); code != http.StatusAccepted {
			t.Fatalf("%s bad payload status = %d, want 202", event, code)
		}
	}
	if code := postWorkflowEvent(t, "workflow_job", workflowJobEnvelope(t, installationID, "queued")); code != http.StatusAccepted {
		t.Fatalf("queued job status = %d, want 202", code)
	}
	if _, ok := workflowRunRow(t, first); ok {
		t.Fatal("a dropped delivery wrote a run row")
	}
	if got := workflowJobPhase(t, first); got != 0 {
		t.Fatalf("a dropped delivery wrote a job row, phase %d", got)
	}
}
