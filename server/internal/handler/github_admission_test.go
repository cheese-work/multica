package handler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

func TestGitHubAdmissionSignedMetadata(t *testing.T) {
	fixture := newGitHubIntakeFixture(t)
	fixture.payload["review"].(map[string]any)["body"] = nil
	fixture.payload["review"].(map[string]any)["updated_at"] = "2026-10-26T08:02:00Z"
	delivery := fixture.issueID + "-T22-metadata"
	if response := fixture.post("pull_request_review", fixture.secret, delivery); response.Code != http.StatusAccepted {
		t.Fatalf("intake = %d", response.Code)
	}
	var event, action, kind, hash, head, state string
	var objectID, senderID, authorID int64
	var createdAt, updatedAt, submittedAt time.Time
	if err := testPool.QueryRow(context.Background(), `SELECT event, action, object_kind, object_id, body_hash,
head_sha, sender_id, author_id, review_state, object_created_at, object_updated_at, object_submitted_at
FROM github_webhook_revision WHERE source_delivery_guid=$1`, delivery).Scan(
		&event, &action, &kind, &objectID, &hash, &head, &senderID, &authorID, &state, &createdAt, &updatedAt, &submittedAt,
	); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(nil)
	if event != "pull_request_review" || action != "submitted" || kind != "review" || objectID != 77 || hash != hex.EncodeToString(digest[:]) || head != "review-head" || senderID != 42 || authorID != 42 || state != "changes_requested" {
		t.Fatalf("signed metadata changed: %s/%s/%s/%d/%s/%s/%d/%d/%s", event, action, kind, objectID, hash, head, senderID, authorID, state)
	}
	if updatedAt.Sub(createdAt) != 2*time.Minute || submittedAt.Sub(createdAt) != time.Minute {
		t.Fatal("signed review timestamps were not retained independently")
	}
	assertGitHubIntakeNoEffects(t, fixture)
}

func TestGitHubAdmissionNormalizationTable(t *testing.T) {
	fixture := newGitHubIntakeFixture(t)
	fixture.payload["comment"] = fixture.payload["review"]
	fixture.payload["issue"] = map[string]any{"number": 2019, "pull_request": map[string]any{"url": "https://api.github.com/repos/acme/widget/pulls/2019"}}
	body, err := json.Marshal(fixture.payload)
	if err != nil {
		t.Fatal(err)
	}
	var payload githubIntakePayload
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatal(err)
	}
	for _, event := range []string{"pull_request_review", "pull_request_review_comment", "issue_comment"} {
		for _, action := range []string{"submitted", "created", "edited", "dismissed", "deleted", "other"} {
			payload.Action = action
			_, _, _, reason := payload.revision(event)
			valid := action == "edited" || (event == "pull_request_review" && (action == "submitted" || action == "dismissed")) || (event != "pull_request_review" && (action == "created" || action == "deleted"))
			if (reason == "") != valid {
				t.Errorf("%s/%s normalization = %q, valid=%t", event, action, reason, valid)
			}
		}
	}
}

func TestGitHubAdmissionRejectsUnknownEvent(t *testing.T) {
	fixture := newGitHubIntakeFixture(t)
	body, err := json.Marshal(fixture.payload)
	if err != nil {
		t.Fatal(err)
	}
	var payload githubIntakePayload
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatal(err)
	}
	_, _, _, reason := payload.revision("unknown")
	if reason != "unsupported_event" {
		t.Fatalf("unknown event admitted: reason = %q, want unsupported_event", reason)
	}
}

func TestGitHubAdmissionT3ForeignEditQuarantined(t *testing.T) {
	fixture := newGitHubIntakeFixture(t)
	fixture.payload["action"] = "edited"
	fixture.payload["sender"] = map[string]any{"id": 99, "login": "other-writer", "type": "User"}
	delivery := fixture.issueID + "-T3"
	if response := fixture.post("pull_request_review", fixture.secret, delivery); response.Code != http.StatusAccepted {
		t.Fatalf("intake = %d %s", response.Code, response.Body.String())
	}
	var state, reason string
	if err := testPool.QueryRow(context.Background(), `SELECT state, reason FROM github_webhook_revision WHERE source_delivery_guid=$1`, delivery).Scan(&state, &reason); err != nil {
		t.Fatal(err)
	}
	if state != "quarantined" || reason != "foreign_editor" {
		t.Fatalf("foreign edit = %s/%s, want quarantined/foreign_editor", state, reason)
	}
	assertGitHubIntakeNoEffects(t, fixture)
}

func TestGitHubAdmissionT23SameBodySelfEditIsNewRevision(t *testing.T) {
	fixture := newGitHubIntakeFixture(t)
	for _, suffix := range []string{"initial", "edit", "redelivery"} {
		if suffix != "initial" {
			fixture.payload["action"] = "edited"
			fixture.payload["review"].(map[string]any)["updated_at"] = "2026-10-26T08:05:00Z"
		}
		if response := fixture.post("pull_request_review", fixture.secret, fixture.issueID+"-T23-"+suffix); response.Code != http.StatusAccepted {
			t.Fatalf("%s = %d", suffix, response.Code)
		}
	}
	if count := dbfx.Count(t, `SELECT count(*) FROM github_webhook_revision WHERE workspace_id=$1`, testWorkspaceID); count != 2 {
		t.Fatalf("initial plus same-body self-edit = %d revisions, want 2", count)
	}
	assertGitHubIntakeNoEffects(t, fixture)
}

func TestGitHubAdmissionT25WithdrawalDeniesPending(t *testing.T) {
	for _, event := range []string{"pull_request_review", "pull_request_review_comment", "issue_comment"} {
		t.Run(event, func(t *testing.T) {
			fixture := newGitHubIntakeFixture(t)
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
				t.Fatalf("initial = %d", response.Code)
			}
			fixture.payload["action"] = action
			fixture.payload["sender"] = map[string]any{"id": 99, "login": "maintainer", "type": "User"}
			if response := fixture.post(event, fixture.secret, fixture.issueID+"-T25-withdrawal"); response.Code != http.StatusAccepted {
				t.Fatalf("withdrawal = %d", response.Code)
			}
			var state, reason string
			if err := testPool.QueryRow(context.Background(), `SELECT state, reason FROM github_webhook_revision WHERE source_delivery_guid=$1`, initial).Scan(&state, &reason); err != nil {
				t.Fatal(err)
			}
			if state != "denied" || reason != action {
				t.Fatalf("pending revision = %s/%s, want denied/%s", state, reason, action)
			}
			assertGitHubIntakeNoEffects(t, fixture)
		})
	}
}
