package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

// legacyUnverifiedPR returns the fixture PR as the pre-fix check left it: head
// flagged as another repository and no links.
func legacyUnverifiedPR(t *testing.T, fixture githubReviewFixture) string {
	t.Helper()
	var prID string
	dbfx.QueryRow(t, `SELECT id FROM github_pull_request WHERE workspace_id=$1 AND pr_number=2019`, testWorkspaceID).Scan(&prID)
	dbfx.Exec(t, `UPDATE github_pull_request SET head_repo_same=false WHERE id=$1`, prID)
	dbfx.Exec(t, `DELETE FROM issue_pull_request WHERE pull_request_id=$1`, prID)
	return prID
}

func repairWithREST(t *testing.T, fixture githubReviewFixture, head map[string]any) int {
	t.Helper()
	pull := githubForkGatePR(t, fixture, head)
	pull["base"] = map[string]any{"ref": "main", "repo": map[string]any{
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
	replayed, err := handler.RepairUnverifiedHeadPullRequests(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return replayed
}

func TestGitHubRepairRelinksSameRepositoryForkFlag(t *testing.T) {
	fixture := newGitHubReviewFixture(t)
	prID := legacyUnverifiedPR(t, fixture)
	same := map[string]any{"id": 424242, "full_name": "acme/widget", "fork": true}
	if replayed := repairWithREST(t, fixture, same); replayed != 1 {
		t.Fatalf("replayed = %d, want 1", replayed)
	}
	if count := dbfx.Count(t, `SELECT count(*) FROM issue_pull_request WHERE pull_request_id=$1 AND issue_id=$2`, prID, fixture.issueID); count != 1 {
		t.Errorf("links = %d, want 1", count)
	}
	if count := dbfx.Count(t, `SELECT count(*) FROM github_pull_request WHERE id=$1 AND head_repo_same`, prID); count != 1 {
		t.Errorf("head_repo_same not repaired")
	}
}

func TestGitHubRepairLeavesRealForkPullRequest(t *testing.T) {
	fixture := newGitHubReviewFixture(t)
	prID := legacyUnverifiedPR(t, fixture)
	if replayed := repairWithREST(t, fixture, map[string]any{"id": 20, "full_name": "outsider/widget", "fork": true}); replayed != 0 {
		t.Fatalf("replayed = %d, want 0", replayed)
	}
	if count := dbfx.Count(t, `SELECT count(*) FROM issue_pull_request WHERE pull_request_id=$1`, prID); count != 0 {
		t.Errorf("fork PR linked: %d", count)
	}
	if count := dbfx.Count(t, `SELECT count(*) FROM github_pull_request WHERE id=$1 AND NOT head_repo_same`, prID); count != 1 {
		t.Errorf("fork PR flag changed")
	}
}

func TestGitHubRepairKeepsExclusion(t *testing.T) {
	fixture := newGitHubReviewFixture(t)
	prID := legacyUnverifiedPR(t, fixture)
	dbfx.Exec(t, `INSERT INTO issue_pull_request_exclusion (issue_id, pull_request_id, workspace_id, excluded_by_type, excluded_by_id) VALUES ($1,$2,$3,'member',$4)`,
		fixture.issueID, prID, testWorkspaceID, testUserID)
	repairWithREST(t, fixture, map[string]any{"id": 424242, "full_name": "acme/widget", "fork": true})
	if count := dbfx.Count(t, `SELECT count(*) FROM issue_pull_request WHERE pull_request_id=$1`, prID); count != 0 {
		t.Errorf("excluded PR relinked: %d", count)
	}
	if count := dbfx.Count(t, `SELECT count(*) FROM github_pull_request WHERE id=$1 AND head_repo_same`, prID); count != 1 {
		t.Errorf("head_repo_same not repaired")
	}
}

// Rows older than the head_repo_same column carry its false default and were
// never judged by the fork check, so the sweep must not spend API calls on them.
func TestGitHubRepairSkipsRowsFromBeforeTheForkCheck(t *testing.T) {
	fixture := newGitHubReviewFixture(t)
	prID := legacyUnverifiedPR(t, fixture)
	dbfx.Exec(t, `UPDATE github_pull_request SET updated_at = '2026-10-01T00:00:00Z' WHERE id=$1`, prID)
	handler := *fixture.handler
	handler.fetchPullRequestJSON = func(context.Context, int64, string, string, int32) (json.RawMessage, error) {
		t.Error("fetched a row from before the fork check")
		return nil, nil
	}
	if replayed, err := handler.RepairUnverifiedHeadPullRequests(context.Background()); err != nil || replayed != 0 {
		t.Fatalf("replayed=%d err=%v, want 0 nil", replayed, err)
	}
}

// A sweep that cannot read GitHub must say so; a silent skip hid a dead sweep.
func TestGitHubRepairWarnsWhenAPIClientMissing(t *testing.T) {
	fixture := newGitHubReviewFixture(t)
	handler := *fixture.handler
	handler.PRRefresh = nil
	handler.fetchPullRequestJSON = nil
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	if replayed, err := handler.RepairUnverifiedHeadPullRequests(context.Background()); err != nil || replayed != 0 {
		t.Fatalf("replayed=%d err=%v, want 0 nil", replayed, err)
	}
	if out := logs.String(); !strings.Contains(out, "level=WARN") || !strings.Contains(out, "unverified head sweep skipped") {
		t.Errorf("no skip warning, logs: %q", out)
	}
}
