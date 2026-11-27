package ghsnapshot

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/reviewindependence"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// CHE-9319: the refresher keeps the identities named on a PR's commits for the
// merge gate, only for a workspace that has the review_independence setting, and
// reads GitHub once per head.
func TestRefreshStoresCommitIdentitiesForTheSnapshotHead(t *testing.T) {
	const withSetting = `{"review_independence":{"attribute":"team","rule":"reviewer_value_not_in_author_values","agent_values":{"p/*":"x"},"commit_identities":{"vendor-x.example":"x"}}}`
	for name, tc := range map[string]struct {
		settings  string
		fail      bool
		wantCalls int
		wantStore bool
	}{
		"setting present":         {withSetting, false, 1, true},
		"setting absent":          {`{}`, false, 0, false},
		"commit list unreadable":  {withSetting, true, 1, false},
		"read once per head only": {withSetting, false, 1, true},
	} {
		t.Run(name, func(t *testing.T) {
			pool := testDBPool(t)
			q := db.New(pool)
			ctx := context.Background()
			const installationID = 555901
			ws := seedWorkspaceWithSettings(t, pool, tc.settings)
			pr := seedPRForWorkspace(t, q, ws, installationID, "ci-repo-"+randHex(t), 5, "A")
			t.Cleanup(func() {
				_, _ = pool.Exec(context.Background(), `DELETE FROM github_pull_request_check_run WHERE pr_id=$1`, pr.ID)
				_, _ = pool.Exec(context.Background(), `DELETE FROM github_pull_request WHERE id=$1`, pr.ID)
			})
			m := NewManager(enabledClient(t), q, pool, nil)
			m.jitter = func() time.Duration { return 0 }
			m.ctx = ctx
			m.fetch = func(context.Context, *Client, int64, string, string, int32) (*PRSnapshot, error) {
				return &PRSnapshot{HeadSHA: "A", Mergeable: "MERGEABLE", MergeStateStatus: "CLEAN"}, nil
			}
			calls := 0
			m.commits = func(context.Context, int64, string, string, int32) ([]reviewindependence.PRCommit, error) {
				calls++
				if tc.fail {
					return nil, errors.New("github down")
				}
				return []reviewindependence.PRCommit{{SHA: "1111111111111111", AuthorEmail: "a@vendor-x.example", CommitterEmail: "noreply@github.com",
					Message: "fix\n\nCo-authored-by: Bot <bot@vendor-y.example>\nsecret body text"}}, nil
			}
			addr := address{InstallationID: installationID, Owner: "o", Repo: pr.RepoName, Number: 5}
			m.process(ctx, addr)
			m.process(ctx, addr)
			if calls != tc.wantCalls && !(tc.fail && calls == 2) {
				t.Fatalf("commit reads = %d, want %d", calls, tc.wantCalls)
			}
			got, err := q.GetGitHubPullRequestByID(ctx, pr.ID)
			if err != nil {
				t.Fatal(err)
			}
			commits, ok := reviewindependence.CommitsAt(got.CommitIdentities, "A")
			if ok != tc.wantStore {
				t.Fatalf("stored commits for head A = %v, want %v (%s)", ok, tc.wantStore, got.CommitIdentities)
			}
			if tc.wantStore {
				if len(commits) != 1 || commits[0].AuthorEmail != "a@vendor-x.example" {
					t.Fatalf("stored commits = %+v", commits)
				}
				if m := commits[0].Message; m != "Co-authored-by: x <bot@vendor-y.example>" {
					t.Fatalf("stored message %q keeps more than the Co-authored-by trailers", m)
				}
			}
			if _, ok := reviewindependence.CommitsAt(got.CommitIdentities, "B"); ok {
				t.Fatal("commits of head A are served for head B")
			}
		})
	}
}
