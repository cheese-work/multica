package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
)

func newGitHubIntakeFixture(t *testing.T) githubReviewFixture {
	t.Helper()
	fixture := newGitHubReviewFixture(t)
	fixture.payload["sender"] = map[string]any{"id": 42, "login": "reviewer", "type": "User"}
	fixture.payload["repository"].(map[string]any)["id"] = 9001
	fixture.payload["repository"].(map[string]any)["full_name"] = "acme/widget"
	fixture.payload["pull_request"].(map[string]any)["head"] = map[string]any{
		"sha": "review-head", "repo": map[string]any{"id": 9001, "full_name": "acme/widget", "fork": false},
	}
	fixture.payload["review"].(map[string]any)["submitted_at"] = "2026-10-26T08:01:00Z"
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DELETE FROM github_webhook_revision WHERE workspace_id=$1`, testWorkspaceID)
		_, _ = testPool.Exec(context.Background(), `DELETE FROM github_webhook_intake WHERE delivery_guid LIKE $1`, fixture.issueID+"%")
	})
	return fixture
}

func assertGitHubIntakeNoEffects(t *testing.T, fixture githubReviewFixture) {
	t.Helper()
	if count := dbfx.Count(t, `SELECT count(*) FROM comment WHERE issue_id=$1`, fixture.issueID); count != 0 {
		t.Fatalf("GitHub text projected before admission: %d comments", count)
	}
	if count := dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id=$1 AND id<>$2`, fixture.issueID, fixture.source); count != 0 {
		t.Fatalf("actionable dispatch before gate G: %d runs", count)
	}
}

func TestGitHubIntakeT1Denied(t *testing.T) {
	for _, scenario := range []struct{ name, reason string }{
		{"unmapped", "unmapped_author"}, {"bot", "bot_author"}, {"fork", "fork_pull_request"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			fixture := newGitHubIntakeFixture(t)
			switch scenario.name {
			case "unmapped":
				dbfx.Exec(t, `UPDATE workspace SET settings='{}' WHERE id=$1`, testWorkspaceID)
			case "bot":
				fixture.payload["review"].(map[string]any)["user"].(map[string]any)["type"] = "Bot"
			case "fork":
				fixture.payload["pull_request"].(map[string]any)["head"].(map[string]any)["repo"] = map[string]any{"id": 9002, "full_name": "outsider/widget", "fork": true}
			}
			delivery := fixture.issueID + "-T1"
			if response := fixture.post("pull_request_review", fixture.secret, delivery); response.Code != http.StatusAccepted {
				t.Fatalf("intake: %d %s", response.Code, response.Body.String())
			}
			assertGitHubIntakeNoEffects(t, fixture)
			var state, reason string
			if err := testPool.QueryRow(context.Background(), `SELECT state, reason FROM github_webhook_revision WHERE source_delivery_guid=$1`, delivery).Scan(&state, &reason); err != nil {
				t.Fatalf("denial must be durable: %v", err)
			}
			if state != "denied" || reason != scenario.reason {
				t.Fatalf("denial = %s/%s, want denied/%s", state, reason, scenario.reason)
			}
		})
	}
}

func TestGitHubIntakeT2BadSignature(t *testing.T) {
	fixture := newGitHubIntakeFixture(t)
	delivery := fixture.issueID + "-T2"
	if response := fixture.post("pull_request_review", "wrong-secret", delivery); response.Code != http.StatusUnauthorized {
		t.Fatalf("bad signature = %d, want 401", response.Code)
	}
	if count := dbfx.Count(t, `SELECT count(*) FROM github_webhook_intake WHERE delivery_guid=$1`, delivery); count != 0 {
		t.Fatalf("bad signature persisted %d rows", count)
	}
	assertGitHubIntakeNoEffects(t, fixture)
}

type githubIntakeFailCommitStarter struct{ txStarter }
type githubIntakeFailCommitTx struct{ pgx.Tx }

func (starter githubIntakeFailCommitStarter) Begin(ctx context.Context) (pgx.Tx, error) {
	transaction, err := starter.txStarter.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return githubIntakeFailCommitTx{transaction}, nil
}

func (transaction githubIntakeFailCommitTx) Commit(context.Context) error {
	return errors.New("test intake database outage before commit")
}

func TestGitHubIntakeT6DatabaseFailureAndRedelivery(t *testing.T) {
	fixture := newGitHubIntakeFixture(t)
	dbfx.Exec(t, `UPDATE workspace SET settings='{}' WHERE id=$1`, testWorkspaceID)
	delivery := fixture.issueID + "-T6"
	starter := fixture.handler.TxStarter
	fixture.handler.TxStarter = githubIntakeFailCommitStarter{starter}
	if response := fixture.post("pull_request_review", fixture.secret, delivery); response.Code != http.StatusInternalServerError {
		t.Fatalf("database outage = %d, want 500", response.Code)
	}
	if count := dbfx.Count(t, `SELECT count(*) FROM github_webhook_intake WHERE delivery_guid=$1`, delivery); count != 0 {
		t.Fatalf("failed transaction persisted %d raw deliveries", count)
	}
	if count := dbfx.Count(t, `SELECT count(*) FROM github_webhook_revision WHERE source_delivery_guid=$1`, delivery); count != 0 {
		t.Fatalf("failed transaction persisted %d revisions", count)
	}
	fixture.handler.TxStarter = starter
	for attempt := 0; attempt < 2; attempt++ {
		if response := fixture.post("pull_request_review", fixture.secret, delivery); response.Code != http.StatusAccepted {
			t.Fatalf("redelivery: %d %s", response.Code, response.Body.String())
		}
	}
	if count := dbfx.Count(t, `SELECT count(*) FROM github_webhook_intake WHERE delivery_guid=$1`, delivery); count != 1 {
		t.Fatalf("redelivery raw rows = %d, want 1", count)
	}
	if count := dbfx.Count(t, `SELECT count(*) FROM github_webhook_revision WHERE source_delivery_guid=$1`, delivery); count != 1 {
		t.Fatalf("redelivery work rows = %d, want 1", count)
	}
	assertGitHubIntakeNoEffects(t, fixture)
}

func TestGitHubIntakeT7RevisionDeduplication(t *testing.T) {
	fixture := newGitHubIntakeFixture(t)
	body, _ := json.Marshal(fixture.payload)
	for _, suffix := range []string{"first", "first", "second"} {
		if response := fixture.post("pull_request_review", fixture.secret, fixture.issueID+"-T7-"+suffix); response.Code != http.StatusAccepted {
			t.Fatalf("intake: %d %s", response.Code, response.Body.String())
		}
	}
	assertGitHubIntakeNoEffects(t, fixture)
	if count := dbfx.Count(t, `SELECT count(*) FROM github_webhook_intake WHERE delivery_guid LIKE $1`, fixture.issueID+"-T7-%"); count != 2 {
		t.Fatalf("unique signed deliveries = %d, want 2", count)
	}
	if count := dbfx.Count(t, `SELECT count(*) FROM github_webhook_revision WHERE workspace_id=$1`, testWorkspaceID); count != 1 {
		t.Fatalf("revision work rows = %d, want 1", count)
	}
	var raw []byte
	var signature, event, state, reason string
	err := testPool.QueryRow(context.Background(), `SELECT payload, signature, event FROM github_webhook_intake WHERE delivery_guid=$1`, fixture.issueID+"-T7-first").Scan(&raw, &signature, &event)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != string(body) || !verifyWebhookSignature(fixture.secret, signature, raw) || event != "pull_request_review" {
		t.Fatalf("signed bytes were not retained exactly: %s", fmt.Sprint(event))
	}
	if err := testPool.QueryRow(context.Background(), `SELECT state, reason FROM github_webhook_revision WHERE workspace_id=$1`, testWorkspaceID).Scan(&state, &reason); err != nil {
		t.Fatal(err)
	}
	if state != "deferred" || reason == "" {
		t.Fatalf("notify-only work = %s/%s, want deferred with reason", state, reason)
	}
}

func TestGitHubIntakeCommentEvents(t *testing.T) {
	for _, event := range []string{"pull_request_review_comment", "issue_comment"} {
		for _, action := range []string{"created", "edited", "deleted"} {
			t.Run(event+"/"+action, func(t *testing.T) {
				fixture := newGitHubIntakeFixture(t)
				fixture.payload["action"] = action
				fixture.payload["comment"] = fixture.payload["review"]
				delete(fixture.payload, "review")
				if event == "issue_comment" {
					fixture.payload["issue"] = map[string]any{"number": 2019, "pull_request": map[string]any{"url": "https://api.github.com/repos/acme/widget/pulls/2019"}}
					delete(fixture.payload, "pull_request")
				}
				delivery := fixture.issueID + "-comment"
				if response := fixture.post(event, fixture.secret, delivery); response.Code != http.StatusAccepted {
					t.Fatalf("comment intake = %d %s", response.Code, response.Body.String())
				}
				var storedEvent, storedAction, reason string
				if err := testPool.QueryRow(context.Background(), `SELECT event, action, reason FROM github_webhook_revision WHERE source_delivery_guid=$1`, delivery).Scan(&storedEvent, &storedAction, &reason); err != nil {
					t.Fatal(err)
				}
				if storedEvent != event || storedAction != action || reason == "" {
					t.Fatalf("comment metadata = %s/%s/%s", storedEvent, storedAction, reason)
				}
				assertGitHubIntakeNoEffects(t, fixture)
			})
		}
	}
}

func TestGitHubIntakeDeliveryConflictAndNewRevision(t *testing.T) {
	fixture := newGitHubIntakeFixture(t)
	delivery := fixture.issueID + "-original"
	if response := fixture.post("pull_request_review", fixture.secret, delivery); response.Code != http.StatusAccepted {
		t.Fatalf("initial delivery = %d", response.Code)
	}
	fixture.payload["review"].(map[string]any)["body"] = "Changed signed revision"
	if response := fixture.post("pull_request_review", fixture.secret, delivery); response.Code != http.StatusConflict {
		t.Fatalf("GUID collision = %d, want 409", response.Code)
	}
	if response := fixture.post("pull_request_review", fixture.secret, fixture.issueID+"-new"); response.Code != http.StatusAccepted {
		t.Fatalf("new revision = %d", response.Code)
	}
	if count := dbfx.Count(t, `SELECT count(*) FROM github_webhook_intake WHERE delivery_guid LIKE $1`, fixture.issueID+"%"); count != 2 {
		t.Fatalf("raw deliveries = %d, want 2", count)
	}
	if count := dbfx.Count(t, `SELECT count(*) FROM github_webhook_revision WHERE workspace_id=$1`, testWorkspaceID); count != 2 {
		t.Fatalf("distinct revisions = %d, want 2", count)
	}
	assertGitHubIntakeNoEffects(t, fixture)
}

func TestGitHubIntakeConcurrentRevisionDeduplication(t *testing.T) {
	fixture := newGitHubIntakeFixture(t)
	var workers sync.WaitGroup
	statuses := make(chan int, 6)
	for delivery := 0; delivery < 6; delivery++ {
		workers.Go(func() {
			statuses <- fixture.post("pull_request_review", fixture.secret, fmt.Sprintf("%s-concurrent-%d", fixture.issueID, delivery)).Code
		})
	}
	workers.Wait()
	close(statuses)
	for status := range statuses {
		if status != http.StatusAccepted {
			t.Errorf("concurrent intake = %d", status)
		}
	}
	if count := dbfx.Count(t, `SELECT count(*) FROM github_webhook_intake WHERE delivery_guid LIKE $1`, fixture.issueID+"%"); count != 6 {
		t.Fatalf("concurrent deliveries = %d, want 6", count)
	}
	if count := dbfx.Count(t, `SELECT count(*) FROM github_webhook_revision WHERE workspace_id=$1`, testWorkspaceID); count != 1 {
		t.Fatalf("concurrent revision rows = %d, want 1", count)
	}
	assertGitHubIntakeNoEffects(t, fixture)
}

func TestGitHubIntakeMissingDeliveryGUID(t *testing.T) {
	fixture := newGitHubIntakeFixture(t)
	if response := fixture.post("pull_request_review", fixture.secret, ""); response.Code != http.StatusBadRequest {
		t.Fatalf("missing GUID = %d, want 400", response.Code)
	}
	assertGitHubIntakeNoEffects(t, fixture)
}

func TestGitHubIntakeWorkspaceDeletion(test *testing.T) {
	fixture := newGitHubIntakeFixture(test)
	delivery := fixture.issueID + "-workspace-deletion"
	if response := fixture.post("pull_request_review", fixture.secret, delivery); response.Code != http.StatusAccepted {
		test.Fatalf("initial delivery = %d", response.Code)
	}
	otherWorkspaceID := dbfx.Workspace(test, "Other intake workspace", "github-intake-"+fixture.issueID)
	ctx := context.Background()
	transaction, err := testPool.Begin(ctx)
	if err != nil {
		test.Fatal(err)
	}
	defer transaction.Rollback(ctx)
	if _, err := transaction.Exec(ctx, `
INSERT INTO github_webhook_revision (
    workspace_id, installation_id, repository_id, pr_number, object_kind, object_id,
    body_hash, source_delivery_guid, event, action, sender_id, author_id, author_login, author_type,
    head_sha, review_state, state, reason
) VALUES ($1, 2019001, 9001, 2019, 'review', 77, repeat('a', 64), $2,
    'pull_request_review', 'submitted', 42, 42, 'reviewer', 'User', '',
    'changes_requested', 'deferred', 'awaiting_admission')`, otherWorkspaceID, delivery); err != nil {
		test.Fatal(err)
	}
	if err := fixture.handler.Queries.WithTx(transaction).DeleteWorkspacePullRequests(ctx, parseUUID(testWorkspaceID)); err != nil {
		test.Fatal(err)
	}
	var remaining int
	if err := transaction.QueryRow(ctx, `SELECT count(*) FROM github_webhook_revision WHERE workspace_id=$1`, testWorkspaceID).Scan(&remaining); err != nil {
		test.Fatal(err)
	}
	if remaining != 0 {
		test.Fatalf("deleted workspace retains %d intake revisions, want 0", remaining)
	}
	if err := transaction.QueryRow(ctx, `SELECT count(*) FROM github_webhook_revision WHERE workspace_id=$1`, otherWorkspaceID).Scan(&remaining); err != nil {
		test.Fatal(err)
	}
	if remaining != 1 {
		test.Fatalf("other workspace retains %d intake revisions, want 1", remaining)
	}
	if err := transaction.QueryRow(ctx, `SELECT count(*) FROM github_webhook_intake WHERE delivery_guid=$1`, delivery).Scan(&remaining); err != nil {
		test.Fatal(err)
	}
	if remaining != 1 {
		test.Fatalf("shared signed deliveries = %d, want 1", remaining)
	}
}
