package handler

import (
	"context"
	"net/http"
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

func TestGitHubIntakeWorkerDispatchedRedeliveryPreservesHistory(test *testing.T) {
	fixture := newGitHubIntakeFixture(test)
	worker := intakeWorkerForTest(test, fixture)
	initial := postIntakeForWorker(test, fixture, "-dispatched-initial")
	if _, err := worker.runOnce(context.Background()); err != nil {
		test.Fatal(err)
	}
	var admissions int
	worker.admit = func(context.Context, *db.Queries, db.GithubWebhookRevision, githubIntakePayload) (githubIntakeDecision, error) {
		admissions++
		return githubIntakeDecision{State: "denied", Reason: "test_provenance_changed"}, nil
	}
	redelivery := postIntakeForWorker(test, fixture, "-dispatched-redelivery")
	if _, err := worker.runOnce(context.Background()); err != nil {
		test.Fatal(err)
	}
	var state, reason string
	dbfx.QueryRow(test, `SELECT state, reason FROM github_webhook_revision WHERE source_delivery_guid=$1`, initial).Scan(&state, &reason)
	if admissions != 0 || state != "dispatched" || reason != "admitted" {
		test.Fatalf("redelivery admissions=%d history=%s/%s, want 0 and dispatched/admitted", admissions, state, reason)
	}
	assertIntakeWorkerState(test, initial, "dispatched")
	assertIntakeWorkerState(test, redelivery, "dispatched")
	assertIntakeWorkerDispatch(test, fixture, 1)
}

func TestGitHubIntakeWorkerDeniedRevisionIsolation(test *testing.T) {
	fixture := newGitHubIntakeFixture(test)
	worker := intakeWorkerForTest(test, fixture)
	fixture.payload["sender"] = map[string]any{"id": 99, "login": "bot", "type": "Bot"}
	denied := postIntakeForWorker(test, fixture, "-revision-denied")
	fixture.payload["sender"] = map[string]any{"id": 42, "login": "reviewer", "type": "User"}
	fixture.payload["action"] = "edited"
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
	for _, scenario := range []string{"initial_first", "edited_first", "edited_again", "same_receipt_tick"} {
		test.Run(scenario, func(test *testing.T) {
			fixture := newGitHubIntakeFixture(test)
			worker := intakeWorkerForTest(test, fixture)
			initial, edited := fixture.issueID+"-pending-1", fixture.issueID+"-pending-2"
			postInitial := func() {
				fixture.payload["action"] = "submitted"
				if scenario == "edited_again" || scenario == "same_receipt_tick" {
					fixture.payload["action"] = "edited"
				}
				fixture.payload["review"].(map[string]any)["body"] = "earlier review body"
				postIntakeForWorker(test, fixture, "-pending-1")
			}
			postEdited := func() {
				fixture.payload["action"] = "edited"
				fixture.payload["review"].(map[string]any)["body"] = "later edited review body"
				postIntakeForWorker(test, fixture, "-pending-2")
			}
			if scenario == "edited_first" {
				postEdited()
				postInitial()
			} else {
				postInitial()
				postEdited()
			}
			if scenario == "same_receipt_tick" {
				dbfx.Exec(test, `UPDATE github_webhook_revision SET created_at='2026-10-26T08:00:00Z' WHERE source_delivery_guid IN ($1,$2)`, initial, edited)
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
	fixture.payload["review"].(map[string]any)["body"] = "edited review body"
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
		if _, err := transaction.Exec(ctx, `UPDATE revision_scope_copy SET id=gen_random_uuid(), `+alternate.field+`=`+alternate.value+`, source_delivery_guid=source_delivery_guid||'-copy'`); err != nil {
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
	fixture.payload["review"].(map[string]any)["body"] = "foreign edited review body"
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

func TestGitHubIntakeWorkerCommentSupersession(test *testing.T) {
	for _, scenario := range []string{"initial_first", "edited_first", "updated_time_outranks_receipt", "same_update_tick"} {
		test.Run(scenario, func(test *testing.T) {
			fixture := newGitHubIntakeFixture(test)
			worker := intakeWorkerForTest(test, fixture)
			comment := fixture.payload["review"].(map[string]any)
			delete(comment, "submitted_at")
			comment["created_at"] = "2026-10-26T08:00:00Z"
			fixture.payload["comment"] = comment
			fixture.payload["issue"] = map[string]any{
				"number":       fixture.payload["pull_request"].(map[string]any)["number"],
				"pull_request": map[string]any{"url": "https://api.github.com/repos/acme/widget/pulls/2019"},
			}
			initial, edited := fixture.issueID+"-comment-1", fixture.issueID+"-comment-2"
			post := func(delivery, action, updated, body string) {
				fixture.payload["action"] = action
				comment["updated_at"], comment["body"] = updated, body
				if response := fixture.post("issue_comment", fixture.secret, delivery); response.Code != http.StatusAccepted {
					test.Fatalf("comment intake = %d: %s", response.Code, response.Body.String())
				}
			}
			postInitial := func() {
				action := "created"
				if scenario == "updated_time_outranks_receipt" || scenario == "same_update_tick" {
					action = "edited"
				}
				post(initial, action, "2026-10-26T08:00:00Z", "older comment body")
			}
			postEdited := func() {
				updated := "2026-10-26T08:05:00Z"
				if scenario == "same_update_tick" {
					updated = "2026-10-26T08:00:00Z"
				}
				post(edited, "edited", updated, "newer comment body")
			}
			if scenario == "edited_first" || scenario == "updated_time_outranks_receipt" {
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
				test.Fatalf("comment admission called for %v, want only %s", admitted, edited)
			}
			var state, reason string
			dbfx.QueryRow(test, `SELECT state, reason FROM github_webhook_revision WHERE source_delivery_guid=$1`, initial).Scan(&state, &reason)
			if state != "denied" || reason != "superseded" {
				test.Fatalf("older comment revision = %s/%s, want denied/superseded", state, reason)
			}
			assertIntakeWorkerState(test, initial, "denied")
			assertIntakeWorkerState(test, edited, "dispatched")
			assertIntakeWorkerDispatch(test, fixture, 1)
		})
	}
}
