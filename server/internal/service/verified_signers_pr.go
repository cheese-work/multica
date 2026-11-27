package service

import (
	"context"
	"errors"
	"slices"

	"github.com/jackc/pgx/v5"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

var (
	// ErrSignersPRNotStored means Multica holds no snapshot of the pull request.
	ErrSignersPRNotStored = errors.New("pull request is not stored")
	// ErrSignersIssueNotLinked means the issue has no link to the pull request.
	ErrSignersIssueNotLinked = errors.New("issue is not linked to the pull request")
)

// PullRequestSigners reads VerifiedSigners for a stored pull request after
// checking that c.IssueID is linked to it, so a caller cannot read the signers
// of a head through an unrelated issue (CHE-10268). q must run on tx.
func PullRequestSigners(ctx context.Context, tx pgx.Tx, q *db.Queries, c MergeCandidate, protection CommentProtection) (VerifiedSignersResult, error) {
	pull, err := q.GetGitHubPullRequest(ctx, db.GetGitHubPullRequestParams{WorkspaceID: c.WorkspaceID, RepoOwner: c.Owner, RepoName: c.Repo, PrNumber: c.Number})
	if errors.Is(err, pgx.ErrNoRows) {
		return VerifiedSignersResult{}, ErrSignersPRNotStored
	}
	if err != nil {
		return VerifiedSignersResult{}, err
	}
	linked, err := q.ListIssueIDsForPullRequest(ctx, pull.ID)
	if err != nil {
		return VerifiedSignersResult{}, err
	}
	if !slices.Contains(linked, c.IssueID) {
		return VerifiedSignersResult{}, ErrSignersIssueNotLinked
	}
	return VerifiedSigners(ctx, tx, q, c, protection)
}
