package service

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func usesPRResolver(config *builtinWakeup, wakeup db.IssueWakeup) bool {
	return (config == nil || !config.Eff.Config.Target.Set) &&
		(wakeup.SystemRule.String == SystemRulePRAttention || wakeup.SystemRule.String == SystemRulePRChecksFailed ||
			wakeup.SystemRule.String == SystemRulePRChecksPassed)
}

func prResolverTarget(ctx context.Context, queries *db.Queries, issue db.Issue, receipt db.IssueWakeupReceipt) (wakeTarget, error) {
	var payload struct {
		RepoOwner string `json:"repo_owner"`
		RepoName  string `json:"repo_name"`
		Number    int32  `json:"pr_number"`
	}
	if err := json.Unmarshal(receipt.Payload, &payload); err != nil {
		return wakeTarget{}, err
	}
	target, found, err := prAuthorTarget(ctx, queries, issue, payload.RepoOwner, payload.RepoName, payload.Number)
	if err != nil || found {
		return target, err
	}
	return resolveWakeTarget(ctx, queries, issue)
}

// prAuthorTarget is the PR's author as a wake target; found is false when the
// PR or an available author agent cannot be resolved.
func prAuthorTarget(ctx context.Context, queries *db.Queries, issue db.Issue, owner, repo string, number int32) (wakeTarget, bool, error) {
	pull, err := queries.GetGitHubPullRequest(ctx, db.GetGitHubPullRequestParams{
		WorkspaceID: issue.WorkspaceID, RepoOwner: owner, RepoName: repo, PrNumber: number,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return wakeTarget{}, false, nil
	}
	if err != nil {
		return wakeTarget{}, false, err
	}
	author, err := GitHubPullRequestAuthor(ctx, queries, pull)
	if errors.Is(err, pgx.ErrNoRows) {
		return wakeTarget{}, false, nil
	}
	if err != nil {
		return wakeTarget{}, false, err
	}
	if !author.RuntimeID.Valid || author.ArchivedAt.Valid {
		return wakeTarget{}, false, nil
	}
	return wakeTarget{Type: "agent", ID: author.ID, Agent: author}, true, nil
}

// prAuthorNotOwner reports whether the PR's author agent differs from the agent
// that holds the issue (the assignee, or the assignee squad's leader). With no
// resolvable author the assignee is the target, so the answer is false.
func prAuthorNotOwner(ctx context.Context, queries *db.Queries, issue db.Issue, owner, repo string, number int32) (bool, error) {
	author, found, err := prAuthorTarget(ctx, queries, issue, owner, repo, number)
	if err != nil || !found {
		return false, err
	}
	assignee, err := resolveWakeTarget(ctx, queries, issue)
	if err != nil {
		return false, err
	}
	return assignee.Agent.ID != author.Agent.ID, nil
}

func prResolverReceipts(ctx context.Context, queries *db.Queries, issue db.Issue, target wakeTarget, receipts []db.IssueWakeupReceipt) ([]db.IssueWakeupReceipt, error) {
	matched := make([]db.IssueWakeupReceipt, 0, len(receipts))
	for _, receipt := range receipts {
		resolved, err := prResolverTarget(ctx, queries, issue, receipt)
		if err != nil {
			return nil, err
		}
		if resolved.key() == target.key() {
			matched = append(matched, receipt)
		}
	}
	return matched, nil
}
