package handler

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/integrations/ghsnapshot"
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
	// CHE-374 review fix T3: the comment now names the PR as a link (still
	// containing "#4242"), includes the PR's html_url, and — since this test's
	// webhook body declares no closing keyword — states that no closing
	// intent was declared for this issue, rather than the old always-on
	// "this PR does not declare completion" wording.
	if !bytes.Contains([]byte(content), []byte("#4242")) {
		t.Errorf("expected comment to name the merged PR, got %q", content)
	}
	if !bytes.Contains([]byte(content), []byte("https://github.com/acme/widget/pull/999")) {
		t.Errorf("expected comment to include the PR html_url, got %q", content)
	}
	if !bytes.Contains([]byte(content), []byte("did not declare closing intent")) {
		t.Errorf("expected deterministic next-action text for a non-close-intent merge, got %q", content)
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

// failingTxStarter is a txStarter whose Begin always errors, used to force
// mirrorPullRequestForWorkspace's transaction open to fail deterministically
// (CHE-374 review fix T1: without a real induced failure, atomicity can only
// be argued from reading the code, not demonstrated).
type failingTxStarter struct{}

func (failingTxStarter) Begin(ctx context.Context) (pgx.Tx, error) {
	return nil, errors.New("failingTxStarter: induced Begin failure for test")
}

// TestWebhook_MergedPR_TransactionFailureIsAtomicAndSurfaced is the T1
// regression guard: if the PR-mirror/link/announcement transaction cannot
// even begin, HandleGitHubWebhook must surface a non-202 response (so GitHub
// redelivers) instead of swallowing the error, and none of the writes that
// would have happened inside that transaction (PR mirror row, announcement
// row) may be visible.
func TestWebhook_MergedPR_TransactionFailureIsAtomicAndSurfaced(t *testing.T) {
	if testHandler == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	ctx := context.Background()
	secret := "tx-failure-secret"
	t.Setenv("GITHUB_WEBHOOK_SECRET", secret)

	req := newRequest("POST", "/api/issues?workspace_id="+testWorkspaceID, map[string]any{
		"title":  "Transaction failure test issue",
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

	const installationID int64 = 99887733
	if _, err := testHandler.Queries.CreateGitHubInstallation(ctx, db.CreateGitHubInstallationParams{
		WorkspaceID:    parseUUID(testWorkspaceID),
		InstallationID: installationID,
		AccountLogin:   "tx-failure-acct",
		AccountType:    "User",
	}); err != nil {
		t.Fatalf("CreateGitHubInstallation: %v", err)
	}

	body := buildMergedPRWebhookBody(created.Identifier, 7171, "acme", "gadget", installationID)
	raw, _ := json.Marshal(body)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(raw)
	sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	realTxStarter := testHandler.TxStarter
	testHandler.TxStarter = failingTxStarter{}
	t.Cleanup(func() { testHandler.TxStarter = realTxStarter })

	postReq := httptest.NewRequest("POST", "/api/webhooks/github", bytes.NewReader(raw))
	postReq.Header.Set("X-GitHub-Event", "pull_request")
	postReq.Header.Set("X-Hub-Signature-256", sig)
	postReq.Header.Set("X-GitHub-Delivery", "tx-failure-delivery-1")

	// Unlike postSignedGitHubWebhook, this must NOT expect 202: a transaction
	// that fails to open must be surfaced as a server error so GitHub
	// redelivers, not silently accepted and dropped.
	rr := httptest.NewRecorder()
	testHandler.HandleGitHubWebhook(rr, postReq)
	if rr.Code == http.StatusAccepted {
		t.Fatalf("expected a non-202 response when the mirror transaction fails to begin, got %d", rr.Code)
	}
	if rr.Code < 500 {
		t.Fatalf("expected a 5xx response surfacing the transaction failure, got %d: %s", rr.Code, rr.Body.String())
	}

	// Restore the real TxStarter before asserting DB state so the assertions
	// themselves can query normally.
	testHandler.TxStarter = realTxStarter

	var prCount int
	if err := testPool.QueryRow(ctx,
		`SELECT count(*) FROM github_pull_request WHERE workspace_id = $1 AND pr_number = $2`,
		testWorkspaceID, 7171,
	).Scan(&prCount); err != nil {
		t.Fatalf("count github_pull_request: %v", err)
	}
	if prCount != 0 {
		t.Errorf("expected no PR mirror row when the transaction never committed, got %d", prCount)
	}

	announcements, err := testHandler.Queries.ListGitHubMergeAnnouncementsByIssue(ctx, parseUUID(created.ID))
	if err != nil {
		t.Fatalf("ListGitHubMergeAnnouncementsByIssue: %v", err)
	}
	if len(announcements) != 0 {
		t.Errorf("expected no merge announcement when the transaction never committed, got %d", len(announcements))
	}
}

// TestWebhook_StalePullRequestPayloadDoesNotDemoteMergedState is the T2
// regression guard: a redelivered or out-of-order `opened`/`synchronize`
// webhook that arrives after the PR has already been recorded as merged must
// not be allowed to overwrite state/merged_at back to open — merged is
// terminal.
func TestWebhook_StalePullRequestPayloadDoesNotDemoteMergedState(t *testing.T) {
	if testHandler == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	ctx := context.Background()
	secret := "stale-payload-secret"
	t.Setenv("GITHUB_WEBHOOK_SECRET", secret)

	req := newRequest("POST", "/api/issues?workspace_id="+testWorkspaceID, map[string]any{
		"title":  "Stale payload test issue",
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

	const installationID int64 = 99887744
	if _, err := testHandler.Queries.CreateGitHubInstallation(ctx, db.CreateGitHubInstallationParams{
		WorkspaceID:    parseUUID(testWorkspaceID),
		InstallationID: installationID,
		AccountLogin:   "stale-payload-acct",
		AccountType:    "User",
	}); err != nil {
		t.Fatalf("CreateGitHubInstallation: %v", err)
	}

	const prNumber = 8181
	mergedBody := buildMergedPRWebhookBody(created.Identifier, prNumber, "acme", "sprocket", installationID)
	postSignedGitHubWebhook(t, secret, mergedBody, "stale-delivery-merged")

	pr, err := testHandler.Queries.GetGitHubPullRequest(ctx, db.GetGitHubPullRequestParams{
		WorkspaceID: parseUUID(testWorkspaceID),
		RepoOwner:   "acme",
		RepoName:    "sprocket",
		PrNumber:    prNumber,
	})
	if err != nil {
		t.Fatalf("GetGitHubPullRequest after merge: %v", err)
	}
	if pr.State != "merged" {
		t.Fatalf("expected PR state 'merged' right after the merge webhook, got %q", pr.State)
	}
	if !pr.MergedAt.Valid {
		t.Fatalf("expected merged_at to be set right after the merge webhook")
	}

	// A stale/out-of-order "opened" webhook for the same PR arrives after the
	// merge was already recorded — a real scenario when GitHub redelivers an
	// earlier event out of order, or a webhook queue is delayed. It must not
	// demote the already-terminal merged state.
	staleBody := map[string]any{
		"action": "opened",
		"pull_request": map[string]any{
			"number":     prNumber,
			"html_url":   "https://github.com/acme/sprocket/pull/999",
			"title":      created.Identifier + ": ship it",
			"body":       "",
			"state":      "open",
			"draft":      false,
			"merged":     false,
			"merged_at":  nil,
			"closed_at":  nil,
			"created_at": "2026-09-10T09:00:00Z",
			"updated_at": "2026-09-10T09:00:00Z",
			"head":       map[string]any{"ref": "fix/ship-it"},
			"user":       map[string]any{"login": "octocat", "avatar_url": ""},
		},
		"repository": map[string]any{
			"id":    424242,
			"name":  "sprocket",
			"owner": map[string]any{"login": "acme"},
		},
		"installation": map[string]any{"id": installationID},
	}
	postSignedGitHubWebhook(t, secret, staleBody, "stale-delivery-reopened")

	prAfterStale, err := testHandler.Queries.GetGitHubPullRequest(ctx, db.GetGitHubPullRequestParams{
		WorkspaceID: parseUUID(testWorkspaceID),
		RepoOwner:   "acme",
		RepoName:    "sprocket",
		PrNumber:    prNumber,
	})
	if err != nil {
		t.Fatalf("GetGitHubPullRequest after stale reopen: %v", err)
	}
	if prAfterStale.State != "merged" {
		t.Errorf("stale 'opened' webhook demoted merged PR state to %q, want it to remain 'merged'", prAfterStale.State)
	}
	if !prAfterStale.MergedAt.Valid {
		t.Errorf("stale 'opened' webhook cleared merged_at, want it to remain set")
	}
}

// TestWebhook_ReopenedPRTransitionsClosedBackToOpen is the round-2 item-4
// regression guard: UpsertGitHubPullRequest's stale-payload guard must only
// protect a stored `merged` state, not `closed` — a genuine GitHub reopen
// (action=reopened) sends merged=false/state=open and must be allowed to move
// a closed PR back to open, since derivePRState explicitly maps that action
// to state="open".
func TestWebhook_ReopenedPRTransitionsClosedBackToOpen(t *testing.T) {
	if testHandler == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	ctx := context.Background()
	secret := "reopen-transition-secret"
	t.Setenv("GITHUB_WEBHOOK_SECRET", secret)

	req := newRequest("POST", "/api/issues?workspace_id="+testWorkspaceID, map[string]any{
		"title":  "Reopen transition test issue",
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

	const installationID int64 = 99887788
	if _, err := testHandler.Queries.CreateGitHubInstallation(ctx, db.CreateGitHubInstallationParams{
		WorkspaceID:    parseUUID(testWorkspaceID),
		InstallationID: installationID,
		AccountLogin:   "reopen-transition-acct",
		AccountType:    "User",
	}); err != nil {
		t.Fatalf("CreateGitHubInstallation: %v", err)
	}

	const prNumber = 9292
	// Close the PR without merging (merged=false, state=closed).
	closedBody := map[string]any{
		"action": "closed",
		"pull_request": map[string]any{
			"number":     prNumber,
			"html_url":   "https://github.com/acme/sprocket/pull/9292",
			"title":      created.Identifier + ": ship it",
			"body":       "",
			"state":      "closed",
			"draft":      false,
			"merged":     false,
			"merged_at":  nil,
			"closed_at":  "2026-09-10T09:00:00Z",
			"created_at": "2026-09-10T08:00:00Z",
			"updated_at": "2026-09-10T09:00:00Z",
			"head":       map[string]any{"ref": "fix/ship-it"},
			"user":       map[string]any{"login": "octocat", "avatar_url": ""},
		},
		"repository": map[string]any{
			"id":    525252,
			"name":  "sprocket",
			"owner": map[string]any{"login": "acme"},
		},
		"installation": map[string]any{"id": installationID},
	}
	postSignedGitHubWebhook(t, secret, closedBody, "reopen-delivery-closed")

	prAfterClose, err := testHandler.Queries.GetGitHubPullRequest(ctx, db.GetGitHubPullRequestParams{
		WorkspaceID: parseUUID(testWorkspaceID),
		RepoOwner:   "acme",
		RepoName:    "sprocket",
		PrNumber:    prNumber,
	})
	if err != nil {
		t.Fatalf("GetGitHubPullRequest after close: %v", err)
	}
	if prAfterClose.State != "closed" {
		t.Fatalf("expected PR state 'closed' right after the close webhook, got %q", prAfterClose.State)
	}

	// GitHub reopens the PR: action=reopened, merged=false, state=open.
	reopenedBody := map[string]any{
		"action": "reopened",
		"pull_request": map[string]any{
			"number":     prNumber,
			"html_url":   "https://github.com/acme/sprocket/pull/9292",
			"title":      created.Identifier + ": ship it",
			"body":       "",
			"state":      "open",
			"draft":      false,
			"merged":     false,
			"merged_at":  nil,
			"closed_at":  nil,
			"created_at": "2026-09-10T08:00:00Z",
			"updated_at": "2026-09-10T09:05:00Z",
			"head":       map[string]any{"ref": "fix/ship-it"},
			"user":       map[string]any{"login": "octocat", "avatar_url": ""},
		},
		"repository": map[string]any{
			"id":    525252,
			"name":  "sprocket",
			"owner": map[string]any{"login": "acme"},
		},
		"installation": map[string]any{"id": installationID},
	}
	postSignedGitHubWebhook(t, secret, reopenedBody, "reopen-delivery-reopened")

	prAfterReopen, err := testHandler.Queries.GetGitHubPullRequest(ctx, db.GetGitHubPullRequestParams{
		WorkspaceID: parseUUID(testWorkspaceID),
		RepoOwner:   "acme",
		RepoName:    "sprocket",
		PrNumber:    prNumber,
	})
	if err != nil {
		t.Fatalf("GetGitHubPullRequest after reopen: %v", err)
	}
	if prAfterReopen.State != "open" {
		t.Errorf("genuine reopen did not transition PR state back to 'open', got %q", prAfterReopen.State)
	}
}

// TestWebhook_StaleReopenRedeliveryDoesNotReopenSecondClose is the round-4
// item-1 regression guard: GitHub does not guarantee webhook delivery order,
// so a DELAYED redelivery of an earlier "reopened" webhook can arrive after
// the PR has since been reopened and closed again. That stale payload still
// carries action="reopened" (is_reopen=true) but its closed_at describes the
// FIRST close, which is chronologically older than the closed_at already
// stored from the SECOND close. Without an ordering check on top of
// is_reopen, that stale redelivery would wrongly reopen a PR that is
// genuinely closed. This exercises the full sequence: close (T1) -> reopen ->
// close again (T2 > T1) -> delayed redelivery of the first "reopened"
// webhook (closed_at=T1) -> PR must stay closed with closed_at still T2.
func TestWebhook_StaleReopenRedeliveryDoesNotReopenSecondClose(t *testing.T) {
	if testHandler == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	ctx := context.Background()
	secret := "stale-reopen-redelivery-secret"
	t.Setenv("GITHUB_WEBHOOK_SECRET", secret)

	req := newRequest("POST", "/api/issues?workspace_id="+testWorkspaceID, map[string]any{
		"title":  "Stale reopen redelivery test issue",
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

	const installationID int64 = 99887799
	if _, err := testHandler.Queries.CreateGitHubInstallation(ctx, db.CreateGitHubInstallationParams{
		WorkspaceID:    parseUUID(testWorkspaceID),
		InstallationID: installationID,
		AccountLogin:   "stale-reopen-redelivery-acct",
		AccountType:    "User",
	}); err != nil {
		t.Fatalf("CreateGitHubInstallation: %v", err)
	}

	const prNumber = 7171
	const repoOwner, repoName = "acme", "widget"
	prURL := "https://github.com/" + repoOwner + "/" + repoName + "/pull/7171"

	// 1. First close: closed_at = T1.
	firstClosedBody := map[string]any{
		"action": "closed",
		"pull_request": map[string]any{
			"number":     prNumber,
			"html_url":   prURL,
			"title":      created.Identifier + ": ship it",
			"body":       "",
			"state":      "closed",
			"draft":      false,
			"merged":     false,
			"merged_at":  nil,
			"closed_at":  "2026-09-10T09:00:00Z", // T1
			"created_at": "2026-09-10T08:00:00Z",
			"updated_at": "2026-09-10T09:00:00Z",
			"head":       map[string]any{"ref": "fix/ship-it"},
			"user":       map[string]any{"login": "octocat", "avatar_url": ""},
		},
		"repository": map[string]any{
			"id":    717171,
			"name":  repoName,
			"owner": map[string]any{"login": repoOwner},
		},
		"installation": map[string]any{"id": installationID},
	}
	postSignedGitHubWebhook(t, secret, firstClosedBody, "stale-reopen-redelivery-close-1")

	// 2. Genuine reopen: closed_at cleared.
	reopenedBody := map[string]any{
		"action": "reopened",
		"pull_request": map[string]any{
			"number":     prNumber,
			"html_url":   prURL,
			"title":      created.Identifier + ": ship it",
			"body":       "",
			"state":      "open",
			"draft":      false,
			"merged":     false,
			"merged_at":  nil,
			"closed_at":  nil,
			"created_at": "2026-09-10T08:00:00Z",
			"updated_at": "2026-09-10T09:05:00Z",
			"head":       map[string]any{"ref": "fix/ship-it"},
			"user":       map[string]any{"login": "octocat", "avatar_url": ""},
		},
		"repository": map[string]any{
			"id":    717171,
			"name":  repoName,
			"owner": map[string]any{"login": repoOwner},
		},
		"installation": map[string]any{"id": installationID},
	}
	postSignedGitHubWebhook(t, secret, reopenedBody, "stale-reopen-redelivery-reopen")

	// 3. Second close: closed_at = T2, T2 > T1.
	secondClosedBody := map[string]any{
		"action": "closed",
		"pull_request": map[string]any{
			"number":     prNumber,
			"html_url":   prURL,
			"title":      created.Identifier + ": ship it",
			"body":       "",
			"state":      "closed",
			"draft":      false,
			"merged":     false,
			"merged_at":  nil,
			"closed_at":  "2026-09-10T10:00:00Z", // T2 > T1
			"created_at": "2026-09-10T08:00:00Z",
			"updated_at": "2026-09-10T10:00:00Z",
			"head":       map[string]any{"ref": "fix/ship-it"},
			"user":       map[string]any{"login": "octocat", "avatar_url": ""},
		},
		"repository": map[string]any{
			"id":    717171,
			"name":  repoName,
			"owner": map[string]any{"login": repoOwner},
		},
		"installation": map[string]any{"id": installationID},
	}
	postSignedGitHubWebhook(t, secret, secondClosedBody, "stale-reopen-redelivery-close-2")

	prAfterSecondClose, err := testHandler.Queries.GetGitHubPullRequest(ctx, db.GetGitHubPullRequestParams{
		WorkspaceID: parseUUID(testWorkspaceID),
		RepoOwner:   repoOwner,
		RepoName:    repoName,
		PrNumber:    prNumber,
	})
	if err != nil {
		t.Fatalf("GetGitHubPullRequest after second close: %v", err)
	}
	if prAfterSecondClose.State != "closed" {
		t.Fatalf("expected PR state 'closed' right after the second close webhook, got %q", prAfterSecondClose.State)
	}
	if !prAfterSecondClose.ClosedAt.Valid || prAfterSecondClose.ClosedAt.Time.UTC().Format("2006-01-02T15:04:05Z") != "2026-09-10T10:00:00Z" {
		t.Fatalf("expected closed_at = T2 (2026-09-10T10:00:00Z) after second close, got %v", prAfterSecondClose.ClosedAt)
	}

	// 4. A delayed redelivery of the FIRST "reopened" webhook arrives late
	// (same delivery GUID scenario as GitHub's at-least-once redelivery, but
	// modeled here as an independent delivery carrying the stale payload). Its
	// closed_at is T1 — older than the currently stored T2 — so it must be
	// rejected: the PR must stay closed with closed_at still T2, not reopened.
	staleReopenedRedeliveryBody := map[string]any{
		"action": "reopened",
		"pull_request": map[string]any{
			"number":     prNumber,
			"html_url":   prURL,
			"title":      created.Identifier + ": ship it",
			"body":       "",
			"state":      "open",
			"draft":      false,
			"merged":     false,
			"merged_at":  nil,
			"closed_at":  "2026-09-10T09:00:00Z", // stale T1, older than stored T2
			"created_at": "2026-09-10T08:00:00Z",
			"updated_at": "2026-09-10T09:05:00Z",
			"head":       map[string]any{"ref": "fix/ship-it"},
			"user":       map[string]any{"login": "octocat", "avatar_url": ""},
		},
		"repository": map[string]any{
			"id":    717171,
			"name":  repoName,
			"owner": map[string]any{"login": repoOwner},
		},
		"installation": map[string]any{"id": installationID},
	}
	postSignedGitHubWebhook(t, secret, staleReopenedRedeliveryBody, "stale-reopen-redelivery-stale-reopen")

	prAfterStaleRedelivery, err := testHandler.Queries.GetGitHubPullRequest(ctx, db.GetGitHubPullRequestParams{
		WorkspaceID: parseUUID(testWorkspaceID),
		RepoOwner:   repoOwner,
		RepoName:    repoName,
		PrNumber:    prNumber,
	})
	if err != nil {
		t.Fatalf("GetGitHubPullRequest after stale reopen redelivery: %v", err)
	}
	if prAfterStaleRedelivery.State != "closed" {
		t.Errorf("stale delayed 'reopened' redelivery reopened a PR that was closed again, got state %q, want it to remain 'closed'", prAfterStaleRedelivery.State)
	}
	if !prAfterStaleRedelivery.ClosedAt.Valid || prAfterStaleRedelivery.ClosedAt.Time.UTC().Format("2006-01-02T15:04:05Z") != "2026-09-10T10:00:00Z" {
		t.Errorf("stale delayed 'reopened' redelivery moved closed_at, want it to remain T2 (2026-09-10T10:00:00Z), got %v", prAfterStaleRedelivery.ClosedAt)
	}
}

// TestWebhook_MergeAnnouncementSkipsJustUnlinkedIssue is the A1 regression
// guard: a PR body edit that drops the closing claim on one issue while a
// sibling issue remains genuinely linked must not enqueue a merge
// announcement for the dropped issue when the PR subsequently merges — only
// for the issue that is still linked at merge time.
func TestWebhook_MergeAnnouncementSkipsJustUnlinkedIssue(t *testing.T) {
	if testHandler == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	ctx := context.Background()
	secret := "unlink-selection-secret"
	t.Setenv("GITHUB_WEBHOOK_SECRET", secret)

	reqDropped := newRequest("POST", "/api/issues?workspace_id="+testWorkspaceID, map[string]any{
		"title":  "Unlink selection: dropped issue",
		"status": "in_progress",
	})
	wDropped := testutil.Call(t, testHandler.CreateIssue, reqDropped).Want(http.StatusCreated)
	var dropped IssueResponse
	json.NewDecoder(wDropped.Body).Decode(&dropped)

	reqKept := newRequest("POST", "/api/issues?workspace_id="+testWorkspaceID, map[string]any{
		"title":  "Unlink selection: kept issue",
		"status": "in_progress",
	})
	wKept := testutil.Call(t, testHandler.CreateIssue, reqKept).Want(http.StatusCreated)
	var kept IssueResponse
	json.NewDecoder(wKept.Body).Decode(&kept)

	t.Cleanup(func() {
		for _, id := range []string{dropped.ID, kept.ID} {
			testPool.Exec(ctx, `DELETE FROM github_merge_announcement WHERE issue_id = $1`, id)
			testPool.Exec(ctx, `DELETE FROM comment WHERE issue_id = $1`, id)
			testPool.Exec(ctx, `DELETE FROM issue_pull_request WHERE issue_id = $1`, id)
			testPool.Exec(ctx, `DELETE FROM activity_log WHERE issue_id = $1`, id)
			testPool.Exec(ctx, `DELETE FROM issue WHERE id = $1`, id)
		}
		testPool.Exec(ctx, `DELETE FROM github_pull_request WHERE workspace_id = $1`, testWorkspaceID)
		testPool.Exec(ctx, `DELETE FROM github_installation WHERE workspace_id = $1`, testWorkspaceID)
	})

	const installationID int64 = 99887755
	if _, err := testHandler.Queries.CreateGitHubInstallation(ctx, db.CreateGitHubInstallationParams{
		WorkspaceID:    parseUUID(testWorkspaceID),
		InstallationID: installationID,
		AccountLogin:   "unlink-selection-acct",
		AccountType:    "User",
	}); err != nil {
		t.Fatalf("CreateGitHubInstallation: %v", err)
	}

	const prNumber = 9191
	body := "Closes " + dropped.Identifier + "\n\nCloses " + kept.Identifier

	// Opened, declaring closing intent on both issues — both get linked.
	firePRWebhook(t, secret, installationID, prNumber, "Two-issue PR", body, "feat/two-issue", "opened")

	droppedLinked, err := testHandler.Queries.ListPullRequestsByIssue(ctx, parseUUID(dropped.ID))
	if err != nil {
		t.Fatalf("ListPullRequestsByIssue(dropped) after open: %v", err)
	}
	if len(droppedLinked) != 1 {
		t.Fatalf("expected the dropped issue to be linked after open, got %d rows", len(droppedLinked))
	}
	keptLinked, err := testHandler.Queries.ListPullRequestsByIssue(ctx, parseUUID(kept.ID))
	if err != nil {
		t.Fatalf("ListPullRequestsByIssue(kept) after open: %v", err)
	}
	if len(keptLinked) != 1 {
		t.Fatalf("expected the kept issue to be linked after open, got %d rows", len(keptLinked))
	}

	// Edited: the claim on `dropped` is withdrawn (bare mention only), the
	// claim on `kept` remains a genuine closing keyword.
	editedBody := "See " + dropped.Identifier + " for context.\n\nCloses " + kept.Identifier
	firePRWebhook(t, secret, installationID, prNumber, "Two-issue PR", editedBody, "feat/two-issue", "edited")

	droppedAfterEdit, err := testHandler.Queries.ListPullRequestsByIssue(ctx, parseUUID(dropped.ID))
	if err != nil {
		t.Fatalf("ListPullRequestsByIssue(dropped) after edit: %v", err)
	}
	if len(droppedAfterEdit) != 0 {
		t.Fatalf("expected the dropped issue to be unlinked after the edit, got %d rows", len(droppedAfterEdit))
	}

	// Merge: the PR merges with the same body (dropped issue still only a
	// bare mention, kept issue still a closing keyword).
	firePRWebhook(t, secret, installationID, prNumber, "Two-issue PR", editedBody, "feat/two-issue", "merged")

	droppedAnnouncements, err := testHandler.Queries.ListGitHubMergeAnnouncementsByIssue(ctx, parseUUID(dropped.ID))
	if err != nil {
		t.Fatalf("ListGitHubMergeAnnouncementsByIssue(dropped): %v", err)
	}
	if len(droppedAnnouncements) != 0 {
		t.Errorf("A1: expected no merge announcement for the just-unlinked issue, got %d", len(droppedAnnouncements))
	}

	keptAnnouncements, err := testHandler.Queries.ListGitHubMergeAnnouncementsByIssue(ctx, parseUUID(kept.ID))
	if err != nil {
		t.Fatalf("ListGitHubMergeAnnouncementsByIssue(kept): %v", err)
	}
	if len(keptAnnouncements) != 1 {
		t.Errorf("A1: expected exactly 1 merge announcement for the still-linked issue, got %d", len(keptAnnouncements))
	}
}

// TestWebhook_MergeAnnouncementCommentReflectsCloseIntent is the T3
// close-intent regression guard: the rendered comment's "remaining issue
// action" text must accurately reflect whether this specific merge declared
// closing intent for the issue, not always deny completion intent.
// TestWebhook_MergedPR_EnqueuesAndDeliversMergeAnnouncement already covers
// the close_intent=false branch (via buildMergedPRWebhookBody, which declares
// no closing keyword); this test exercises the close_intent=true branch.
func TestWebhook_MergeAnnouncementCommentReflectsCloseIntent(t *testing.T) {
	if testHandler == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	ctx := context.Background()
	secret := "close-intent-render-secret"
	t.Setenv("GITHUB_WEBHOOK_SECRET", secret)

	req := newRequest("POST", "/api/issues?workspace_id="+testWorkspaceID, map[string]any{
		"title":  "Close-intent rendering test issue",
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

	const installationID int64 = 99887766
	if _, err := testHandler.Queries.CreateGitHubInstallation(ctx, db.CreateGitHubInstallationParams{
		WorkspaceID:    parseUUID(testWorkspaceID),
		InstallationID: installationID,
		AccountLogin:   "close-intent-render-acct",
		AccountType:    "User",
	}); err != nil {
		t.Fatalf("CreateGitHubInstallation: %v", err)
	}

	// Opened and merged with a genuine closing keyword this time (unlike
	// buildMergedPRWebhookBody's bare-mention body), so close_intent=true is
	// captured on the announcement row.
	firePRWebhook(t, secret, installationID, 1010, "Closing PR", "Closes "+created.Identifier, "fix/close", "opened")
	firePRWebhook(t, secret, installationID, 1010, "Closing PR", "Closes "+created.Identifier, "fix/close", "merged")

	worker := NewMergeAnnouncementWorker(testHandler)
	worked, err := worker.ProcessNext(ctx)
	if err != nil {
		t.Fatalf("ProcessNext: %v", err)
	}
	if !worked {
		t.Fatalf("expected ProcessNext to claim and deliver the pending announcement")
	}

	var content string
	if err := testPool.QueryRow(ctx,
		`SELECT content FROM comment WHERE issue_id = $1 AND author_type = 'system' LIMIT 1`,
		created.ID,
	).Scan(&content); err != nil {
		t.Fatalf("read system comment content: %v", err)
	}
	if !bytes.Contains([]byte(content), []byte("will auto-advance once no linked PR is still open")) {
		t.Errorf("expected close-intent-accurate next-action text for a close-intent merge, got %q", content)
	}
	if bytes.Contains([]byte(content), []byte("did not declare closing intent")) {
		t.Errorf("close-intent merge must not render the non-close-intent wording, got %q", content)
	}
}

// TestWebhook_MergeAnnouncementUsesPersistedLinkWhenAutoLinkDisabled is the
// round-2 item-1 regression guard: a workspace can have github_enabled=true
// while github_auto_link_prs_enabled=false. In that state the webhook must
// not create new link rows from this event's identifiers, but a link row
// persisted by an earlier event (while auto-link was still on) must still
// drive both the merge announcement and the advance-to-done re-evaluation —
// using the link row's already-stored close_intent, not anything re-parsed
// from this merge event's payload.
func TestWebhook_MergeAnnouncementUsesPersistedLinkWhenAutoLinkDisabled(t *testing.T) {
	if testHandler == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	ctx := context.Background()
	secret := "auto-link-disabled-secret"
	t.Setenv("GITHUB_WEBHOOK_SECRET", secret)

	req := newRequest("POST", "/api/issues?workspace_id="+testWorkspaceID, map[string]any{
		"title":  "Auto-link-disabled persisted-link test issue",
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

	const installationID int64 = 99887799
	if _, err := testHandler.Queries.CreateGitHubInstallation(ctx, db.CreateGitHubInstallationParams{
		WorkspaceID:    parseUUID(testWorkspaceID),
		InstallationID: installationID,
		AccountLogin:   "auto-link-disabled-acct",
		AccountType:    "User",
	}); err != nil {
		t.Fatalf("CreateGitHubInstallation: %v", err)
	}

	const prNumber = 6161
	// Auto-link is still on for the "opened" event: this persists the link
	// row (with close_intent=true from the closing keyword) that the rest of
	// the test relies on being read back, not re-derived.
	firePRWebhook(t, secret, installationID, prNumber, "Auto-link-disabled PR", "Closes "+created.Identifier, "fix/auto-link-disabled", "opened")

	linked, err := testHandler.Queries.ListPullRequestsByIssue(ctx, parseUUID(created.ID))
	if err != nil {
		t.Fatalf("ListPullRequestsByIssue after open: %v", err)
	}
	if len(linked) != 1 {
		t.Fatalf("expected the issue to be linked after open, got %d rows", len(linked))
	}

	// Now disable auto-link (github_enabled stays on) before the merge event
	// fires, and restore it afterward so this test doesn't leak workspace
	// settings into others.
	var originalSettings []byte
	if err := testPool.QueryRow(ctx, `SELECT settings FROM workspace WHERE id = $1`, testWorkspaceID).Scan(&originalSettings); err != nil {
		t.Fatalf("read original workspace settings: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(ctx, `UPDATE workspace SET settings = $2 WHERE id = $1`, testWorkspaceID, originalSettings)
	})
	if _, err := testPool.Exec(ctx,
		`UPDATE workspace SET settings = '{"github_enabled": true, "github_auto_link_prs_enabled": false}'::jsonb WHERE id = $1`,
		testWorkspaceID,
	); err != nil {
		t.Fatalf("disable auto-link: %v", err)
	}

	firePRWebhook(t, secret, installationID, prNumber, "Auto-link-disabled PR", "Closes "+created.Identifier, "fix/auto-link-disabled", "merged")

	announcements, err := testHandler.Queries.ListGitHubMergeAnnouncementsByIssue(ctx, parseUUID(created.ID))
	if err != nil {
		t.Fatalf("ListGitHubMergeAnnouncementsByIssue: %v", err)
	}
	if len(announcements) != 1 {
		t.Fatalf("expected a merge announcement driven off the persisted link even with auto-link disabled, got %d", len(announcements))
	}
	if !announcements[0].CloseIntent.Valid || !announcements[0].CloseIntent.Bool {
		t.Errorf("expected the announcement's close_intent to come from the persisted link row (true), got %+v", announcements[0].CloseIntent)
	}

	worker := NewMergeAnnouncementWorker(testHandler)
	worked, err := worker.ProcessNext(ctx)
	if err != nil {
		t.Fatalf("ProcessNext: %v", err)
	}
	if !worked {
		t.Fatalf("expected ProcessNext to claim and deliver the pending announcement")
	}

	// Advance-to-done must also have fired off the persisted link's
	// close_intent, using the merge event that closed the PR (no open linked
	// PRs remain, and the persisted link carries close_intent=true).
	afterMerge, err := testHandler.Queries.GetIssue(ctx, parseUUID(created.ID))
	if err != nil {
		t.Fatalf("GetIssue after merge: %v", err)
	}
	if afterMerge.Status != "done" {
		t.Errorf("expected the issue to auto-advance to done using the persisted link's close_intent, got %q", afterMerge.Status)
	}
}

// TestWebhook_GitHubDisabledProducesNoMirrorSideEffects is the round-2
// item-1 zero-side-effects guard: when a workspace's master GitHub switch
// (github_enabled) is off, a merge webhook must not create a PR mirror row,
// a link row, a merge announcement, or an advance-to-done side effect. No
// existing test in this package exercised github_enabled=false end to end —
// TestWebhook_PullRequest_UnreadableWorkspaceWithholdsCloseIntent covers a
// different unreadable-settings case, not the explicit-off case.
func TestWebhook_GitHubDisabledProducesNoMirrorSideEffects(t *testing.T) {
	if testHandler == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	ctx := context.Background()
	secret := "github-disabled-secret"
	t.Setenv("GITHUB_WEBHOOK_SECRET", secret)

	req := newRequest("POST", "/api/issues?workspace_id="+testWorkspaceID, map[string]any{
		"title":  "GitHub-disabled zero-side-effects test issue",
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

	const installationID int64 = 99887700
	if _, err := testHandler.Queries.CreateGitHubInstallation(ctx, db.CreateGitHubInstallationParams{
		WorkspaceID:    parseUUID(testWorkspaceID),
		InstallationID: installationID,
		AccountLogin:   "github-disabled-acct",
		AccountType:    "User",
	}); err != nil {
		t.Fatalf("CreateGitHubInstallation: %v", err)
	}

	var originalSettings []byte
	if err := testPool.QueryRow(ctx, `SELECT settings FROM workspace WHERE id = $1`, testWorkspaceID).Scan(&originalSettings); err != nil {
		t.Fatalf("read original workspace settings: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(ctx, `UPDATE workspace SET settings = $2 WHERE id = $1`, testWorkspaceID, originalSettings)
	})
	if _, err := testPool.Exec(ctx,
		`UPDATE workspace SET settings = '{"github_enabled": false}'::jsonb WHERE id = $1`,
		testWorkspaceID,
	); err != nil {
		t.Fatalf("disable github: %v", err)
	}

	const prNumber = 6262
	firePRWebhook(t, secret, installationID, prNumber, "GitHub-disabled PR", "Closes "+created.Identifier, "fix/github-disabled", "opened")
	firePRWebhook(t, secret, installationID, prNumber, "GitHub-disabled PR", "Closes "+created.Identifier, "fix/github-disabled", "merged")

	linked, err := testHandler.Queries.ListPullRequestsByIssue(ctx, parseUUID(created.ID))
	if err != nil {
		t.Fatalf("ListPullRequestsByIssue: %v", err)
	}
	if len(linked) != 0 {
		t.Errorf("expected no link row when github_enabled is false, got %d", len(linked))
	}

	// CHE-374 review round 3, item 4: master-off must suppress the PR mirror
	// upsert itself, not just the issue-facing effects — the round-2
	// implementation still wrote this row unconditionally, which this test's
	// name and header comment claimed did not happen without checking it.
	_, err = testHandler.Queries.GetGitHubPullRequest(ctx, db.GetGitHubPullRequestParams{
		WorkspaceID: parseUUID(testWorkspaceID),
		RepoOwner:   "acme",
		RepoName:    "widget",
		PrNumber:    prNumber,
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("expected no github_pull_request mirror row when github_enabled is false, got err=%v", err)
	}

	announcements, err := testHandler.Queries.ListGitHubMergeAnnouncementsByIssue(ctx, parseUUID(created.ID))
	if err != nil {
		t.Fatalf("ListGitHubMergeAnnouncementsByIssue: %v", err)
	}
	if len(announcements) != 0 {
		t.Errorf("expected no merge announcement when github_enabled is false, got %d", len(announcements))
	}

	afterMerge, err := testHandler.Queries.GetIssue(ctx, parseUUID(created.ID))
	if err != nil {
		t.Fatalf("GetIssue after merge: %v", err)
	}
	if afterMerge.Status != "in_progress" {
		t.Errorf("expected issue status unchanged when github_enabled is false, got %q", afterMerge.Status)
	}
}

// TestWebhook_MergeAnnouncementWorkerSkipsUnlinkedIssueAtDelivery is the
// round-2 item-2 regression guard: ProcessNext must revalidate eligibility at
// delivery time, not just trust that the row was eligible when it was
// enqueued. Here the issue↔PR link is removed after the announcement is
// already queued (a body edit dropping the closing claim, or a manual
// unlink) but before the worker runs — ProcessNext must terminally skip the
// row (status='skipped') and must not create a comment.
func TestWebhook_MergeAnnouncementWorkerSkipsUnlinkedIssueAtDelivery(t *testing.T) {
	if testHandler == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	ctx := context.Background()
	secret := "worker-revalidate-unlink-secret"
	t.Setenv("GITHUB_WEBHOOK_SECRET", secret)

	req := newRequest("POST", "/api/issues?workspace_id="+testWorkspaceID, map[string]any{
		"title":  "Worker revalidation: unlink test issue",
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

	const installationID int64 = 99887701
	if _, err := testHandler.Queries.CreateGitHubInstallation(ctx, db.CreateGitHubInstallationParams{
		WorkspaceID:    parseUUID(testWorkspaceID),
		InstallationID: installationID,
		AccountLogin:   "worker-revalidate-unlink-acct",
		AccountType:    "User",
	}); err != nil {
		t.Fatalf("CreateGitHubInstallation: %v", err)
	}

	body := buildMergedPRWebhookBody(created.Identifier, 7373, "acme", "widget-unlink", installationID)
	postSignedGitHubWebhook(t, secret, body, "worker-revalidate-unlink-delivery-1")

	announcements, err := testHandler.Queries.ListGitHubMergeAnnouncementsByIssue(ctx, parseUUID(created.ID))
	if err != nil {
		t.Fatalf("ListGitHubMergeAnnouncementsByIssue: %v", err)
	}
	if len(announcements) != 1 || announcements[0].Status != "pending" {
		t.Fatalf("expected 1 pending announcement before the worker runs, got %+v", announcements)
	}
	pending := announcements[0]

	// Remove the issue↔PR link directly — the same end state a post-merge
	// edit dropping the closing claim, or a manual unlink, would produce —
	// without touching the announcement row itself.
	if err := testHandler.Queries.UnlinkIssueFromPullRequest(ctx, db.UnlinkIssueFromPullRequestParams{
		IssueID:       parseUUID(created.ID),
		PullRequestID: pending.PullRequestID,
	}); err != nil {
		t.Fatalf("UnlinkIssueFromPullRequest: %v", err)
	}

	worker := NewMergeAnnouncementWorker(testHandler)
	worked, err := worker.ProcessNext(ctx)
	if err != nil {
		t.Fatalf("ProcessNext: %v", err)
	}
	if !worked {
		t.Fatalf("expected ProcessNext to claim the pending announcement (and then skip it)")
	}

	afterWorker, err := testHandler.Queries.ListGitHubMergeAnnouncementsByIssue(ctx, parseUUID(created.ID))
	if err != nil {
		t.Fatalf("ListGitHubMergeAnnouncementsByIssue after worker: %v", err)
	}
	if len(afterWorker) != 1 || afterWorker[0].Status != "skipped" {
		t.Fatalf("expected the announcement to terminally skip after the link was removed, got %+v", afterWorker)
	}

	var commentCount int
	if err := testPool.QueryRow(ctx,
		`SELECT count(*) FROM comment WHERE issue_id = $1 AND author_type = 'system'`,
		created.ID,
	).Scan(&commentCount); err != nil {
		t.Fatalf("count system comments: %v", err)
	}
	if commentCount != 0 {
		t.Errorf("expected no system comment for a skipped (unlinked) announcement, got %d", commentCount)
	}
}

// TestWebhook_MergeAnnouncementWorkerSkipsWhenGitHubDisabledAtDelivery
// exercises the other item-2 required scenario: the workspace disables its
// master GitHub switch after the announcement is queued but before the
// worker delivers it. ProcessNext must terminally skip, not deliver a
// comment for a workspace that has since turned GitHub off.
func TestWebhook_MergeAnnouncementWorkerSkipsWhenGitHubDisabledAtDelivery(t *testing.T) {
	if testHandler == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	ctx := context.Background()
	secret := "worker-revalidate-disable-secret"
	t.Setenv("GITHUB_WEBHOOK_SECRET", secret)

	req := newRequest("POST", "/api/issues?workspace_id="+testWorkspaceID, map[string]any{
		"title":  "Worker revalidation: github-disabled test issue",
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

	const installationID int64 = 99887702
	if _, err := testHandler.Queries.CreateGitHubInstallation(ctx, db.CreateGitHubInstallationParams{
		WorkspaceID:    parseUUID(testWorkspaceID),
		InstallationID: installationID,
		AccountLogin:   "worker-revalidate-disable-acct",
		AccountType:    "User",
	}); err != nil {
		t.Fatalf("CreateGitHubInstallation: %v", err)
	}

	body := buildMergedPRWebhookBody(created.Identifier, 7474, "acme", "widget-disable", installationID)
	postSignedGitHubWebhook(t, secret, body, "worker-revalidate-disable-delivery-1")

	announcements, err := testHandler.Queries.ListGitHubMergeAnnouncementsByIssue(ctx, parseUUID(created.ID))
	if err != nil {
		t.Fatalf("ListGitHubMergeAnnouncementsByIssue: %v", err)
	}
	if len(announcements) != 1 || announcements[0].Status != "pending" {
		t.Fatalf("expected 1 pending announcement before the worker runs, got %+v", announcements)
	}

	var originalSettings []byte
	if err := testPool.QueryRow(ctx, `SELECT settings FROM workspace WHERE id = $1`, testWorkspaceID).Scan(&originalSettings); err != nil {
		t.Fatalf("read original workspace settings: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(ctx, `UPDATE workspace SET settings = $2 WHERE id = $1`, testWorkspaceID, originalSettings)
	})
	if _, err := testPool.Exec(ctx,
		`UPDATE workspace SET settings = '{"github_enabled": false}'::jsonb WHERE id = $1`,
		testWorkspaceID,
	); err != nil {
		t.Fatalf("disable github: %v", err)
	}

	worker := NewMergeAnnouncementWorker(testHandler)
	worked, err := worker.ProcessNext(ctx)
	if err != nil {
		t.Fatalf("ProcessNext: %v", err)
	}
	if !worked {
		t.Fatalf("expected ProcessNext to claim the pending announcement (and then skip it)")
	}

	afterWorker, err := testHandler.Queries.ListGitHubMergeAnnouncementsByIssue(ctx, parseUUID(created.ID))
	if err != nil {
		t.Fatalf("ListGitHubMergeAnnouncementsByIssue after worker: %v", err)
	}
	if len(afterWorker) != 1 || afterWorker[0].Status != "skipped" {
		t.Fatalf("expected the announcement to terminally skip once github_enabled is false, got %+v", afterWorker)
	}

	var commentCount int
	if err := testPool.QueryRow(ctx,
		`SELECT count(*) FROM comment WHERE issue_id = $1 AND author_type = 'system'`,
		created.ID,
	).Scan(&commentCount); err != nil {
		t.Fatalf("count system comments: %v", err)
	}
	if commentCount != 0 {
		t.Errorf("expected no system comment for a skipped (github disabled) announcement, got %d", commentCount)
	}
}

// failCompleteDeliveryTx wraps a real pgx.Tx and fails only the
// CompleteGitHubMergeAnnouncementDelivery UPDATE (matched by a substring
// unique to that statement, so the comment INSERT earlier in the same
// transaction still succeeds normally). Same shape as contextLockTestTx in
// channel_context_lock_order_test.go: intercept QueryRow, return a Row whose
// Scan fails, and roll back the real tx so the connection is left clean for
// the next test.
type failCompleteDeliveryTx struct {
	pgx.Tx
}

func (tx failCompleteDeliveryTx) QueryRow(ctx context.Context, query string, args ...any) pgx.Row {
	if strings.Contains(query, "SET status = 'delivered'") {
		_ = tx.Tx.Rollback(ctx)
		return errRow{err: errors.New("failCompleteDeliveryTx: induced CompleteGitHubMergeAnnouncementDelivery failure")}
	}
	return tx.Tx.QueryRow(ctx, query, args...)
}

func (tx failCompleteDeliveryTx) Commit(ctx context.Context) error {
	// The forced QueryRow failure above already rolled back the underlying
	// tx, so Commit must not attempt to commit an already-closed tx (pgx
	// would error, masking the intended failure path). ProcessNext never
	// reaches Commit for this test since it returns right after the failed
	// QueryRow, but implementing this defensively keeps the wrapper honest.
	return errors.New("failCompleteDeliveryTx: tx was already rolled back")
}

type failCompleteDeliveryTxStarter struct {
	pool *pgxpool.Pool
}

func (s failCompleteDeliveryTxStarter) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return failCompleteDeliveryTx{Tx: tx}, nil
}

// errRow (pgx.Row whose Scan always returns err) is reused from
// claim_project_context_test.go — same package, no need to redefine it here.

// TestWebhook_MergeAnnouncementWorkerRetriesOnCompleteDeliveryFailure is the
// item-3 regression guard (CHE-374 review round 2): a non-ErrNoRows failure
// from CompleteGitHubMergeAnnouncementDelivery must route through
// retryOrFail — attempt_count increments and the row stays "pending" and
// reclaimable — instead of returning a raw error that bypasses attempt
// bookkeeping and, prior to the fix, could leave the row's completion
// state ambiguous on every failure without ever tripping the terminal-fail
// path.
func TestWebhook_MergeAnnouncementWorkerRetriesOnCompleteDeliveryFailure(t *testing.T) {
	if testHandler == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	ctx := context.Background()
	secret := "worker-retry-complete-failure-secret"
	t.Setenv("GITHUB_WEBHOOK_SECRET", secret)

	req := newRequest("POST", "/api/issues?workspace_id="+testWorkspaceID, map[string]any{
		"title":  "Worker retry-on-complete-failure test issue",
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
		AccountLogin:   "worker-retry-complete-failure-acct",
		AccountType:    "User",
	}); err != nil {
		t.Fatalf("CreateGitHubInstallation: %v", err)
	}

	body := buildMergedPRWebhookBody(created.Identifier, 7575, "acme", "widget-retry", installationID)
	postSignedGitHubWebhook(t, secret, body, "worker-retry-complete-failure-delivery-1")

	before, err := testHandler.Queries.ListGitHubMergeAnnouncementsByIssue(ctx, parseUUID(created.ID))
	if err != nil {
		t.Fatalf("ListGitHubMergeAnnouncementsByIssue: %v", err)
	}
	if len(before) != 1 || before[0].Status != "pending" || before[0].AttemptCount != 0 {
		t.Fatalf("expected 1 pending announcement with attempt_count 0 before the worker runs, got %+v", before)
	}

	realTxStarter := testHandler.TxStarter
	testHandler.TxStarter = failCompleteDeliveryTxStarter{pool: testPool}
	t.Cleanup(func() { testHandler.TxStarter = realTxStarter })

	worker := NewMergeAnnouncementWorker(testHandler)
	worked, err := worker.ProcessNext(ctx)
	if err != nil {
		t.Fatalf("ProcessNext: %v", err)
	}
	if !worked {
		t.Fatalf("expected ProcessNext to claim the pending announcement (and then retry it after the induced failure)")
	}

	// Restore the real TxStarter before asserting DB state so the
	// assertions themselves query normally.
	testHandler.TxStarter = realTxStarter

	after, err := testHandler.Queries.ListGitHubMergeAnnouncementsByIssue(ctx, parseUUID(created.ID))
	if err != nil {
		t.Fatalf("ListGitHubMergeAnnouncementsByIssue after worker: %v", err)
	}
	if len(after) != 1 {
		t.Fatalf("expected exactly 1 announcement row to survive the induced failure, got %d", len(after))
	}
	got := after[0]
	if got.Status != "pending" {
		t.Fatalf("expected the row to remain pending (reclaimable) after a completion-write failure, got status %q", got.Status)
	}
	if got.AttemptCount != 1 {
		t.Fatalf("expected attempt_count to increment to 1 after the induced completion-write failure, got %d", got.AttemptCount)
	}
	if !got.LastError.Valid || got.LastError.String == "" {
		t.Fatalf("expected last_error to be recorded for the induced completion-write failure, got %+v", got.LastError)
	}
	if got.LeaseToken.Valid {
		t.Fatalf("expected lease_token to be cleared so the row is reclaimable, got %+v", got.LeaseToken)
	}

	var commentCount int
	if err := testPool.QueryRow(ctx,
		`SELECT count(*) FROM comment WHERE issue_id = $1 AND author_type = 'system'`,
		created.ID,
	).Scan(&commentCount); err != nil {
		t.Fatalf("count system comments: %v", err)
	}
	if commentCount != 0 {
		t.Errorf("expected no system comment to be visible: the comment insert was rolled back along with the failed completion write, got %d", commentCount)
	}
}

// failListInstallationsDBTX delegates every call to the real pool except
// ListGitHubInstallationsByInstallationID's Query (matched by a substring
// unique to that statement), which fails deterministically. Same shape as
// failQueryDBTX in claim_project_context_test.go, but overrides Query
// instead of QueryRow since ListGitHubInstallationsByInstallationID is a
// :many query.
type failListInstallationsDBTX struct {
	db.DBTX
}

func (f failListInstallationsDBTX) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	if strings.Contains(sql, "FROM github_installation") && strings.Contains(sql, "WHERE installation_id = $1") {
		return nil, errors.New("failListInstallationsDBTX: induced ListGitHubInstallationsByInstallationID failure")
	}
	return f.DBTX.Query(ctx, sql, args...)
}

// TestWebhook_PullRequestEventSurfacesInstallationLookupFailure is the
// item-5 regression guard (CHE-374 review round 2): a real (non-empty-result)
// error from ListGitHubInstallationsByInstallationID inside
// handlePullRequestEvent must be surfaced as a non-202 response so GitHub
// redelivers, matching every other failure branch in this path (T1). Before
// the fix, this specific lookup failure was slog.Warn'd and swallowed,
// returning 202 and silently dropping an event that might have fanned out to
// a real bound workspace.
func TestWebhook_PullRequestEventSurfacesInstallationLookupFailure(t *testing.T) {
	if testHandler == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	ctx := context.Background()
	secret := "installation-lookup-failure-secret"
	t.Setenv("GITHUB_WEBHOOK_SECRET", secret)

	req := newRequest("POST", "/api/issues?workspace_id="+testWorkspaceID, map[string]any{
		"title":  "Installation lookup failure test issue",
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

	const installationID int64 = 99887799
	if _, err := testHandler.Queries.CreateGitHubInstallation(ctx, db.CreateGitHubInstallationParams{
		WorkspaceID:    parseUUID(testWorkspaceID),
		InstallationID: installationID,
		AccountLogin:   "installation-lookup-failure-acct",
		AccountType:    "User",
	}); err != nil {
		t.Fatalf("CreateGitHubInstallation: %v", err)
	}

	body := buildMergedPRWebhookBody(created.Identifier, 7676, "acme", "widget-lookup-fail", installationID)
	raw, _ := json.Marshal(body)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(raw)
	sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	realQueries := testHandler.Queries
	testHandler.Queries = db.New(failListInstallationsDBTX{DBTX: testPool})
	t.Cleanup(func() { testHandler.Queries = realQueries })

	postReq := httptest.NewRequest("POST", "/api/webhooks/github", bytes.NewReader(raw))
	postReq.Header.Set("X-GitHub-Event", "pull_request")
	postReq.Header.Set("X-Hub-Signature-256", sig)
	postReq.Header.Set("X-GitHub-Delivery", "installation-lookup-failure-delivery-1")

	// Unlike postSignedGitHubWebhook, this must NOT expect 202: a real
	// installation-lookup failure must be surfaced as a server error so
	// GitHub redelivers, not silently accepted and dropped.
	rr := httptest.NewRecorder()
	testHandler.HandleGitHubWebhook(rr, postReq)

	// Restore the real Queries before asserting DB state.
	testHandler.Queries = realQueries

	if rr.Code == http.StatusAccepted {
		t.Fatalf("expected a non-202 response when the installation lookup fails, got %d", rr.Code)
	}
	if rr.Code < 500 {
		t.Fatalf("expected a 5xx response surfacing the installation lookup failure, got %d: %s", rr.Code, rr.Body.String())
	}

	var prCount int
	if err := testPool.QueryRow(ctx,
		`SELECT count(*) FROM github_pull_request WHERE workspace_id = $1 AND pr_number = $2`,
		testWorkspaceID, 7676,
	).Scan(&prCount); err != nil {
		t.Fatalf("count github_pull_request: %v", err)
	}
	if prCount != 0 {
		t.Errorf("expected no PR mirror row when the installation lookup failed before fan-out, got %d", prCount)
	}
}

// TestWebhook_StaleOpenedPayloadDoesNotReopenClosedPR is the round-3 item-1
// regression guard: a stale/out-of-order "opened" (or "synchronize") webhook
// arriving after a genuine close must not reopen the PR — only a webhook
// whose action is literally "reopened" may move a closed PR back to open.
// This is the closed-state sibling of
// TestWebhook_StalePullRequestPayloadDoesNotDemoteMergedState (merged-state
// case) and must coexist with TestWebhook_ReopenedPRTransitionsClosedBackToOpen
// (genuine reopen case) without breaking it.
func TestWebhook_StaleOpenedPayloadDoesNotReopenClosedPR(t *testing.T) {
	if testHandler == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	ctx := context.Background()
	secret := "stale-open-after-close-secret"
	t.Setenv("GITHUB_WEBHOOK_SECRET", secret)

	req := newRequest("POST", "/api/issues?workspace_id="+testWorkspaceID, map[string]any{
		"title":  "Stale open-after-close test issue",
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

	const installationID int64 = 99887755
	if _, err := testHandler.Queries.CreateGitHubInstallation(ctx, db.CreateGitHubInstallationParams{
		WorkspaceID:    parseUUID(testWorkspaceID),
		InstallationID: installationID,
		AccountLogin:   "stale-open-after-close-acct",
		AccountType:    "User",
	}); err != nil {
		t.Fatalf("CreateGitHubInstallation: %v", err)
	}

	const prNumber = 8383
	closedBody := map[string]any{
		"action": "closed",
		"pull_request": map[string]any{
			"number":     prNumber,
			"html_url":   "https://github.com/acme/sprocket/pull/8383",
			"title":      created.Identifier + ": ship it",
			"body":       "",
			"state":      "closed",
			"draft":      false,
			"merged":     false,
			"merged_at":  nil,
			"closed_at":  "2026-09-10T09:00:00Z",
			"created_at": "2026-09-10T08:00:00Z",
			"updated_at": "2026-09-10T09:00:00Z",
			"head":       map[string]any{"ref": "fix/ship-it"},
			"user":       map[string]any{"login": "octocat", "avatar_url": ""},
		},
		"repository": map[string]any{
			"id":    636363,
			"name":  "sprocket",
			"owner": map[string]any{"login": "acme"},
		},
		"installation": map[string]any{"id": installationID},
	}
	postSignedGitHubWebhook(t, secret, closedBody, "stale-open-after-close-delivery-closed")

	prAfterClose, err := testHandler.Queries.GetGitHubPullRequest(ctx, db.GetGitHubPullRequestParams{
		WorkspaceID: parseUUID(testWorkspaceID),
		RepoOwner:   "acme",
		RepoName:    "sprocket",
		PrNumber:    prNumber,
	})
	if err != nil {
		t.Fatalf("GetGitHubPullRequest after close: %v", err)
	}
	if prAfterClose.State != "closed" {
		t.Fatalf("expected PR state 'closed' right after the close webhook, got %q", prAfterClose.State)
	}

	// A stale "opened" redelivery arrives after the close was already
	// recorded (action="opened", state="open", NOT action="reopened"). This
	// must not reopen the PR — only a webhook whose action is literally
	// "reopened" may do that.
	staleOpenedBody := map[string]any{
		"action": "opened",
		"pull_request": map[string]any{
			"number":     prNumber,
			"html_url":   "https://github.com/acme/sprocket/pull/8383",
			"title":      created.Identifier + ": ship it",
			"body":       "",
			"state":      "open",
			"draft":      false,
			"merged":     false,
			"merged_at":  nil,
			"closed_at":  nil,
			"created_at": "2026-09-10T08:00:00Z",
			"updated_at": "2026-09-10T08:30:00Z",
			"head":       map[string]any{"ref": "fix/ship-it"},
			"user":       map[string]any{"login": "octocat", "avatar_url": ""},
		},
		"repository": map[string]any{
			"id":    636363,
			"name":  "sprocket",
			"owner": map[string]any{"login": "acme"},
		},
		"installation": map[string]any{"id": installationID},
	}
	postSignedGitHubWebhook(t, secret, staleOpenedBody, "stale-open-after-close-delivery-stale-opened")

	prAfterStale, err := testHandler.Queries.GetGitHubPullRequest(ctx, db.GetGitHubPullRequestParams{
		WorkspaceID: parseUUID(testWorkspaceID),
		RepoOwner:   "acme",
		RepoName:    "sprocket",
		PrNumber:    prNumber,
	})
	if err != nil {
		t.Fatalf("GetGitHubPullRequest after stale opened redelivery: %v", err)
	}
	if prAfterStale.State != "closed" {
		t.Errorf("stale 'opened' webhook reopened a closed PR, got state %q, want it to remain 'closed'", prAfterStale.State)
	}
	if !prAfterStale.ClosedAt.Valid {
		t.Errorf("stale 'opened' webhook cleared closed_at, want it to remain set")
	}

	// A stale "synchronize" redelivery (a new push landed before the close
	// webhook, but the synchronize event was delayed past it) must be
	// rejected the same way.
	staleSyncBody := map[string]any{
		"action": "synchronize",
		"pull_request": map[string]any{
			"number":     prNumber,
			"html_url":   "https://github.com/acme/sprocket/pull/8383",
			"title":      created.Identifier + ": ship it",
			"body":       "",
			"state":      "open",
			"draft":      false,
			"merged":     false,
			"merged_at":  nil,
			"closed_at":  nil,
			"created_at": "2026-09-10T08:00:00Z",
			"updated_at": "2026-09-10T08:31:00Z",
			"head":       map[string]any{"ref": "fix/ship-it"},
			"user":       map[string]any{"login": "octocat", "avatar_url": ""},
		},
		"repository": map[string]any{
			"id":    636363,
			"name":  "sprocket",
			"owner": map[string]any{"login": "acme"},
		},
		"installation": map[string]any{"id": installationID},
	}
	postSignedGitHubWebhook(t, secret, staleSyncBody, "stale-open-after-close-delivery-stale-sync")

	prAfterStaleSync, err := testHandler.Queries.GetGitHubPullRequest(ctx, db.GetGitHubPullRequestParams{
		WorkspaceID: parseUUID(testWorkspaceID),
		RepoOwner:   "acme",
		RepoName:    "sprocket",
		PrNumber:    prNumber,
	})
	if err != nil {
		t.Fatalf("GetGitHubPullRequest after stale synchronize redelivery: %v", err)
	}
	if prAfterStaleSync.State != "closed" {
		t.Errorf("stale 'synchronize' webhook reopened a closed PR, got state %q, want it to remain 'closed'", prAfterStaleSync.State)
	}
}

// failGetWorkspaceDBTX delegates every call to the real pool except the
// GetWorkspace SELECT (matched by a substring unique to that statement),
// which fails deterministically. Used to prove that an indeterminate
// github_enabled read at delivery time routes through retryOrFail rather
// than being folded into the permissive default (CHE-374 review round 3,
// item 2).
type failGetWorkspaceDBTX struct {
	db.DBTX
}

func (f failGetWorkspaceDBTX) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if strings.Contains(sql, "FROM workspace") && strings.Contains(sql, "WHERE id = $1") {
		return errRow{err: errors.New("failGetWorkspaceDBTX: induced GetWorkspace failure")}
	}
	return f.DBTX.QueryRow(ctx, sql, args...)
}

// TestWebhook_MergeAnnouncementWorkerRetriesWhenGitHubEnabledCheckFails is
// the round-3 item-2 regression guard: when the worker's delivery-time
// githubEnabledForWorkspaceChecked call itself fails (GetWorkspace error),
// ProcessNext must route through retryOrFail — attempt_count increments and
// the row stays pending/reclaimable — instead of silently treating the
// indeterminate answer as "enabled" and delivering the comment anyway.
func TestWebhook_MergeAnnouncementWorkerRetriesWhenGitHubEnabledCheckFails(t *testing.T) {
	if testHandler == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	ctx := context.Background()
	secret := "worker-retry-enabled-check-failure-secret"
	t.Setenv("GITHUB_WEBHOOK_SECRET", secret)

	req := newRequest("POST", "/api/issues?workspace_id="+testWorkspaceID, map[string]any{
		"title":  "Worker retry-on-enabled-check-failure test issue",
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
		AccountLogin:   "worker-retry-enabled-check-failure-acct",
		AccountType:    "User",
	}); err != nil {
		t.Fatalf("CreateGitHubInstallation: %v", err)
	}

	body := buildMergedPRWebhookBody(created.Identifier, 7878, "acme", "widget-enabled-check-fail", installationID)
	postSignedGitHubWebhook(t, secret, body, "worker-retry-enabled-check-failure-delivery-1")

	before, err := testHandler.Queries.ListGitHubMergeAnnouncementsByIssue(ctx, parseUUID(created.ID))
	if err != nil {
		t.Fatalf("ListGitHubMergeAnnouncementsByIssue: %v", err)
	}
	if len(before) != 1 || before[0].Status != "pending" || before[0].AttemptCount != 0 {
		t.Fatalf("expected 1 pending announcement with attempt_count 0 before the worker runs, got %+v", before)
	}

	realQueries := testHandler.Queries
	testHandler.Queries = db.New(failGetWorkspaceDBTX{DBTX: testPool})
	t.Cleanup(func() { testHandler.Queries = realQueries })

	worker := NewMergeAnnouncementWorker(testHandler)
	worked, err := worker.ProcessNext(ctx)
	if err != nil {
		t.Fatalf("ProcessNext: %v", err)
	}
	if !worked {
		t.Fatalf("expected ProcessNext to claim the pending announcement (and then retry it after the induced GetWorkspace failure)")
	}

	testHandler.Queries = realQueries

	after, err := testHandler.Queries.ListGitHubMergeAnnouncementsByIssue(ctx, parseUUID(created.ID))
	if err != nil {
		t.Fatalf("ListGitHubMergeAnnouncementsByIssue after worker: %v", err)
	}
	if len(after) != 1 {
		t.Fatalf("expected exactly 1 announcement row to survive the induced failure, got %d", len(after))
	}
	got := after[0]
	if got.Status != "pending" {
		t.Fatalf("expected the row to remain pending (reclaimable) after an indeterminate github_enabled check, got status %q", got.Status)
	}
	if got.AttemptCount != 1 {
		t.Fatalf("expected attempt_count to increment to 1 after the induced GetWorkspace failure, got %d", got.AttemptCount)
	}
	if got.LeaseToken.Valid {
		t.Fatalf("expected lease_token to be cleared so the row is reclaimable, got %+v", got.LeaseToken)
	}

	var commentCount int
	if err := testPool.QueryRow(ctx,
		`SELECT count(*) FROM comment WHERE issue_id = $1 AND author_type = 'system'`,
		created.ID,
	).Scan(&commentCount); err != nil {
		t.Fatalf("count system comments: %v", err)
	}
	if commentCount != 0 {
		t.Errorf("expected no system comment when the github_enabled check itself failed (indeterminate), got %d", commentCount)
	}
}

// TestWebhook_MergeAnnouncementWorkerSkipsWhenSourceInstallationRemoved is
// the round-3 item-3 regression guard: the announcement's source PR was
// mirrored under one installation; that specific installation is later
// removed from the workspace while a *different* installation remains
// bound. ProcessNext must terminally skip (not deliver), since revalidating
// against "any installation bound to the workspace" would incorrectly pass.
func TestWebhook_MergeAnnouncementWorkerSkipsWhenSourceInstallationRemoved(t *testing.T) {
	if testHandler == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	ctx := context.Background()
	secret := "worker-revalidate-installation-removed-secret"
	t.Setenv("GITHUB_WEBHOOK_SECRET", secret)

	req := newRequest("POST", "/api/issues?workspace_id="+testWorkspaceID, map[string]any{
		"title":  "Worker revalidation: installation-removed test issue",
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

	const sourceInstallationID int64 = 99887733
	const otherInstallationID int64 = 99887744
	if _, err := testHandler.Queries.CreateGitHubInstallation(ctx, db.CreateGitHubInstallationParams{
		WorkspaceID:    parseUUID(testWorkspaceID),
		InstallationID: sourceInstallationID,
		AccountLogin:   "worker-revalidate-installation-removed-source-acct",
		AccountType:    "User",
	}); err != nil {
		t.Fatalf("CreateGitHubInstallation (source): %v", err)
	}
	if _, err := testHandler.Queries.CreateGitHubInstallation(ctx, db.CreateGitHubInstallationParams{
		WorkspaceID:    parseUUID(testWorkspaceID),
		InstallationID: otherInstallationID,
		AccountLogin:   "worker-revalidate-installation-removed-other-acct",
		AccountType:    "User",
	}); err != nil {
		t.Fatalf("CreateGitHubInstallation (other): %v", err)
	}

	body := buildMergedPRWebhookBody(created.Identifier, 7979, "acme", "widget-installation-removed", sourceInstallationID)
	postSignedGitHubWebhook(t, secret, body, "worker-revalidate-installation-removed-delivery-1")

	announcements, err := testHandler.Queries.ListGitHubMergeAnnouncementsByIssue(ctx, parseUUID(created.ID))
	if err != nil {
		t.Fatalf("ListGitHubMergeAnnouncementsByIssue: %v", err)
	}
	if len(announcements) != 1 || announcements[0].Status != "pending" {
		t.Fatalf("expected 1 pending announcement before the worker runs, got %+v", announcements)
	}

	// Remove the SOURCE installation only; otherInstallationID remains bound
	// to the workspace, so a check that just asks "does the workspace have
	// any installation at all" would wrongly pass.
	if _, err := testPool.Exec(ctx,
		`DELETE FROM github_installation WHERE workspace_id = $1 AND installation_id = $2`,
		testWorkspaceID, sourceInstallationID,
	); err != nil {
		t.Fatalf("delete source installation: %v", err)
	}

	worker := NewMergeAnnouncementWorker(testHandler)
	worked, err := worker.ProcessNext(ctx)
	if err != nil {
		t.Fatalf("ProcessNext: %v", err)
	}
	if !worked {
		t.Fatalf("expected ProcessNext to claim the pending announcement (and then skip it)")
	}

	afterWorker, err := testHandler.Queries.ListGitHubMergeAnnouncementsByIssue(ctx, parseUUID(created.ID))
	if err != nil {
		t.Fatalf("ListGitHubMergeAnnouncementsByIssue after worker: %v", err)
	}
	if len(afterWorker) != 1 || afterWorker[0].Status != "skipped" {
		t.Fatalf("expected the announcement to terminally skip once the source installation is removed, got %+v", afterWorker)
	}

	var commentCount int
	if err := testPool.QueryRow(ctx,
		`SELECT count(*) FROM comment WHERE issue_id = $1 AND author_type = 'system'`,
		created.ID,
	).Scan(&commentCount); err != nil {
		t.Fatalf("count system comments: %v", err)
	}
	if commentCount != 0 {
		t.Errorf("expected no system comment for a skipped (source installation removed) announcement, got %d", commentCount)
	}
}

// failCommitTx wraps a real pgx.Tx and fails only Commit, leaving QueryRow
// (the comment insert and CompleteGitHubMergeAnnouncementDelivery) to
// succeed normally against the real transaction. Proves the round-3 item-5
// requirement: a tx.Commit failure must roll back so no comment survives,
// and the announcement row must remain pending/reclaimable with
// attempt_count incremented via retryOrFail — not silently treated as a
// successful delivery.
type failCommitTx struct {
	pgx.Tx
}

func (tx failCommitTx) Commit(ctx context.Context) error {
	_ = tx.Tx.Rollback(ctx)
	return errors.New("failCommitTx: induced Commit failure")
}

type failCommitTxStarter struct {
	pool *pgxpool.Pool
}

func (s failCommitTxStarter) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return failCommitTx{Tx: tx}, nil
}

// TestWebhook_MergeAnnouncementWorkerRetriesOnCommitFailure is the round-3
// item-5 regression guard: a tx.Commit failure on the worker's delivery
// transaction must roll back the comment insert (no comment survives) and
// route through retryOrFail (attempt_count increments, row stays
// pending/reclaimable) rather than leaving the row's completion state
// ambiguous or treating an uncommitted transaction as delivered.
func TestWebhook_MergeAnnouncementWorkerRetriesOnCommitFailure(t *testing.T) {
	if testHandler == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	ctx := context.Background()
	secret := "worker-retry-commit-failure-secret"
	t.Setenv("GITHUB_WEBHOOK_SECRET", secret)

	req := newRequest("POST", "/api/issues?workspace_id="+testWorkspaceID, map[string]any{
		"title":  "Worker retry-on-commit-failure test issue",
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

	const installationID int64 = 99887766
	if _, err := testHandler.Queries.CreateGitHubInstallation(ctx, db.CreateGitHubInstallationParams{
		WorkspaceID:    parseUUID(testWorkspaceID),
		InstallationID: installationID,
		AccountLogin:   "worker-retry-commit-failure-acct",
		AccountType:    "User",
	}); err != nil {
		t.Fatalf("CreateGitHubInstallation: %v", err)
	}

	body := buildMergedPRWebhookBody(created.Identifier, 8080, "acme", "widget-commit-fail", installationID)
	postSignedGitHubWebhook(t, secret, body, "worker-retry-commit-failure-delivery-1")

	before, err := testHandler.Queries.ListGitHubMergeAnnouncementsByIssue(ctx, parseUUID(created.ID))
	if err != nil {
		t.Fatalf("ListGitHubMergeAnnouncementsByIssue: %v", err)
	}
	if len(before) != 1 || before[0].Status != "pending" || before[0].AttemptCount != 0 {
		t.Fatalf("expected 1 pending announcement with attempt_count 0 before the worker runs, got %+v", before)
	}

	realTxStarter := testHandler.TxStarter
	testHandler.TxStarter = failCommitTxStarter{pool: testPool}
	t.Cleanup(func() { testHandler.TxStarter = realTxStarter })

	worker := NewMergeAnnouncementWorker(testHandler)
	worked, err := worker.ProcessNext(ctx)
	if err != nil {
		t.Fatalf("ProcessNext: %v", err)
	}
	if !worked {
		t.Fatalf("expected ProcessNext to claim the pending announcement (and then retry it after the induced Commit failure)")
	}

	testHandler.TxStarter = realTxStarter

	after, err := testHandler.Queries.ListGitHubMergeAnnouncementsByIssue(ctx, parseUUID(created.ID))
	if err != nil {
		t.Fatalf("ListGitHubMergeAnnouncementsByIssue after worker: %v", err)
	}
	if len(after) != 1 {
		t.Fatalf("expected exactly 1 announcement row to survive the induced Commit failure, got %d", len(after))
	}
	got := after[0]
	if got.Status != "pending" {
		t.Fatalf("expected the row to remain pending (reclaimable) after a Commit failure, got status %q", got.Status)
	}
	if got.AttemptCount != 1 {
		t.Fatalf("expected attempt_count to increment to 1 after the induced Commit failure, got %d", got.AttemptCount)
	}
	if !got.LastError.Valid || got.LastError.String == "" {
		t.Fatalf("expected last_error to be recorded for the induced Commit failure, got %+v", got.LastError)
	}
	if got.LeaseToken.Valid {
		t.Fatalf("expected lease_token to be cleared so the row is reclaimable, got %+v", got.LeaseToken)
	}

	var commentCount int
	if err := testPool.QueryRow(ctx,
		`SELECT count(*) FROM comment WHERE issue_id = $1 AND author_type = 'system'`,
		created.ID,
	).Scan(&commentCount); err != nil {
		t.Fatalf("count system comments: %v", err)
	}
	if commentCount != 0 {
		t.Errorf("expected no system comment to survive: the comment insert was rolled back along with the failed Commit, got %d", commentCount)
	}
}

// ── Diagnostics + selected recovery (CHE-384/01-02 task 1) ──────────────────

// fakeGitHubAppServer stands in for GitHub's App-authenticated API for the
// recovery path: it mints an installation token and answers the merge
// identity GraphQL query with mergeFn's response for the given variables.
func fakeGitHubAppServer(t *testing.T, mergeFn func(vars map[string]any) string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/access_tokens") {
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"token":"ghs_secret","expires_at":"` +
				timeNowPlusHourRFC3339() + `"}`))
			return
		}
		if strings.HasSuffix(r.URL.Path, "/graphql") {
			body, _ := io.ReadAll(r.Body)
			var req struct {
				Variables map[string]any `json:"variables"`
			}
			_ = json.Unmarshal(body, &req)
			_, _ = w.Write([]byte(`{"data":` + mergeFn(req.Variables) + `}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func timeNowPlusHourRFC3339() string {
	return time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
}

// withTestPRRefresh swaps testHandler.PRRefresh for a Manager backed by a
// ghsnapshot.Client pointed at srv, restoring the original afterward — the
// same seam MaybeEnqueueOnView/etc already use in production, just aimed at
// a fake GitHub App API instead of a live one.
func withTestPRRefresh(t *testing.T, srv *httptest.Server) {
	t.Helper()
	original := testHandler.PRRefresh
	testHandler.PRRefresh = ghsnapshot.NewManager(
		ghsnapshot.NewClientForTest(srv.URL),
		testHandler.Queries,
		testHandler.TxStarter,
		testHandler.broadcastPRSnapshotApplied,
	)
	t.Cleanup(func() { testHandler.PRRefresh = original })
}

// seedMergedGitHubPRLink inserts an installation, a merged github_pull_request
// row, and an issue_pull_request link — the state that would exist if a
// merged PR was correctly mirrored/linked by an earlier webhook, but never
// produced an announcement (e.g. it merged before the feature existed).
// Returns the PR row's UUID.
func seedMergedGitHubPRLink(t *testing.T, ctx context.Context, issueID, owner, repo string, prNumber int32, installationID int64, closeIntent bool) string {
	t.Helper()
	if _, err := testHandler.Queries.CreateGitHubInstallation(ctx, db.CreateGitHubInstallationParams{
		WorkspaceID:    parseUUID(testWorkspaceID),
		InstallationID: installationID,
		AccountLogin:   "recovery-test-acct",
		AccountType:    "User",
	}); err != nil {
		t.Fatalf("CreateGitHubInstallation: %v", err)
	}

	pr, err := testHandler.Queries.UpsertGitHubPullRequest(ctx, db.UpsertGitHubPullRequestParams{
		WorkspaceID:         parseUUID(testWorkspaceID),
		InstallationID:      installationID,
		RepoOwner:           owner,
		RepoName:            repo,
		PrNumber:            prNumber,
		Title:               "Recovered PR",
		State:               "merged",
		HtmlUrl:             fmt.Sprintf("https://github.com/%s/%s/pull/%d", owner, repo, prNumber),
		PrCreatedAt:         pgtype.Timestamptz{Time: time.Now().Add(-time.Hour), Valid: true},
		PrUpdatedAt:         pgtype.Timestamptz{Time: time.Now(), Valid: true},
		ClearMergeableState: pgtype.Bool{Bool: true, Valid: true},
	})
	if err != nil {
		t.Fatalf("UpsertGitHubPullRequest: %v", err)
	}

	if err := testHandler.Queries.LinkIssueToPullRequest(ctx, db.LinkIssueToPullRequestParams{
		IssueID:       parseUUID(issueID),
		PullRequestID: pr.ID,
		CloseIntent:   closeIntent,
		LinkedByType:  pgtype.Text{String: "system", Valid: true},
	}); err != nil {
		t.Fatalf("LinkIssueToPullRequest: %v", err)
	}
	return uuidToString(pr.ID)
}

func createTestIssueForMergeAnnouncement(t *testing.T, title string) IssueResponse {
	t.Helper()
	req := newRequest("POST", "/api/issues?workspace_id="+testWorkspaceID, map[string]any{
		"title":  title,
		"status": "in_progress",
	})
	w := testutil.Call(t, testHandler.CreateIssue, req).Want(http.StatusCreated)
	var created IssueResponse
	json.NewDecoder(w.Body).Decode(&created)
	return created
}

func cleanupMergeAnnouncementFixture(t *testing.T, issueID string) {
	t.Helper()
	ctx := context.Background()
	t.Cleanup(func() {
		testPool.Exec(ctx, `DELETE FROM github_merge_announcement WHERE issue_id = $1`, issueID)
		testPool.Exec(ctx, `DELETE FROM comment WHERE issue_id = $1`, issueID)
		testPool.Exec(ctx, `DELETE FROM issue_pull_request WHERE issue_id = $1`, issueID)
		testPool.Exec(ctx, `DELETE FROM github_pull_request WHERE workspace_id = $1`, testWorkspaceID)
		testPool.Exec(ctx, `DELETE FROM github_installation WHERE workspace_id = $1`, testWorkspaceID)
		testPool.Exec(ctx, `DELETE FROM activity_log WHERE issue_id = $1`, issueID)
		testPool.Exec(ctx, `DELETE FROM issue WHERE id = $1`, issueID)
	})
}

// TestListPullRequestsForIssue_ExposesMergeAnnouncementDiagnostics is the
// primary test for task 1's read side: a delivered announcement must be
// visible through the existing `issue pull-requests` read path, in the shape
// the CLI/docs promise (01-RESEARCH.md's "Access Gap" / review condition A).
func TestListPullRequestsForIssue_ExposesMergeAnnouncementDiagnostics(t *testing.T) {
	if testHandler == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	ctx := context.Background()
	secret := "merge-diag-test-secret"
	t.Setenv("GITHUB_WEBHOOK_SECRET", secret)

	created := createTestIssueForMergeAnnouncement(t, "Diagnostics test issue")
	cleanupMergeAnnouncementFixture(t, created.ID)

	const installationID int64 = 99887722
	if _, err := testHandler.Queries.CreateGitHubInstallation(ctx, db.CreateGitHubInstallationParams{
		WorkspaceID:    parseUUID(testWorkspaceID),
		InstallationID: installationID,
		AccountLogin:   "merge-diag-acct",
		AccountType:    "User",
	}); err != nil {
		t.Fatalf("CreateGitHubInstallation: %v", err)
	}

	body := buildMergedPRWebhookBody(created.Identifier, 5252, "acme", "diag", installationID)
	postSignedGitHubWebhook(t, secret, body, "diag-delivery-1")

	worker := NewMergeAnnouncementWorker(testHandler)
	if _, err := worker.ProcessNext(ctx); err != nil {
		t.Fatalf("ProcessNext: %v", err)
	}

	req := withURLParam(newRequest("GET", "/api/issues/"+created.ID+"/pull-requests", nil), "id", created.ID)
	w := testutil.Call(t, testHandler.ListPullRequestsForIssue, req).Want(http.StatusOK)
	var out struct {
		PullRequests []GitHubPullRequestResponse `json:"pull_requests"`
	}
	if err := json.NewDecoder(w.Body).Decode(&out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(out.PullRequests) != 1 {
		t.Fatalf("expected 1 PR in response, got %d", len(out.PullRequests))
	}
	ma := out.PullRequests[0].MergeAnnouncement
	if ma == nil {
		t.Fatalf("expected merge_announcement to be populated after delivery")
	}
	if ma.Status != "delivered" {
		t.Errorf("expected status 'delivered', got %q", ma.Status)
	}
	if ma.CommentID == nil || *ma.CommentID == "" {
		t.Errorf("expected comment_id to be populated once delivered")
	}
	if ma.SentAt == nil {
		t.Errorf("expected sent_at to be populated once delivered")
	}
	if ma.LastError != nil {
		t.Errorf("expected no last_error on a clean delivery, got %v", *ma.LastError)
	}
}

// TestListPullRequestsForIssue_OmitsMergeAnnouncementWhenNoneEnqueued covers
// the "never merged while linked" / "merged before the feature existed" case:
// a PR with no announcement record must simply omit the field, not error.
func TestListPullRequestsForIssue_OmitsMergeAnnouncementWhenNoneEnqueued(t *testing.T) {
	if testHandler == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	ctx := context.Background()
	created := createTestIssueForMergeAnnouncement(t, "No announcement issue")
	cleanupMergeAnnouncementFixture(t, created.ID)

	seedMergedGitHubPRLink(t, ctx, created.ID, "acme", "noannounce", 6161, 99887733, false)

	req := withURLParam(newRequest("GET", "/api/issues/"+created.ID+"/pull-requests", nil), "id", created.ID)
	w := testutil.Call(t, testHandler.ListPullRequestsForIssue, req).Want(http.StatusOK)
	var out struct {
		PullRequests []GitHubPullRequestResponse `json:"pull_requests"`
	}
	json.NewDecoder(w.Body).Decode(&out)
	if len(out.PullRequests) != 1 {
		t.Fatalf("expected 1 PR, got %d", len(out.PullRequests))
	}
	if out.PullRequests[0].MergeAnnouncement != nil {
		t.Errorf("expected no merge_announcement for a PR with no enqueued record, got %+v", out.PullRequests[0].MergeAnnouncement)
	}
}

// TestAnnounceMergeForIssue_RecoversHistoricalMerge is the primary recovery
// test: an issue linked to a merged PR that never got an announcement row
// recovers exactly one correctly attributed comment, sourced from a fake
// GitHub App API standing in for the real one (01-VALIDATION.md MERGE-05/06 —
// no production write, no caller-supplied merge evidence trusted).
func TestAnnounceMergeForIssue_RecoversHistoricalMerge(t *testing.T) {
	if testHandler == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	ctx := context.Background()
	created := createTestIssueForMergeAnnouncement(t, "Recovery test issue")
	cleanupMergeAnnouncementFixture(t, created.ID)

	const installationID int64 = 99887744
	prID := seedMergedGitHubPRLink(t, ctx, created.ID, "acme", "recover", 7171, installationID, true)
	_ = prID

	const wantMergedAt = "2026-08-01T12:00:00Z"
	const wantMergeSHA = "deadbeefcafefeed0000111122223333deadbeef"
	srv := fakeGitHubAppServer(t, func(vars map[string]any) string {
		return fmt.Sprintf(`{"repository":{"databaseId":424242,"pullRequest":{
			"merged":true,"mergedAt":%q,
			"mergeCommit":{"oid":%q},
			"url":"https://github.com/acme/recover/pull/7171"
		}}}`, wantMergedAt, wantMergeSHA)
	})
	withTestPRRefresh(t, srv)

	req := withURLParam(newRequest("POST", "/api/issues/"+created.ID+"/pull-requests/merge-announcements", map[string]any{
		"pr_url": "https://github.com/acme/recover/pull/7171",
	}), "id", created.ID)
	w := testutil.Call(t, testHandler.AnnounceMergeForIssue, req)
	if w.Code != http.StatusOK && w.Code != http.StatusAccepted {
		t.Fatalf("expected 200 or 202, got %d: %s", w.Code, w.Body.String())
	}
	var resp AnnounceMergeResponse
	json.NewDecoder(w.Body).Decode(&resp)
	if resp.Status != "delivered" {
		t.Fatalf("expected recovery to deliver synchronously, got status %q (full response: %+v)", resp.Status, resp)
	}
	if resp.CommentID == nil || *resp.CommentID == "" {
		t.Fatalf("expected comment_id in the response once delivered")
	}
	if resp.MergedAt == nil {
		t.Errorf("expected recovered merged_at to be populated, got nil")
	} else {
		got, err := time.Parse(time.RFC3339, *resp.MergedAt)
		if err != nil {
			t.Fatalf("recovered merged_at %q is not RFC3339: %v", *resp.MergedAt, err)
		}
		want, _ := time.Parse(time.RFC3339, wantMergedAt)
		if !got.Equal(want) {
			t.Errorf("expected recovered merged_at to be the original merge time %q, got %q", wantMergedAt, *resp.MergedAt)
		}
	}
	if resp.MergeCommitSHA == nil || *resp.MergeCommitSHA != wantMergeSHA {
		t.Errorf("expected recovered merge_commit_sha to be %q, got %v", wantMergeSHA, resp.MergeCommitSHA)
	}

	var commentCount int
	if err := testPool.QueryRow(ctx,
		`SELECT count(*) FROM comment WHERE issue_id = $1 AND author_type = 'system'`,
		created.ID,
	).Scan(&commentCount); err != nil {
		t.Fatalf("count system comments: %v", err)
	}
	if commentCount != 1 {
		t.Fatalf("expected exactly 1 system comment after recovery, got %d", commentCount)
	}

	var content string
	testPool.QueryRow(ctx, `SELECT content FROM comment WHERE issue_id = $1 AND author_type = 'system' LIMIT 1`, created.ID).Scan(&content)
	if !strings.Contains(content, "#7171") {
		t.Errorf("expected recovered comment to name the PR, got %q", content)
	}
	if !strings.Contains(content, "2026-08-01T12:00:00Z") {
		t.Errorf("expected recovered comment to state the true original merge time, got %q", content)
	}

	// Issue status must be untouched by recovery alone.
	afterIssue, err := testHandler.Queries.GetIssue(ctx, parseUUID(created.ID))
	if err != nil {
		t.Fatalf("GetIssue: %v", err)
	}
	if afterIssue.Status != "in_progress" {
		t.Errorf("expected issue status unchanged by recovery, got %q", afterIssue.Status)
	}
}

// TestAnnounceMergeForIssue_RetryIsIdempotent proves recovering the same
// issue+PR twice returns the first call's outcome instead of a second
// comment (01-02-PLAN.md task 1: "Retry is idempotent").
func TestAnnounceMergeForIssue_RetryIsIdempotent(t *testing.T) {
	if testHandler == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	ctx := context.Background()
	created := createTestIssueForMergeAnnouncement(t, "Idempotent recovery issue")
	cleanupMergeAnnouncementFixture(t, created.ID)

	const installationID int64 = 99887755
	seedMergedGitHubPRLink(t, ctx, created.ID, "acme", "idempotent", 8181, installationID, false)

	srv := fakeGitHubAppServer(t, func(vars map[string]any) string {
		return `{"repository":{"databaseId":424243,"pullRequest":{
			"merged":true,"mergedAt":"2026-07-01T00:00:00Z",
			"mergeCommit":{"oid":"1111111111111111111111111111111111111111"},
			"url":"https://github.com/acme/idempotent/pull/8181"
		}}}`
	})
	withTestPRRefresh(t, srv)

	callOnce := func() AnnounceMergeResponse {
		req := withURLParam(newRequest("POST", "/api/issues/"+created.ID+"/pull-requests/merge-announcements", map[string]any{
			"pr_url": "https://github.com/acme/idempotent/pull/8181",
		}), "id", created.ID)
		w := testutil.Call(t, testHandler.AnnounceMergeForIssue, req)
		if w.Code != http.StatusOK && w.Code != http.StatusAccepted {
			t.Fatalf("expected 200 or 202, got %d: %s", w.Code, w.Body.String())
		}
		var resp AnnounceMergeResponse
		json.NewDecoder(w.Body).Decode(&resp)
		return resp
	}

	first := callOnce()
	second := callOnce()

	if first.AnnouncementID == nil || second.AnnouncementID == nil || *first.AnnouncementID != *second.AnnouncementID {
		t.Fatalf("expected retry to resolve to the same announcement identity, got %+v vs %+v", first, second)
	}
	if second.Status != "delivered" {
		t.Errorf("expected second call to observe 'delivered' status, got %q", second.Status)
	}

	var commentCount int
	if err := testPool.QueryRow(ctx,
		`SELECT count(*) FROM comment WHERE issue_id = $1 AND author_type = 'system'`,
		created.ID,
	).Scan(&commentCount); err != nil {
		t.Fatalf("count system comments: %v", err)
	}
	if commentCount != 1 {
		t.Fatalf("expected exactly 1 system comment despite two recovery calls, got %d", commentCount)
	}
}

// TestAnnounceMergeForIssue_RejectsUnmergedPR covers MERGE-05's negative
// case: recovery must refuse to fabricate an announcement for a PR GitHub
// itself reports as not merged.
func TestAnnounceMergeForIssue_RejectsUnmergedPR(t *testing.T) {
	if testHandler == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	ctx := context.Background()
	created := createTestIssueForMergeAnnouncement(t, "Unmerged recovery issue")
	cleanupMergeAnnouncementFixture(t, created.ID)

	const installationID int64 = 99887766
	// Seed the link as though the PR were merged in our own mirror (state
	// drift scenario) — the recovery path must trust GitHub's live answer,
	// not the locally mirrored state.
	seedMergedGitHubPRLink(t, ctx, created.ID, "acme", "notmerged", 9191, installationID, false)

	srv := fakeGitHubAppServer(t, func(vars map[string]any) string {
		return `{"repository":{"databaseId":424244,"pullRequest":{
			"merged":false,"mergedAt":"","mergeCommit":null,
			"url":"https://github.com/acme/notmerged/pull/9191"
		}}}`
	})
	withTestPRRefresh(t, srv)

	req := withURLParam(newRequest("POST", "/api/issues/"+created.ID+"/pull-requests/merge-announcements", map[string]any{
		"pr_url": "https://github.com/acme/notmerged/pull/9191",
	}), "id", created.ID)
	testutil.Call(t, testHandler.AnnounceMergeForIssue, req).Want(http.StatusConflict)

	var count int
	testPool.QueryRow(ctx, `SELECT count(*) FROM github_merge_announcement WHERE issue_id = $1`, created.ID).Scan(&count)
	if count != 0 {
		t.Errorf("expected no announcement record created for an unmerged PR, got %d", count)
	}
}

// TestAnnounceMergeForIssue_RejectsUnlinkedIssue covers the "no working link"
// guard: recovery must refuse when the issue was never linked to the PR.
func TestAnnounceMergeForIssue_RejectsUnlinkedIssue(t *testing.T) {
	if testHandler == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	created := createTestIssueForMergeAnnouncement(t, "Unlinked recovery issue")
	cleanupMergeAnnouncementFixture(t, created.ID)

	req := withURLParam(newRequest("POST", "/api/issues/"+created.ID+"/pull-requests/merge-announcements", map[string]any{
		"pr_url": "https://github.com/acme/never-linked/pull/1",
	}), "id", created.ID)
	testutil.Call(t, testHandler.AnnounceMergeForIssue, req).Want(http.StatusNotFound)
}

// TestAnnounceMergeForIssue_RejectsWhenGitHubDisabled covers the master
// github_enabled switch: recovery must refuse rather than fail open.
func TestAnnounceMergeForIssue_RejectsWhenGitHubDisabled(t *testing.T) {
	if testHandler == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	ctx := context.Background()
	created := createTestIssueForMergeAnnouncement(t, "Disabled github recovery issue")
	cleanupMergeAnnouncementFixture(t, created.ID)

	const installationID int64 = 99887777
	seedMergedGitHubPRLink(t, ctx, created.ID, "acme", "disabled", 1010, installationID, false)

	var previousSettings []byte
	testPool.QueryRow(ctx, `SELECT settings FROM workspace WHERE id = $1`, testWorkspaceID).Scan(&previousSettings)
	testPool.Exec(ctx, `UPDATE workspace SET settings = jsonb_set(coalesce(settings, '{}'::jsonb), '{github_enabled}', 'false') WHERE id = $1`, testWorkspaceID)
	t.Cleanup(func() {
		testPool.Exec(ctx, `UPDATE workspace SET settings = $1 WHERE id = $2`, previousSettings, testWorkspaceID)
	})

	req := withURLParam(newRequest("POST", "/api/issues/"+created.ID+"/pull-requests/merge-announcements", map[string]any{
		"pr_url": "https://github.com/acme/disabled/pull/1010",
	}), "id", created.ID)
	testutil.Call(t, testHandler.AnnounceMergeForIssue, req).Want(http.StatusConflict)
}

// TestAnnounceMergeForIssue_RejectsMalformedPRURL covers input validation:
// a non-github.com URL or non-PR path must be rejected before any lookup.
func TestAnnounceMergeForIssue_RejectsMalformedPRURL(t *testing.T) {
	if testHandler == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	created := createTestIssueForMergeAnnouncement(t, "Malformed URL recovery issue")
	cleanupMergeAnnouncementFixture(t, created.ID)

	for _, badURL := range []string{
		"not-a-url",
		"https://gitlab.com/acme/repo/-/merge_requests/1",
		"https://github.com/acme/repo/issues/1",
	} {
		req := withURLParam(newRequest("POST", "/api/issues/"+created.ID+"/pull-requests/merge-announcements", map[string]any{
			"pr_url": badURL,
		}), "id", created.ID)
		testutil.Call(t, testHandler.AnnounceMergeForIssue, req).Want(http.StatusBadRequest)
	}
}

// TestAnnounceMergeForIssue_DoesNotCrossClaimUnrelatedPendingRow is the
// regression test both the independent code review and QA required
// (CHE-384/01-02 review fix): with an OLDER pending announcement already
// queued for a completely unrelated issue, recovering a different issue's
// merge must still deliver THAT issue's own announcement — never the
// decoy's — and must report "delivered" with a real comment_id, not
// "pending" while a stranger's row silently gets consumed instead.
//
// Before the fix, AnnounceMergeForIssue delivered via the durable worker's
// ProcessNext, whose claim is `ORDER BY available_at, created_at LIMIT 1`
// with no filter on which row the caller actually wants. Since a decoy
// created first always sorts ahead of a target created afterward, the old
// code deterministically cross-claimed here.
func TestAnnounceMergeForIssue_DoesNotCrossClaimUnrelatedPendingRow(t *testing.T) {
	if testHandler == nil {
		t.Skip("handler test fixture not initialized (no DB?)")
	}
	ctx := context.Background()

	// The decoy: a fully independent issue/PR with its own pending
	// announcement, enqueued first so it sorts ahead of the target under
	// the worker's global (available_at, created_at) ordering.
	decoyIssue := createTestIssueForMergeAnnouncement(t, "Decoy issue for cross-claim regression")
	cleanupMergeAnnouncementFixture(t, decoyIssue.ID)
	const decoyInstallationID int64 = 99887788
	decoyPRID := seedMergedGitHubPRLink(t, ctx, decoyIssue.ID, "acme", "decoy", 5050, decoyInstallationID, false)
	decoyAnnouncement, err := testHandler.Queries.CreateGitHubMergeAnnouncement(ctx, db.CreateGitHubMergeAnnouncementParams{
		WorkspaceID:    parseUUID(testWorkspaceID),
		Provider:       "github",
		RepositoryID:   999001,
		RepoOwner:      "acme",
		RepoName:       "decoy",
		PrNumber:       5050,
		PullRequestID:  parseUUID(decoyPRID),
		IssueID:        parseUUID(decoyIssue.ID),
		EventKind:      "merged",
		MergeCommitSha: "decoy0000000000000000000000000000000000",
		MergedAt:       pgtype.Timestamptz{Time: time.Now().Add(-time.Hour), Valid: true},
	})
	if err != nil {
		t.Fatalf("seed decoy CreateGitHubMergeAnnouncement: %v", err)
	}
	if decoyAnnouncement.Status != "pending" {
		t.Fatalf("expected decoy announcement to start pending, got %q", decoyAnnouncement.Status)
	}

	// The actual recovery target, created afterward — its available_at/
	// created_at sort strictly after the decoy's.
	created := createTestIssueForMergeAnnouncement(t, "Cross-claim regression target issue")
	cleanupMergeAnnouncementFixture(t, created.ID)
	const installationID int64 = 99887799
	seedMergedGitHubPRLink(t, ctx, created.ID, "acme", "crossclaim", 6060, installationID, false)

	srv := fakeGitHubAppServer(t, func(vars map[string]any) string {
		return `{"repository":{"databaseId":424245,"pullRequest":{
			"merged":true,"mergedAt":"2026-08-15T00:00:00Z",
			"mergeCommit":{"oid":"target00000000000000000000000000000000"},
			"url":"https://github.com/acme/crossclaim/pull/6060"
		}}}`
	})
	withTestPRRefresh(t, srv)

	req := withURLParam(newRequest("POST", "/api/issues/"+created.ID+"/pull-requests/merge-announcements", map[string]any{
		"pr_url": "https://github.com/acme/crossclaim/pull/6060",
	}), "id", created.ID)
	w := testutil.Call(t, testHandler.AnnounceMergeForIssue, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 (delivered synchronously), got %d: %s", w.Code, w.Body.String())
	}
	var resp AnnounceMergeResponse
	json.NewDecoder(w.Body).Decode(&resp)
	if resp.Status != "delivered" {
		t.Fatalf("expected the target's own announcement to be delivered, got status %q (cross-claim regression)", resp.Status)
	}
	if resp.CommentID == nil || *resp.CommentID == "" {
		t.Fatalf("expected a real comment_id for the target's own delivery, got %+v", resp)
	}

	// The target issue must have its own system comment naming ITS PR.
	var targetContent string
	if err := testPool.QueryRow(ctx,
		`SELECT content FROM comment WHERE issue_id = $1 AND author_type = 'system' LIMIT 1`,
		created.ID,
	).Scan(&targetContent); err != nil {
		t.Fatalf("expected target issue to have its own system comment: %v", err)
	}
	if !strings.Contains(targetContent, "#6060") {
		t.Errorf("expected target's comment to name its own PR #6060, got %q", targetContent)
	}

	// The decoy must NOT have been silently consumed by the target's
	// recovery call: it is untouched (still pending) since only the
	// background worker or its own recovery call may deliver it.
	decoyAfter, err := testHandler.Queries.ListGitHubMergeAnnouncementsByIssue(ctx, parseUUID(decoyIssue.ID))
	if err != nil {
		t.Fatalf("ListGitHubMergeAnnouncementsByIssue(decoy): %v", err)
	}
	if len(decoyAfter) != 1 {
		t.Fatalf("expected exactly 1 decoy announcement row, got %d", len(decoyAfter))
	}
	if decoyAfter[0].Status != "pending" {
		t.Errorf("expected decoy announcement to remain pending (untouched by the target's recovery call), got %q", decoyAfter[0].Status)
	}

	var decoyCommentCount int
	if err := testPool.QueryRow(ctx,
		`SELECT count(*) FROM comment WHERE issue_id = $1 AND author_type = 'system'`,
		decoyIssue.ID,
	).Scan(&decoyCommentCount); err != nil {
		t.Fatalf("count decoy system comments: %v", err)
	}
	if decoyCommentCount != 0 {
		t.Errorf("expected the decoy issue to have NO system comment from the target's recovery call, got %d", decoyCommentCount)
	}
}
