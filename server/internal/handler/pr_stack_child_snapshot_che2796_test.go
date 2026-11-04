package handler

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/integrations/ghsnapshot"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// CHE-2796: a stacked child PR (base = its parent's head branch) keeps a
// decided CLEAN snapshot when the parent merges, because the merge handler only
// invalidated PRs on the merged PR's own base. GitHub then retargets the child
// to the default branch and it can turn CONFLICTING with no snapshot refresh.
func seedDecidedCleanPR(t *testing.T, workspaceID pgtype.UUID, installationID int64, number int32, baseRef string) pgtype.UUID {
	t.Helper()
	now := time.Now().UTC()
	pr, err := testHandler.Queries.UpsertGitHubPullRequest(context.Background(), db.UpsertGitHubPullRequestParams{
		WorkspaceID: workspaceID, InstallationID: installationID,
		RepoOwner: "acme", RepoName: "widget", PrNumber: number, Title: "stack child", State: "open",
		HtmlUrl:     fmt.Sprintf("https://github.com/acme/widget/pull/%d", number),
		PrCreatedAt: pgtype.Timestamptz{Time: now, Valid: true}, PrUpdatedAt: pgtype.Timestamptz{Time: now, Valid: true},
		HeadSha: fmt.Sprintf("che2796-%d", number), ClearMergeableState: pgtype.Bool{Bool: true, Valid: true},
	})
	if err != nil {
		t.Fatalf("UpsertGitHubPullRequest: %v", err)
	}
	if _, err := testPool.Exec(context.Background(), `UPDATE github_pull_request SET snapshot_base_ref=$2, snapshot_head_sha=head_sha, snapshot_fetched_at=now(),
		api_mergeable='MERGEABLE', api_merge_state_status='CLEAN', checks_rollup_state='SUCCESS' WHERE id=$1`, pr.ID, baseRef); err != nil {
		t.Fatalf("seed snapshot: %v", err)
	}
	return pr.ID
}

// enableRefreshWithFailingFetch turns the refresh pipeline on so the merge
// handler's invalidation runs; every fetch fails, leaving the cleared state.
func enableRefreshWithFailingFetch(t *testing.T) {
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
	refresh := ghsnapshot.NewManagerWithFetcher(client, testHandler.Queries, testPool,
		func(_ context.Context, _ *ghsnapshot.Client, _ int64, _, _ string, number int32) (*ghsnapshot.PRSnapshot, error) {
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
}

func storedMergeState(t *testing.T, id pgtype.UUID) pgtype.Text {
	t.Helper()
	var state pgtype.Text
	if err := testPool.QueryRow(context.Background(), `SELECT api_merge_state_status FROM github_pull_request WHERE id=$1`, id).Scan(&state); err != nil {
		t.Fatalf("read merge state: %v", err)
	}
	return state
}

func TestParentMergeInvalidatesStackChildSnapshot(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	const installationID int64 = 2796001
	fx := newPRMergeWebhookFixture(t, installationID)
	enableRefreshWithFailingFetch(t)
	child := seedDecidedCleanPR(t, parseUUID(testWorkspaceID), installationID, 2796301, "fix/ship-it")

	payload := buildMergedPRWebhookBody(fx.child.Identifier, 2796300, "acme", "widget", installationID)
	payload["pull_request"].(map[string]any)["base"] = map[string]any{"ref": "main"}
	postSignedGitHubWebhook(t, fx.secret, payload, "che2796-parent-merge")

	if state := storedMergeState(t, child); state.Valid {
		t.Fatalf("stack child kept merge state %q after its parent merged", state.String)
	}
}

func TestRetargetEditInvalidatesAPIMergeState(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	const installationID int64 = 2796002
	fx := newPRMergeWebhookFixture(t, installationID)
	enableRefreshWithFailingFetch(t)
	child := seedDecidedCleanPR(t, parseUUID(testWorkspaceID), installationID, 2796302, "fix/ship-it")

	payload := buildMergedPRWebhookBody(fx.child.Identifier, 2796302, "acme", "widget", installationID)
	payload["action"] = "edited"
	pr := payload["pull_request"].(map[string]any)
	pr["state"], pr["merged"], pr["merged_at"], pr["closed_at"] = "open", false, nil, nil
	pr["updated_at"] = time.Now().UTC().Add(time.Minute).Format(time.RFC3339)
	pr["base"] = map[string]any{"ref": "main"}
	payload["changes"] = map[string]any{"base": map[string]any{"ref": map[string]any{"from": "fix/ship-it"}}}
	postSignedGitHubWebhook(t, fx.secret, payload, "che2796-retarget")

	if state := storedMergeState(t, child); state.Valid {
		t.Fatalf("retargeted PR kept merge state %q", state.String)
	}
}

// A GitHub-disabled workspace sharing the installation keeps its stored verdict:
// the refresh pipeline excludes it, so a cleared row could never be repaired.
func TestRetargetEditSkipsGitHubDisabledWorkspace(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	const installationID int64 = 2796003
	disabled := bindSecondWorkspaceForTest(t, "che2796-disabled", "C96", installationID)
	fx := newPRMergeWebhookFixture(t, installationID)
	enableRefreshWithFailingFetch(t)
	if _, err := testPool.Exec(context.Background(), `UPDATE workspace SET settings = '{"github_enabled": false}' WHERE id = $1`, disabled); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM github_pull_request WHERE workspace_id = $1`, disabled)
	})
	enabledPR := seedDecidedCleanPR(t, parseUUID(testWorkspaceID), installationID, 2796303, "fix/ship-it")
	disabledPR := seedDecidedCleanPR(t, disabled, installationID, 2796303, "fix/ship-it")

	payload := buildMergedPRWebhookBody(fx.child.Identifier, 2796303, "acme", "widget", installationID)
	payload["action"] = "edited"
	pr := payload["pull_request"].(map[string]any)
	pr["state"], pr["merged"], pr["merged_at"], pr["closed_at"] = "open", false, nil, nil
	pr["updated_at"] = time.Now().UTC().Add(time.Minute).Format(time.RFC3339)
	pr["base"] = map[string]any{"ref": "main"}
	payload["changes"] = map[string]any{"base": map[string]any{"ref": map[string]any{"from": "fix/ship-it"}}}
	postSignedGitHubWebhook(t, fx.secret, payload, "che2796-retarget-disabled")

	if state := storedMergeState(t, enabledPR); state.Valid {
		t.Fatalf("enabled workspace kept merge state %q", state.String)
	}
	if state := storedMergeState(t, disabledPR); state.String != "CLEAN" {
		t.Fatalf("GitHub-disabled workspace merge state = %q, want CLEAN", state.String)
	}
}
