package ghsnapshot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// DeliveryAttempt is one entry of the App's webhook delivery log. A redelivery
// keeps the original GUID and gets a new ID, so attempts of one delivery share
// a GUID.
type DeliveryAttempt struct {
	ID          int64     `json:"id"`
	GUID        string    `json:"guid"`
	DeliveredAt time.Time `json:"delivered_at"`
	Event       string    `json:"event"`
	StatusCode  int       `json:"status_code"`
}

func (attempt DeliveryAttempt) Succeeded() bool {
	return attempt.StatusCode >= 200 && attempt.StatusCode < 300
}

// PullObject is a review, inline comment or PR conversation comment read from
// the REST API. The reconciler hashes Body and never stores or returns it.
type PullObject struct {
	Kind        string
	ID          int64
	Body        string
	AuthorID    int64
	AuthorLogin string
	AuthorType  string
	ReviewState string
	CreatedAt   *time.Time
	UpdatedAt   *time.Time
	SubmittedAt *time.Time
}

type PullObjects struct {
	RepositoryID int64
	HeadSHA      string
	Open         bool
	SameRepo     bool
	Objects      []PullObject
}

var nextCursor = regexp.MustCompile(`<[^>]*[?&]cursor=([A-Za-z0-9_\-=.]+)[^>]*>;\s*rel="next"`)

// ListHookDeliveries reads one page of GET /app/hook/deliveries (App JWT).
// next is the cursor for the following page, or "" on the last page. The Link
// URL itself is never followed.
func (client *Client) ListHookDeliveries(ctx context.Context, cursor string) (attempts []DeliveryAttempt, next string, err error) {
	query := "per_page=100"
	if cursor != "" {
		query += "&cursor=" + url.QueryEscape(cursor)
	}
	status, header, body, err := client.appRequest(ctx, http.MethodGet, "/app/hook/deliveries?"+query)
	if err != nil {
		return nil, "", err
	}
	if status != http.StatusOK {
		return nil, "", fmt.Errorf("github hook deliveries: unexpected status %d", status)
	}
	if json.Unmarshal(body, &attempts) != nil || attempts == nil {
		return nil, "", errors.New("github hook deliveries: malformed response")
	}
	if match := nextCursor.FindStringSubmatch(header.Get("Link")); match != nil {
		next = match[1]
	}
	return attempts, next, nil
}

// RedeliverHook asks GitHub to send a recorded delivery again. The redelivered
// payload is signed by GitHub and reaches the normal webhook path.
func (client *Client) RedeliverHook(ctx context.Context, deliveryID int64) error {
	status, _, _, err := client.appRequest(ctx, http.MethodPost, fmt.Sprintf("/app/hook/deliveries/%d/attempts", deliveryID))
	if err != nil {
		return err
	}
	if status != http.StatusAccepted {
		return fmt.Errorf("github hook redelivery: unexpected status %d", status)
	}
	return nil
}

// appRequest returns the status, headers and body so no caller holds a closed response.
func (client *Client) appRequest(ctx context.Context, method, path string) (int, http.Header, []byte, error) {
	if !client.Enabled() {
		return 0, nil, nil, errors.New("github: App API is not configured")
	}
	appJWT, err := client.signAppJWT(client.now())
	if err != nil {
		return 0, nil, nil, err
	}
	request, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(client.apiBase, "/")+path, nil)
	if err != nil {
		return 0, nil, nil, err
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("Authorization", "Bearer "+appJWT)
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	response, err := client.httpClient.Do(request)
	if err != nil {
		return 0, nil, nil, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 8<<20))
	if err != nil {
		return 0, nil, nil, errors.New("github: failed to read response")
	}
	if isRateLimited(response, body) {
		return 0, nil, nil, rateLimitFromResponse(response, client.now())
	}
	return response.StatusCode, response.Header, body, nil
}

// ListPullObjects reads a pull request and its reviews, inline comments and
// conversation comments. Pending and dismissed reviews are left out.
func (client *Client) ListPullObjects(ctx context.Context, installationID int64, owner, repo string, number int32) (PullObjects, error) {
	if !client.Enabled() {
		return PullObjects{}, errors.New("github: pull request objects API is not configured")
	}
	base := fmt.Sprintf("/repos/%s/%s", url.PathEscape(owner), url.PathEscape(repo))
	pullPath := fmt.Sprintf("%s/pulls/%d", base, number)
	data, err := client.restGet(ctx, installationID, pullPath)
	if err != nil {
		return PullObjects{}, err
	}
	var pull struct {
		State string `json:"state"`
		Head  struct {
			SHA  string `json:"sha"`
			Repo *struct {
				ID int64 `json:"id"`
			} `json:"repo"`
		} `json:"head"`
		Base struct {
			Repo struct {
				ID int64 `json:"id"`
			} `json:"repo"`
		} `json:"base"`
	}
	if json.Unmarshal(data, &pull) != nil || pull.Head.SHA == "" || pull.Base.Repo.ID <= 0 {
		return PullObjects{}, errors.New("github: malformed pull request data")
	}
	result := PullObjects{
		RepositoryID: pull.Base.Repo.ID, HeadSHA: pull.Head.SHA, Open: pull.State == "open",
		SameRepo: pull.Head.Repo != nil && pull.Head.Repo.ID == pull.Base.Repo.ID,
	}
	if !result.Open || !result.SameRepo {
		return result, nil
	}
	type restObject struct {
		ID          int64      `json:"id"`
		Body        *string    `json:"body"`
		State       string     `json:"state"`
		CreatedAt   *time.Time `json:"created_at"`
		UpdatedAt   *time.Time `json:"updated_at"`
		SubmittedAt *time.Time `json:"submitted_at"`
		User        *struct {
			ID    int64  `json:"id"`
			Login string `json:"login"`
			Type  string `json:"type"`
		} `json:"user"`
	}
	for _, source := range []struct{ kind, path string }{
		{"review", pullPath + "/reviews"},
		{"review_comment", pullPath + "/comments"},
		{"issue_comment", fmt.Sprintf("%s/issues/%d/comments", base, number)},
	} {
		entries, err := fetchObjectPages[restObject](ctx, client, installationID, source.path)
		if err != nil {
			return PullObjects{}, err
		}
		for _, entry := range entries {
			state := strings.ToLower(entry.State)
			if entry.User == nil || entry.ID <= 0 || entry.User.ID <= 0 || (source.kind == "review" && (state == "pending" || state == "dismissed")) {
				continue
			}
			object := PullObject{
				Kind: source.kind, ID: entry.ID, AuthorID: entry.User.ID, AuthorLogin: entry.User.Login, AuthorType: entry.User.Type,
				CreatedAt: entry.CreatedAt, UpdatedAt: entry.UpdatedAt, SubmittedAt: entry.SubmittedAt,
			}
			if entry.Body != nil {
				object.Body = *entry.Body
			}
			if source.kind == "review" {
				object.ReviewState = state
			}
			result.Objects = append(result.Objects, object)
		}
	}
	return result, nil
}

func fetchObjectPages[T any](ctx context.Context, client *Client, installationID int64, path string) ([]T, error) {
	var items []T
	for page := 1; page <= maxSnapshotContextPages; page++ {
		data, err := client.restGet(ctx, installationID, fmt.Sprintf("%s?per_page=%d&page=%d", path, snapshotPageSize, page))
		if err != nil {
			return nil, err
		}
		var entries []T
		if json.Unmarshal(data, &entries) != nil || entries == nil || len(entries) > snapshotPageSize {
			return nil, errors.New("github: malformed pull request object page")
		}
		items = append(items, entries...)
		if len(entries) < snapshotPageSize {
			return items, nil
		}
	}
	return nil, errors.New("github: pull request object pagination limit exceeded")
}

// Manager wrappers keep the handler off the private client, as reviews.go does.

func (manager *Manager) ListHookDeliveries(ctx context.Context, cursor string) ([]DeliveryAttempt, string, error) {
	if manager == nil || !manager.client.Enabled() {
		return nil, "", errors.New("github: App API is not configured")
	}
	return manager.client.ListHookDeliveries(ctx, cursor)
}

func (manager *Manager) RedeliverHook(ctx context.Context, deliveryID int64) error {
	if manager == nil || !manager.client.Enabled() {
		return errors.New("github: App API is not configured")
	}
	return manager.client.RedeliverHook(ctx, deliveryID)
}

func (manager *Manager) ListPullObjects(ctx context.Context, installationID int64, owner, repo string, number int32) (PullObjects, error) {
	if manager == nil || !manager.client.Enabled() {
		return PullObjects{}, errors.New("github: pull request objects API is not configured")
	}
	return manager.client.ListPullObjects(ctx, installationID, owner, repo, number)
}
