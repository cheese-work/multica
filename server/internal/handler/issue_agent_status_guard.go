package handler

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/multica-ai/multica/server/internal/issuestatus"
	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

var errAgentIssueOpenPR = errors.New("agents cannot set a done-category status while a linked PR is open and unmerged")

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
	return nil
}

func writeAgentIssueStatusError(writer http.ResponseWriter, err error) bool {
	switch {
	case errors.Is(err, service.ErrAgentIssueApproval):
		writeError(writer, http.StatusForbidden, err.Error())
	case errors.Is(err, errAgentIssueOpenPR):
		writeError(writer, http.StatusConflict, err.Error())
	default:
		return false
	}
	return true
}
