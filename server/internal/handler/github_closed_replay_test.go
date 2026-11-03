package handler

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// CHE-3402: the pull_request "closed" delivery for a merged PR never landed,
// so the row stayed open. The refresh pipeline reports the PR closed and the
// handler replays GitHub's current PR through the normal webhook path.
func TestGitHubReplayClosedPullRequestRecordsMerge(t *testing.T) {
	for _, tc := range []struct {
		name string
		head map[string]any
	}{
		{"same repository", map[string]any{"id": 424242, "full_name": "acme/widget"}},
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
	pull := githubForkGatePR(t, fixture, map[string]any{"id": 424242, "full_name": "acme/widget"})
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
