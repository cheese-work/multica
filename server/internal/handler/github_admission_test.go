package handler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func TestGitHubAdmissionSignedMetadata(test *testing.T) {
	fixture := newGitHubIntakeFixture(test)
	fixture.payload["review"].(map[string]any)["body"] = nil
	delivery := fixture.issueID + "-T22-metadata"
	if response := fixture.post("pull_request_review", fixture.secret, delivery); response.Code != http.StatusAccepted {
		test.Fatalf("intake = %d", response.Code)
	}
	var event, action, kind, hash, head, state string
	var objectID, senderID, authorID int64
	var createdAt, updatedAt, submittedAt pgtype.Timestamptz
	if err := testPool.QueryRow(context.Background(), `SELECT event, action, object_kind, object_id, body_hash,
head_sha, sender_id, author_id, review_state, object_created_at, object_updated_at, object_submitted_at
FROM github_webhook_revision WHERE source_delivery_guid=$1`, delivery).Scan(
		&event, &action, &kind, &objectID, &hash, &head, &senderID, &authorID, &state, &createdAt, &updatedAt, &submittedAt,
	); err != nil {
		test.Fatal(err)
	}
	digest := sha256.Sum256(nil)
	if event != "pull_request_review" || action != "submitted" || kind != "review" || objectID != 77 || hash != hex.EncodeToString(digest[:]) || head != "review-head" || senderID != 42 || authorID != 42 || state != "changes_requested" {
		test.Fatalf("signed metadata changed: %s/%s/%s/%d/%s/%s/%d/%d/%s", event, action, kind, objectID, hash, head, senderID, authorID, state)
	}
	if createdAt.Valid || updatedAt.Valid || !submittedAt.Valid || submittedAt.Time.UTC().Format(time.RFC3339) != "2026-10-26T08:01:00Z" {
		test.Fatalf("review timestamps = %v/%v/%v, want only signed submitted_at", createdAt, updatedAt, submittedAt)
	}
	assertGitHubIntakeNoEffects(test, fixture)
}

func TestGitHubAdmissionNormalizationTable(test *testing.T) {
	fixture := newGitHubIntakeFixture(test)
	fixture.payload["comment"] = fixture.payload["review"]
	fixture.payload["issue"] = map[string]any{"number": 2019, "pull_request": map[string]any{"url": "https://api.github.com/repos/acme/widget/pulls/2019"}}
	body, err := json.Marshal(fixture.payload)
	if err != nil {
		test.Fatal(err)
	}
	var payload githubIntakePayload
	if err := json.Unmarshal(body, &payload); err != nil {
		test.Fatal(err)
	}
	for _, event := range []string{"pull_request_review", "pull_request_review_comment", "issue_comment"} {
		for _, action := range []string{"submitted", "created", "edited", "dismissed", "deleted", "other"} {
			payload.Action = action
			_, _, _, reason := payload.revision(event)
			valid := action == "edited" || (event == "pull_request_review" && (action == "submitted" || action == "dismissed")) || (event != "pull_request_review" && (action == "created" || action == "deleted"))
			if (reason == "") != valid {
				test.Errorf("%s/%s normalization = %q, valid=%t", event, action, reason, valid)
			}
		}
	}
}

func TestGitHubAdmissionRejectsUnknownEvent(test *testing.T) {
	fixture := newGitHubIntakeFixture(test)
	body, err := json.Marshal(fixture.payload)
	if err != nil {
		test.Fatal(err)
	}
	var payload githubIntakePayload
	if err := json.Unmarshal(body, &payload); err != nil {
		test.Fatal(err)
	}
	_, _, _, reason := payload.revision("unknown")
	if reason != "unsupported_event" {
		test.Fatalf("unknown event admitted: reason = %q, want unsupported_event", reason)
	}
}

func TestGitHubAdmissionT3ForeignEditQuarantined(test *testing.T) {
	fixture := newGitHubIntakeFixture(test)
	fixture.payload["action"] = "edited"
	fixture.payload["sender"] = map[string]any{"id": 99, "login": "other-writer", "type": "User"}
	delivery := fixture.issueID + "-T3"
	if response := fixture.post("pull_request_review", fixture.secret, delivery); response.Code != http.StatusAccepted {
		test.Fatalf("intake = %d %s", response.Code, response.Body.String())
	}
	var state, reason string
	if err := testPool.QueryRow(context.Background(), `SELECT state, reason FROM github_webhook_revision WHERE source_delivery_guid=$1`, delivery).Scan(&state, &reason); err != nil {
		test.Fatal(err)
	}
	if state != "quarantined" || reason != "foreign_editor" {
		test.Fatalf("foreign edit = %s/%s, want quarantined/foreign_editor", state, reason)
	}
	assertGitHubIntakeNoEffects(test, fixture)
}

func TestGitHubAdmissionMissingForkFlag(test *testing.T) {
	fixture := newGitHubIntakeFixture(test)
	delete(fixture.payload["pull_request"].(map[string]any)["head"].(map[string]any)["repo"].(map[string]any), "fork")
	delivery := fixture.issueID + "-missing-fork-flag"
	if response := fixture.post("pull_request_review", fixture.secret, delivery); response.Code != http.StatusAccepted {
		test.Fatalf("intake: %d %s", response.Code, response.Body.String())
	}
	var state, reason string
	dbfx.QueryRow(test, `SELECT state, reason FROM github_webhook_revision WHERE source_delivery_guid=$1`, delivery).Scan(&state, &reason)
	if state != "deferred" || reason != "missing_pull_request_metadata" {
		test.Fatalf("missing fork flag = %s/%s, want deferred/missing_pull_request_metadata", state, reason)
	}
	assertGitHubIntakeNoEffects(test, fixture)
}

func TestGitHubAdmissionForeignEditPermanentDenials(test *testing.T) {
	for _, scenario := range []struct{ name, reason string }{
		{"fork", "fork_pull_request"},
		{"no_linked_issue", "no_linked_issue"},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			fixture := newGitHubIntakeFixture(test)
			fixture.payload["action"] = "edited"
			fixture.payload["sender"] = map[string]any{"id": 99, "login": "other-writer", "type": "User"}
			switch scenario.name {
			case "fork":
				fixture.payload["pull_request"].(map[string]any)["head"].(map[string]any)["repo"] = map[string]any{"id": 9002, "full_name": "outsider/widget", "fork": true}
			case "no_linked_issue":
				dbfx.Exec(test, `DELETE FROM issue_pull_request WHERE issue_id=$1`, fixture.issueID)
			}
			delivery := fixture.issueID + "-foreign-" + scenario.name
			if response := fixture.post("pull_request_review", fixture.secret, delivery); response.Code != http.StatusAccepted {
				test.Fatalf("intake = %d %s", response.Code, response.Body.String())
			}
			assertGitHubIntakeNoEffects(test, fixture)
			var state, reason string
			if err := testPool.QueryRow(context.Background(), `SELECT state, reason FROM github_webhook_revision WHERE source_delivery_guid=$1`, delivery).Scan(&state, &reason); err != nil {
				test.Fatal(err)
			}
			if state != "denied" || reason != scenario.reason {
				test.Fatalf("foreign edit = %s/%s, want denied/%s", state, reason, scenario.reason)
			}
		})
	}
}

func TestGitHubAdmissionT23SameBodySelfEditIsNewRevision(test *testing.T) {
	fixture := newGitHubIntakeFixture(test)
	for _, suffix := range []string{"initial", "edit", "redelivery"} {
		if suffix != "initial" {
			fixture.payload["action"] = "edited"
		}
		if response := fixture.post("pull_request_review", fixture.secret, fixture.issueID+"-T23-"+suffix); response.Code != http.StatusAccepted {
			test.Fatalf("%s = %d", suffix, response.Code)
		}
	}
	if count := dbfx.Count(test, `SELECT count(*) FROM github_webhook_revision WHERE workspace_id=$1`, testWorkspaceID); count != 2 {
		test.Fatalf("initial plus same-body self-edit = %d revisions, want 2", count)
	}
	assertGitHubIntakeNoEffects(test, fixture)
}

func TestGitHubAdmissionReviewRepeatedBodyReusesRevision(test *testing.T) {
	fixture := newGitHubIntakeFixture(test)
	fixture.payload["action"] = "edited"
	review := fixture.payload["review"].(map[string]any)
	for _, revision := range []struct {
		suffix, body, state string
		count               int
	}{
		{"first", "first edited body", "changes_requested", 1},
		{"second", "second edited body", "changes_requested", 2},
		{"return", "first edited body", "changes_requested", 2},
		{"state", "first edited body", "approved", 3},
	} {
		review["body"], review["state"] = revision.body, revision.state
		if response := fixture.post("pull_request_review", fixture.secret, fixture.issueID+"-identity-"+revision.suffix); response.Code != http.StatusAccepted {
			test.Fatalf("%s = %d: %s", revision.suffix, response.Code, response.Body.String())
		}
		if count := dbfx.Count(test, `SELECT count(*) FROM github_webhook_revision WHERE workspace_id=$1`, testWorkspaceID); count != revision.count {
			test.Fatalf("%s: %d revisions, want %d", revision.suffix, count, revision.count)
		}
		if revision.suffix == "first" {
			dbfx.Exec(test, `UPDATE github_webhook_revision SET state='denied', reason='superseded' WHERE source_delivery_guid=$1`, fixture.issueID+"-identity-first")
		}
	}
	var state, reason string
	dbfx.QueryRow(test, `SELECT state, reason FROM github_webhook_revision WHERE source_delivery_guid=$1`, fixture.issueID+"-identity-first").Scan(&state, &reason)
	if state != "denied" || reason != "superseded" {
		test.Fatalf("repeated review body revived revision: %s/%s", state, reason)
	}
	assertGitHubIntakeNoEffects(test, fixture)
}

func TestGitHubAdmissionT25WithdrawalDeniesPending(test *testing.T) {
	for _, event := range []string{"pull_request_review", "pull_request_review_comment", "issue_comment"} {
		test.Run(event, func(test *testing.T) {
			fixture := newGitHubIntakeFixture(test)
			action := "dismissed"
			if event != "pull_request_review" {
				fixture.payload["action"] = "created"
				fixture.payload["comment"] = fixture.payload["review"]
				action = "deleted"
			}
			if event == "issue_comment" {
				fixture.payload["issue"] = map[string]any{"number": 2019, "pull_request": map[string]any{"url": "https://api.github.com/repos/acme/widget/pulls/2019"}}
			}
			initial := fixture.issueID + "-T25-initial"
			if response := fixture.post(event, fixture.secret, initial); response.Code != http.StatusAccepted {
				test.Fatalf("initial = %d", response.Code)
			}
			fixture.payload["action"] = action
			fixture.payload["sender"] = map[string]any{"id": 99, "login": "maintainer", "type": "User"}
			if response := fixture.post(event, fixture.secret, fixture.issueID+"-T25-withdrawal"); response.Code != http.StatusAccepted {
				test.Fatalf("withdrawal = %d", response.Code)
			}
			var state, reason string
			if err := testPool.QueryRow(context.Background(), `SELECT state, reason FROM github_webhook_revision WHERE source_delivery_guid=$1`, initial).Scan(&state, &reason); err != nil {
				test.Fatal(err)
			}
			if state != "denied" || reason != action {
				test.Fatalf("pending revision = %s/%s, want denied/%s", state, reason, action)
			}
			assertGitHubIntakeNoEffects(test, fixture)
		})
	}
}

func TestGitHubAdmissionWithdrawalScopeAndDispatchHistory(test *testing.T) {
	fixture := newGitHubIntakeFixture(test)
	delivery := fixture.issueID + "-withdrawal-scope"
	if response := fixture.post("pull_request_review", fixture.secret, delivery); response.Code != http.StatusAccepted {
		test.Fatalf("initial = %d", response.Code)
	}
	transaction, err := testPool.Begin(context.Background())
	if err != nil {
		test.Fatal(err)
	}
	defer transaction.Rollback(context.Background())
	for _, alternate := range []struct {
		field, value string
	}{
		{"workspace_id", "'00000000-0000-0000-0000-000000000001'::uuid"},
		{"installation_id", "2019002"}, {"repository_id", "9002"},
		{"object_kind", "'issue_comment'"}, {"object_id", "78"},
		{"state", "'dispatched'"},
	} {
		statement := `CREATE TEMP TABLE withdrawal_copy ON COMMIT DROP AS SELECT * FROM github_webhook_revision WHERE source_delivery_guid=$1 AND body_hash<>repeat('a',64)`
		if _, err := transaction.Exec(context.Background(), statement, delivery); err != nil {
			test.Fatal(err)
		}
		if _, err := transaction.Exec(context.Background(), `UPDATE withdrawal_copy SET id=gen_random_uuid(), `+alternate.field+`=`+alternate.value+`, body_hash=repeat('a',64)`); err != nil {
			test.Fatal(err)
		}
		if _, err := transaction.Exec(context.Background(), `INSERT INTO github_webhook_revision SELECT * FROM withdrawal_copy`); err != nil {
			test.Fatal(err)
		}
		if _, err := transaction.Exec(context.Background(), `DROP TABLE withdrawal_copy`); err != nil {
			test.Fatal(err)
		}
	}
	if err := fixture.handler.Queries.WithTx(transaction).DenyPendingGitHubWebhookRevisions(context.Background(), db.DenyPendingGitHubWebhookRevisionsParams{
		WorkspaceID: parseUUID(testWorkspaceID), InstallationID: 2019001, RepositoryID: 9001,
		ObjectKind: "review", ObjectID: 77, Reason: "dismissed",
	}); err != nil {
		test.Fatal(err)
	}
	var denied, unchanged, dispatched int
	if err := transaction.QueryRow(context.Background(), `SELECT count(*) FILTER (WHERE state='denied'), count(*) FILTER (WHERE state='deferred'), count(*) FILTER (WHERE state='dispatched') FROM github_webhook_revision WHERE source_delivery_guid=$1`, delivery).Scan(&denied, &unchanged, &dispatched); err != nil {
		test.Fatal(err)
	}
	if denied != 1 || unchanged != 5 || dispatched != 1 {
		test.Fatalf("withdrawal scope: denied=%d unchanged=%d dispatched=%d, want 1/5/1", denied, unchanged, dispatched)
	}
}
