package handler

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/integrations/ghsnapshot"
)

// CHE-1417 (CHE-982 G6): owners hear about a linked PR that can no longer
// merge as is, a merge into any branch, and a PR sent back to draft after its
// issue was accepted — without a person reporting it.

// prAttentionNote returns the newest wakeup prompt of one system rule.
func prAttentionNote(t *testing.T, issueID, agentID, rule string) string {
	t.Helper()
	var note string
	if err := testPool.QueryRow(context.Background(), `SELECT handoff_note FROM agent_task_queue WHERE issue_id=$1 AND agent_id=$2 AND context->>'wakeup_system'=$3 ORDER BY created_at DESC LIMIT 1`, issueID, agentID, rule).Scan(&note); err != nil {
		t.Fatalf("read %s wakeup instruction: %v", rule, err)
	}
	return note
}

func prRuleWakeups(t *testing.T, issueID, agentID, rule string) int {
	t.Helper()
	return dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE issue_id=$1 AND agent_id=$2 AND context->>'wakeup_system'=$3`, issueID, agentID, rule)
}

// startSnapshotRefresh swaps in a refresh manager whose fetcher returns
// whatever *snap holds, and returns a function that refreshes the PR once and
// waits for the applied callback.
func startSnapshotRefresh(t *testing.T, installationID int64, number int32, snap func() ghsnapshot.PRSnapshot) func() {
	t.Helper()
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
	applied := make(chan struct{}, 1)
	refresh := ghsnapshot.NewManagerWithFetcher(client, testHandler.Queries, testPool,
		func(_ context.Context, _ *ghsnapshot.Client, _ int64, _, _ string, _ int32) (*ghsnapshot.PRSnapshot, error) {
			s := snap()
			return &s, nil
		},
		func(ctx context.Context, prID pgtype.UUID) {
			testHandler.broadcastPRSnapshotApplied(ctx, prID)
			applied <- struct{}{}
		},
	)
	previous := testHandler.PRRefresh
	testHandler.PRRefresh = refresh
	ctx, cancel := context.WithCancel(context.Background())
	refresh.Start(ctx)
	t.Cleanup(func() {
		cancel()
		testHandler.PRRefresh = previous
	})
	return func() {
		t.Helper()
		refresh.Enqueue(installationID, "acme", "widget", number)
		select {
		case <-applied:
		case <-time.After(5 * time.Second):
			t.Fatal("snapshot refresh did not apply")
		}
	}
}

// Trigger 1: DIRTY or BEHIND wakes once per head and base pair; a new base
// (retarget) or a new head is a new pair.
func TestPRMergeabilityWakesOwnerOncePerHeadAndBase(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	const installationID int64 = 1417001
	const prNumber int32 = 141701
	fx := newPRMergeWebhookFixture(t, installationID)
	agentID := createHandlerTestAgent(t, "CHE-1417 mergeability", nil)
	setIssueAssigneeDirect(t, fx.child.ID, "agent", agentID)
	linkedPRForWakeup(t, fx, installationID, prNumber, "che1417-head-a")
	setWorkspacePRWakeSetting(t, "github_enabled", true)

	var mu sync.Mutex
	current := ghsnapshot.PRSnapshot{HeadSHA: "che1417-head-a", BaseRef: "main", Mergeable: "MERGEABLE", MergeStateStatus: "CLEAN", RollupState: "SUCCESS", HasChecks: true}
	set := func(merge, base string) {
		mu.Lock()
		defer mu.Unlock()
		current.MergeStateStatus, current.BaseRef = merge, base
		if merge == "DIRTY" {
			current.Mergeable = "CONFLICTING"
		} else {
			current.Mergeable = "MERGEABLE"
		}
	}
	refresh := startSnapshotRefresh(t, installationID, prNumber, func() ghsnapshot.PRSnapshot {
		mu.Lock()
		defer mu.Unlock()
		return current
	})
	count := func() int { return prRuleWakeups(t, fx.child.ID, agentID, "pr_needs_attention") }
	finish := func() {
		testPool.Exec(context.Background(), `UPDATE agent_task_queue SET status='completed',completed_at=clock_timestamp() WHERE issue_id=$1 AND status IN ('queued','dispatched','running')`, fx.child.ID)
	}

	refresh()
	if got := count(); got != 0 {
		t.Fatalf("CLEAN snapshot queued %d attention wakeups, want 0", got)
	}

	set("DIRTY", "main")
	refresh()
	if got := count(); got != 1 {
		t.Fatalf("DIRTY snapshot queued %d attention wakeups, want 1", got)
	}
	note := prAttentionNote(t, fx.child.ID, agentID, "pr_needs_attention")
	for _, want := range []string{"#141701", "DIRTY", "che1417-head-a", "main"} {
		if !strings.Contains(note, want) {
			t.Errorf("mergeability instruction %q does not contain %q", note, want)
		}
	}
	finish()

	refresh()
	set("BEHIND", "main")
	refresh()
	if got := count(); got != 1 {
		t.Fatalf("same head and base woke again: %d attention wakeups, want 1", got)
	}

	set("BEHIND", "stack/layer-1")
	refresh()
	if got := count(); got != 2 {
		t.Fatalf("base change queued %d attention wakeups in total, want 2", got)
	}
	if note := prAttentionNote(t, fx.child.ID, agentID, "pr_needs_attention"); !strings.Contains(note, "BEHIND") || !strings.Contains(note, "stack/layer-1") {
		t.Errorf("retargeted mergeability instruction %q does not name BEHIND on stack/layer-1", note)
	}
}

func TestPRMergeabilityRespectsWorkspaceOptOut(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	const installationID int64 = 1417002
	const prNumber int32 = 141702
	fx := newPRMergeWebhookFixture(t, installationID)
	agentID := createHandlerTestAgent(t, "CHE-1417 mergeability opt-out", nil)
	setIssueAssigneeDirect(t, fx.child.ID, "agent", agentID)
	linkedPRForWakeup(t, fx, installationID, prNumber, "che1417-head-optout")
	setWorkspacePRWakeSetting(t, "github_enabled", true)
	setWorkspacePRWakeSetting(t, "github_wake_on_pr_attention", false)
	refresh := startSnapshotRefresh(t, installationID, prNumber, func() ghsnapshot.PRSnapshot {
		return ghsnapshot.PRSnapshot{HeadSHA: "che1417-head-optout", BaseRef: "main", Mergeable: "CONFLICTING", MergeStateStatus: "DIRTY"}
	})
	refresh()
	if got := prRuleWakeups(t, fx.child.ID, agentID, "pr_needs_attention"); got != 0 {
		t.Fatalf("opted-out workspace queued %d attention wakeups, want 0", got)
	}
}

// A merge elsewhere moves the base: the open PRs on that base are refreshed so
// a conflict it caused is seen without anyone opening the page.
func TestPRMergeRefreshesOpenPRsOnTheSameBase(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	const installationID int64 = 1417003
	fx := newPRMergeWebhookFixture(t, installationID)
	sibling := linkedPRForWakeup(t, fx, installationID, 141731, "che1417-sibling-head")
	testPool.Exec(context.Background(), `UPDATE github_pull_request SET snapshot_base_ref='main' WHERE id=$1`, sibling.ID)

	var mu sync.Mutex
	fetched := map[int32]int{}
	refreshed := make(chan int32, 4)
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
			fetched[number]++
			mu.Unlock()
			refreshed <- number
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

	payload := buildMergedPRWebhookBody(fx.child.Identifier, 141730, "acme", "widget", installationID)
	payload["pull_request"].(map[string]any)["base"] = map[string]any{"ref": "main"}
	postSignedGitHubWebhook(t, fx.secret, payload, "che1417-sibling-merge")

	deadline := time.After(5 * time.Second)
	for {
		mu.Lock()
		done := fetched[141731] > 0
		mu.Unlock()
		if done {
			return
		}
		select {
		case <-refreshed:
		case <-deadline:
			t.Fatalf("merging #141730 into main did not refresh open sibling #141731 on main (fetched %v)", fetched)
		}
	}
}

// Trigger 2: a merge into any branch names that branch; a merge into a
// non-default branch is a delivered stack layer, which GitHub's closing
// keywords never act on.
func TestPRMergeIntoStackBranchNamesBranchAsLayerDelivered(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	const installationID int64 = 1417004
	fx := newPRMergeWebhookFixture(t, installationID)
	agentID := createHandlerTestAgent(t, "CHE-1417 stack merge", nil)
	setIssueAssigneeDirect(t, fx.child.ID, "agent", agentID)

	payload := buildMergedPRWebhookBody(fx.child.Identifier, 141740, "acme", "widget", installationID)
	payload["pull_request"].(map[string]any)["base"] = map[string]any{"ref": "stack/layer-1"}
	payload["repository"].(map[string]any)["default_branch"] = "main"
	postSignedGitHubWebhook(t, fx.secret, payload, "che1417-stack-merge")

	if got := prRuleWakeups(t, fx.child.ID, agentID, "pr_merged"); got != 1 {
		t.Fatalf("stack-branch merge queued %d merge wakeups, want 1", got)
	}
	note := prAttentionNote(t, fx.child.ID, agentID, "pr_merged")
	for _, want := range []string{"#141740", "stack/layer-1", "layer delivered"} {
		if !strings.Contains(note, want) {
			t.Errorf("stack merge instruction %q does not contain %q", note, want)
		}
	}
}

func TestPRMergeIntoDefaultBranchNamesBranch(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	const installationID int64 = 1417005
	fx := newPRMergeWebhookFixture(t, installationID)
	agentID := createHandlerTestAgent(t, "CHE-1417 default merge", nil)
	setIssueAssigneeDirect(t, fx.child.ID, "agent", agentID)

	payload := buildMergedPRWebhookBody(fx.child.Identifier, 141750, "acme", "widget", installationID)
	payload["pull_request"].(map[string]any)["base"] = map[string]any{"ref": "main"}
	payload["repository"].(map[string]any)["default_branch"] = "main"
	postSignedGitHubWebhook(t, fx.secret, payload, "che1417-default-merge")

	note := prAttentionNote(t, fx.child.ID, agentID, "pr_merged")
	if !strings.Contains(note, "into main") {
		t.Errorf("default-branch merge instruction %q does not name main", note)
	}
	if strings.Contains(note, "layer delivered") {
		t.Errorf("default-branch merge instruction %q calls it a stack layer", note)
	}
}

func buildDraftedPRWebhookBody(issueIdentifier string, prNumber int, installationID int64, headSHA string) map[string]any {
	body := buildMergedPRWebhookBody(issueIdentifier, prNumber, "acme", "widget", installationID)
	body["action"] = "converted_to_draft"
	pr := body["pull_request"].(map[string]any)
	pr["state"], pr["draft"], pr["merged"], pr["merged_at"], pr["closed_at"], pr["merge_commit_sha"] = "open", true, false, "", "", ""
	// Newer than the mirrored row, or the upsert treats it as a stale event.
	pr["updated_at"] = time.Now().UTC().Add(time.Minute).Format(time.RFC3339)
	pr["head"] = map[string]any{"ref": "fix/ship-it", "sha": headSHA}
	pr["base"] = map[string]any{"ref": "main"}
	return body
}

// Trigger 3: a PR sent back to draft after its issue was accepted (the issue
// sits in a done-category status) wakes the owner to re-ready or explain.
func TestPRReturnedToDraftAfterAcceptanceWakesOwner(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	const installationID int64 = 1417006
	const prNumber int32 = 141760
	fx := newPRMergeWebhookFixture(t, installationID)
	agentID := createHandlerTestAgent(t, "CHE-1417 draft after accept", nil)
	setIssueAssigneeDirect(t, fx.child.ID, "agent", agentID)
	linkedPRForWakeup(t, fx, installationID, prNumber, "che1417-draft-head")
	testPool.Exec(context.Background(), `UPDATE issue SET status='done' WHERE id=$1`, fx.child.ID)

	postSignedGitHubWebhook(t, fx.secret, buildDraftedPRWebhookBody(fx.child.Identifier, int(prNumber), installationID, "che1417-draft-head"), "che1417-draft-1")
	if got := prRuleWakeups(t, fx.child.ID, agentID, "pr_needs_attention"); got != 1 {
		t.Fatalf("draft after acceptance queued %d attention wakeups, want 1", got)
	}
	note := prAttentionNote(t, fx.child.ID, agentID, "pr_needs_attention")
	for _, want := range []string{"#141760", "draft", "re-ready or explain"} {
		if !strings.Contains(note, want) {
			t.Errorf("draft instruction %q does not contain %q", note, want)
		}
	}
	postSignedGitHubWebhook(t, fx.secret, buildDraftedPRWebhookBody(fx.child.Identifier, int(prNumber), installationID, "che1417-draft-head"), "che1417-draft-2")
	if got := prRuleWakeups(t, fx.child.ID, agentID, "pr_needs_attention"); got != 1 {
		t.Fatalf("redelivered draft event queued %d attention wakeups in total, want 1", got)
	}
}

func TestPRReturnedToDraftBeforeAcceptanceDoesNotWake(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	const installationID int64 = 1417007
	const prNumber int32 = 141770
	fx := newPRMergeWebhookFixture(t, installationID)
	agentID := createHandlerTestAgent(t, "CHE-1417 draft in progress", nil)
	setIssueAssigneeDirect(t, fx.child.ID, "agent", agentID)
	linkedPRForWakeup(t, fx, installationID, prNumber, "che1417-wip-head")

	postSignedGitHubWebhook(t, fx.secret, buildDraftedPRWebhookBody(fx.child.Identifier, int(prNumber), installationID, "che1417-wip-head"), "che1417-wip-1")
	if got := prRuleWakeups(t, fx.child.ID, agentID, "pr_needs_attention"); got != 0 {
		t.Fatalf("draft on an in-progress issue queued %d attention wakeups, want 0", got)
	}
}
