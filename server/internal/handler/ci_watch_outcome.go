package handler

import (
	"context"
	"log/slog"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/ciwatch"
	"github.com/multica-ai/multica/server/internal/featureflags"
	"github.com/multica-ai/multica/server/internal/integrations/ghrun"
)

// CI watcher outcome labelling (CHE-4280). Both hooks sit behind the
// ci_watcher flag and only log on failure: labels are shadow data, so they
// must never fail a webhook delivery or an issue update.

func (h *Handler) ciWatchLabeler() *ciwatch.Labeler { return ciwatch.NewLabeler(h.DB, nil) }

// ciWatchRunCompleted labels earlier failures once a run finished and counts
// flakes. Call it after the run mirror row is written.
func (h *Handler) ciWatchRunCompleted(ctx context.Context, workspaceID pgtype.UUID, e ghrun.RunEvent) {
	if e.StatusRank != ghrun.RankCompleted || !featureflags.CIWatcherEnabled(ctx, h.FeatureFlags) {
		return
	}
	err := h.ciWatchLabeler().RunCompleted(ctx, ciwatch.Run{
		WorkspaceID: uuidToString(workspaceID), InstallationID: e.InstallationID,
		RepoOwner: e.RepoOwner, RepoName: e.RepoName, RunID: e.RunID, RunAttempt: e.RunAttempt,
		WorkflowPath: e.WorkflowPath, WorkflowName: e.WorkflowName, Event: e.Event,
		HeadBranch: e.HeadBranch, HeadSHA: e.HeadSHA, Conclusion: e.Conclusion,
	})
	if err != nil {
		slog.Warn("ci watch: label completed run", "run_id", e.RunID, "error", err)
	}
}

// ciWatchHumanOverride marks the open failures a person took over by acting on
// the signature's owner issue.
func (h *Handler) ciWatchHumanOverride(ctx context.Context, workspaceID, issueID pgtype.UUID) {
	if !featureflags.CIWatcherEnabled(ctx, h.FeatureFlags) {
		return
	}
	if err := h.ciWatchLabeler().OverrideByIssue(ctx, uuidToString(workspaceID), uuidToString(issueID)); err != nil {
		slog.Warn("ci watch: mark human override", "issue_id", uuidToString(issueID), "error", err)
	}
}
