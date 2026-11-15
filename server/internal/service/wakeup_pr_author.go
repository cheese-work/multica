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
	if err != nil {
		return target, err
	}
	if found && receipt.EventType == "pr.checks_passed" {
		// A green the author does not hold goes to the assignee (CHE-6070).
		if assignee, err := resolveWakeTarget(ctx, queries, issue); err != nil || assignee.Agent.ID.Valid && assignee.Agent.ID != target.Agent.ID {
			return assignee, err
		}
	}
	if found {
		return target, nil
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

// prGreenRoute decides who a green head wakes. notOwner is true when the PR's
// author agent differs from the agent that holds the issue (the assignee, or the
// assignee squad's leader); with no resolvable author the assignee is the
// target, so notOwner is false. wakeAssignee is true when the holder is an agent
// with no active or queued run on the issue and no run since the head was pushed
// (dispatch then targets the assignee); otherwise a not-owner green is dropped.
func prGreenRoute(ctx context.Context, tx pgx.Tx, queries *db.Queries, issue db.Issue, in PullRequestWakeupInput) (notOwner, wakeAssignee bool, err error) {
	author, found, err := prAuthorTarget(ctx, queries, issue, in.RepoOwner, in.RepoName, in.Number)
	if err != nil || !found {
		return false, false, err
	}
	assignee, err := resolveWakeTarget(ctx, queries, issue)
	if err != nil {
		return false, false, err
	}
	if assignee.Agent.ID == author.Agent.ID {
		return false, false, nil
	}
	if !assignee.Agent.ID.Valid {
		return true, false, nil
	}
	// ponytail: the head's push time is not stored; the earliest check-suite
	// update on the head stands in for it. A reviewer who started between push
	// and that first suite update still counts as having run after the push.
	var covered bool
	err = tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM agent_task_queue t
		WHERE t.issue_id=$1 AND t.agent_id=$2 AND (
			t.status IN ('queued','dispatched','running','waiting_local_directory')
			OR t.created_at > COALESCE((SELECT min(cs.updated_at) FROM github_pull_request_check_suite cs
				JOIN github_pull_request p ON p.id=cs.pr_id
				WHERE p.workspace_id=$3 AND p.repo_owner=$4 AND p.repo_name=$5 AND p.pr_number=$6 AND cs.head_sha=$7), 'infinity')))`,
		issue.ID, assignee.Agent.ID, issue.WorkspaceID, in.RepoOwner, in.RepoName, in.Number, in.HeadSHA).Scan(&covered)
	return true, !covered, err
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
