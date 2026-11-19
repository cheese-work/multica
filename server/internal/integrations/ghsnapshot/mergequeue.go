package ghsnapshot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// QueueTimelineItem is one merge-queue removal, PR commit or force-push.
type QueueTimelineItem struct {
	Type      string // RemovedFromMergeQueueEvent, PullRequestCommit or HeadRefForcePushedEvent
	NodeID    string
	BeforeOID string // removal only
	Reason    string // removal only
	At        time.Time
}

// QueueRun is a merge_group workflow run.
type QueueRun struct {
	ID         int64
	Attempt    int32
	Status     string
	Conclusion string
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

const queueTimelineQuery = `query($owner: String!, $name: String!, $number: Int!) {
  repository(owner: $owner, name: $name) { pullRequest(number: $number) {
    timelineItems(last: 100, itemTypes: [REMOVED_FROM_MERGE_QUEUE_EVENT, PULL_REQUEST_COMMIT, HEAD_REF_FORCE_PUSHED_EVENT]) {
      nodes { __typename
        ... on RemovedFromMergeQueueEvent { id createdAt reason beforeCommit { oid } }
        ... on PullRequestCommit { id commit { committedDate pushedDate } }
        ... on HeadRefForcePushedEvent { id createdAt }
      }
    }
  } }
}`

// PRQueueTimeline reads a PR's merge-queue removals, commits and force-pushes,
// oldest first. It is a read; the server never writes to GitHub here.
func (client *Client) PRQueueTimeline(ctx context.Context, installationID int64, owner, repo string, pr int) ([]QueueTimelineItem, error) {
	token, err := client.installationToken(ctx, installationID)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(map[string]any{"query": queueTimelineQuery, "variables": map[string]any{"owner": owner, "name": repo, "number": pr}})
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(client.apiBase, "/")+"/graphql", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/vnd.github+json")
	response, err := client.httpClient.Do(request)
	if err != nil {
		return nil, errors.New("github: queue timeline request failed")
	}
	defer response.Body.Close()
	const limit = 4 << 20
	data, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil || len(data) > limit || response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("github: queue timeline unavailable (status %d)", response.StatusCode)
	}
	var result struct {
		Data struct {
			Repository *struct {
				PullRequest *struct {
					TimelineItems struct {
						Nodes []struct {
							Type         string    `json:"__typename"`
							ID           string    `json:"id"`
							CreatedAt    time.Time `json:"createdAt"`
							Reason       string    `json:"reason"`
							BeforeCommit *struct {
								OID string `json:"oid"`
							} `json:"beforeCommit"`
							Commit *struct {
								CommittedDate time.Time  `json:"committedDate"`
								PushedDate    *time.Time `json:"pushedDate"`
							} `json:"commit"`
						} `json:"nodes"`
					} `json:"timelineItems"`
				} `json:"pullRequest"`
			} `json:"repository"`
		} `json:"data"`
		Errors []json.RawMessage `json:"errors"`
	}
	if json.Unmarshal(data, &result) != nil || len(result.Errors) != 0 || result.Data.Repository == nil || result.Data.Repository.PullRequest == nil {
		return nil, errors.New("github: invalid queue timeline response")
	}
	nodes := result.Data.Repository.PullRequest.TimelineItems.Nodes
	items := make([]QueueTimelineItem, 0, len(nodes))
	for _, n := range nodes {
		it := QueueTimelineItem{Type: n.Type, NodeID: n.ID, At: n.CreatedAt, Reason: strings.ToUpper(n.Reason)}
		if n.BeforeCommit != nil {
			it.BeforeOID = n.BeforeCommit.OID
		}
		if n.Commit != nil {
			// A commit counts from when it reached the PR, not when it was authored.
			it.At = n.Commit.CommittedDate
			if n.Commit.PushedDate != nil {
				it.At = *n.Commit.PushedDate
			}
		}
		items = append(items, it)
	}
	return items, nil
}

// ListMergeGroupRuns lists the merge_group workflow runs of a head SHA.
func (client *Client) ListMergeGroupRuns(ctx context.Context, installationID int64, owner, repo, headSHA string) ([]QueueRun, error) {
	path := fmt.Sprintf("/repos/%s/%s/actions/runs?event=merge_group&head_sha=%s&per_page=100", owner, repo, headSHA)
	data, _, err := client.restDoHeader(ctx, installationID, path)
	if err != nil {
		return nil, err
	}
	var body struct {
		Runs []struct {
			ID         int64     `json:"id"`
			Attempt    int32     `json:"run_attempt"`
			Status     string    `json:"status"`
			Conclusion string    `json:"conclusion"`
			CreatedAt  time.Time `json:"created_at"`
			UpdatedAt  time.Time `json:"updated_at"`
		} `json:"workflow_runs"`
	}
	if err := json.Unmarshal(data, &body); err != nil {
		return nil, fmt.Errorf("github runs: decode: %w", err)
	}
	runs := make([]QueueRun, 0, len(body.Runs))
	for _, r := range body.Runs {
		runs = append(runs, QueueRun{ID: r.ID, Attempt: r.Attempt, Status: r.Status, Conclusion: r.Conclusion, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt})
	}
	return runs, nil
}
