package ghsnapshot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
)

type ReviewComment struct {
	ID   int64  `json:"id"`
	Path string `json:"path"`
	Line int32  `json:"line"`
	Body string `json:"body"`
}

func (manager *Manager) FetchReviewComments(ctx context.Context, installationID int64, owner, repo string, number int32, reviewID int64) ([]ReviewComment, error) {
	if manager == nil || !manager.client.Enabled() {
		return nil, errors.New("github: review comments API is not configured")
	}
	return manager.client.FetchReviewComments(ctx, installationID, owner, repo, number, reviewID)
}

func (client *Client) FetchReviewComments(ctx context.Context, installationID int64, owner, repo string, number int32, reviewID int64) ([]ReviewComment, error) {
	if !client.Enabled() {
		return nil, errors.New("github: review comments API is not configured")
	}
	path := fmt.Sprintf("/repos/%s/%s/pulls/%d/reviews/%d/comments", url.PathEscape(owner), url.PathEscape(repo), number, reviewID)
	var comments []ReviewComment
	contentBytes := 0
	for page := 1; page <= maxSnapshotContextPages; page++ {
		body, err := client.restGet(ctx, installationID, fmt.Sprintf("%s?per_page=%d&page=%d", path, snapshotPageSize, page))
		if err != nil {
			return nil, err
		}
		var entries []ReviewComment
		if json.Unmarshal(body, &entries) != nil || entries == nil || len(entries) > snapshotPageSize {
			return nil, errors.New("github: malformed review comments page")
		}
		for _, entry := range entries {
			contentBytes += len(entry.Body) + len(entry.Path)
		}
		if contentBytes > 1<<20 {
			return nil, errors.New("github: review comments exceed size limit")
		}
		comments = append(comments, entries...)
		if len(entries) < snapshotPageSize {
			return comments, nil
		}
	}
	return nil, errors.New("github: review comments pagination limit exceeded")
}
