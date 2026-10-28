package scheduler

import (
	"context"
	"fmt"
	"time"

	"github.com/multica-ai/multica/server/internal/integrations/ghrun"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const JobNameGitHubWorkflowRetention = "github_workflow_retention"

type githubWorkflowExpirer interface {
	DeleteExpired(ctx context.Context, cutoff time.Time) (int64, error)
}

type githubWorkflowQueriesExpirer struct{ queries *db.Queries }

func (e githubWorkflowQueriesExpirer) DeleteExpired(ctx context.Context, cutoff time.Time) (int64, error) {
	return ghrun.DeleteExpired(ctx, e.queries, cutoff)
}

// GitHubWorkflowRetentionJob drops mirrored workflow run and job rows that
// have had no event for ghrun.Retention (CHE-2147).
func GitHubWorkflowRetentionJob(queries *db.Queries) JobSpec {
	return JobSpec{
		Name:              JobNameGitHubWorkflowRetention,
		Cadence:           time.Hour,
		ScheduleDelay:     5 * time.Minute,
		CatchUpMode:       CatchUpLatestOnly,
		CatchUpWindow:     24 * time.Hour,
		RunTimeout:        10 * time.Minute,
		StaleTimeout:      15 * time.Minute,
		HeartbeatInterval: 30 * time.Second,
		AllowStaleReentry: true,
		MaxAttempts:       3,
		RetryBackoff:      []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute},
		Scopes:            StaticScopes(ScopeGlobal),
		Handler:           makeGitHubWorkflowRetentionHandler(githubWorkflowQueriesExpirer{queries}, time.Now),
	}
}

func makeGitHubWorkflowRetentionHandler(expirer githubWorkflowExpirer, now func() time.Time) Handler {
	return func(ctx context.Context, _ HandlerInput) (HandlerResult, error) {
		deleted, err := expirer.DeleteExpired(ctx, now().Add(-ghrun.Retention))
		if err != nil {
			return HandlerResult{}, fmt.Errorf("delete expired github workflow rows: %w", err)
		}
		return HandlerResult{RowsAffected: deleted}, nil
	}
}
