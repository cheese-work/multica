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
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
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
		testPool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE issue_id IN ($1, $2)`, fx.child.ID, fx.parent.ID)
		testPool.Exec(context.Background(), `DELETE FROM issue_wakeup_receipt WHERE wakeup_id IN (SELECT id FROM issue_wakeup WHERE issue_id IN ($1, $2))`, fx.child.ID, fx.parent.ID)
		testPool.Exec(context.Background(), `DELETE FROM issue_wakeup WHERE issue_id IN ($1, $2)`, fx.child.ID, fx.parent.ID)
		testPool.Exec(context.Background(), `DELETE FROM activity_log WHERE issue_id IN ($1, $2)`, fx.child.ID, fx.parent.ID)
	})

	return prMergeWebhookFixture{parent: fx.parent, child: fx.child, secret: secret}
}

func linkedPRForWakeup(t *testing.T, fx prMergeWebhookFixture, installationID int64, prNumber int32, headSHA string) db.GithubPullRequest {
	t.Helper()
	now := time.Now().UTC()
	pr, err := testHandler.Queries.UpsertGitHubPullRequest(context.Background(), db.UpsertGitHubPullRequestParams{
		WorkspaceID: parseUUID(testWorkspaceID), InstallationID: installationID,
		RepoOwner: "acme", RepoName: "widget", PrNumber: prNumber,
		Title: fx.child.Identifier + ": test", State: "open",
		HtmlUrl:     "https://github.com/acme/widget/pull/" + strconv.Itoa(int(prNumber)),
		PrCreatedAt: pgtype.Timestamptz{Time: now, Valid: true},
		PrUpdatedAt: pgtype.Timestamptz{Time: now, Valid: true},
		HeadSha:     headSHA, ClearMergeableState: pgtype.Bool{Bool: true, Valid: true},
	})
	if err != nil {
		t.Fatalf("UpsertGitHubPullRequest: %v", err)
	}
	if _, err := testHandler.Queries.LinkIssueToPullRequest(context.Background(), db.LinkIssueToPullRequestParams{
		IssueID: parseUUID(fx.child.ID), PullRequestID: pr.ID,
	}); err != nil {
		t.Fatalf("LinkIssueToPullRequest: %v", err)
	}
	return pr
}

func setWorkspacePRWakeSetting(t *testing.T, key string, enabled bool) {
	t.Helper()
	var original []byte
	if err := testPool.QueryRow(context.Background(), `SELECT COALESCE(settings, '{}'::jsonb) FROM workspace WHERE id=$1`, testWorkspaceID).Scan(&original); err != nil {
		t.Fatalf("read workspace settings: %v", err)
	}
	t.Cleanup(func() {
		if _, err := testPool.Exec(context.Background(), `UPDATE workspace SET settings=$2::jsonb WHERE id=$1`, testWorkspaceID, original); err != nil {
			t.Errorf("restore workspace settings: %v", err)
		}
	})
	if _, err := testPool.Exec(context.Background(), `UPDATE workspace SET settings=COALESCE(settings, '{}'::jsonb)||jsonb_build_object($2::text, $3::boolean) WHERE id=$1`, testWorkspaceID, key, enabled); err != nil {
		t.Fatalf("set workspace setting %s: %v", key, err)
	}
}

// postSignedGitHubCIEvent posts a signed non-pull_request GitHub webhook
// (check_suite/check_run/status) against the real handler. Unlike the shared
// postSignedGitHubWebhook helper, this needs a variable
// X-GitHub-Event header, so it stays a separate helper.
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

	payload := buildMergedPRWebhookBody(fx.child.Identifier, 52701, "acme", "widget", installationID)
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

	payload := buildMergedPRWebhookBody(fx.child.Identifier, 52702, "acme", "widget", installationID)
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

	payload := buildMergedPRWebhookBody(fx.child.Identifier, 52703, "acme", "widget", installationID)

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

func TestMergedPRWakesTheLinkedIssueAssigneeWithReceipt(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	const installationID int64 = 527005
	fx := newPRMergeWebhookFixture(t, installationID)
	agentID := createHandlerTestAgent(t, "CHE-973 linked PR merge", nil)
	setIssueAssigneeDirect(t, fx.child.ID, "agent", agentID)

	payload := buildMergedPRWebhookBody(fx.child.Identifier, 52705, "acme", "widget", installationID)
	postSignedGitHubWebhook(t, fx.secret, payload, "che973-merge-1")

	if got := countPendingTasksForAgent(t, fx.child.ID, agentID); got != 1 {
		t.Fatalf("linked issue pending tasks = %d, want 1", got)
	}
	if got := countPendingTasksForAgent(t, fx.parent.ID, agentID); got != 0 {
		t.Fatalf("parent pending tasks = %d, want 0", got)
	}
	var note string
	if err := testPool.QueryRow(context.Background(), `SELECT handoff_note FROM agent_task_queue WHERE issue_id=$1 AND agent_id=$2 ORDER BY created_at DESC LIMIT 1`, fx.child.ID, agentID).Scan(&note); err != nil {
		t.Fatalf("read wakeup instruction: %v", err)
	}
	for _, want := range []string{"#52705", "ae18acf237df4b9862d52f82aa7d866052613507", "done"} {
		if !strings.Contains(note, want) {
			t.Errorf("wakeup instruction %q does not contain %q", note, want)
		}
	}
	if got := dbfx.Count(t, `SELECT count(*) FROM issue_wakeup_receipt r JOIN issue_wakeup w ON w.id=r.wakeup_id WHERE w.issue_id=$1 AND w.system_rule='pr_merged' AND r.event_type='pr.merged' AND r.processed_at IS NOT NULL`, fx.child.ID); got != 1 {
		t.Fatalf("processed PR merge receipts = %d, want 1", got)
	}
}

func TestMergedPRWakeRedeliveryDoesNotDuplicateRun(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	const installationID int64 = 527006
	fx := newPRMergeWebhookFixture(t, installationID)
	agentID := createHandlerTestAgent(t, "CHE-973 merge redelivery", nil)
	setIssueAssigneeDirect(t, fx.child.ID, "agent", agentID)
	payload := buildMergedPRWebhookBody(fx.child.Identifier, 52706, "acme", "widget", installationID)

	postSignedGitHubWebhook(t, fx.secret, payload, "che973-merge-first")
	postSignedGitHubWebhook(t, fx.secret, payload, "che973-merge-redelivery")
	if got := countPendingTasksForAgent(t, fx.child.ID, agentID); got != 1 {
		t.Fatalf("pending tasks after merge redelivery = %d, want 1", got)
	}
	if got := dbfx.Count(t, `SELECT count(*) FROM issue_wakeup_receipt r JOIN issue_wakeup w ON w.id=r.wakeup_id WHERE w.issue_id=$1 AND w.system_rule='pr_merged' AND r.event_type='pr.merged'`, fx.child.ID); got != 1 {
		t.Fatalf("PR merge receipts after redelivery = %d, want 1", got)
	}
	var instruction string
	if err := testPool.QueryRow(context.Background(), `SELECT handoff_note FROM agent_task_queue WHERE issue_id=$1 AND agent_id=$2 AND context->>'wakeup_system'='pr_merged'`, fx.child.ID, agentID).Scan(&instruction); err != nil {
		t.Fatalf("read merge wake instruction: %v", err)
	}
	for _, want := range []string{"PR #52706", "ae18acf237df4b9862d52f82aa7d866052613507", "issue status when it merged was done"} {
		if !strings.Contains(instruction, want) {
			t.Errorf("merge wake instruction %q does not contain %q", instruction, want)
		}
	}
}

func TestPRMergeWakeSkipsUnassignedMemberAndCancelledIssues(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	cases := []struct {
		name         string
		installation int64
		prNumber     int
		assigneeType string
		assigneeID   string
		cancelled    bool
	}{
		{name: "unassigned", installation: 527012, prNumber: 52712},
		{name: "member", installation: 527013, prNumber: 52713, assigneeType: "member", assigneeID: testUserID},
		{name: "cancelled", installation: 527014, prNumber: 52714, assigneeType: "agent", cancelled: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx := newPRMergeWebhookFixture(t, tc.installation)
			if tc.assigneeType == "agent" {
				tc.assigneeID = createHandlerTestAgent(t, "CHE-973 cancelled", nil)
			}
			if tc.assigneeType != "" {
				setIssueAssigneeDirect(t, fx.child.ID, tc.assigneeType, tc.assigneeID)
			}
			if tc.cancelled {
				if _, err := testPool.Exec(context.Background(), `UPDATE issue SET status='cancelled' WHERE id=$1`, fx.child.ID); err != nil {
					t.Fatalf("cancel issue: %v", err)
				}
			}
			postSignedGitHubWebhook(t, fx.secret, buildMergedPRWebhookBody(fx.child.Identifier, tc.prNumber, "acme", "widget", tc.installation), "che973-skip-"+tc.name)
			if got := dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id=$1 AND context->>'wakeup_system'='pr_merged'`, fx.child.ID); got != 0 {
				t.Fatalf("PR merge wake runs = %d, want 0", got)
			}
		})
	}
}

func TestMergedPRWakesLinkedIssueSquadLeader(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	const installationID int64 = 527007
	fx := newPRMergeWebhookFixture(t, installationID)
	squad := newSquadCommentTriggerFixture(t)
	setIssueAssigneeDirect(t, fx.child.ID, "squad", squad.SquadID)
	payload := buildMergedPRWebhookBody(fx.child.Identifier, 52707, "acme", "widget", installationID)

	postSignedGitHubWebhook(t, fx.secret, payload, "che973-merge-squad")
	if got := countPendingTasksForAgent(t, fx.child.ID, squad.LeaderID); got != 1 {
		t.Fatalf("squad leader pending tasks = %d, want 1", got)
	}
}

func TestMergedPRDoesNotQueueWhileAssignedAgentIsAlreadyActive(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	const installationID int64 = 527011
	fx := newPRMergeWebhookFixture(t, installationID)
	agentID := createHandlerTestAgent(t, "CHE-973 active merge", nil)
	setIssueAssigneeDirect(t, fx.child.ID, "agent", agentID)
	if _, err := testPool.Exec(context.Background(), `
		INSERT INTO agent_task_queue (agent_id, runtime_id, issue_id, status, priority, started_at)
		SELECT id, runtime_id, $2, 'running', 1, now() FROM agent WHERE id=$1`, agentID, fx.child.ID); err != nil {
		t.Fatalf("create active issue task: %v", err)
	}
	postSignedGitHubWebhook(t, fx.secret, buildMergedPRWebhookBody(fx.child.Identifier, 52711, "acme", "widget", installationID), "che973-merge-active")
	if got := dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id=$1 AND agent_id=$2 AND context->>'wakeup_system'='pr_merged'`, fx.child.ID, agentID); got != 0 {
		t.Fatalf("PR wake runs with active agent task = %d, want 0", got)
	}
}

func TestPRWakeSettingsDisableMergeAndCheckFailure(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	setWorkspacePRWakeSetting(t, "github_wake_on_pr_merge", false)
	setWorkspacePRWakeSetting(t, "github_wake_on_ci_failure", false)
	const installationID int64 = 527008
	fx := newPRMergeWebhookFixture(t, installationID)
	agentID := createHandlerTestAgent(t, "CHE-973 wake disabled", nil)
	setIssueAssigneeDirect(t, fx.child.ID, "agent", agentID)

	postSignedGitHubWebhook(t, fx.secret, buildMergedPRWebhookBody(fx.child.Identifier, 52708, "acme", "widget", installationID), "che973-merge-disabled")
	pr := linkedPRForWakeup(t, fx, installationID, 52709, "che973-head-x")
	if _, err := testPool.Exec(context.Background(), `UPDATE github_pull_request SET snapshot_head_sha=head_sha, checks_rollup_state='FAILURE' WHERE id=$1`, pr.ID); err != nil {
		t.Fatalf("set failing snapshot: %v", err)
	}
	testHandler.broadcastPRSnapshotApplied(context.Background(), pr.ID)
	if got := countPendingTasksForAgent(t, fx.child.ID, agentID); got != 0 {
		t.Fatalf("pending tasks with wake settings off = %d, want 0", got)
	}
}

func TestPRCheckFailureWakesOncePerHeadAndIgnoresSuccess(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	const installationID int64 = 527009
	fx := newPRMergeWebhookFixture(t, installationID)
	agentID := createHandlerTestAgent(t, "CHE-973 checks failure", nil)
	setIssueAssigneeDirect(t, fx.child.ID, "agent", agentID)
	pr := linkedPRForWakeup(t, fx, installationID, 52710, "che973-head-x")

	setSnapshot := func(head, rollup string) {
		t.Helper()
		if _, err := testPool.Exec(context.Background(), `UPDATE github_pull_request SET head_sha=$2, snapshot_head_sha=$2, checks_rollup_state=$3 WHERE id=$1`, pr.ID, head, rollup); err != nil {
			t.Fatalf("set %s snapshot: %v", rollup, err)
		}
		testHandler.broadcastPRSnapshotApplied(context.Background(), pr.ID)
	}
	setSnapshot("che973-head-x", "SUCCESS")
	if got := countPendingTasksForAgent(t, fx.child.ID, agentID); got != 0 {
		t.Fatalf("success enqueued %d pending tasks, want 0", got)
	}
	setSnapshot("che973-head-x", "FAILURE")
	if got := countPendingTasksForAgent(t, fx.child.ID, agentID); got != 1 {
		t.Fatalf("head X failure enqueued %d pending tasks, want 1", got)
	}
	var instruction string
	if err := testPool.QueryRow(context.Background(), `SELECT handoff_note FROM agent_task_queue WHERE issue_id=$1 AND agent_id=$2 AND context->>'wakeup_system'='pr_checks_failed'`, fx.child.ID, agentID).Scan(&instruction); err != nil {
		t.Fatalf("read check-failure wake instruction: %v", err)
	}
	for _, want := range []string{"PR #52710", "che973-head-x", "issue status when those checks finished was in_progress"} {
		if !strings.Contains(instruction, want) {
			t.Errorf("check-failure wake instruction %q does not contain %q", instruction, want)
		}
	}
	setSnapshot("che973-head-x", "FAILURE")
	if got := dbfx.Count(t, `SELECT count(*) FROM issue_wakeup_receipt r JOIN issue_wakeup w ON w.id=r.wakeup_id WHERE w.issue_id=$1 AND w.system_rule='pr_checks_failed' AND r.event_type='pr.checks_failed'`, fx.child.ID); got != 1 {
		t.Fatalf("head X failure receipts after replay = %d, want 1", got)
	}
	if _, err := testPool.Exec(context.Background(), `UPDATE agent_task_queue SET status='completed', completed_at=now() WHERE issue_id=$1 AND agent_id=$2`, fx.child.ID, agentID); err != nil {
		t.Fatalf("complete first wakeup: %v", err)
	}
	setSnapshot("che973-head-y", "ERROR")
	if got := dbfx.Count(t, `SELECT count(*) FROM issue_wakeup_receipt r JOIN issue_wakeup w ON w.id=r.wakeup_id WHERE w.issue_id=$1 AND w.system_rule='pr_checks_failed' AND r.event_type='pr.checks_failed'`, fx.child.ID); got != 2 {
		t.Fatalf("failure receipts for heads X and Y = %d, want 2", got)
	}
	if got := dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id=$1 AND agent_id=$2 AND context->>'wakeup_system'='pr_checks_failed'`, fx.child.ID, agentID); got != 2 {
		t.Fatalf("wake runs for heads X and Y = %d, want 2", got)
	}
	setSnapshot("che973-head-z", "SUCCESS")
	if got := dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id=$1 AND agent_id=$2 AND context->>'wakeup_system'='pr_checks_failed'`, fx.child.ID, agentID); got != 2 {
		t.Fatalf("success on head Z increased wake runs to %d, want 2", got)
	}
}
