package handler

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/integrations/ghsnapshot"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// CHE-1417 review round 2 (Sol's FAIL on 8a04f1036): a failed base
// invalidation must not be acknowledged. GitHub redelivers a failed delivery,
// and the redelivery must clear the sibling's stale CLEAN verdict.
func TestPRMergeBaseInvalidationFailureIsRedelivered(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	const installationID int64 = 1417201
	const sibling int32 = 1417301
	fx := newPRMergeWebhookFixture(t, installationID)
	now := time.Now().UTC()
	pr, err := testHandler.Queries.UpsertGitHubPullRequest(context.Background(), db.UpsertGitHubPullRequestParams{
		WorkspaceID: parseUUID(testWorkspaceID), InstallationID: installationID,
		RepoOwner: "acme", RepoName: "widget", PrNumber: sibling, Title: "unrelated sibling", State: "open",
		HtmlUrl:     fmt.Sprintf("https://github.com/acme/widget/pull/%d", sibling),
		PrCreatedAt: pgtype.Timestamptz{Time: now, Valid: true}, PrUpdatedAt: pgtype.Timestamptz{Time: now, Valid: true},
		HeadSha: "che1417-r2-sibling", ClearMergeableState: pgtype.Bool{Bool: true, Valid: true},
	})
	if err != nil {
		t.Fatalf("UpsertGitHubPullRequest: %v", err)
	}
	testPool.Exec(context.Background(), `UPDATE github_pull_request SET snapshot_base_ref='main', snapshot_head_sha=head_sha, snapshot_fetched_at=now(),
		api_mergeable='MERGEABLE', api_merge_state_status='CLEAN', checks_rollup_state='SUCCESS' WHERE id=$1`, pr.ID)

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

	// Sol's fault: the sibling's invalidation UPDATE raises.
	for _, stmt := range []string{
		`CREATE OR REPLACE FUNCTION che1417_fail_invalidation() RETURNS trigger LANGUAGE plpgsql AS $$
		 BEGIN IF OLD.pr_number = 1417301 AND OLD.api_mergeable = 'MERGEABLE' AND NEW.api_mergeable IS NULL
		 THEN RAISE EXCEPTION 'che1417 injected invalidation failure'; END IF; RETURN NEW; END $$`,
		`CREATE TRIGGER che1417_fail_invalidation BEFORE UPDATE ON github_pull_request FOR EACH ROW EXECUTE FUNCTION che1417_fail_invalidation()`,
	} {
		if _, err := testPool.Exec(context.Background(), stmt); err != nil {
			t.Fatalf("install failure trigger: %v", err)
		}
	}
	dropTrigger := func() {
		testPool.Exec(context.Background(), `DROP TRIGGER IF EXISTS che1417_fail_invalidation ON github_pull_request`)
		testPool.Exec(context.Background(), `DROP FUNCTION IF EXISTS che1417_fail_invalidation()`)
	}
	t.Cleanup(dropTrigger)

	payload := buildMergedPRWebhookBody(fx.child.Identifier, 1417300, "acme", "widget", installationID)
	payload["pull_request"].(map[string]any)["base"] = map[string]any{"ref": "main"}
	if w := postSignedGitHubCIEvent(t, fx.secret, "pull_request", payload); w.Code != http.StatusInternalServerError {
		t.Fatalf("failed base invalidation returned %d, want 500 so GitHub redelivers: %s", w.Code, w.Body.String())
	}
	staleState := func() pgtype.Text {
		var state pgtype.Text
		testPool.QueryRow(context.Background(), `SELECT api_merge_state_status FROM github_pull_request WHERE id=$1`, pr.ID).Scan(&state)
		return state
	}
	if state := staleState(); !state.Valid || state.String != "CLEAN" {
		t.Fatalf("failed invalidation changed the sibling's merge state to %+v", state)
	}

	dropTrigger()
	postSignedGitHubWebhook(t, fx.secret, payload, "che1417-r2-redelivery")
	if state := staleState(); state.Valid {
		t.Fatalf("redelivery left the sibling's stale merge state %q", state.String)
	}
}
