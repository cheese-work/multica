package handler

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// buildMergedPRWebhookBody returns a minimal `pull_request` webhook body for
// a merged PR referencing issueIdentifier by title prefix (link-only, no
// closing keyword) so the merge announcement path can be exercised
// independent of the close_intent / advance-to-done gate.
func buildMergedPRWebhookBody(issueIdentifier string, prNumber int, repoOwner, repoName string, installationID int64) map[string]any {
	return map[string]any{
		"action": "closed",
		"pull_request": map[string]any{
			"number":           prNumber,
			"html_url":         "https://github.com/" + repoOwner + "/" + repoName + "/pull/999",
			"title":            issueIdentifier + ": ship it",
			"body":             "",
			"state":            "closed",
			"draft":            false,
			"merged":           true,
			"merged_at":        "2026-09-10T09:54:34Z",
			"closed_at":        "2026-09-10T09:54:34Z",
			"created_at":       "2026-09-10T09:00:00Z",
			"updated_at":       "2026-09-10T09:54:34Z",
			"merge_commit_sha": "ae18acf237df4b9862d52f82aa7d866052613507",
			"head":             map[string]any{"ref": "fix/ship-it"},
			"user":             map[string]any{"login": "octocat", "avatar_url": ""},
		},
		"repository": map[string]any{
			"id":    424242,
			"name":  repoName,
			"owner": map[string]any{"login": repoOwner},
		},
		"installation": map[string]any{"id": installationID},
	}
}

func postSignedGitHubWebhook(t *testing.T, secret string, body map[string]any, deliveryGUID string) *testutil.Response {
	t.Helper()
	raw, _ := json.Marshal(body)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(raw)
	sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	req := httptest.NewRequest("POST", "/api/webhooks/github", bytes.NewReader(raw))
	req.Header.Set("X-GitHub-Event", "pull_request")
	req.Header.Set("X-Hub-Signature-256", sig)
	if deliveryGUID != "" {
		req.Header.Set("X-GitHub-Delivery", deliveryGUID)
	}
	return testutil.Call(t, testHandler.HandleGitHubWebhook, req).Want(http.StatusAccepted)
}

// TestWebhook_MergedPR_EnqueuesAndDeliversMergeAnnouncement is the primary
// TDD case for CHE-374/CHE-379: a title-linked PR (no closing keyword, so it
// carries no close_intent and must not advance the issue) merges. The
// webhook must durably enqueue exactly one github_merge_announcement row,
// and running the worker must produce exactly one system comment on the
// issue, with the issue's status left untouched.
func TestWebhook_MergedPR_EnqueuesAndDeliversMergeAnnouncement(t *testing.T) {
	if testHandler == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	ctx := context.Background()
	secret := "merge-announce-test-secret"
	t.Setenv("GITHUB_WEBHOOK_SECRET", secret)

	req := newRequest("POST", "/api/issues?workspace_id="+testWorkspaceID, map[string]any{
		"title":  "Merge announcement test issue",
		"status": "in_progress",
	})
	w := testutil.Call(t, testHandler.CreateIssue, req).Want(http.StatusCreated)
	var created IssueResponse
	json.NewDecoder(w.Body).Decode(&created)

	t.Cleanup(func() {
		testPool.Exec(ctx, `DELETE FROM github_merge_announcement WHERE issue_id = $1`, created.ID)
		testPool.Exec(ctx, `DELETE FROM comment WHERE issue_id = $1`, created.ID)
		testPool.Exec(ctx, `DELETE FROM issue_pull_request WHERE issue_id = $1`, created.ID)
		testPool.Exec(ctx, `DELETE FROM github_pull_request WHERE workspace_id = $1`, testWorkspaceID)
		testPool.Exec(ctx, `DELETE FROM github_installation WHERE workspace_id = $1`, testWorkspaceID)
		testPool.Exec(ctx, `DELETE FROM activity_log WHERE issue_id = $1`, created.ID)
		testPool.Exec(ctx, `DELETE FROM issue WHERE id = $1`, created.ID)
	})

	const installationID int64 = 99887711
	if _, err := testHandler.Queries.CreateGitHubInstallation(ctx, db.CreateGitHubInstallationParams{
		WorkspaceID:    parseUUID(testWorkspaceID),
		InstallationID: installationID,
		AccountLogin:   "merge-announce-acct",
		AccountType:    "User",
	}); err != nil {
		t.Fatalf("CreateGitHubInstallation: %v", err)
	}

	body := buildMergedPRWebhookBody(created.Identifier, 4242, "acme", "widget", installationID)
	postSignedGitHubWebhook(t, secret, body, "delivery-guid-1")

	// Step 1: exactly one pending (or already-delivered, if the inline
	// enqueue synchronously raced the assertion) announcement row exists —
	// this is the durable-enqueue assertion, independent of the worker.
	announcements, err := testHandler.Queries.ListGitHubMergeAnnouncementsByIssue(ctx, parseUUID(created.ID))
	if err != nil {
		t.Fatalf("ListGitHubMergeAnnouncementsByIssue: %v", err)
	}
	if len(announcements) != 1 {
		t.Fatalf("expected 1 enqueued merge announcement, got %d", len(announcements))
	}
	if announcements[0].Status != "pending" {
		t.Fatalf("expected announcement status 'pending' before worker runs, got %q", announcements[0].Status)
	}
	if announcements[0].MergeCommitSha != "ae18acf237df4b9862d52f82aa7d866052613507" {
		t.Errorf("expected merge_commit_sha to be captured from payload, got %q", announcements[0].MergeCommitSha)
	}

	// Issue status must be untouched by the enqueue — no close_intent was
	// declared, so the advance-to-done gate must not have fired either.
	beforeWorker, err := testHandler.Queries.GetIssue(ctx, parseUUID(created.ID))
	if err != nil {
		t.Fatalf("GetIssue: %v", err)
	}
	if beforeWorker.Status != "in_progress" {
		t.Fatalf("expected issue status unchanged at 'in_progress' before worker delivery, got %q", beforeWorker.Status)
	}

	// Step 2: run the worker synchronously (ProcessNext, not Run — no
	// goroutine needed for a deterministic test) to deliver the comment.
	worker := NewMergeAnnouncementWorker(testHandler)
	worked, err := worker.ProcessNext(ctx)
	if err != nil {
		t.Fatalf("ProcessNext: %v", err)
	}
	if !worked {
		t.Fatalf("expected ProcessNext to claim and deliver the pending announcement")
	}

	var commentCount int
	if err := testPool.QueryRow(ctx,
		`SELECT count(*) FROM comment WHERE issue_id = $1 AND author_type = 'system'`,
		created.ID,
	).Scan(&commentCount); err != nil {
		t.Fatalf("count system comments: %v", err)
	}
	if commentCount != 1 {
		t.Fatalf("expected exactly 1 system comment after delivery, got %d", commentCount)
	}

	var content string
	if err := testPool.QueryRow(ctx,
		`SELECT content FROM comment WHERE issue_id = $1 AND author_type = 'system' LIMIT 1`,
		created.ID,
	).Scan(&content); err != nil {
		t.Fatalf("read system comment content: %v", err)
	}
	if !bytes.Contains([]byte(content), []byte("PR #4242 merged")) {
		t.Errorf("expected comment to name the merged PR, got %q", content)
	}
	if !bytes.Contains([]byte(content), []byte("this PR does not declare completion")) {
		t.Errorf("expected deterministic next-action text, got %q", content)
	}

	afterWorker, err := testHandler.Queries.GetIssue(ctx, parseUUID(created.ID))
	if err != nil {
		t.Fatalf("GetIssue: %v", err)
	}
	if afterWorker.Status != "in_progress" {
		t.Errorf("expected issue status unchanged at 'in_progress' after delivery, got %q", afterWorker.Status)
	}

	delivered, err := testHandler.Queries.ListGitHubMergeAnnouncementsByIssue(ctx, parseUUID(created.ID))
	if err != nil {
		t.Fatalf("ListGitHubMergeAnnouncementsByIssue after delivery: %v", err)
	}
	if len(delivered) != 1 || delivered[0].Status != "delivered" {
		t.Fatalf("expected 1 delivered announcement, got %+v", delivered)
	}
	if !delivered[0].CommentID.Valid {
		t.Errorf("expected delivered announcement to record comment_id")
	}
}

// TestWebhook_MergedPR_RedeliveredWebhookDoesNotDuplicateAnnouncement covers
// the redelivery dedup requirement: GitHub mints a new delivery GUID on
// every redelivery of the same logical merge event, so the identity index
// (workspace, provider, repository, pr_number, issue, event_kind) — not the
// delivery GUID — must be what prevents a second pending row and a second
// comment.
func TestWebhook_MergedPR_RedeliveredWebhookDoesNotDuplicateAnnouncement(t *testing.T) {
	if testHandler == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	ctx := context.Background()
	secret := "merge-announce-redelivery-secret"
	t.Setenv("GITHUB_WEBHOOK_SECRET", secret)

	req := newRequest("POST", "/api/issues?workspace_id="+testWorkspaceID, map[string]any{
		"title":  "Redelivery dedup test issue",
		"status": "in_progress",
	})
	w := testutil.Call(t, testHandler.CreateIssue, req).Want(http.StatusCreated)
	var created IssueResponse
	json.NewDecoder(w.Body).Decode(&created)

	t.Cleanup(func() {
		testPool.Exec(ctx, `DELETE FROM github_merge_announcement WHERE issue_id = $1`, created.ID)
		testPool.Exec(ctx, `DELETE FROM comment WHERE issue_id = $1`, created.ID)
		testPool.Exec(ctx, `DELETE FROM issue_pull_request WHERE issue_id = $1`, created.ID)
		testPool.Exec(ctx, `DELETE FROM github_pull_request WHERE workspace_id = $1`, testWorkspaceID)
		testPool.Exec(ctx, `DELETE FROM github_installation WHERE workspace_id = $1`, testWorkspaceID)
		testPool.Exec(ctx, `DELETE FROM activity_log WHERE issue_id = $1`, created.ID)
		testPool.Exec(ctx, `DELETE FROM issue WHERE id = $1`, created.ID)
	})

	const installationID int64 = 99887722
	if _, err := testHandler.Queries.CreateGitHubInstallation(ctx, db.CreateGitHubInstallationParams{
		WorkspaceID:    parseUUID(testWorkspaceID),
		InstallationID: installationID,
		AccountLogin:   "redelivery-acct",
		AccountType:    "User",
	}); err != nil {
		t.Fatalf("CreateGitHubInstallation: %v", err)
	}

	body := buildMergedPRWebhookBody(created.Identifier, 5353, "acme", "gizmo", installationID)

	// First delivery.
	postSignedGitHubWebhook(t, secret, body, "delivery-guid-A")
	// Redelivery: GitHub's own retry semantics — identical payload, new
	// delivery GUID.
	postSignedGitHubWebhook(t, secret, body, "delivery-guid-B")

	announcements, err := testHandler.Queries.ListGitHubMergeAnnouncementsByIssue(ctx, parseUUID(created.ID))
	if err != nil {
		t.Fatalf("ListGitHubMergeAnnouncementsByIssue: %v", err)
	}
	if len(announcements) != 1 {
		t.Fatalf("expected redelivery to converge on 1 announcement row, got %d", len(announcements))
	}

	worker := NewMergeAnnouncementWorker(testHandler)
	for {
		worked, err := worker.ProcessNext(ctx)
		if err != nil {
			t.Fatalf("ProcessNext: %v", err)
		}
		if !worked {
			break
		}
	}

	var commentCount int
	if err := testPool.QueryRow(ctx,
		`SELECT count(*) FROM comment WHERE issue_id = $1 AND author_type = 'system'`,
		created.ID,
	).Scan(&commentCount); err != nil {
		t.Fatalf("count system comments: %v", err)
	}
	if commentCount != 1 {
		t.Fatalf("expected redelivery to still produce exactly 1 comment, got %d", commentCount)
	}
}
