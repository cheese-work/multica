package handler

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/auth"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func intakeWorkerForTest(t *testing.T, fixture githubReviewFixture) githubIntakeWorker {
	t.Helper()
	issue, err := fixture.handler.Queries.GetIssue(context.Background(), parseUUID(fixture.issueID))
	if err != nil {
		t.Fatal(err)
	}
	project := uuidToString(issue.ProjectID)
	if !issue.ProjectID.Valid {
		project = dbfx.Project(t, "Intake worker route")
		dbfx.Exec(t, `UPDATE issue SET project_id=$1 WHERE id=$2`, project, fixture.issueID)
	}
	dbfx.Exec(t, `INSERT INTO github_route_grant(project_id,user_id) VALUES($1,$2) ON CONFLICT DO NOTHING`, project, testUserID)
	t.Cleanup(func() {
		dbfx.Exec(t, `DELETE FROM github_route_grant WHERE project_id=$1 AND user_id=$2`, project, testUserID)
		_, _ = testPool.Exec(context.Background(), `DELETE FROM github_webhook_dispatch WHERE workspace_id=$1`, testWorkspaceID)
	})
	return githubIntakeWorker{
		handler: fixture.handler, dispatchEnabled: true,
		admit: func(context.Context, *db.Queries, db.GithubWebhookRevision, githubIntakePayload) (githubIntakeDecision, error) {
			return githubIntakeDecision{State: "dispatched", Reason: "admitted", Targets: []githubIntakeTarget{{
				IssueID: parseUUID(fixture.issueID), AgentID: parseUUID(fixture.author), UserID: parseUUID(testUserID), Content: "test admitted revision",
			}}}, nil
		},
	}
}

func postIntakeForWorker(t *testing.T, fixture githubReviewFixture, suffix string) string {
	t.Helper()
	delivery := fixture.issueID + suffix
	if response := fixture.post("pull_request_review", fixture.secret, delivery); response.Code != http.StatusAccepted {
		t.Fatalf("intake = %d: %s", response.Code, response.Body.String())
	}
	return delivery
}

func assertIntakeWorkerState(t *testing.T, delivery, expected string) {
	t.Helper()
	var actual string
	dbfx.QueryRow(t, `SELECT state FROM github_webhook_intake WHERE delivery_guid=$1`, delivery).Scan(&actual)
	if actual != expected {
		t.Fatalf("worker state = %s, want %s", actual, expected)
	}
}

func assertIntakeWorkerDispatch(t *testing.T, fixture githubReviewFixture, expected int) {
	t.Helper()
	for _, query := range []string{
		`SELECT count(*) FROM comment WHERE issue_id=$1`,
		`SELECT count(*) FROM agent_task_queue WHERE issue_id=$1`,
		`SELECT count(*) FROM github_webhook_dispatch WHERE issue_id=$1`,
	} {
		if actual := dbfx.Count(t, query, fixture.issueID); actual != expected {
			t.Fatalf("atomic dispatch count = %d, want %d: %s", actual, expected, query)
		}
	}
	if orphaned := dbfx.Count(t, `SELECT count(*) FROM github_webhook_dispatch receipt
		LEFT JOIN agent_task_queue task ON task.id=receipt.task_id AND task.trigger_comment_id=receipt.comment_id
		LEFT JOIN comment ON comment.id=receipt.comment_id AND comment.issue_id=receipt.issue_id
		WHERE receipt.issue_id=$1 AND (task.id IS NULL OR comment.id IS NULL)`, fixture.issueID); orphaned != 0 {
		t.Fatalf("%d receipts without their run and comment", orphaned)
	}
}

func TestGitHubIntakeWorkerT8Resume(t *testing.T) {
	fixture := newGitHubIntakeFixture(t)
	delivery := postIntakeForWorker(t, fixture, "-T8-worker")
	assertGitHubIntakeNoEffects(t, fixture)
	worker := intakeWorkerForTest(t, fixture)
	if _, err := worker.runOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertIntakeWorkerState(t, delivery, "dispatched")
	assertIntakeWorkerDispatch(t, fixture, 1)
	if _, err := intakeWorkerForTest(t, fixture).runOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertIntakeWorkerDispatch(t, fixture, 1)
}

func TestGitHubIntakeWorkerUncommittedReceiptInvisible(t *testing.T) {
	fixture := newGitHubIntakeFixture(t)
	worker := intakeWorkerForTest(t, fixture)
	delivery := postIntakeForWorker(t, fixture, "-uncommitted")
	ctx := context.Background()
	transaction, err := fixture.handler.TxStarter.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer transaction.Rollback(ctx)
	queries := fixture.handler.Queries.WithTx(transaction)
	var revision db.GithubWebhookRevision
	dbfx.QueryRow(t, `SELECT workspace_id,id,pull_request_id,installation_id,object_kind,object_id,body_hash,author_id
FROM github_webhook_revision WHERE source_delivery_guid=$1`, delivery).Scan(&revision.WorkspaceID, &revision.ID,
		&revision.PullRequestID, &revision.InstallationID, &revision.ObjectKind, &revision.ObjectID, &revision.BodyHash, &revision.AuthorID)
	decision, err := worker.admit(ctx, queries, revision, githubIntakePayload{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := worker.dispatch(ctx, queries, revision, decision.Targets[0]); err != nil {
		t.Fatalf("atomic dispatch: %v", err)
	}
	assertIntakeWorkerDispatch(t, fixture, 0)
	if err := transaction.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	assertIntakeWorkerDispatch(t, fixture, 0)
}

func TestGitHubIntakeWorkerGateClosed(t *testing.T) {
	fixture := newGitHubIntakeFixture(t)
	delivery := postIntakeForWorker(t, fixture, "-gate-closed")
	worker := intakeWorkerForTest(t, fixture)
	worker.dispatchEnabled = false
	if _, err := worker.runOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertIntakeWorkerState(t, delivery, "projected")
	if count := dbfx.Count(t, `SELECT count(*) FROM comment WHERE issue_id=$1`, fixture.issueID); count != 1 {
		t.Fatalf("closed gate comments = %d, want 1", count)
	}
	if count := dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id=$1`, fixture.issueID); count != 0 {
		t.Fatalf("closed gate started %d runs", count)
	}
	if count := dbfx.Count(t, `SELECT count(*) FROM github_webhook_dispatch WHERE issue_id=$1 AND state='projected' AND task_id IS NULL`, fixture.issueID); count != 1 {
		t.Fatalf("closed gate receipts = %d, want 1", count)
	}
}

func TestGitHubIntakeWorkerT5Retry(t *testing.T) {
	fixture := newGitHubIntakeFixture(t)
	delivery := postIntakeForWorker(t, fixture, "-T5-worker")
	worker := intakeWorkerForTest(t, fixture)
	admit := worker.admit
	worker.admit = func(context.Context, *db.Queries, db.GithubWebhookRevision, githubIntakePayload) (githubIntakeDecision, error) {
		return githubIntakeDecision{}, errors.New("test GitHub API outage; untrusted body must not be stored as an error")
	}
	dbfx.Exec(t, `UPDATE github_webhook_intake SET received_at=now()-interval '2 hours' WHERE delivery_guid=$1`, delivery)
	if _, err := worker.runOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	var reason string
	dbfx.QueryRow(t, `SELECT reason FROM github_webhook_intake WHERE delivery_guid=$1`, delivery).Scan(&reason)
	if reason != "processing_error" {
		t.Fatalf("outage reason = %s, want processing_error", reason)
	}
	var attempts int
	var delayed bool
	dbfx.QueryRow(t, `SELECT attempts, next_attempt_at > now() FROM github_webhook_intake WHERE delivery_guid=$1`, delivery).Scan(&attempts, &delayed)
	if attempts != 1 || !delayed {
		t.Fatalf("retry attempts=%d, delayed=%v", attempts, delayed)
	}
	deferred, err := fixture.handler.Queries.ListDeferredGitHubWebhookIntakes(context.Background(), 10)
	if err != nil || len(deferred) != 1 || deferred[0].AgeSeconds < 7200 {
		t.Fatalf("deferred age is not visible: rows=%+v error=%v", deferred, err)
	}
	if processed, err := worker.runOnce(context.Background()); err != nil || processed {
		t.Fatalf("retry ignored backoff: processed=%v error=%v", processed, err)
	}
	assertIntakeWorkerDispatch(t, fixture, 0)
	dbfx.Exec(t, `UPDATE github_webhook_intake SET next_attempt_at=now()-interval '1 second' WHERE delivery_guid=$1`, delivery)
	worker.admit = admit
	if _, err := worker.runOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertIntakeWorkerState(t, delivery, "dispatched")
	assertIntakeWorkerDispatch(t, fixture, 1)
}

func TestGitHubIntakeWorkerProductionAdmissionStaysClosed(t *testing.T) {
	fixture := newGitHubIntakeFixture(t)
	delivery := postIntakeForWorker(t, fixture, "-production-closed")
	worker := githubIntakeWorker{handler: fixture.handler, admit: fixture.handler.admitGitHubIntake}
	if _, err := worker.runOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertIntakeWorkerState(t, delivery, "deferred")
	var reason string
	dbfx.QueryRow(t, `SELECT reason FROM github_webhook_intake WHERE delivery_guid=$1`, delivery).Scan(&reason)
	if reason != "provenance_lookup_failed" {
		t.Fatalf("production admission reason = %s", reason)
	}
	assertGitHubIntakeNoEffects(t, fixture)
}

func TestGitHubIntakeWorkerCommittedReceiptDedup(t *testing.T) {
	fixture := newGitHubIntakeFixture(t)
	delivery := postIntakeForWorker(t, fixture, "-committed-receipt")
	worker := intakeWorkerForTest(t, fixture)
	if _, err := worker.runOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertIntakeWorkerDispatch(t, fixture, 1)
	dbfx.Exec(t, `UPDATE github_webhook_revision SET state='deferred', reason='test_retry' WHERE workspace_id=$1`, testWorkspaceID)
	dbfx.Exec(t, `UPDATE github_webhook_intake SET state='deferred', reason='test_retry' WHERE delivery_guid=$1`, delivery)
	if _, err := worker.runOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertIntakeWorkerState(t, delivery, "dispatched")
	assertIntakeWorkerDispatch(t, fixture, 1)
}

func TestGitHubIntakeWorkerDispatchedStateNeedsReceipt(t *testing.T) {
	fixture := newGitHubIntakeFixture(t)
	delivery := postIntakeForWorker(t, fixture, "-missing-receipt")
	dbfx.Exec(t, `UPDATE github_webhook_revision SET state='dispatched', reason='test_incomplete_work' WHERE workspace_id=$1`, testWorkspaceID)
	worker := intakeWorkerForTest(t, fixture)
	if _, err := worker.runOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertIntakeWorkerState(t, delivery, "dispatched")
	assertIntakeWorkerDispatch(t, fixture, 1)
}

func TestGitHubIntakeWorkerRevisionKeyIncludesBodyHash(t *testing.T) {
	fixture := newGitHubIntakeFixture(t)
	worker := intakeWorkerForTest(t, fixture)
	first := postIntakeForWorker(t, fixture, "-body-first")
	if _, err := worker.runOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	fixture.payload["action"] = "edited"
	fixture.payload["review"].(map[string]any)["body"] = "a different signed body"
	second := postIntakeForWorker(t, fixture, "-body-second")
	if _, err := worker.runOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertIntakeWorkerState(t, first, "dispatched")
	assertIntakeWorkerState(t, second, "dispatched")
	assertIntakeWorkerDispatch(t, fixture, 2)
}

func TestGitHubIntakeWorkerDeniedRevisionSettlesDelivery(t *testing.T) {
	fixture := newGitHubIntakeFixture(t)
	dbfx.Exec(t, `UPDATE workspace SET settings='{}' WHERE id=$1`, testWorkspaceID)
	delivery := postIntakeForWorker(t, fixture, "-unmapped")
	worker := githubIntakeWorker{handler: fixture.handler, admit: fixture.handler.admitGitHubIntake}
	if _, err := worker.runOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertIntakeWorkerState(t, delivery, "denied")
	assertGitHubIntakeNoEffects(t, fixture)
}

func TestGitHubIntakeWorkerReceiptErrorRollsBack(t *testing.T) {
	fixture := newGitHubIntakeFixture(t)
	delivery := postIntakeForWorker(t, fixture, "-receipt-error")
	worker := intakeWorkerForTest(t, fixture)
	dbfx.Exec(t, `ALTER TABLE github_webhook_dispatch ADD CONSTRAINT test_receipt_outage CHECK (false) NOT VALID`)
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `ALTER TABLE github_webhook_dispatch DROP CONSTRAINT IF EXISTS test_receipt_outage`)
	})
	if _, err := worker.runOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertIntakeWorkerState(t, delivery, "deferred")
	assertIntakeWorkerDispatch(t, fixture, 0)
	dbfx.Exec(t, `ALTER TABLE github_webhook_dispatch DROP CONSTRAINT test_receipt_outage`)
	dbfx.Exec(t, `UPDATE github_webhook_intake SET next_attempt_at=now() WHERE delivery_guid=$1`, delivery)
	if _, err := worker.runOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertIntakeWorkerState(t, delivery, "dispatched")
	assertIntakeWorkerDispatch(t, fixture, 1)
}

func TestGitHubIntakeWorkerTerminalStates(t *testing.T) {
	for _, state := range []string{"denied", "quarantined", "unsigned", "projected"} {
		t.Run(state, func(t *testing.T) {
			fixture := newGitHubIntakeFixture(t)
			delivery := postIntakeForWorker(t, fixture, "-terminal-"+state)
			worker := intakeWorkerForTest(t, fixture)
			worker.admit = func(context.Context, *db.Queries, db.GithubWebhookRevision, githubIntakePayload) (githubIntakeDecision, error) {
				return githubIntakeDecision{State: state, Reason: "test_disposition"}, nil
			}
			if _, err := worker.runOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
			assertIntakeWorkerState(t, delivery, state)
			if processed, err := worker.runOnce(context.Background()); err != nil || processed {
				t.Fatalf("terminal state reprocessed: %v, %v", processed, err)
			}
			assertGitHubIntakeNoEffects(t, fixture)
		})
	}
}

func TestGitHubIntakeWorkerT7ConcurrentDedup(t *testing.T) {
	fixture := newGitHubIntakeFixture(t)
	first := postIntakeForWorker(t, fixture, "-T7-worker-first")
	postIntakeForWorker(t, fixture, "-T7-worker-first")
	second := postIntakeForWorker(t, fixture, "-T7-worker-second")
	worker := intakeWorkerForTest(t, fixture)
	var group sync.WaitGroup
	for attempt := 0; attempt < 4; attempt++ {
		group.Add(1)
		go func() {
			defer group.Done()
			if _, err := worker.runOnce(context.Background()); err != nil {
				t.Error(err)
			}
		}()
	}
	group.Wait()
	assertIntakeWorkerState(t, first, "dispatched")
	assertIntakeWorkerState(t, second, "dispatched")
	assertIntakeWorkerDispatch(t, fixture, 1)
}

func TestGitHubIntakeWorkerT8CommitRollback(t *testing.T) {
	fixture := newGitHubIntakeFixture(t)
	delivery := postIntakeForWorker(t, fixture, "-T8-worker-rollback")
	worker := intakeWorkerForTest(t, fixture)
	starter := fixture.handler.TxStarter
	fixture.handler.TxStarter = githubIntakeFailCommitStarter{starter}
	if _, err := worker.runOnce(context.Background()); err == nil {
		t.Fatal("expected commit failure")
	}
	assertIntakeWorkerState(t, delivery, "deferred")
	assertIntakeWorkerDispatch(t, fixture, 0)
	fixture.handler.TxStarter = starter
	if _, err := worker.runOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertIntakeWorkerState(t, delivery, "dispatched")
	assertIntakeWorkerDispatch(t, fixture, 1)
}

func TestGitHubIntakeWorkerKillSwitchStopsAtProjected(t *testing.T) {
	fixture := newGitHubIntakeFixture(t)
	delivery := postIntakeForWorker(t, fixture, "-kill-switch")
	worker := intakeWorkerForTest(t, fixture)
	ctx := context.Background()
	if err := fixture.handler.Queries.HaltGitHubDispatch(ctx, db.HaltGitHubDispatchParams{WorkspaceID: parseUUID(testWorkspaceID), Scope: "*"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		testPool.Exec(ctx, `DELETE FROM github_dispatch_halt WHERE workspace_id = $1`, testWorkspaceID)
	})
	if _, err := worker.runOnce(ctx); err != nil {
		t.Fatal(err)
	}
	assertIntakeWorkerState(t, delivery, "projected")
	if count := dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id=$1`, fixture.issueID); count != 0 {
		t.Fatalf("kill switch let %d runs start", count)
	}
	if count := dbfx.Count(t, `SELECT count(*) FROM github_webhook_dispatch WHERE issue_id=$1 AND state='projected' AND reason='kill_switch' AND task_id IS NULL`, fixture.issueID); count != 1 {
		t.Fatalf("kill switch receipts = %d, want 1", count)
	}
}

// A switch that commits between the worker's check and its commit must not lose
// to the worker: the worker waits for the switch, then sees it on.
func TestGitHubIntakeWorkerWaitsForSwitchInFlight(t *testing.T) {
	fixture := newGitHubIntakeFixture(t)
	delivery := postIntakeForWorker(t, fixture, "-switch-in-flight")
	worker := intakeWorkerForTest(t, fixture)
	ctx := context.Background()
	t.Cleanup(func() {
		testPool.Exec(ctx, `DELETE FROM github_dispatch_halt WHERE workspace_id = $1`, testWorkspaceID)
	})
	switchTx, err := fixture.handler.TxStarter.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer switchTx.Rollback(ctx)
	switchQueries := fixture.handler.Queries.WithTx(switchTx)
	if err := switchQueries.LockGitHubDispatchHaltExclusive(ctx, parseUUID(testWorkspaceID)); err != nil {
		t.Fatal(err)
	}
	if err := switchQueries.HaltGitHubDispatch(ctx, db.HaltGitHubDispatchParams{WorkspaceID: parseUUID(testWorkspaceID), Scope: "*"}); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := worker.runOnce(ctx)
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("worker finished while the switch was uncommitted: %v", err)
	case <-time.After(500 * time.Millisecond):
	}
	if err := switchTx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	assertIntakeWorkerState(t, delivery, "projected")
	if count := dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id=$1`, fixture.issueID); count != 0 {
		t.Fatalf("worker started %d runs past the switch", count)
	}
}

// A run the worker already started, with its token, is swept when the switch turns on.
func TestGitHubDispatchHaltRevokesRunStartedByIntake(t *testing.T) {
	fixture := newGitHubIntakeFixture(t)
	postIntakeForWorker(t, fixture, "-sweep-intake-run")
	worker := intakeWorkerForTest(t, fixture)
	ctx := context.Background()
	t.Cleanup(func() {
		testPool.Exec(ctx, `DELETE FROM github_dispatch_halt WHERE workspace_id = $1`, testWorkspaceID)
		testPool.Exec(ctx, `DELETE FROM github_dispatch_halt_audit WHERE workspace_id = $1`, testWorkspaceID)
	})
	if _, err := worker.runOnce(ctx); err != nil {
		t.Fatal(err)
	}
	var taskID string
	dbfx.QueryRow(t, `SELECT id FROM agent_task_queue WHERE issue_id=$1`, fixture.issueID).Scan(&taskID)
	dbfx.Insert(t, "task_token", testutil.Cols{
		"token_hash": auth.HashToken("mat_intake_" + taskID), "task_id": taskID, "agent_id": fixture.author,
		"workspace_id": testWorkspaceID, "user_id": testUserID, "expires_at": time.Now().Add(time.Hour),
	})
	if w := setHalt(t, "*", true, false); w.Code != http.StatusOK {
		t.Fatalf("halt: %d %s", w.Code, w.Body)
	}
	if status := taskStatus(t, taskID); status != "cancelled" {
		t.Fatalf("intake run status = %s, want cancelled", status)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM task_token WHERE task_id=$1`, taskID); n != 0 {
		t.Fatalf("intake run token survived: %d", n)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM github_dispatch_halt_audit WHERE workspace_id=$1 AND halted AND runs_revoked=1`, testWorkspaceID); n != 1 {
		t.Fatalf("audit rows with the revoked count = %d, want 1", n)
	}
}

// dispatchIntakeRunForSweep starts one run through the worker and returns its id and the source PR's id.
func dispatchIntakeRunForSweep(t *testing.T, fixture githubReviewFixture, suffix string) (taskID, pullID string) {
	t.Helper()
	ctx := context.Background()
	postIntakeForWorker(t, fixture, suffix)
	if _, err := intakeWorkerForTest(t, fixture).runOnce(ctx); err != nil {
		t.Fatal(err)
	}
	dbfx.QueryRow(t, `SELECT id FROM agent_task_queue WHERE issue_id=$1`, fixture.issueID).Scan(&taskID)
	dbfx.QueryRow(t, `SELECT pull_request_id FROM issue_pull_request WHERE issue_id=$1`, fixture.issueID).Scan(&pullID)
	t.Cleanup(func() {
		testPool.Exec(ctx, `DELETE FROM github_dispatch_halt WHERE workspace_id = $1`, testWorkspaceID)
		testPool.Exec(ctx, `DELETE FROM github_dispatch_halt_audit WHERE workspace_id = $1`, testWorkspaceID)
	})
	return taskID, pullID
}

// The sweep matches the repo that triggered the run, not whatever the issue links today.
func TestGitHubDispatchHaltMatchesIntakeRunBySourceRepo(t *testing.T) {
	fixture := newGitHubIntakeFixture(t)
	taskID, _ := dispatchIntakeRunForSweep(t, fixture, "-sweep-source-repo")
	other, err := fixture.handler.Queries.UpsertGitHubPullRequest(context.Background(), db.UpsertGitHubPullRequestParams{
		WorkspaceID: parseUUID(testWorkspaceID), InstallationID: 777001, RepoOwner: "other", RepoName: "repo", PrNumber: 5,
		Title: "other repo PR", State: "open", HtmlUrl: "https://github.com/other/repo/pull/5",
		PrCreatedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true}, PrUpdatedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
		HeadSha: "other-head", ClearMergeableState: pgtype.Bool{Bool: true, Valid: true}, HeadRepoSame: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.handler.Queries.LinkIssueToPullRequest(context.Background(), db.LinkIssueToPullRequestParams{
		IssueID: parseUUID(fixture.issueID), PullRequestID: other.ID,
	}); err != nil {
		t.Fatal(err)
	}
	if w := setHalt(t, "other/repo", true, false); w.Code != http.StatusOK {
		t.Fatalf("halt: %d %s", w.Code, w.Body)
	}
	if status := taskStatus(t, taskID); status != "queued" {
		t.Fatalf("run triggered by acme/widget status after halting other/repo = %s, want queued", status)
	}
	if w := setHalt(t, "acme/widget", true, false); w.Code != http.StatusOK {
		t.Fatalf("halt: %d %s", w.Code, w.Body)
	}
	if status := taskStatus(t, taskID); status != "cancelled" {
		t.Fatalf("run status after halting its source repo = %s, want cancelled", status)
	}
}

// A run whose source PR was unlinked before the sweep still stops under a workspace-wide halt.
func TestGitHubDispatchHaltSweepsIntakeRunAfterSourceUnlinked(t *testing.T) {
	fixture := newGitHubIntakeFixture(t)
	taskID, pullID := dispatchIntakeRunForSweep(t, fixture, "-sweep-unlinked")
	dbfx.Exec(t, `DELETE FROM issue_pull_request WHERE issue_id=$1 AND pull_request_id=$2`, fixture.issueID, pullID)
	if w := setHalt(t, "*", true, false); w.Code != http.StatusOK {
		t.Fatalf("halt: %d %s", w.Code, w.Body)
	}
	if status := taskStatus(t, taskID); status != "cancelled" {
		t.Fatalf("run status after its PR was unlinked = %s, want cancelled", status)
	}
}

// A restore waits for the sweep of the halt before it: a run started after the restore must not be cancelled by it.
func TestGitHubDispatchHaltRestoreWaitsForSweep(t *testing.T) {
	fixture := newGitHubIntakeFixture(t)
	taskID, _ := dispatchIntakeRunForSweep(t, fixture, "-restore-waits")
	ctx := context.Background()
	hold, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer hold.Rollback(ctx)
	if _, err := hold.Exec(ctx, `SELECT 1 FROM agent_task_queue WHERE id=$1 FOR UPDATE`, taskID); err != nil {
		t.Fatal(err)
	}
	halted, restored := make(chan int, 1), make(chan int, 1)
	go func() { halted <- setHalt(t, "*", true, false).Code }()
	time.Sleep(500 * time.Millisecond)
	go func() { restored <- setHalt(t, "*", false, false).Code }()
	select {
	case code := <-restored:
		t.Fatalf("restore finished (%d) while the halt's sweep was still running", code)
	case <-time.After(700 * time.Millisecond):
	}
	if err := hold.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if code := <-halted; code != http.StatusOK {
		t.Fatalf("halt status = %d", code)
	}
	if code := <-restored; code != http.StatusOK {
		t.Fatalf("restore status = %d", code)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM github_dispatch_halt WHERE workspace_id=$1`, testWorkspaceID); n != 0 {
		t.Fatalf("halts after restore = %d, want 0", n)
	}
}

func cleanHaltTables(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	clean := func() {
		testPool.Exec(ctx, `DELETE FROM github_dispatch_halt WHERE workspace_id = $1`, testWorkspaceID)
		testPool.Exec(ctx, `DELETE FROM github_dispatch_halt_audit WHERE workspace_id = $1`, testWorkspaceID)
	}
	clean()
	t.Cleanup(clean)
}

// A failing sweep must not take the halt or its audit row with it.
func TestGitHubDispatchHaltSurvivesSweepFailure(t *testing.T) {
	fixture := newGitHubIntakeFixture(t)
	taskID, _ := dispatchIntakeRunForSweep(t, fixture, "-sweep-fails")
	cleanHaltTables(t)
	ctx := context.Background()
	dbfx.Insert(t, "task_token", testutil.Cols{
		"token_hash": auth.HashToken("mat_sweepfail_" + taskID), "task_id": taskID, "agent_id": fixture.author,
		"workspace_id": testWorkspaceID, "user_id": testUserID, "expires_at": time.Now().Add(time.Hour),
	})
	t.Cleanup(func() {
		testPool.Exec(ctx, `DROP TRIGGER IF EXISTS test_token_delete_fail ON task_token`)
		testPool.Exec(ctx, `DROP FUNCTION IF EXISTS test_token_delete_fail()`)
	})
	for _, ddl := range []string{
		`CREATE OR REPLACE FUNCTION test_token_delete_fail() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'token delete refused'; END $$`,
		`CREATE TRIGGER test_token_delete_fail BEFORE DELETE ON task_token FOR EACH ROW EXECUTE FUNCTION test_token_delete_fail()`,
	} {
		if _, err := testPool.Exec(ctx, ddl); err != nil {
			t.Fatal(err)
		}
	}
	if w := setHalt(t, "*", true, false); w.Code != http.StatusInternalServerError {
		t.Fatalf("status with a failing sweep = %d, want 500", w.Code)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM github_dispatch_halt WHERE workspace_id=$1 AND scope='*'`, testWorkspaceID); n != 1 {
		t.Fatalf("halt rows after a failed sweep = %d, want 1", n)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM github_dispatch_halt_audit WHERE workspace_id=$1 AND halted AND sweep_failed`, testWorkspaceID); n != 1 {
		t.Fatalf("audit rows marked sweep_failed = %d, want 1", n)
	}
}

// The halt is durable and visible before the sweep finishes, so a crash during the sweep keeps it.
func TestGitHubDispatchHaltIsCommittedBeforeSweep(t *testing.T) {
	fixture := newGitHubIntakeFixture(t)
	taskID, _ := dispatchIntakeRunForSweep(t, fixture, "-halt-before-sweep")
	cleanHaltTables(t)
	ctx := context.Background()
	hold, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer hold.Rollback(ctx)
	if _, err := hold.Exec(ctx, `SELECT 1 FROM agent_task_queue WHERE id=$1 FOR UPDATE`, taskID); err != nil {
		t.Fatal(err)
	}
	done := make(chan int, 1)
	go func() { done <- setHalt(t, "*", true, false).Code }()
	deadline := time.Now().Add(3 * time.Second)
	for dbfx.Count(t, `SELECT count(*) FROM github_dispatch_halt WHERE workspace_id=$1`, testWorkspaceID) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("halt not visible to other connections while the sweep is running")
		}
		time.Sleep(20 * time.Millisecond)
	}
	select {
	case code := <-done:
		t.Fatalf("halt finished (%d) while the sweep was blocked", code)
	default:
	}
	if err := hold.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if code := <-done; code != http.StatusOK {
		t.Fatalf("halt status = %d", code)
	}
}

// An external wakeup in flight while a switch change is activating waits for it, then sees the halt.
func TestGitHubDispatchHaltWakeupWaitsForActivation(t *testing.T) {
	fx := newPRMergeWebhookFixture(t, 222011)
	agent := createHandlerTestAgent(t, "CHE-2223 activation agent", nil)
	setIssueAssigneeDirect(t, fx.child.ID, "agent", agent)
	cleanHaltTables(t)
	ctx := context.Background()
	activation, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer activation.Rollback(ctx)
	if _, err := activation.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('github_dispatch_halt:' || $1::text, 0))`, testWorkspaceID); err != nil {
		t.Fatal(err)
	}
	if _, err := activation.Exec(ctx, `INSERT INTO github_dispatch_halt (workspace_id, scope) VALUES ($1, '*')`, testWorkspaceID); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		wakeup := service.IssueWakeupService{Tasks: testHandler.TaskService}
		done <- wakeup.TriggerPullRequestWakeup(ctx, parseUUID(fx.child.ID), service.PullRequestWakeupInput{
			Rule: service.SystemRulePRMerged, RepoOwner: "acme", RepoName: "widget", Number: 7,
			URL: "https://github.com/acme/widget/pull/7", MergeCommit: "activation-test",
		})
	}()
	select {
	case err := <-done:
		t.Fatalf("wakeup finished (%v) while a switch change was activating", err)
	case <-time.After(500 * time.Millisecond):
	}
	if err := activation.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := mergedReceipts(t, fx.child.ID); got != 0 {
		t.Fatalf("wakeup admitted across the activation: receipts = %d, want 0", got)
	}
}

// A sweep that fails on a token must stay retryable: the run is not cancelled until its token is gone.
func TestGitHubDispatchHaltRetryRevokesTokenAfterFailedSweep(t *testing.T) {
	fixture := newGitHubIntakeFixture(t)
	taskID, _ := dispatchIntakeRunForSweep(t, fixture, "-sweep-retry")
	cleanHaltTables(t)
	ctx := context.Background()
	dbfx.Insert(t, "task_token", testutil.Cols{
		"token_hash": auth.HashToken("mat_sweepretry_" + taskID), "task_id": taskID, "agent_id": fixture.author,
		"workspace_id": testWorkspaceID, "user_id": testUserID, "expires_at": time.Now().Add(time.Hour),
	})
	dropTrigger := func() {
		testPool.Exec(ctx, `DROP TRIGGER IF EXISTS test_token_delete_fail ON task_token`)
		testPool.Exec(ctx, `DROP FUNCTION IF EXISTS test_token_delete_fail()`)
	}
	t.Cleanup(dropTrigger)
	for _, ddl := range []string{
		`CREATE OR REPLACE FUNCTION test_token_delete_fail() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'token delete refused'; END $$`,
		`CREATE TRIGGER test_token_delete_fail BEFORE DELETE ON task_token FOR EACH ROW EXECUTE FUNCTION test_token_delete_fail()`,
	} {
		if _, err := testPool.Exec(ctx, ddl); err != nil {
			t.Fatal(err)
		}
	}
	if w := setHalt(t, "*", true, false); w.Code != http.StatusInternalServerError {
		t.Fatalf("status with a failing sweep = %d, want 500", w.Code)
	}
	dropTrigger()
	if w := setHalt(t, "*", true, false); w.Code != http.StatusOK {
		t.Fatalf("retry status = %d: %s", w.Code, w.Body)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM task_token WHERE task_id=$1`, taskID); n != 0 {
		t.Fatalf("token still valid after the retry: %d", n)
	}
	if status := taskStatus(t, taskID); status != "cancelled" {
		t.Fatalf("run status after the retry = %s, want cancelled", status)
	}
}

// A restore cannot land between the halt's commit and its sweep: it waits for the whole activation.
func TestGitHubDispatchHaltRestoreWaitsForActivationGap(t *testing.T) {
	newGitHubIntakeFixture(t)
	cleanHaltTables(t)
	ctx := context.Background()
	activation, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer activation.Rollback(ctx)
	if _, err := activation.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('github_dispatch_halt_change:' || $1::text, 0))`, testWorkspaceID); err != nil {
		t.Fatal(err)
	}
	// The halt row is committed, as after the activation's first step; its sweep has not run yet.
	if _, err := testPool.Exec(ctx, `INSERT INTO github_dispatch_halt (workspace_id, scope) VALUES ($1, '*')`, testWorkspaceID); err != nil {
		t.Fatal(err)
	}
	restored := make(chan int, 1)
	go func() { restored <- setHalt(t, "*", false, false).Code }()
	select {
	case code := <-restored:
		t.Fatalf("restore finished (%d) while an activation was between its commit and its sweep", code)
	case <-time.After(500 * time.Millisecond):
	}
	if err := activation.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if code := <-restored; code != http.StatusOK {
		t.Fatalf("restore status = %d", code)
	}
}
