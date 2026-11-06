package ghsnapshot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ActionJob is one job of a workflow run attempt.
type ActionJob struct {
	ID          int64
	Name        string
	Conclusion  string
	CompletedAt time.Time
	// RunnerLost: the job failed with a step that never completed, which is
	// how GitHub reports a runner that vanished mid-job.
	RunnerLost bool
}

// ListRunJobs reads one page (100 jobs) of the latest jobs of a run attempt.
func (client *Client) ListRunJobs(ctx context.Context, installationID int64, owner, repo string, runID int64, attempt int32, page int) ([]ActionJob, bool, error) {
	path := fmt.Sprintf("/repos/%s/%s/actions/runs/%d/attempts/%d/jobs?filter=latest&page=%d&per_page=100", owner, repo, runID, attempt, page)
	data, header, err := client.restDoHeader(ctx, installationID, path)
	if err != nil {
		return nil, false, err
	}
	var body struct {
		Jobs []struct {
			ID          int64     `json:"id"`
			Name        string    `json:"name"`
			Conclusion  string    `json:"conclusion"`
			CompletedAt time.Time `json:"completed_at"`
			Steps       []struct {
				Status string `json:"status"`
			} `json:"steps"`
		} `json:"jobs"`
	}
	if err := json.Unmarshal(data, &body); err != nil {
		return nil, false, fmt.Errorf("github jobs: decode: %w", err)
	}
	jobs := make([]ActionJob, 0, len(body.Jobs))
	for _, j := range body.Jobs {
		job := ActionJob{ID: j.ID, Name: j.Name, Conclusion: j.Conclusion, CompletedAt: j.CompletedAt}
		for _, s := range j.Steps {
			job.RunnerLost = job.RunnerLost || (j.Conclusion == "failure" && s.Status != "completed")
		}
		jobs = append(jobs, job)
	}
	return jobs, strings.Contains(header.Get("Link"), `rel="next"`), nil
}

// JobLog streams a job log from byte 0. GitHub answers 302 to a signed blob
// URL; the client follows it and drops the token for the other host. The body
// is not read into memory: the caller bounds it. No Range header is sent.
func (client *Client) JobLog(ctx context.Context, installationID int64, owner, repo string, jobID int64) (io.ReadCloser, error) {
	token, err := client.installationToken(ctx, installationID)
	if err != nil {
		return nil, err
	}
	endpoint := strings.TrimRight(client.apiBase, "/") + fmt.Sprintf("/repos/%s/%s/actions/jobs/%d/logs", owner, repo, jobID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	// Go keeps Authorization across ports of one hostname; the signed blob
	// URL must never see the installation token, so drop it on any host change.
	hc := *client.httpClient
	hc.CheckRedirect = func(next *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("github job log: too many redirects")
		}
		if next.URL.Host != via[0].URL.Host {
			next.Header.Del("Authorization")
		}
		return nil
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusOK {
		return resp.Body, nil
	}
	defer resp.Body.Close()
	small, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if isRateLimited(resp, small) {
		return nil, rateLimitFromResponse(resp, client.now())
	}
	return nil, &StatusError{Status: resp.StatusCode}
}
