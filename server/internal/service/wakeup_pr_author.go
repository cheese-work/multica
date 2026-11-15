package service

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
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
	assignee, err := resolveWakeTarget(ctx, queries, issue)
	if err != nil || assignee.Type != "member" {
		return assignee, err
	}
	// A member holds the issue and no PR author resolves: the last agent that
	// worked on it takes the PR action, so the member is not the owner (CHE-5837).
	last, err := queries.LastWorkingAgentOnIssue(ctx, db.LastWorkingAgentOnIssueParams{IssueID: issue.ID, WorkspaceID: issue.WorkspaceID})
	if errors.Is(err, pgx.ErrNoRows) {
		return assignee, nil
	}
	if err != nil {
		return wakeTarget{}, err
	}
	return wakeTarget{Type: "agent", ID: last.ID, Agent: last}, nil
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

// prNotice is one member notice (CHE-5901): the receipts of one pull request,
// the stored events they carry (which stop being offered to the bridge once the
// notice is written), and the key of the one unread row per issue and pull
// request. A receipt that names no pull request in the workspace gives no key.
type prNotice struct {
	Receipts []db.IssueWakeupReceipt
	Events   []pgtype.UUID
	Key      pgtype.Text
}

// prNotices splits the receipts of one wake into one notice per pull request,
// in first-seen order, so two pull requests batched in one wake are never
// settled by one row. Receipts that name no pull request form one notice
// without a key.
func prNotices(ctx context.Context, queries *db.Queries, issue db.Issue, receipts []db.IssueWakeupReceipt) ([]prNotice, error) {
	type pullRef struct {
		owner, repo string
		number      int32
	}
	var notices []prNotice
	at := map[pullRef]int{}
	for _, receipt := range receipts {
		var payload struct {
			EventID   pgtype.UUID `json:"event_id"`
			RepoOwner string      `json:"repo_owner"`
			RepoName  string      `json:"repo_name"`
			Number    int32       `json:"pr_number"`
		}
		_ = json.Unmarshal(receipt.Payload, &payload)
		ref := pullRef{payload.RepoOwner, payload.RepoName, payload.Number}
		i, seen := at[ref]
		if !seen {
			i = len(notices)
			at[ref] = i
			notices = append(notices, prNotice{})
			pull, err := queries.GetGitHubPullRequest(ctx, db.GetGitHubPullRequestParams{
				WorkspaceID: issue.WorkspaceID, RepoOwner: ref.owner, RepoName: ref.repo, PrNumber: ref.number,
			})
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return nil, err
			}
			if err == nil {
				notices[i].Key = pgtype.Text{String: "pr_fyi:" + util.UUIDToString(issue.ID) + ":" + util.UUIDToString(pull.ID), Valid: true}
			}
		}
		notices[i].Receipts = append(notices[i].Receipts, receipt)
		if payload.EventID.Valid {
			notices[i].Events = append(notices[i].Events, payload.EventID)
		}
	}
	return notices, nil
}
