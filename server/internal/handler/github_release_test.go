package handler

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/integrations/ghrun"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// authorizeReleaseRun declares owner/name a workspace repository and mirrors the
// workflow_run that the receipt job belongs to, as the signed workflow_run
// delivery does long before the deploy finishes.
func authorizeReleaseRun(t *testing.T, ws pgtype.UUID, repoURL string, runID int64, path string) {
	t.Helper()
	ctx := context.Background()
	if repoURL != "" {
		if _, err := testPool.Exec(ctx, `UPDATE workspace SET repos = $1 WHERE id = $2`, []byte(`[{"url":"`+repoURL+`"}]`), ws); err != nil {
			t.Fatal(err)
		}
		// The release repository is configured explicitly, apart from the repo list.
		if _, err := testPool.Exec(ctx, `UPDATE workspace SET settings = settings || jsonb_build_object('release_deploy_repo', $1::text) WHERE id = $2`,
			strings.TrimSuffix(strings.TrimPrefix(repoURL, "https://github.com/"), ".git"), ws); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			testPool.Exec(context.Background(), `UPDATE workspace SET repos = '[]', settings = settings - 'release_deploy_repo' WHERE id = $1`, ws)
		})
	}
	if path == "" {
		return
	}
	if err := ghrun.ApplyRunEvent(ctx, testPool, ws, ghrun.RunEvent{
		InstallationID: 1, RepoOwner: "cheese-work", RepoName: "multica", RunID: runID, RunAttempt: 1,
		StatusRank: ghrun.RankStub + 1, Status: "in_progress", GhUpdatedAt: time.Now(),
		WorkflowPath: path, WorkflowName: "CD deploy to C00", HeadSHA: relNew, HeadBranch: "main", Event: "push",
	}); err != nil {
		t.Fatal(err)
	}
}

const (
	relOld    = "1111111111111111111111111111111111111111"
	relNew    = "2222222222222222222222222222222222222222"
	relDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
)

// releaseDeliveryBody is a completed, successful cd-deploy `deploy` job whose
// receipt step names the deployed tuple.
func releaseDeliveryBody(installationID, jobID int64, attempt int, mutate func(job map[string]any)) map[string]any {
	job := map[string]any{
		"id": jobID, "run_id": 880001, "run_attempt": attempt, "name": "release-receipt", "head_sha": relNew,
		"workflow_name": "CD deploy to C00", "head_branch": "main", "conclusion": "success",
		"steps": []any{
			map[string]any{"name": "Deploy to C00 on local runner", "conclusion": "success"},
			map[string]any{"name": "release-receipt old=" + relOld + " new=" + relNew + " digest=" + relDigest, "conclusion": "success"},
		},
	}
	if mutate != nil {
		mutate(job)
	}
	return map[string]any{
		"action": "completed", "workflow_job": job,
		"repository":   map[string]any{"name": "multica", "owner": map[string]any{"login": "cheese-work"}},
		"installation": map[string]any{"id": installationID},
	}
}

// releaseIntakeFixture binds one workspace (the fixture workspace has an owner)
// and cleans up everything a delivery can create.
func releaseIntakeFixture(t *testing.T, installationID int64) pgtype.UUID {
	t.Helper()
	if testHandler == nil || testPool == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	t.Setenv("GITHUB_WEBHOOK_SECRET", workflowSecret)
	ws := parseUUID(testWorkspaceID)
	if _, err := testHandler.Queries.CreateGitHubInstallation(context.Background(), db.CreateGitHubInstallationParams{
		WorkspaceID: ws, InstallationID: installationID, AccountLogin: "cheese-work", AccountType: "Organization",
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		testPool.Exec(ctx, `DELETE FROM issue WHERE id IN (SELECT issue_id FROM github_release_deploy WHERE workspace_id = $1 AND source_sha = $2)`, ws, relNew)
		testPool.Exec(ctx, `DELETE FROM github_release_deploy WHERE workspace_id = $1 AND source_sha = $2`, ws, relNew)
		testPool.Exec(ctx, `DELETE FROM github_release_pending WHERE workspace_id = $1 AND source_sha = $2`, ws, relNew)
		for _, table := range []string{"github_workflow_run", "github_workflow_job"} {
			testPool.Exec(ctx, `DELETE FROM `+table+` WHERE repo_owner = 'cheese-work' AND repo_name = 'multica' AND run_id IN ($1, 880001, 880002, 880004, 880010)`, cdDeployRunID)
			testPool.Exec(ctx, `DELETE FROM `+table+` WHERE repo_owner = 'evil' AND repo_name = 'multica'`)
		}
		testPool.Exec(ctx, `DELETE FROM github_installation WHERE installation_id = $1`, installationID)
	})
	authorizeReleaseRun(t, ws, "https://github.com/cheese-work/multica.git", 880001, deployWorkflowPath)
	return ws
}

func releaseIssues(t *testing.T, ws pgtype.UUID) []db.Issue {
	t.Helper()
	rows, err := testPool.Query(context.Background(), `SELECT id, title, description FROM issue WHERE workspace_id = $1 AND title LIKE 'release(multica): %'`, ws)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []db.Issue
	for rows.Next() {
		var i db.Issue
		if err := rows.Scan(&i.ID, &i.Title, &i.Description); err != nil {
			t.Fatal(err)
		}
		out = append(out, i)
	}
	return out
}

func wantReleaseIssues(t *testing.T, ws pgtype.UUID, before, want int) []db.Issue {
	t.Helper()
	got := releaseIssues(t, ws)
	if len(got)-before != want {
		t.Fatalf("release issues created = %d, want %d", len(got)-before, want)
	}
	return got
}

func TestReleaseIntakeVerifiedDeployCreatesOneLabeledIssue(t *testing.T) {
	const inst = 880101
	ws := releaseIntakeFixture(t, inst)
	before := len(releaseIssues(t, ws))
	if code := postWorkflowEvent(t, "workflow_job", releaseDeliveryBody(inst, 1, 1, nil)); code != http.StatusAccepted {
		t.Fatalf("status = %d", code)
	}
	issue := wantReleaseIssues(t, ws, before, 1)[before]
	if want := "release(multica): 1111111..2222222"; issue.Title != want {
		t.Errorf("title = %q, want %q", issue.Title, want)
	}
	for _, want := range []string{relNew, relDigest} {
		if !strings.Contains(issue.Description.String, want) {
			t.Errorf("description lacks %s", want)
		}
	}
	var labels int
	testPool.QueryRow(context.Background(), `SELECT count(*) FROM issue_to_label il JOIN issue_label l ON l.id = il.label_id WHERE il.issue_id = $1 AND LOWER(l.name) = 'release'`, issue.ID).Scan(&labels)
	if labels != 1 {
		t.Errorf("release label rows = %d, want 1", labels)
	}
}

func TestReleaseIntakeRefusesWhatIsNotAVerifiedDeploy(t *testing.T) {
	const inst = 880102
	ws := releaseIntakeFixture(t, inst)
	before := len(releaseIssues(t, ws))
	skipped := func(job map[string]any) {
		job["steps"].([]any)[1].(map[string]any)["conclusion"] = "skipped"
	}
	for name, mutate := range map[string]func(map[string]any){
		"deploy skipped (receipt step skipped)":     skipped,
		"job failed":                                func(j map[string]any) { j["conclusion"] = "failure" },
		"the deploy job itself (readback not done)": func(j map[string]any) { j["name"] = "deploy" },
		"not main":       func(j map[string]any) { j["head_branch"] = "feature" },
		"other workflow": func(j map[string]any) { j["workflow_name"] = "CI" },
	} {
		if code := postWorkflowEvent(t, "workflow_job", releaseDeliveryBody(inst, 2, 1, mutate)); code != http.StatusAccepted {
			t.Fatalf("%s: status = %d", name, code)
		}
	}
	// The raw captured successful run whose deploy was skipped carries no receipt.
	if code := postWorkflowEvent(t, "workflow_job", workflowJobEnvelope(t, inst, "completed")); code != http.StatusAccepted {
		t.Fatalf("raw skipped deploy: status = %d", code)
	}
	wantReleaseIssues(t, ws, before, 0)
}

func TestReleaseIntakeRefusesBadSignatureAndUnboundInstallation(t *testing.T) {
	const inst = 880103
	ws := releaseIntakeFixture(t, inst)
	before := len(releaseIssues(t, ws))
	t.Setenv("GITHUB_WEBHOOK_SECRET", "a-different-secret")
	if code := postWorkflowEvent(t, "workflow_job", releaseDeliveryBody(inst, 3, 1, nil)); code == http.StatusAccepted {
		t.Fatalf("bad signature accepted (status %d)", code)
	}
	t.Setenv("GITHUB_WEBHOOK_SECRET", workflowSecret)
	if code := postWorkflowEvent(t, "workflow_job", releaseDeliveryBody(inst+1, 3, 1, nil)); code != http.StatusAccepted {
		t.Fatalf("unbound installation: status = %d", code)
	}
	wantReleaseIssues(t, ws, before, 0)
}

func TestReleaseIntakeDeduplicatesRedeliveryAndRerun(t *testing.T) {
	const inst = 880104
	ws := releaseIntakeFixture(t, inst)
	before := len(releaseIssues(t, ws))
	for _, d := range []struct {
		job     int64
		attempt int
	}{{4, 1}, {4, 1}, {5, 2}} { // first delivery, redelivery, rerun with a new job id
		if code := postWorkflowEvent(t, "workflow_job", releaseDeliveryBody(inst, d.job, d.attempt, nil)); code != http.StatusAccepted {
			t.Fatalf("status = %d", code)
		}
	}
	wantReleaseIssues(t, ws, before, 1)
}

// A matching impostor: the same workflow, job and step names, but from a
// repository the workspace never declared, or from a workflow file that is not
// the C00 deploy. Both would let anyone with push access mint a release issue
// with arbitrary SHA and digest values.
func TestReleaseIntakeRefusesAnImpostorJob(t *testing.T) {
	const inst = 880105
	ws := releaseIntakeFixture(t, inst)
	before := len(releaseIssues(t, ws))

	// Same repo, but the run is a different workflow file.
	authorizeReleaseRun(t, ws, "", 880002, ".github/workflows/impostor.yml")
	otherRun := func(j map[string]any) { j["run_id"] = 880002 }
	if code := postWorkflowEvent(t, "workflow_job", releaseDeliveryBody(inst, 6, 1, otherRun)); code != http.StatusAccepted {
		t.Fatalf("other workflow file: status = %d", code)
	}
	// No mirrored run at all for this run id.
	noRun := func(j map[string]any) { j["run_id"] = 880003 }
	if code := postWorkflowEvent(t, "workflow_job", releaseDeliveryBody(inst, 7, 1, noRun)); code != http.StatusAccepted {
		t.Fatalf("unmirrored run: status = %d", code)
	}
	wantReleaseIssues(t, ws, before, 0)

	// The real run, but the repository is no longer a workspace repository.
	authorizeReleaseRun(t, ws, "https://github.com/someone-else/multica.git", 880001, "")
	if code := postWorkflowEvent(t, "workflow_job", releaseDeliveryBody(inst, 8, 1, nil)); code != http.StatusAccepted {
		t.Fatalf("undeclared repository: status = %d", code)
	}
	wantReleaseIssues(t, ws, before, 0)
}

// The release repository is an explicit workspace setting. Two declared
// repositories can both carry a workflow file at the deploy path; only the
// configured one may open a release issue.
func TestReleaseIntakeBindsToTheConfiguredDeployRepository(t *testing.T) {
	const inst = 880106
	ws := releaseIntakeFixture(t, inst)
	before := len(releaseIssues(t, ws))
	if _, err := testPool.Exec(context.Background(), `UPDATE workspace SET repos = $1 WHERE id = $2`,
		[]byte(`[{"url":"https://github.com/cheese-work/multica.git"},{"url":"https://github.com/evil/multica.git"}]`), ws); err != nil {
		t.Fatal(err)
	}
	if err := ghrun.ApplyRunEvent(context.Background(), testPool, ws, ghrun.RunEvent{
		InstallationID: 1, RepoOwner: "evil", RepoName: "multica", RunID: 880004, RunAttempt: 1,
		StatusRank: ghrun.RankStub + 1, Status: "in_progress", GhUpdatedAt: time.Now(),
		WorkflowPath: deployWorkflowPath, WorkflowName: "CD deploy to C00", HeadSHA: relNew, HeadBranch: "main", Event: "push",
	}); err != nil {
		t.Fatal(err)
	}
	body := releaseDeliveryBody(inst, 9, 1, func(j map[string]any) { j["run_id"] = 880004 })
	body["repository"] = map[string]any{"name": "multica", "owner": map[string]any{"login": "evil"}}
	if code := postWorkflowEvent(t, "workflow_job", body); code != http.StatusAccepted {
		t.Fatalf("declared but unconfigured repository: status = %d", code)
	}
	wantReleaseIssues(t, ws, before, 0)
	if code := postWorkflowEvent(t, "workflow_job", releaseDeliveryBody(inst, 10, 1, nil)); code != http.StatusAccepted {
		t.Fatalf("configured repository: status = %d", code)
	}
	wantReleaseIssues(t, ws, before, 1)
}

// GitHub does not order deliveries: the receipt job can complete before the
// workflow_run delivery that names its workflow file. The receipt is kept and
// opens the issue when the verified run arrives; a run from another workflow
// file discards it.
func TestReleaseIntakeKeepsAReceiptUntilItsRunArrives(t *testing.T) {
	const inst = 880107
	ws := releaseIntakeFixture(t, inst)
	before := len(releaseIssues(t, ws))
	early := func(j map[string]any) { j["run_id"] = 880010 }
	if code := postWorkflowEvent(t, "workflow_job", releaseDeliveryBody(inst, 11, 1, early)); code != http.StatusAccepted {
		t.Fatalf("early receipt: status = %d", code)
	}
	wantReleaseIssues(t, ws, before, 0)

	run := workflowRunEnvelope(t, inst, "requested", "queued")
	run["workflow_run"].(map[string]any)["id"] = 880010
	run["workflow_run"].(map[string]any)["path"] = deployWorkflowPath
	if code := postWorkflowEvent(t, "workflow_run", run); code != http.StatusAccepted {
		t.Fatalf("run delivery: status = %d", code)
	}
	wantReleaseIssues(t, ws, before, 1)
	// Redelivery of the run and of the receipt creates nothing more.
	postWorkflowEvent(t, "workflow_run", run)
	postWorkflowEvent(t, "workflow_job", releaseDeliveryBody(inst, 11, 1, early))
	wantReleaseIssues(t, ws, before, 1)
}

func TestReleaseIntakeDiscardsAReceiptWhoseRunIsAnotherWorkflow(t *testing.T) {
	const inst = 880108
	ws := releaseIntakeFixture(t, inst)
	before := len(releaseIssues(t, ws))
	early := func(j map[string]any) { j["run_id"] = 880010 }
	postWorkflowEvent(t, "workflow_job", releaseDeliveryBody(inst, 12, 1, early))
	run := workflowRunEnvelope(t, inst, "requested", "queued")
	run["workflow_run"].(map[string]any)["id"] = 880010
	run["workflow_run"].(map[string]any)["path"] = ".github/workflows/impostor.yml"
	postWorkflowEvent(t, "workflow_run", run)
	wantReleaseIssues(t, ws, before, 0)
}

// A lookup that fails is not a refusal: the delivery must fail so it can be
// redelivered, instead of dropping a valid receipt.
func TestReleaseAuthorizationTransientErrorIsNotARefusal(t *testing.T) {
	const inst = 880109
	ws := releaseIntakeFixture(t, inst)
	w, err := testHandler.Queries.GetWorkspace(context.Background(), ws)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = testHandler.checkRelease(ctx, w, ghrun.ReleaseReceipt{RepoOwner: "cheese-work", RepoName: "multica", RunID: 880001})
	if err == nil {
		t.Fatal("a failed lookup was reported as a verdict")
	}
}

// failRunLookup is a DBTX whose workflow-run read fails, as a dropped
// connection would, while every other statement goes through.
type failRunLookup struct{ db.DBTX }

type failedRow struct{}

func (failedRow) Scan(...any) error { return errors.New("connection reset") }

func (f failRunLookup) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if strings.Contains(sql, "FROM github_workflow_run") {
		return failedRow{}
	}
	return f.DBTX.QueryRow(ctx, sql, args...)
}

func (f failRunLookup) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	return f.DBTX.Exec(ctx, sql, args...)
}

func pendingReceipts(t *testing.T, ws pgtype.UUID) int {
	t.Helper()
	var n int
	if err := testPool.QueryRow(context.Background(), `SELECT count(*) FROM github_release_pending WHERE workspace_id = $1 AND source_sha = $2`, ws, relNew).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// A lookup that fails after the receipt was read must not lose it: the receipt
// is stored before the lookup, so a later sweep (no manual redelivery) opens the
// issue. The same stored receipt recovers the interleaving where the run
// handler listed pending receipts before the job handler stored this one.
func TestReleaseReceiptSurvivesAFailedLookupAndTheSweepOpensTheIssue(t *testing.T) {
	const inst = 880110
	ws := releaseIntakeFixture(t, inst)
	before := len(releaseIssues(t, ws))
	w, err := testHandler.Queries.GetWorkspace(context.Background(), ws)
	if err != nil {
		t.Fatal(err)
	}
	flaky := *testHandler
	flaky.Queries = db.New(failRunLookup{testPool})
	r := ghrun.ReleaseReceipt{RepoOwner: "cheese-work", RepoName: "multica", RunID: 880001, OldSHA: relOld, NewSHA: relNew, Digest: relDigest}
	if err := flaky.openRelease(context.Background(), w, r); err == nil {
		t.Fatal("a failed lookup was swallowed")
	}
	if n := pendingReceipts(t, ws); n != 1 {
		t.Fatalf("pending receipts after the failed lookup = %d, want 1", n)
	}
	wantReleaseIssues(t, ws, before, 0)

	if err := testHandler.SweepReleaseReceipts(context.Background()); err != nil {
		t.Fatal(err)
	}
	wantReleaseIssues(t, ws, before, 1)
	if n := pendingReceipts(t, ws); n != 0 {
		t.Fatalf("pending receipts after the sweep = %d, want 0", n)
	}
	// A second sweep creates nothing more.
	if err := testHandler.SweepReleaseReceipts(context.Background()); err != nil {
		t.Fatal(err)
	}
	wantReleaseIssues(t, ws, before, 1)
}

// The sweep keeps a receipt whose run has still not arrived, and drops one that
// no run claimed within a week.
func TestReleaseSweepKeepsAWaitingReceiptAndExpiresAStaleOne(t *testing.T) {
	const inst = 880111
	ws := releaseIntakeFixture(t, inst)
	before := len(releaseIssues(t, ws))
	w, err := testHandler.Queries.GetWorkspace(context.Background(), ws)
	if err != nil {
		t.Fatal(err)
	}
	r := ghrun.ReleaseReceipt{RepoOwner: "cheese-work", RepoName: "multica", RunID: 880010, OldSHA: relOld, NewSHA: relNew, Digest: relDigest}
	if err := testHandler.openRelease(context.Background(), w, r); err != nil {
		t.Fatal(err)
	}
	if err := testHandler.SweepReleaseReceipts(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := pendingReceipts(t, ws); n != 1 {
		t.Fatalf("waiting receipt after the sweep = %d, want 1", n)
	}
	if _, err := testPool.Exec(context.Background(), `UPDATE github_release_pending SET created_at = now() - interval '8 days' WHERE workspace_id = $1 AND source_sha = $2`, ws, relNew); err != nil {
		t.Fatal(err)
	}
	if err := testHandler.SweepReleaseReceipts(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := pendingReceipts(t, ws); n != 0 {
		t.Fatalf("stale receipt after the sweep = %d, want 0", n)
	}
	wantReleaseIssues(t, ws, before, 0)
}
