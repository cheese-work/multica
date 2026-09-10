package handler

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5"
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
