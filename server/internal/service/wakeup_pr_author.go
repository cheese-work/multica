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
		(wakeup.SystemRule.String == SystemRulePRAttention || wakeup.SystemRule.String == SystemRulePRChecksFailed)
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
	pull, err := queries.GetGitHubPullRequest(ctx, db.GetGitHubPullRequestParams{
		WorkspaceID: issue.WorkspaceID, RepoOwner: payload.RepoOwner, RepoName: payload.RepoName, PrNumber: payload.Number,
	})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return wakeTarget{}, err
	}
	if err == nil {
		author, err := GitHubPullRequestAuthor(ctx, queries, pull)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return wakeTarget{}, err
		}
		if err == nil && author.RuntimeID.Valid && !author.ArchivedAt.Valid {
			return wakeTarget{Type: "agent", ID: author.ID, Agent: author}, nil
		}
	}
	return resolveWakeTarget(ctx, queries, issue)
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
