package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/integrations/ghsnapshot"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

type ghReviewPayload struct {
	Action       string `json:"action"`
	Installation struct {
		ID int64 `json:"id"`
	} `json:"installation"`
	Repository struct {
		ID    int64  `json:"id"`
		Name  string `json:"name"`
		Owner struct {
			Login string `json:"login"`
		} `json:"owner"`
	} `json:"repository"`
	PullRequest struct {
		Number int32 `json:"number"`
		Head   struct {
			Repo *ghPRHeadRepository `json:"repo"`
		} `json:"head"`
	} `json:"pull_request"`
	Review struct {
		ID    int64  `json:"id"`
		State string `json:"state"`
		Body  string `json:"body"`
		User  struct {
			ID    int64  `json:"id"`
			Login string `json:"login"`
			Type  string `json:"type"`
		} `json:"user"`
	} `json:"review"`
}

func (h *Handler) handlePullRequestReviewEvent(ctx context.Context, body []byte) error {
	var payload ghReviewPayload
	if json.Unmarshal(body, &payload) != nil || payload.Action != "submitted" || payload.Installation.ID <= 0 ||
		payload.Review.ID <= 0 || payload.Review.User.ID <= 0 || payload.Review.User.Type != "User" || payload.PullRequest.Number <= 0 {
		return nil
	}
	switch payload.Review.State {
	case "approved", "commented", "changes_requested":
	default:
		return nil
	}
	if !payload.PullRequest.Head.Repo.sameRepository(payload.Repository.ID, payload.Repository.Owner.Login, payload.Repository.Name) {
		slog.Info("github: denied pull request review", "reason", "fork_or_unverified_head_repository",
			"installation_id", payload.Installation.ID, "repo_owner", payload.Repository.Owner.Login, "repo_name", payload.Repository.Name, "pr_number", payload.PullRequest.Number)
		return nil
	}
	installations, err := h.Queries.ListGitHubInstallationsByInstallationID(ctx, payload.Installation.ID)
	if err != nil {
		return err
	}
	var failures []error
	for _, installation := range installations {
		if err := h.handleGitHubReviewForWorkspace(ctx, installation.WorkspaceID, &payload); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

func (h *Handler) handleGitHubReviewForWorkspace(ctx context.Context, workspaceID pgtype.UUID, payload *ghReviewPayload) error {
	workspace, err := h.Queries.GetWorkspace(ctx, workspaceID)
	if err != nil {
		return err
	}
	var settings struct {
		ReviewMembers map[string]string `json:"github_review_members"`
		ReviewAuthors map[string]string `json:"github_review_authors"`
	}
	if !githubFeaturesEnabled(workspace) || json.Unmarshal(workspace.Settings, &settings) != nil {
		return nil
	}
	userID, err := util.ParseUUID(settings.ReviewMembers[strconv.FormatInt(payload.Review.User.ID, 10)])
	if err != nil {
		return nil
	}
	if _, err := h.Queries.GetMemberByUserAndWorkspace(ctx, db.GetMemberByUserAndWorkspaceParams{UserID: userID, WorkspaceID: workspaceID}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return err
	}
	pull, err := h.Queries.GetGitHubPullRequest(ctx, db.GetGitHubPullRequestParams{
		WorkspaceID: workspaceID, RepoOwner: payload.Repository.Owner.Login, RepoName: payload.Repository.Name, PrNumber: payload.PullRequest.Number,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if pull.InstallationID != payload.Installation.ID {
		return nil
	}
	if halted, err := h.Queries.GitHubDispatchHalted(ctx, db.GitHubDispatchHaltedParams{WorkspaceID: workspaceID, RepoOwner: pull.RepoOwner, RepoName: pull.RepoName}); err != nil {
		return err
	} else if halted {
		slog.Warn("github dispatch halted: review wakeup dropped", "workspace_id", uuidToString(workspaceID), "repo", pull.RepoOwner+"/"+pull.RepoName)
		return nil
	}
	author, err := service.GitHubPullRequestAuthor(ctx, h.Queries, pull)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	issueIDs, err := h.Queries.ListIssueIDsForPullRequest(ctx, pull.ID)
	if err != nil {
		return err
	}
	var comments []ghsnapshot.ReviewComment
	commentsLoaded := false
	var failures []error
	for _, issueID := range issueIDs {
		issue, err := h.Queries.GetIssue(ctx, issueID)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			failures = append(failures, err)
			continue
		}
		if issue.WorkspaceID != workspaceID {
			continue
		}
		seen, err := h.Queries.HasGitHubReviewWakeup(ctx, db.HasGitHubReviewWakeupParams{
			IssueID: issue.ID, ReviewKey: githubReviewKey(issue.ID, pull.ID, payload.Review.ID),
		})
		if err != nil {
			failures = append(failures, err)
			continue
		}
		if seen {
			continue
		}
		mention := fmt.Sprintf("[@PR author](mention://agent/%s)", uuidToString(author.ID))
		triggers, _ := h.resolveMentionedAgentCommentTriggers(ctx, issue, util.ParseMentions(mention), nil, "member", uuidToString(userID), commentTriggerComputeOptions{OriginatorUserID: uuidToString(userID)})
		if len(triggers) != 1 {
			continue
		}
		if !commentsLoaded {
			comments, err = h.PRRefresh.FetchReviewComments(ctx, pull.InstallationID, pull.RepoOwner, pull.RepoName, pull.PrNumber, payload.Review.ID)
			if err != nil {
				return err
			}
			commentsLoaded = true
		}
		content := githubReviewContent(mention, pull, payload, comments)
		if len(content) > 1<<20 {
			return errors.New("github: review content exceeds size limit")
		}
		if err := h.createGitHubReviewWakeup(ctx, issue, author.ID, userID, pull.ID, payload.Review.ID, content); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

func githubReviewContent(mention string, pull db.GithubPullRequest, payload *ghReviewPayload, comments []ghsnapshot.ReviewComment) string {
	neutralize := func(text string) string { return strings.ReplaceAll(text, "mention://", "mention: //") }
	pullURL := fmt.Sprintf("https://github.com/%s/%s/pull/%d", pull.RepoOwner, pull.RepoName, pull.PrNumber)
	var content strings.Builder
	fmt.Fprintf(&content, "%s\n\nGitHub PR review by %s (%s): %s#pullrequestreview-%d\n\n%s", mention, neutralize(payload.Review.User.Login), payload.Review.State, pullURL, payload.Review.ID, neutralize(payload.Review.Body))
	for _, comment := range comments {
		fmt.Fprintf(&content, "\n\n%s:%d\n%s\n%s#discussion_r%d", neutralize(comment.Path), comment.Line, neutralize(comment.Body), pullURL, comment.ID)
	}
	return content.String()
}

func (h *Handler) createGitHubReviewWakeup(ctx context.Context, issue db.Issue, authorID, userID, pullID pgtype.UUID, reviewID int64, content string) error {
	key := githubReviewKey(issue.ID, pullID, reviewID)
	commentID := pgtype.UUID{Bytes: uuid.NewSHA1(uuid.NameSpaceURL, []byte(key)), Valid: true}
	params, err := h.TaskService.PrepareTaskForMention(ctx, h.Queries, issue, authorID, commentID, userID, service.OriginDerived)
	if err != nil {
		return err
	}
	tx, err := h.TxStarter.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1, 0))", key); err != nil {
		return err
	}
	queries := h.Queries.WithTx(tx)
	seen, err := queries.HasGitHubReviewWakeup(ctx, db.HasGitHubReviewWakeupParams{IssueID: issue.ID, ReviewKey: key})
	if err != nil {
		return err
	}
	if seen {
		return nil
	}
	created, err := queries.CreateComment(ctx, db.CreateCommentParams{
		ID: commentID, IssueID: issue.ID, WorkspaceID: issue.WorkspaceID, AuthorType: "system", AuthorID: pgtype.UUID{Valid: true}, Content: content, Type: "comment",
	})
	if err != nil {
		return err
	}
	task, err := h.TaskService.EnqueuePreparedTaskForMentionInTx(ctx, queries, issue, params, service.OriginDerived)
	if err != nil {
		return err
	}
	if err := queries.MarkGitHubReviewWakeup(ctx, db.MarkGitHubReviewWakeupParams{TaskID: task.ID, IssueID: issue.ID, ReviewKey: key}); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	h.publish(protocol.EventCommentCreated, uuidToString(issue.WorkspaceID), "system", "", map[string]any{
		"comment": commentToResponse(created.Comment(), nil, nil), "issue_title": issue.Title,
		"issue_assignee_type": textToPtr(issue.AssigneeType), "issue_assignee_id": uuidToPtr(issue.AssigneeID),
		"issue_status": issue.Status, "issue_revision": created.IssueRevision,
	})
	h.TaskService.PublishTaskEnqueued(ctx, task)
	return nil
}

func githubReviewKey(issueID, pullID pgtype.UUID, reviewID int64) string {
	return fmt.Sprintf("github.pr_review_submitted:%s:%s:%d", uuidToString(issueID), uuidToString(pullID), reviewID)
}
