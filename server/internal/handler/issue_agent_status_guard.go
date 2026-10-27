package handler

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/multica-ai/multica/server/internal/issuestatus"
	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

var errAgentIssueOpenPR = errors.New("agents cannot set a done-category status while a linked PR is open and unmerged")
var errAgentIssueOpenDecision = errors.New("agents cannot set a done-category status while a decision request is open or its answer awaits delivery")

func guardAgentIssueStatus(ctx context.Context, queries *db.Queries, params db.UpdateIssueParams) error {
	actor, _ := ctx.Value(wakeupActorKey{}).(wakeupActor)
	if actor.kind != "agent" || !params.Status.Valid {
		return nil
	}
	issue, err := queries.GetIssue(ctx, params.ID)
	if err != nil {
		return err
	}
	category, err := issuestatus.CategoryWithError(ctx, queries, issue.WorkspaceID, params.Status.String)
	if err != nil {
		return err
	}
	if category != issuestatus.CategoryDone && params.Status.String != "approved" && params.Status.String != "agent_accepted" {
		return nil
	}
	denial := errAgentIssueOpenPR
	if params.Status.String == "approved" {
		denial = service.ErrAgentIssueApproval
	}
	pullRequests, err := queries.ListPullRequestsByIssue(ctx, issue.ID)
	if err != nil {
		return fmt.Errorf("check linked GitHub PRs: %w", err)
	}
	for _, pullRequest := range pullRequests {
		if pullRequest.WorkspaceID == issue.WorkspaceID && !pullRequest.MergedAt.Valid && (pullRequest.State == "open" || pullRequest.State == "draft") {
			return fmt.Errorf("%w: %s/%s#%d (%s)", denial, pullRequest.RepoOwner, pullRequest.RepoName, pullRequest.PrNumber, pullRequest.HtmlUrl)
		}
	}
	vcsPullRequests, err := queries.ListVCSPullRequestsByIssue(ctx, issue.ID)
	if err != nil {
		return fmt.Errorf("check linked VCS PRs: %w", err)
	}
	for _, pullRequest := range vcsPullRequests {
		if pullRequest.WorkspaceID == issue.WorkspaceID && !pullRequest.MergedAt.Valid && (pullRequest.State == "open" || pullRequest.State == "draft") {
			return fmt.Errorf("%w: %s/%s#%d (%s)", denial, pullRequest.RepoOwner, pullRequest.RepoName, pullRequest.PrNumber, pullRequest.HtmlUrl)
		}
	}
	if params.Status.String == "approved" {
		return service.ErrAgentIssueApproval
	}
	if category == issuestatus.CategoryDone {
		if _, err := queries.LockDecisionIssue(ctx, db.LockDecisionIssueParams{WorkspaceID: issue.WorkspaceID, ID: issue.ID}); err != nil {
			return fmt.Errorf("lock issue for decision check: %w", err)
		}
		requests, err := queries.ListDecisionRequestsByIssue(ctx, db.ListDecisionRequestsByIssueParams{WorkspaceID: issue.WorkspaceID, IssueID: issue.ID})
		if err != nil {
			return fmt.Errorf("check open decision requests: %w", err)
		}
		for _, request := range requests {
			if request.Status == "open" {
				return fmt.Errorf("%w: %s; answer or withdraw the request before closing the issue", errAgentIssueOpenDecision, uuidToString(request.ID))
			}
			if request.Status == "answered" {
				answer, err := queries.LockDecisionLiveAnswer(ctx, db.LockDecisionLiveAnswerParams{WorkspaceID: issue.WorkspaceID, ID: request.ID})
				if err != nil && !errors.Is(err, pgx.ErrNoRows) {
					return fmt.Errorf("check undelivered decision answer: %w", err)
				}
				if err == nil && !answer.WokenAt.Valid {
					return fmt.Errorf("%w: %s; wait for answer delivery or withdraw the request before closing the issue", errAgentIssueOpenDecision, uuidToString(request.ID))
				}
			}
		}
	}
	return nil
}

func writeAgentIssueStatusError(writer http.ResponseWriter, err error) bool {
	switch {
	case errors.Is(err, service.ErrAgentIssueApproval):
		writeError(writer, http.StatusForbidden, err.Error())
	case errors.Is(err, errAgentIssueOpenPR), errors.Is(err, errAgentIssueOpenDecision):
		writeError(writer, http.StatusConflict, err.Error())
	default:
		return false
	}
	return true
}
