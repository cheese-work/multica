package handler

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"

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
