package handler

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/integrations/ghsnapshot"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// CHE-1417 review round 1 (Sol's FAIL on 6d658081a): regressions for each
// finding.

// ensureDoneStatus makes key a done-category status of the test workspace for
// the test's duration, unless the workspace already has it.
func ensureDoneStatus(t *testing.T, key string) {
	t.Helper()
	tag, err := testPool.Exec(context.Background(), `INSERT INTO issue_status (workspace_id, key, name, description, category, color, position)
		VALUES ($1, $2, $2, '', 'done', '#00aa00', 90) ON CONFLICT DO NOTHING`, testWorkspaceID, key)
	if err != nil {
		t.Fatalf("create status %s: %v", key, err)
	}
	if tag.RowsAffected() == 1 {
		t.Cleanup(func() {
			testPool.Exec(context.Background(), `DELETE FROM issue_status WHERE workspace_id=$1 AND key=$2`, testWorkspaceID, key)
		})
	}
}

func draftConversionAt(identifier string, prNumber int, installationID int64, headSHA, action string, at time.Time) map[string]any {
	body := buildDraftedPRWebhookBody(identifier, prNumber, installationID, headSHA)
	body["action"] = action
	pr := body["pull_request"].(map[string]any)
	pr["draft"] = action == "converted_to_draft"
	pr["updated_at"] = at.UTC().Format(time.RFC3339)
	return body
}

// Finding 1: a rejected verdict is a done-category status in this workspace,
// but it is not acceptance.
func TestPRDraftAfterRejectedVerdictDoesNotWake(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	for i, status := range []string{"changes_requested", "blockings_found"} {
		t.Run(status, func(t *testing.T) {
			installationID := int64(1417101 + i)
			prNumber := int32(141710 + i)
			fx := newPRMergeWebhookFixture(t, installationID)
			agentID := createHandlerTestAgent(t, "CHE-1417 rejected "+status, nil)
			setIssueAssigneeDirect(t, fx.child.ID, "agent", agentID)
			linkedPRForWakeup(t, fx, installationID, prNumber, "che1417-rejected-head")
			ensureDoneStatus(t, status)
			testPool.Exec(context.Background(), `UPDATE issue SET status=$2 WHERE id=$1`, fx.child.ID, status)

			postSignedGitHubWebhook(t, fx.secret, buildDraftedPRWebhookBody(fx.child.Identifier, int(prNumber), installationID, "che1417-rejected-head"), "che1417-rejected-"+status)
			if got := prRuleWakeups(t, fx.child.ID, agentID, "pr_needs_attention"); got != 0 {
				t.Fatalf("draft after %s queued %d attention wakeups, want 0", status, got)
			}
		})
	}
}

func TestPRDraftAfterAgentAcceptedWakes(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	const installationID int64 = 1417103
	const prNumber int32 = 141713
	fx := newPRMergeWebhookFixture(t, installationID)
	agentID := createHandlerTestAgent(t, "CHE-1417 agent accepted", nil)
	setIssueAssigneeDirect(t, fx.child.ID, "agent", agentID)
	linkedPRForWakeup(t, fx, installationID, prNumber, "che1417-accepted-head")
	ensureDoneStatus(t, "agent_accepted")
	testPool.Exec(context.Background(), `UPDATE issue SET status='agent_accepted' WHERE id=$1`, fx.child.ID)

	postSignedGitHubWebhook(t, fx.secret, buildDraftedPRWebhookBody(fx.child.Identifier, int(prNumber), installationID, "che1417-accepted-head"), "che1417-agent-accepted")
	if got := prRuleWakeups(t, fx.child.ID, agentID, "pr_needs_attention"); got != 1 {
		t.Fatalf("draft after agent_accepted queued %d attention wakeups, want 1", got)
	}
}

// Finding 2: the mirror commits the draft state before the wakeup is captured.
// A capture failure must stay recoverable by GitHub's redelivery.
func TestPRDraftWakeupSurvivesCaptureFailureAndRedelivery(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	const installationID int64 = 1417104
	const prNumber int32 = 141714
	fx := newPRMergeWebhookFixture(t, installationID)
	agentID := createHandlerTestAgent(t, "CHE-1417 draft redelivery", nil)
	setIssueAssigneeDirect(t, fx.child.ID, "agent", agentID)
	linkedPRForWakeup(t, fx, installationID, prNumber, "che1417-redeliver-head")
	testPool.Exec(context.Background(), `UPDATE issue SET status='done' WHERE id=$1`, fx.child.ID)

	// Fail receipt capture for this PR only: the ledger insert raises inside
	// the wakeup transaction, which rolls back.
	for _, stmt := range []string{
		`CREATE OR REPLACE FUNCTION che1417_fail_capture() RETURNS trigger LANGUAGE plpgsql AS $$
		 BEGIN IF NEW.event_key LIKE '%#141714:%' THEN RAISE EXCEPTION 'che1417 injected capture failure'; END IF; RETURN NEW; END $$`,
		`CREATE TRIGGER che1417_fail_capture BEFORE INSERT ON issue_wakeup_pr_event FOR EACH ROW EXECUTE FUNCTION che1417_fail_capture()`,
	} {
		if _, err := testPool.Exec(context.Background(), stmt); err != nil {
			t.Fatalf("install failure trigger: %v", err)
		}
	}
	dropTrigger := func() {
		testPool.Exec(context.Background(), `DROP TRIGGER IF EXISTS che1417_fail_capture ON issue_wakeup_pr_event`)
		testPool.Exec(context.Background(), `DROP FUNCTION IF EXISTS che1417_fail_capture()`)
	}
	t.Cleanup(dropTrigger)

	body := draftConversionAt(fx.child.Identifier, int(prNumber), installationID, "che1417-redeliver-head", "converted_to_draft", time.Now().Add(time.Minute))
	if w := postSignedGitHubCIEvent(t, fx.secret, "pull_request", body); w.Code != http.StatusInternalServerError {
		t.Fatalf("failed capture returned %d, want 500 so GitHub redelivers: %s", w.Code, w.Body.String())
	}
	if got := prRuleWakeups(t, fx.child.ID, agentID, "pr_needs_attention"); got != 0 {
		t.Fatalf("failed capture queued %d attention wakeups, want 0", got)
	}
	dropTrigger()

	postSignedGitHubWebhook(t, fx.secret, body, "che1417-redeliver-2")
	if got := prRuleWakeups(t, fx.child.ID, agentID, "pr_needs_attention"); got != 1 {
		t.Fatalf("redelivery after a failed capture queued %d attention wakeups, want 1", got)
	}
	postSignedGitHubWebhook(t, fx.secret, body, "che1417-redeliver-3")
	if got := prRuleWakeups(t, fx.child.ID, agentID, "pr_needs_attention"); got != 1 {
		t.Fatalf("second redelivery queued %d attention wakeups in total, want 1", got)
	}
}

// Finding 3: draft, ready, then draft again at the same head is a second
// conversion, not a redelivery.
func TestPRRepeatDraftConversionAtSameHeadWakesAgain(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	const installationID int64 = 1417105
	const prNumber int32 = 141715
	const head = "che1417-repeat-head"
	fx := newPRMergeWebhookFixture(t, installationID)
	agentID := createHandlerTestAgent(t, "CHE-1417 repeat draft", nil)
	setIssueAssigneeDirect(t, fx.child.ID, "agent", agentID)
	linkedPRForWakeup(t, fx, installationID, prNumber, head)
	testPool.Exec(context.Background(), `UPDATE issue SET status='done' WHERE id=$1`, fx.child.ID)
	count := func() int { return prRuleWakeups(t, fx.child.ID, agentID, "pr_needs_attention") }
	finish := func() {
		testPool.Exec(context.Background(), `UPDATE agent_task_queue SET status='completed',completed_at=clock_timestamp() WHERE issue_id=$1 AND status IN ('queued','dispatched','running')`, fx.child.ID)
	}
	base := time.Now().Add(time.Minute)

	first := draftConversionAt(fx.child.Identifier, int(prNumber), installationID, head, "converted_to_draft", base)
	postSignedGitHubWebhook(t, fx.secret, first, "che1417-repeat-1")
	if got := count(); got != 1 {
		t.Fatalf("first draft conversion queued %d attention wakeups, want 1", got)
	}
	finish()
	postSignedGitHubWebhook(t, fx.secret, draftConversionAt(fx.child.Identifier, int(prNumber), installationID, head, "ready_for_review", base.Add(time.Minute)), "che1417-repeat-2")
	second := draftConversionAt(fx.child.Identifier, int(prNumber), installationID, head, "converted_to_draft", base.Add(2*time.Minute))
	postSignedGitHubWebhook(t, fx.secret, second, "che1417-repeat-3")
	if got := count(); got != 2 {
		t.Fatalf("second draft conversion at the same head queued %d attention wakeups in total, want 2", got)
	}
	finish()
	postSignedGitHubWebhook(t, fx.secret, second, "che1417-repeat-4")
	if got := count(); got != 2 {
		t.Fatalf("redelivered second conversion queued %d attention wakeups in total, want 2", got)
	}
}

// Finding 4: every open PR on a moved base is refreshed, not only the newest
// 100. A PR whose last snapshot was CLEAN must not keep that verdict.
func TestPRMergeRefreshesEveryOpenPROnTheBase(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	const installationID int64 = 1417106
	const eligible = 101
	const firstNumber = 1417200
	fx := newPRMergeWebhookFixture(t, installationID)
	now := time.Now().UTC()
	for i := 0; i < eligible; i++ {
		number := int32(firstNumber + i)
		pr, err := testHandler.Queries.UpsertGitHubPullRequest(context.Background(), db.UpsertGitHubPullRequestParams{
			WorkspaceID: parseUUID(testWorkspaceID), InstallationID: installationID,
			RepoOwner: "acme", RepoName: "widget", PrNumber: number, Title: "unrelated " + strconv.Itoa(int(number)), State: "open",
			HtmlUrl:     fmt.Sprintf("https://github.com/acme/widget/pull/%d", number),
			PrCreatedAt: pgtype.Timestamptz{Time: now, Valid: true}, PrUpdatedAt: pgtype.Timestamptz{Time: now, Valid: true},
			HeadSha: fmt.Sprintf("che1417-cap-%d", number), ClearMergeableState: pgtype.Bool{Bool: true, Valid: true},
		})
		if err != nil {
			t.Fatalf("UpsertGitHubPullRequest #%d: %v", number, err)
		}
		testPool.Exec(context.Background(), `UPDATE github_pull_request SET snapshot_base_ref='main', snapshot_head_sha=head_sha, snapshot_fetched_at=now(),
			api_mergeable='MERGEABLE', api_merge_state_status='CLEAN', checks_rollup_state='SUCCESS' WHERE id=$1`, pr.ID)
	}

	var mu sync.Mutex
	fetched := map[int32]bool{}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("GITHUB_APP_ID", "123")
	t.Setenv("GITHUB_APP_PRIVATE_KEY", string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})))
	client, err := ghsnapshot.NewClientFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	refresh := ghsnapshot.NewManagerWithFetcher(client, testHandler.Queries, testPool,
		func(_ context.Context, _ *ghsnapshot.Client, _ int64, _, _ string, number int32) (*ghsnapshot.PRSnapshot, error) {
			mu.Lock()
			fetched[number] = true
			mu.Unlock()
			return nil, fmt.Errorf("test fetch for #%d", number)
		}, nil)
	previous := testHandler.PRRefresh
	testHandler.PRRefresh = refresh
	ctx, cancel := context.WithCancel(context.Background())
	refresh.Start(ctx)
	t.Cleanup(func() {
		cancel()
		testHandler.PRRefresh = previous
	})

	payload := buildMergedPRWebhookBody(fx.child.Identifier, 1417199, "acme", "widget", installationID)
	payload["pull_request"].(map[string]any)["base"] = map[string]any{"ref": "main"}
	postSignedGitHubWebhook(t, fx.secret, payload, "che1417-cap-merge")

	// The oldest PR is the 101st by number. Either it is refreshed now, or its
	// stale CLEAN verdict is cleared so the snapshot sweep refreshes it.
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		done := fetched[firstNumber]
		mu.Unlock()
		var state pgtype.Text
		testPool.QueryRow(context.Background(), `SELECT api_merge_state_status FROM github_pull_request WHERE workspace_id=$1 AND pr_number=$2`, testWorkspaceID, firstNumber).Scan(&state)
		if done || !state.Valid {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("merge left the 101st open PR #%d on main unrefreshed with stale merge state %q", firstNumber, state.String)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
