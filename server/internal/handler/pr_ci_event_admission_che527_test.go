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
	"strconv"
	"testing"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// pr_ci_event_admission_che527_test.go is the CHE-527 (C4b) admission-boundary
// regression: PR and CI event-driven issue dispatch. The dispatch-relevant
// path a merged/closed PR webhook reaches — advanceIssueToDone ->
// notifyParentOfChildDone -> dispatchParentAssigneeTrigger ->
// triggerChildDoneAgent/triggerChildDoneSquad — is the SAME code C4a
// (CHE-526) already routed through the shared admission boundary
// (logCommentEnqueueFailure / service.ErrDuplicatePendingTask). These tests
// exercise that path through its real entry point, HandleGitHubWebhook,
// rather than through UpdateIssue, so a regression in webhook-specific
// plumbing (installation/workspace fan-out, delivery-GUID handling, signature
// verification) would be caught here even though the underlying dispatch
// function is already covered by child_stage_wake_admission_che526_test.go.

// prMergeWebhookFixture creates a parent+child pair, a GitHub installation
// bound to the workspace, and returns everything needed to POST a signed
// pull_request/closed(merged) webhook that resolves to the child issue via
// its identifier in the PR title.
type prMergeWebhookFixture struct {
	parent IssueResponse
	child  IssueResponse
	secret string
}

func newPRMergeWebhookFixture(t *testing.T, installationID int64) prMergeWebhookFixture {
	t.Helper()
	ctx := context.Background()

	fx := newChildDoneFixture(t, "in_progress")

	secret := "che527-pr-ci-admission-secret"
	t.Setenv("GITHUB_WEBHOOK_SECRET", secret)

	if _, err := testHandler.Queries.CreateGitHubInstallation(ctx, db.CreateGitHubInstallationParams{
		WorkspaceID:    parseUUID(testWorkspaceID),
		InstallationID: installationID,
		AccountLogin:   "che527-acct",
		AccountType:    "User",
	}); err != nil {
		t.Fatalf("CreateGitHubInstallation: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM issue_pull_request WHERE issue_id IN ($1, $2)`, fx.child.ID, fx.parent.ID)
		testPool.Exec(context.Background(), `DELETE FROM github_pull_request WHERE workspace_id = $1`, testWorkspaceID)
		testPool.Exec(context.Background(), `DELETE FROM github_installation WHERE installation_id = $1`, installationID)
		testPool.Exec(context.Background(), `DELETE FROM activity_log WHERE issue_id IN ($1, $2)`, fx.child.ID, fx.parent.ID)
		testPool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE issue_id = $1`, fx.parent.ID)
	})

	return prMergeWebhookFixture{parent: fx.parent, child: fx.child, secret: secret}
}

// postSignedGitHubCIEvent posts a signed non-pull_request GitHub webhook
// (check_suite/check_run/status) against the real handler. Unlike the
// pull_request-only postSignedGitHubWebhook helper in
// github_merge_announcement_test.go, this needs a variable X-GitHub-Event
// header, so it stays a separate, narrowly-scoped helper rather than
// widening that one's signature.
func postSignedGitHubCIEvent(t *testing.T, secret, event string, payload map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal webhook payload: %v", err)
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	req := httptest.NewRequest("POST", "/api/webhooks/github", bytes.NewReader(body))
	req.Header.Set("X-GitHub-Event", event)
	req.Header.Set("X-Hub-Signature-256", sig)

	w := httptest.NewRecorder()
	testHandler.HandleGitHubWebhook(w, req)
	return w
}

func mergedPRPayload(childIdentifier string, prNumber int, owner, repo, installationLogin string, installationID int64) map[string]any {
	return map[string]any{
		"action": "closed",
		"pull_request": map[string]any{
			"number":     prNumber,
			"html_url":   "https://github.com/" + owner + "/" + repo + "/pull/" + strconv.Itoa(prNumber),
			"title":      "Fix " + childIdentifier,
			"body":       "",
			"state":      "closed",
			"draft":      false,
			"merged":     true,
			"merged_at":  "2026-09-16T00:00:00Z",
			"closed_at":  "2026-09-16T00:00:00Z",
			"created_at": "2026-09-15T00:00:00Z",
			"updated_at": "2026-09-16T00:00:00Z",
			"head":       map[string]any{"ref": "fix/che527"},
			"user":       map[string]any{"login": "octocat", "avatar_url": ""},
		},
		"repository": map[string]any{
			"name":  repo,
			"owner": map[string]any{"login": owner},
		},
		"installation": map[string]any{"id": installationID, "account": map[string]any{"login": installationLogin}},
	}
}

// TestPRMergeWebhookWakesAssignedParentAgentThroughAdmission proves the C4b
// deliverable end to end: merging a child's PR through the real webhook
// entry point wakes the parent's agent assignee exactly once, going through
// the same admission boundary as every other trigger family.
func TestPRMergeWebhookWakesAssignedParentAgentThroughAdmission(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	const installationID int64 = 527001
	fx := newPRMergeWebhookFixture(t, installationID)
	agentID := createHandlerTestAgent(t, "CHE-527 pr-merge wake", nil)
	setIssueAssigneeDirect(t, fx.parent.ID, "agent", agentID)

	payload := mergedPRPayload(fx.child.Identifier, 52701, "acme", "widget", "che527-acct", installationID)
	postSignedGitHubWebhook(t, fx.secret, payload, "")

	updatedChild, err := testHandler.Queries.GetIssue(context.Background(), parseUUID(fx.child.ID))
	if err != nil {
		t.Fatalf("GetIssue child: %v", err)
	}
	if updatedChild.Status != "done" {
		t.Fatalf("expected child status 'done', got %q", updatedChild.Status)
	}

	if got := countPendingTasksForAgent(t, fx.parent.ID, agentID); got != 1 {
		t.Fatalf("after PR-merge webhook: parent pending tasks = %d, want exactly 1", got)
	}
}

// TestPRMergeWebhookWakesAssignedParentSquadThroughAdmission mirrors the
// agent-assignee case for a squad-leader assignee, matching the two
// assignee-type variants C4a covers for the underlying dispatch functions.
func TestPRMergeWebhookWakesAssignedParentSquadThroughAdmission(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	const installationID int64 = 527002
	fx := newPRMergeWebhookFixture(t, installationID)
	sq := newSquadCommentTriggerFixture(t)
	setIssueAssigneeDirect(t, fx.parent.ID, "squad", sq.SquadID)

	payload := mergedPRPayload(fx.child.Identifier, 52702, "acme", "widget", "che527-acct", installationID)
	postSignedGitHubWebhook(t, fx.secret, payload, "")

	if got := countPendingTasksForAgent(t, fx.parent.ID, sq.LeaderID); got != 1 {
		t.Fatalf("after PR-merge webhook: parent squad-leader pending tasks = %d, want exactly 1", got)
	}
}

// TestPRMergeWebhookRedeliveryDoesNotDuplicateWake proves GitHub's
// at-least-once redelivery semantics (a fresh X-GitHub-Delivery GUID on the
// exact same logical event, per the comment in HandleGitHubWebhook) coalesce
// at the admission boundary rather than stacking a second pending task —
// the retry/duplicate half of this issue's verification ask.
func TestPRMergeWebhookRedeliveryDoesNotDuplicateWake(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	const installationID int64 = 527003
	fx := newPRMergeWebhookFixture(t, installationID)
	agentID := createHandlerTestAgent(t, "CHE-527 redelivery", nil)
	setIssueAssigneeDirect(t, fx.parent.ID, "agent", agentID)

	payload := mergedPRPayload(fx.child.Identifier, 52703, "acme", "widget", "che527-acct", installationID)

	postSignedGitHubWebhook(t, fx.secret, payload, "che527-delivery-1")
	if got := countPendingTasksForAgent(t, fx.parent.ID, agentID); got != 1 {
		t.Fatalf("after first delivery: pending tasks = %d, want 1", got)
	}

	// Redeliver the identical logical event under a fresh delivery GUID
	// (GitHub's own at-least-once semantics) — the child is already 'done',
	// so notifyParentOfChildDone's own not-a-transition guard is the first
	// line of defense; this proves it still holds when reached through the
	// webhook rather than UpdateIssue.
	postSignedGitHubWebhook(t, fx.secret, payload, "che527-delivery-2")
	if got := countPendingTasksForAgent(t, fx.parent.ID, agentID); got != 1 {
		t.Fatalf("after redelivery: pending tasks = %d, want still 1 (no duplicate wake)", got)
	}
}

// TestCheckSuiteWebhookNeverEnqueuesAgentTask is the "no regression to
// refresh-only events" guarantee this issue's verification ask names
// explicitly: check_suite/check_run/status events are pure PR-snapshot
// refresh triggers (Plan C, MUL-5265) and must never reach dispatch, whether
// or not the linked issue's parent has an agent/squad assignee.
func TestCheckSuiteWebhookNeverEnqueuesAgentTask(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	const installationID int64 = 527004
	fx := newPRMergeWebhookFixture(t, installationID)
	agentID := createHandlerTestAgent(t, "CHE-527 refresh-only", nil)
	setIssueAssigneeDirect(t, fx.parent.ID, "agent", agentID)

	payload := map[string]any{
		"action": "completed",
		"check_suite": map[string]any{
			"head_sha":      "che527deadbeefcafefeedfacefeedfacefeed01",
			"pull_requests": []any{},
		},
		"repository": map[string]any{
			"name":  "widget",
			"owner": map[string]any{"login": "acme"},
		},
		"installation": map[string]any{"id": installationID},
	}
	w := postSignedGitHubCIEvent(t, fx.secret, "check_suite", payload)
	if w.Code != http.StatusAccepted {
		t.Fatalf("check_suite webhook: expected 202, got %d: %s", w.Code, w.Body.String())
	}

	updatedChild, err := testHandler.Queries.GetIssue(context.Background(), parseUUID(fx.child.ID))
	if err != nil {
		t.Fatalf("GetIssue child: %v", err)
	}
	if updatedChild.Status != "in_progress" {
		t.Fatalf("check_suite must not change issue status, got %q", updatedChild.Status)
	}
	if got := countPendingTasksForAgent(t, fx.parent.ID, agentID); got != 0 {
		t.Fatalf("check_suite (refresh-only) enqueued %d pending task(s) on parent, want 0", got)
	}
}
