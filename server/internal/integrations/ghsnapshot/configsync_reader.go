package ghsnapshot

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/multica-ai/multica/server/internal/configsync"
)

// maxConfigSyncPages caps paged reads (100 per page). Hitting the cap is an
// error, never a silent truncation: a missed auto_merge_enabled event would
// let a queued merge pass the direct-merge check.
const maxConfigSyncPages = 10

// prCommitsAPICap is the most commits GitHub's PR-commits endpoint returns. It
// gives no sign of truncation, so a list that reaches the cap is unprovable.
const prCommitsAPICap = 250

// ConfigSyncReader is the GitHub App read surface of one config repo
// (configsync.Reader) plus the receipt comment write. Every value comes from
// the API, never from a webhook payload.
type ConfigSyncReader struct {
	client      *Client
	installID   int64
	owner, repo string
}

func NewConfigSyncReader(c *Client, installationID int64, owner, repo string) *ConfigSyncReader {
	return &ConfigSyncReader{client: c, installID: installationID, owner: owner, repo: repo}
}

var _ configsync.Reader = (*ConfigSyncReader)(nil)

func (r *ConfigSyncReader) path(format string, args ...any) string {
	return "/repos/" + url.PathEscape(r.owner) + "/" + url.PathEscape(r.repo) + fmt.Sprintf(format, args...)
}

func (r *ConfigSyncReader) get(ctx context.Context, path string, out any) error {
	raw, err := r.client.restGet(ctx, r.installID, path)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, out)
}

func (r *ConfigSyncReader) PullRequest(ctx context.Context, number int) (configsync.PullRequest, error) {
	var in struct {
		Merged   bool `json:"merged"`
		MergedBy *struct {
			Login string `json:"login"`
			Type  string `json:"type"`
		} `json:"merged_by"`
		Base struct {
			Ref string `json:"ref"`
		} `json:"base"`
		MergeCommitSHA string `json:"merge_commit_sha"`
	}
	if err := r.get(ctx, r.path("/pulls/%d", number), &in); err != nil {
		return configsync.PullRequest{}, err
	}
	pr := configsync.PullRequest{Merged: in.Merged, BaseRef: in.Base.Ref, MergeCommitSHA: in.MergeCommitSHA}
	if in.MergedBy != nil {
		pr.MergedByLogin, pr.MergedByType = in.MergedBy.Login, in.MergedBy.Type
	}
	return pr, nil
}

// pages walks a 100-per-page list and errors past maxConfigSyncPages.
func pages[T any](ctx context.Context, r *ConfigSyncReader, basePath string) ([]T, error) {
	var all []T
	for page := 1; page <= maxConfigSyncPages; page++ {
		var items []T
		if err := r.get(ctx, fmt.Sprintf("%s?per_page=100&page=%d", basePath, page), &items); err != nil {
			return nil, err
		}
		all = append(all, items...)
		if len(items) < 100 {
			return all, nil
		}
	}
	return nil, errors.New("github REST: list exceeds page cap")
}

func (r *ConfigSyncReader) TimelineEvents(ctx context.Context, number int) ([]string, error) {
	items, err := pages[struct {
		Event string `json:"event"`
	}](ctx, r, r.path("/issues/%d/timeline", number))
	if err != nil {
		return nil, err
	}
	events := make([]string, len(items))
	for i, it := range items {
		events[i] = it.Event
	}
	return events, nil
}

func (r *ConfigSyncReader) PRCommitMessages(ctx context.Context, number int) ([]string, error) {
	items, err := pages[struct {
		Commit struct {
			Message string `json:"message"`
		} `json:"commit"`
	}](ctx, r, r.path("/pulls/%d/commits", number))
	if err != nil {
		return nil, err
	}
	if len(items) >= prCommitsAPICap {
		return nil, fmt.Errorf("github REST: PR %d has %d or more commits, past the API's list cap", number, prCommitsAPICap)
	}
	msgs := make([]string, len(items))
	for i, it := range items {
		msgs[i] = it.Commit.Message
	}
	return msgs, nil
}

func (r *ConfigSyncReader) Commit(ctx context.Context, sha string) (configsync.Commit, error) {
	var in struct {
		Parents []struct {
			SHA string `json:"sha"`
		} `json:"parents"`
		Commit struct {
			Message string `json:"message"`
		} `json:"commit"`
	}
	if err := r.get(ctx, r.path("/commits/%s", url.PathEscape(sha)), &in); err != nil {
		return configsync.Commit{}, err
	}
	c := configsync.Commit{Message: in.Commit.Message}
	for _, p := range in.Parents {
		c.Parents = append(c.Parents, p.SHA)
	}
	return c, nil
}

// File reads path at ref through the contents API. 404 is found=false. A
// directory, a non-file entry, or a file over the API's 1 MB inline limit is
// an error: the caller must not read it as "missing".
func (r *ConfigSyncReader) File(ctx context.Context, ref, path string) ([]byte, bool, error) {
	segs := strings.Split(path, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	raw, err := r.client.restGet(ctx, r.installID, r.path("/contents/%s?ref=%s", strings.Join(segs, "/"), url.QueryEscape(ref)))
	var se *StatusError
	if errors.As(err, &se) && se.Status == 404 {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var in struct {
		Type     string `json:"type"`
		Encoding string `json:"encoding"`
		Content  string `json:"content"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, false, errors.New("github contents: not a single file")
	}
	if in.Type != "file" || in.Encoding != "base64" {
		return nil, false, errors.New("github contents: unsupported entry (not an inline file)")
	}
	data, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(in.Content, "\n", ""))
	if err != nil {
		return nil, false, errors.New("github contents: bad base64")
	}
	return data, true, nil
}

// PostComment writes the dry-run receipt on the PR.
func (r *ConfigSyncReader) PostComment(ctx context.Context, number int, body string) error {
	_, err := r.client.restPost(ctx, r.installID, r.path("/issues/%d/comments", number), map[string]string{"body": body})
	return err
}
