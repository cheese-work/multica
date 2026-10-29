package handler

import (
	"context"
	"fmt"
	"regexp"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

var githubNotifyLoginPattern = regexp.MustCompile(`^[A-Za-z0-9-]{1,39}$`)
var githubNotifyRepositoryPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)
var githubNotifyIntakePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

func githubNotifyContent(ctx context.Context, queries *db.Queries, revision db.GithubWebhookRevision) (string, error) {
	pull, err := queries.GetGitHubPullRequestInWorkspace(ctx, db.GetGitHubPullRequestInWorkspaceParams{ID: revision.PullRequestID, WorkspaceID: revision.WorkspaceID})
	if err != nil {
		return "", err
	}
	anchor := map[string]string{"review": "pullrequestreview-", "review_comment": "discussion_r", "issue_comment": "issuecomment-"}[revision.ObjectKind]
	if anchor == "" || !githubNotifyLoginPattern.MatchString(revision.AuthorLogin) ||
		!githubNotifyLoginPattern.MatchString(pull.RepoOwner) || !githubNotifyRepositoryPattern.MatchString(pull.RepoName) ||
		!revision.SourceDeliveryGuid.Valid || !githubNotifyIntakePattern.MatchString(revision.SourceDeliveryGuid.String) {
		return "", fmt.Errorf("github: invalid notify-only metadata")
	}
	return fmt.Sprintf("GitHub %s\nhttps://github.com/%s/%s/pull/%d#%s%d\nAuthor: %s (%d)\nObject ID: %d\nIntake ID: %s",
		revision.Event, pull.RepoOwner, pull.RepoName, pull.PrNumber, anchor, revision.ObjectID,
		revision.AuthorLogin, revision.AuthorID, revision.ObjectID, revision.SourceDeliveryGuid.String), nil
}
