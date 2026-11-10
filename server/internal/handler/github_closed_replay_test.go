package handler

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// failOnceDB fails the first query whose SQL carries marker, like a database
// error after the mirror transaction committed.
type failOnceDB struct {
	db.DBTX
	marker string
	failed bool
}

func (f *failOnceDB) QueryRow(ctx context.Context, query string, args ...any) pgx.Row {
	if !f.failed && strings.Contains(query, f.marker) {
		f.failed = true
		return errRow{err: errors.New("injected failure")}
	}
	return f.DBTX.QueryRow(ctx, query, args...)
}

func (f *failOnceDB) Query(ctx context.Context, query string, args ...any) (pgx.Rows, error) {
	if !f.failed && strings.Contains(query, f.marker) {
		f.failed = true
		return nil, errors.New("injected failure")
	}
	return f.DBTX.Query(ctx, query, args...)
}

// CHE-3402: the pull_request "closed" delivery for a merged PR never landed,
// so the row stayed open. The refresh pipeline reports the PR closed and the
// handler replays GitHub's current PR through the normal webhook path.
func TestGitHubReplayClosedPullRequestRecordsMerge(t *testing.T) {
	for _, tc := range []struct {
		name string
		head map[string]any
	}{
		{"same repository", map[string]any{"id": 424242, "full_name": "acme/widget", "fork": false}},
		{"fork", map[string]any{"id": 20, "full_name": "outsider/widget", "fork": true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newGitHubReviewFixture(t)
			var prID string
			dbfx.QueryRow(t, `SELECT id FROM github_pull_request WHERE workspace_id=$1 AND pr_number=2019 AND state='open'`, testWorkspaceID).Scan(&prID)
			mergedAt := time.Now().UTC().Truncate(time.Second)
			pull := githubForkGatePR(t, fixture, tc.head)
			pull["state"], pull["merged"] = "closed", true
			pull["merged_at"], pull["closed_at"] = mergedAt.Format(time.RFC3339), mergedAt.Format(time.RFC3339)
			pull["merge_commit_sha"] = "merge-sha"
			pull["base"] = map[string]any{"ref": "main", "sha": "base-sha", "repo": map[string]any{
				"id": 424242, "name": "widget", "full_name": "acme/widget", "default_branch": "main", "owner": map[string]any{"login": "acme"},
			}}
			raw, err := json.Marshal(pull)
			if err != nil {
				t.Fatal(err)
			}
			handler := *fixture.handler
			handler.fetchPullRequestJSON = func(_ context.Context, installationID int64, owner, repo string, number int32) (json.RawMessage, error) {
				if installationID != 2019001 || owner != "acme" || repo != "widget" || number != 2019 {
					t.Errorf("fetched %d %s/%s#%d", installationID, owner, repo, number)
				}
				return raw, nil
			}
			if err := handler.replayClosedPullRequest(context.Background(), 2019001, "acme", "widget", 2019); err != nil {
				t.Fatal(err)
			}
			var state string
			var storedMergedAt *time.Time
			dbfx.QueryRow(t, `SELECT state, merged_at FROM github_pull_request WHERE id=$1`, prID).Scan(&state, &storedMergedAt)
			if state != "merged" || storedMergedAt == nil || !storedMergedAt.Equal(mergedAt) {
				t.Fatalf("row state=%q merged_at=%v, want merged at %v", state, storedMergedAt, mergedAt)
			}
		})
	}
}

// A PR whose base repository is not the mirrored row's repository is never
// replayed into that row.
func TestGitHubReplayClosedPullRequestRejectsOtherBaseRepository(t *testing.T) {
	fixture := newGitHubReviewFixture(t)
	pull := githubForkGatePR(t, fixture, map[string]any{"id": 424242, "full_name": "acme/widget", "fork": false})
	pull["state"], pull["merged"] = "closed", true
	pull["base"] = map[string]any{"ref": "main", "repo": map[string]any{
		"id": 7, "name": "other", "full_name": "acme/other", "owner": map[string]any{"login": "acme"},
	}}
	raw, _ := json.Marshal(pull)
	handler := *fixture.handler
	handler.fetchPullRequestJSON = func(context.Context, int64, string, string, int32) (json.RawMessage, error) { return raw, nil }
	if err := handler.replayClosedPullRequest(context.Background(), 2019001, "acme", "widget", 2019); err == nil {
		t.Fatal("replayed a PR from another base repository")
	}
	if count := dbfx.Count(t, `SELECT count(*) FROM github_pull_request WHERE workspace_id=$1 AND pr_number=2019 AND state='open'`, testWorkspaceID); count != 1 {
		t.Fatalf("open rows = %d, want 1", count)
	}
}

// A failure after the mirror commit (here: the lifecycle step) leaves the row
// merged, so the refresh no longer reports it. The replay stays owed until it
// finishes, and a retry completes it.
func TestGitHubReplayClosedPullRequestStaysOwedUntilItFinishes(t *testing.T) {
	for _, tc := range []struct {
		name   string
		inject func(t *testing.T, handler *Handler, failing *failOnceDB)
		marker string
	}{
		{"lifecycle", func(t *testing.T, _ *Handler, failing *failOnceDB) {
			tasks := testHandler.TaskService
			original := tasks.Queries
			tasks.Queries = db.New(failing)
			t.Cleanup(func() { tasks.Queries = original })
		}, "-- name: ListIssueIDsForPR "},
		{"auto-completion", func(_ *testing.T, handler *Handler, failing *failOnceDB) {
			handler.Queries = db.New(failing)
		}, "-- name: GetIssueInWorkspace "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			testReplayStaysOwedUntilItFinishes(t, tc.inject, tc.marker)
		})
	}
}

func testReplayStaysOwedUntilItFinishes(t *testing.T, inject func(*testing.T, *Handler, *failOnceDB), marker string) {
	fixture := newGitHubReviewFixture(t)
	pull := githubForkGatePR(t, fixture, map[string]any{"id": 424242, "full_name": "acme/widget", "fork": false})
	mergedAt := time.Now().UTC().Truncate(time.Second).Format(time.RFC3339)
	pull["state"], pull["merged"], pull["merged_at"], pull["closed_at"] = "closed", true, mergedAt, mergedAt
	pull["base"] = map[string]any{"ref": "main", "sha": "base-sha", "repo": map[string]any{
		"id": 424242, "name": "widget", "full_name": "acme/widget", "default_branch": "main", "owner": map[string]any{"login": "acme"},
	}}
	raw, err := json.Marshal(pull)
	if err != nil {
		t.Fatal(err)
	}
	handler := *fixture.handler
	handler.fetchPullRequestJSON = func(context.Context, int64, string, string, int32) (json.RawMessage, error) { return raw, nil }
	dbfx.Cleanup(t, `DELETE FROM github_pr_closed_replay WHERE repo_owner='acme' AND repo_name='widget' AND pr_number=2019`)
	owed := func() int {
		return dbfx.Count(t, `SELECT count(*) FROM github_pr_closed_replay WHERE repo_owner='acme' AND repo_name='widget' AND pr_number=2019`)
	}

	failing := &failOnceDB{DBTX: testPool, marker: marker}
	inject(t, &handler, failing)
	if err := handler.replayClosedPullRequest(context.Background(), 2019001, "acme", "widget", 2019); err == nil {
		t.Fatal("replay hid the failure")
	}
	if !failing.failed {
		t.Fatalf("injected failure on %q never ran", marker)
	}
	var state string
	dbfx.QueryRow(t, `SELECT state FROM github_pull_request WHERE workspace_id=$1 AND pr_number=2019`, testWorkspaceID).Scan(&state)
	if state != "merged" {
		t.Fatalf("row state = %q, want merged (committed before the failure)", state)
	}
	if got := owed(); got != 1 {
		t.Fatalf("owed replays after failure = %d, want 1", got)
	}

	if err := handler.replayClosedPullRequest(context.Background(), 2019001, "acme", "widget", 2019); err != nil {
		t.Fatal(err)
	}
	if status := issueStatusForTest(t, fixture.issueID); status != "done" {
		t.Fatalf("linked issue status after retry = %q, want done (auto-completion owed by the merge)", status)
	}
	if got := owed(); got != 0 {
		t.Fatalf("owed replays after retry = %d, want 0", got)
	}
}

// A deleted workspace leaves no route for its installation address. The
// replay marker has no workspace_id, so the replay must finish as a no-op and
// clear it instead of staying owed forever.
func TestGitHubReplayClosedPullRequestClearsMarkerWithoutRoutedWorkspace(t *testing.T) {
	fixture := newGitHubReviewFixture(t)
	pull := githubForkGatePR(t, fixture, map[string]any{"id": 424242, "full_name": "acme/widget", "fork": false})
	pull["state"], pull["merged"] = "closed", true
	pull["base"] = map[string]any{"ref": "main", "sha": "base-sha", "repo": map[string]any{
		"id": 424242, "name": "widget", "full_name": "acme/widget", "default_branch": "main", "owner": map[string]any{"login": "acme"},
	}}
	raw, err := json.Marshal(pull)
	if err != nil {
		t.Fatal(err)
	}
	handler := *fixture.handler
	handler.fetchPullRequestJSON = func(context.Context, int64, string, string, int32) (json.RawMessage, error) { return raw, nil }
	const unboundInstallation = 987654321
	if err := handler.replayClosedPullRequest(context.Background(), unboundInstallation, "acme", "widget", 2019); err != nil {
		t.Fatal(err)
	}
	var owed int
	dbfx.QueryRow(t, `SELECT count(*) FROM github_pr_closed_replay WHERE installation_id=$1`, int64(unboundInstallation)).Scan(&owed)
	if owed != 0 {
		t.Fatalf("owed replays = %d, want 0", owed)
	}
}

// A replay for an installation with no allowed binding (revoked or denied after
// the refresh selected the row) must not reach GitHub or record an owed marker.
func TestGitHubReplayClosedPullRequestSkipsInstallationWithoutAllowedBinding(t *testing.T) {
	fixture := newGitHubReviewFixture(t)
	handler := *fixture.handler
	fetches := 0
	handler.fetchPullRequestJSON = func(context.Context, int64, string, string, int32) (json.RawMessage, error) {
		fetches++
		return nil, errors.New("must not fetch")
	}
	const unboundInstallation = 987654322
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM github_pr_closed_replay WHERE installation_id=$1`, int64(unboundInstallation))
	})
	if err := handler.replayClosedPullRequest(context.Background(), unboundInstallation, "acme", "widget", 2019); err != nil {
		t.Fatal(err)
	}
	if fetches != 0 {
		t.Fatalf("GitHub fetches = %d, want 0", fetches)
	}
	if owed := dbfx.Count(t, `SELECT count(*) FROM github_pr_closed_replay WHERE installation_id=$1`, int64(unboundInstallation)); owed != 0 {
		t.Fatalf("owed replays = %d, want 0", owed)
	}
}
