package handler

import (
	"context"
	"testing"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func TestGitHubIntakeWorkerSameBodyRevisionsAndRedelivery(test *testing.T) {
	fixture := newGitHubIntakeFixture(test)
	worker := intakeWorkerForTest(test, fixture)
	initial := postIntakeForWorker(test, fixture, "-revision-initial")
	if _, err := worker.runOnce(context.Background()); err != nil {
		test.Fatal(err)
	}
	assertIntakeWorkerDispatch(test, fixture, 1)
	fixture.payload["action"] = "edited"
	fixture.payload["review"].(map[string]any)["updated_at"] = "2026-10-26T08:05:00Z"
	edited := postIntakeForWorker(test, fixture, "-revision-edited")
	if _, err := worker.runOnce(context.Background()); err != nil {
		test.Fatal(err)
	}
	assertIntakeWorkerDispatch(test, fixture, 2)
	redelivery := postIntakeForWorker(test, fixture, "-revision-redelivery")
	if _, err := worker.runOnce(context.Background()); err != nil {
		test.Fatal(err)
	}
	assertIntakeWorkerDispatch(test, fixture, 2)
	for _, delivery := range []string{initial, edited, redelivery} {
		assertIntakeWorkerState(test, delivery, "dispatched")
	}
}

func TestGitHubIntakeWorkerDeniedRevisionIsolation(test *testing.T) {
	fixture := newGitHubIntakeFixture(test)
	worker := intakeWorkerForTest(test, fixture)
	fixture.payload["sender"] = map[string]any{"id": 99, "login": "bot", "type": "Bot"}
	denied := postIntakeForWorker(test, fixture, "-revision-denied")
	fixture.payload["sender"] = map[string]any{"id": 42, "login": "reviewer", "type": "User"}
	fixture.payload["action"] = "edited"
	fixture.payload["review"].(map[string]any)["updated_at"] = "2026-10-26T08:05:00Z"
	edited := postIntakeForWorker(test, fixture, "-revision-eligible")
	if _, err := worker.runOnce(context.Background()); err != nil {
		test.Fatal(err)
	}
	assertIntakeWorkerDispatch(test, fixture, 0)
	var state, reason string
	dbfx.QueryRow(test, `SELECT state, reason FROM github_webhook_revision WHERE source_delivery_guid=$1`, denied).Scan(&state, &reason)
	if state != "denied" || reason != "bot_author" {
		test.Fatalf("denied revision = %s/%s, want denied/bot_author", state, reason)
	}
	if _, err := worker.runOnce(context.Background()); err != nil {
		test.Fatal(err)
	}
	assertIntakeWorkerState(test, denied, "denied")
	assertIntakeWorkerState(test, edited, "dispatched")
	assertIntakeWorkerDispatch(test, fixture, 1)
	dbfx.QueryRow(test, `SELECT state, reason FROM github_webhook_revision WHERE source_delivery_guid=$1`, denied).Scan(&state, &reason)
	if state != "denied" || reason != "bot_author" {
		test.Fatalf("denied revision overwritten = %s/%s", state, reason)
	}
}

func TestGitHubIntakeWorkerSupersedesPendingBeforeAdmission(test *testing.T) {
	for _, scenario := range []string{"initial_first", "edited_first", "same_clock_tick"} {
		test.Run(scenario, func(test *testing.T) {
			fixture := newGitHubIntakeFixture(test)
			worker := intakeWorkerForTest(test, fixture)
			initial, edited := fixture.issueID+"-pending-initial", fixture.issueID+"-pending-edited"
			postInitial := func() {
				fixture.payload["action"] = "submitted"
				fixture.payload["review"].(map[string]any)["updated_at"] = "2026-10-26T08:00:00Z"
				postIntakeForWorker(test, fixture, "-pending-initial")
			}
			postEdited := func() {
				fixture.payload["action"] = "edited"
				updated := "2026-10-26T08:05:00Z"
				if scenario == "same_clock_tick" {
					updated = "2026-10-26T08:00:00Z"
				}
				fixture.payload["review"].(map[string]any)["updated_at"] = updated
				postIntakeForWorker(test, fixture, "-pending-edited")
			}
			if scenario == "edited_first" {
				postEdited()
				postInitial()
			} else {
				postInitial()
				postEdited()
			}
			admit := worker.admit
			var admitted []string
			worker.admit = func(ctx context.Context, queries *db.Queries, revision db.GithubWebhookRevision, payload githubIntakePayload) (githubIntakeDecision, error) {
				admitted = append(admitted, revision.SourceDeliveryGuid.String)
				return admit(ctx, queries, revision, payload)
			}
			for attempt := 0; attempt < 2; attempt++ {
				if _, err := worker.runOnce(context.Background()); err != nil {
					test.Fatal(err)
				}
			}
			if len(admitted) != 1 || admitted[0] != edited {
				test.Fatalf("admission called for %v, want only %s", admitted, edited)
			}
			var state, reason string
			dbfx.QueryRow(test, `SELECT state, reason FROM github_webhook_revision WHERE source_delivery_guid=$1`, initial).Scan(&state, &reason)
			if state != "denied" || reason != "superseded" {
				test.Fatalf("older pending revision = %s/%s, want denied/superseded", state, reason)
			}
			assertIntakeWorkerState(test, initial, "denied")
			assertIntakeWorkerState(test, edited, "dispatched")
			assertIntakeWorkerDispatch(test, fixture, 1)
		})
	}
}

func TestGitHubIntakeWorkerSupersessionScope(test *testing.T) {
	fixture := newGitHubIntakeFixture(test)
	initial := postIntakeForWorker(test, fixture, "-scope-initial")
	fixture.payload["action"] = "edited"
	fixture.payload["review"].(map[string]any)["updated_at"] = "2026-10-26T08:05:00Z"
	edited := postIntakeForWorker(test, fixture, "-scope-edited")
	ctx := context.Background()
	transaction, err := testPool.Begin(ctx)
	if err != nil {
		test.Fatal(err)
	}
	defer transaction.Rollback(ctx)
	for _, alternate := range []struct{ field, value string }{
		{"workspace_id", "'00000000-0000-0000-0000-000000000001'::uuid"},
		{"installation_id", "2019002"}, {"repository_id", "9002"},
		{"object_kind", "'issue_comment'"}, {"object_id", "78"},
		{"author_id", "41"},
	} {
		if _, err := transaction.Exec(ctx, `CREATE TEMP TABLE revision_scope_copy ON COMMIT DROP AS SELECT * FROM github_webhook_revision WHERE source_delivery_guid=$1`, initial); err != nil {
			test.Fatal(err)
		}
		if _, err := transaction.Exec(ctx, `UPDATE revision_scope_copy SET `+alternate.field+`=`+alternate.value+`, source_delivery_guid=source_delivery_guid||'-copy'`); err != nil {
			test.Fatal(err)
		}
		if _, err := transaction.Exec(ctx, `INSERT INTO github_webhook_revision SELECT * FROM revision_scope_copy`); err != nil {
			test.Fatal(err)
		}
		if _, err := transaction.Exec(ctx, `DROP TABLE revision_scope_copy`); err != nil {
			test.Fatal(err)
		}
	}
	queries := fixture.handler.Queries.WithTx(transaction)
	intake, err := queries.GetGitHubWebhookIntake(ctx, edited)
	if err != nil {
		test.Fatal(err)
	}
	worker := intakeWorkerForTest(test, fixture)
	worker.dispatchEnabled = false
	if _, _, _, err := worker.process(ctx, queries, intake); err != nil {
		test.Fatal(err)
	}
	var protected, superseded int
	if err := transaction.QueryRow(ctx, `SELECT count(*) FILTER (WHERE state='deferred'), count(*) FILTER (WHERE state='denied' AND reason='superseded')
		FROM github_webhook_revision WHERE source_delivery_guid IN ($1, $1||'-copy')`, initial).Scan(&protected, &superseded); err != nil {
		test.Fatal(err)
	}
	if protected != 6 || superseded != 1 {
		test.Fatalf("supersession scope: protected=%d superseded=%d, want 6/1", protected, superseded)
	}
}

func TestGitHubIntakeWorkerForeignEditDoesNotSupersede(test *testing.T) {
	fixture := newGitHubIntakeFixture(test)
	initial := postIntakeForWorker(test, fixture, "-foreign-initial")
	fixture.payload["action"] = "edited"
	fixture.payload["sender"] = map[string]any{"id": 99, "login": "other-writer", "type": "User"}
	fixture.payload["review"].(map[string]any)["updated_at"] = "2026-10-26T08:05:00Z"
	foreign := postIntakeForWorker(test, fixture, "-foreign-edit")
	worker := intakeWorkerForTest(test, fixture)
	worker.admit = func(context.Context, *db.Queries, db.GithubWebhookRevision, githubIntakePayload) (githubIntakeDecision, error) {
		return githubIntakeDecision{State: "deferred", Reason: "test_pending_provenance"}, nil
	}
	for attempt := 0; attempt < 2; attempt++ {
		if _, err := worker.runOnce(context.Background()); err != nil {
			test.Fatal(err)
		}
	}
	var state, reason string
	dbfx.QueryRow(test, `SELECT state, reason FROM github_webhook_revision WHERE source_delivery_guid=$1`, initial).Scan(&state, &reason)
	if state != "deferred" || reason != "test_pending_provenance" {
		test.Fatalf("foreign edit replaced initial = %s/%s", state, reason)
	}
	assertIntakeWorkerState(test, foreign, "quarantined")
	assertGitHubIntakeNoEffects(test, fixture)
}
