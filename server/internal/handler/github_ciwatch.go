package handler

import (
	"context"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/multica-ai/multica/server/internal/ciwatch"
	"github.com/multica-ai/multica/server/internal/featureflags"
	"github.com/multica-ai/multica/server/internal/ghbudget"
	"github.com/multica-ai/multica/server/internal/integrations/ghrun"
	"github.com/multica-ai/multica/server/internal/integrations/ghsnapshot"
)

// ciWatcher records a completed workflow run for the CI failure watcher.
type ciWatcher interface {
	Watch(ctx context.Context, workspace pgtype.UUID, run ciwatch.Run, defaultBranch string) error
}

// newCIWatcher wires the watcher to this handler's database and flag.
func (h *Handler) newCIWatcher(gh ciwatch.GitHub, budget ciwatch.Budget) *ciwatch.Watcher {
	labeler := h.ciWatchLabeler()
	return &ciwatch.Watcher{
		FlagOn: func(ctx context.Context) bool { return featureflags.CIWatcherEnabled(ctx, h.FeatureFlags) },
		Config: h.Queries,
		Pipeline: ciwatch.Pipeline{
			Collector: &ciwatch.Collector{GH: gh, Budget: budget, Runs: ciwatch.DBRuns{Q: h.Queries}},
			Tx:        ciwatch.PgxTx(h.TxStarter),
			Observer: ciwatch.Observer{W: h.Queries, Labels: labeler, Flake: func(ctx context.Context, run ciwatch.Run, sig string) (int, error) {
				return labeler.FlakeCount7d(ctx, run.WorkspaceID, run.RepoOwner, run.RepoName, sig)
			}},
		},
	}
}

// watchCompletedRun hands a finished run to the CI watcher. A returned error
// fails the delivery so it is redelivered; the pipeline is idempotent.
func (h *Handler) watchCompletedRun(ctx context.Context, workspace pgtype.UUID, event ghrun.RunEvent) error {
	if h.ciWatch == nil || event.StatusRank != ghrun.RankCompleted {
		return nil
	}
	return h.ciWatch.Watch(ctx, workspace, ciwatch.Run{
		InstallationID: event.InstallationID, RepoOwner: event.RepoOwner, RepoName: event.RepoName,
		RunID: event.RunID, RunAttempt: event.RunAttempt, WorkflowPath: event.WorkflowPath, WorkflowName: event.WorkflowName,
		Event: event.Event, HeadBranch: event.HeadBranch, HeadSHA: event.HeadSHA, Conclusion: event.Conclusion,
	}, event.DefaultBranch)
}

// enableCIWatch turns the watcher on for an App-backed GitHub client. The
// installation ledger needs the pool, so a handler without one stays off. The
// ci_watcher flag still gates every run.
func (h *Handler) enableCIWatch(client *ghsnapshot.Client) {
	pool, ok := h.TxStarter.(*pgxpool.Pool)
	if !ok || !client.Enabled() {
		return
	}
	ledger := ghbudget.New(pool, ghbudget.FetcherFunc(client.RateLimit))
	h.ciWatch = h.newCIWatcher(ciwatch.AppGitHub{C: client}, ciwatch.LedgerBudget{L: ledger})
}
