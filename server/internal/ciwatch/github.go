package ciwatch

import (
	"context"
	"errors"
	"io"
	"net/http"

	"github.com/multica-ai/multica/server/internal/integrations/ghsnapshot"
)

// AppClient is the part of the GitHub App client the collector uses.
type AppClient interface {
	ListRunJobs(ctx context.Context, installationID int64, owner, repo string, runID int64, attempt int32, page int) ([]ghsnapshot.ActionJob, bool, http.Header, error)
	JobLog(ctx context.Context, installationID int64, owner, repo string, jobID int64) (io.ReadCloser, http.Header, error)
}

// AppGitHub adapts the App client to GitHub, mapping its throttle error.
type AppGitHub struct{ C AppClient }

func (g AppGitHub) ListJobs(ctx context.Context, run Run, page int) ([]Job, bool, http.Header, error) {
	js, more, h, err := g.C.ListRunJobs(ctx, run.InstallationID, run.RepoOwner, run.RepoName, run.RunID, run.RunAttempt, page)
	if err != nil {
		return nil, false, nil, mapRateLimit(err)
	}
	jobs := make([]Job, len(js))
	for i, j := range js {
		jobs[i] = Job{ID: j.ID, Name: j.Name, Conclusion: j.Conclusion, CompletedAt: j.CompletedAt, RunnerLost: j.RunnerLost}
	}
	return jobs, more, h, nil
}

func (g AppGitHub) JobLog(ctx context.Context, run Run, jobID int64) (io.ReadCloser, http.Header, error) {
	body, h, err := g.C.JobLog(ctx, run.InstallationID, run.RepoOwner, run.RepoName, jobID)
	return body, h, mapRateLimit(err)
}

func mapRateLimit(err error) error {
	var rl *ghsnapshot.RateLimitError
	if errors.As(err, &rl) {
		return &RateLimitError{RetryAfter: rl.RetryAfter, Header: rl.Header}
	}
	return err
}
