package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/integrations/ghrun"
	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// workflowWriter applies one workflow delivery to one workspace's mirror.
type workflowWriter interface {
	ApplyRunEvent(ctx context.Context, workspaceID pgtype.UUID, event ghrun.RunEvent) error
	ApplyJobEvent(ctx context.Context, workspaceID pgtype.UUID, event ghrun.JobEvent) error
}

// handleWorkflowRunEvent mirrors a workflow_run delivery into every bound
// workspace. A body redelivery cannot fix is dropped (nil); a storage error
// returns so the caller answers 5xx and the delivery shows as failed in the
// App's Recent Deliveries, for manual redelivery. GitHub does not retry.
func (h *Handler) handleWorkflowRunEvent(ctx context.Context, body []byte) error {
	event, ok := ghrun.ParseRunEvent(body)
	if !ok {
		slog.Warn("github: unusable workflow_run payload")
		return nil
	}
	writer := h.defaultWorkflowWriter()
	return h.fanOutWorkflowEvent(ctx, event.InstallationID, func(ws db.Workspace) error {
		workspaceID := ws.ID
		if err := writer.ApplyRunEvent(ctx, workspaceID, event); err != nil {
			return err
		}
		err := errors.Join(h.watchCompletedRun(ctx, workspaceID, event), h.reconcileRelease(ctx, ws, event))
		h.ciWatchRunCompleted(ctx, workspaceID, event)
		return err
	})
}

// handleWorkflowJobEvent mirrors a workflow_job delivery and, when it is a
// completed C00 deploy carrying a release receipt, opens the release issue in
// each bound workspace. Both writes are idempotent, so a failed delivery can be
// redelivered whole.
func (h *Handler) handleWorkflowJobEvent(ctx context.Context, body []byte) error {
	receipt, deployed := ghrun.ParseReleaseReceipt(body)
	event, ok := ghrun.ParseJobEvent(body)
	if !ok && !deployed {
		return nil
	}
	writer := h.defaultWorkflowWriter()
	installationID := event.InstallationID
	if deployed {
		installationID = receipt.InstallationID
	}
	return h.fanOutWorkflowEvent(ctx, installationID, func(ws db.Workspace) error {
		workspaceID := ws.ID
		var errs []error
		if ok {
			errs = append(errs, writer.ApplyJobEvent(ctx, workspaceID, event))
		}
		if deployed {
			errs = append(errs, h.openRelease(ctx, ws, receipt))
		}
		return errors.Join(errs...)
	})
}

// deployWorkflowPath is the workflow file of the C00 deploy.
const deployWorkflowPath = ".github/workflows/cd-deploy.yml"

type releaseCheck int

const (
	releaseRefused releaseCheck = iota // not the authorized deploy: final
	releasePending                     // the run that proves it has not arrived yet
	releaseOK
)

// checkRelease binds a receipt to this workspace. The job's own names are not
// proof: any repository the App is installed on can run a job called
// `release-receipt` that prints any SHA and digest. A receipt counts only when
// its repository is the workspace's configured release repository
// (`release_deploy_repo` in the workspace settings, "owner/name"), and its run
// was mirrored from the signed workflow_run delivery as the C00 deploy workflow
// file. A run with no path yet is a stub: its workflow_run delivery has not
// arrived, so the receipt is pending, not refused. A failed lookup is an error,
// never a refusal.
func (h *Handler) checkRelease(ctx context.Context, ws db.Workspace, r ghrun.ReleaseReceipt) (releaseCheck, error) {
	if !releaseRepoConfigured(ws, r.RepoOwner, r.RepoName) {
		slog.Warn("github: release receipt from a repository that is not the configured release repository", "repo", r.RepoOwner+"/"+r.RepoName, "run_id", r.RunID)
		return releaseRefused, nil
	}
	run, err := h.Queries.GetGitHubWorkflowRun(ctx, db.GetGitHubWorkflowRunParams{
		WorkspaceID: ws.ID, RepoOwner: r.RepoOwner, RepoName: r.RepoName, RunID: r.RunID,
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return releasePending, nil
	case err != nil:
		return releaseRefused, fmt.Errorf("load workflow run: %w", err)
	case run.WorkflowPath == "":
		return releasePending, nil
	case run.WorkflowPath != deployWorkflowPath:
		slog.Warn("github: release receipt from a run that is not the C00 deploy workflow", "repo", r.RepoOwner+"/"+r.RepoName, "run_id", r.RunID)
		return releaseRefused, nil
	}
	return releaseOK, nil
}

// releaseRepoConfigured reports whether owner/name (lowercase) is the
// workspace's release repository. Unset means no release issues.
func releaseRepoConfigured(ws db.Workspace, owner, name string) bool {
	var s struct {
		Repo string `json:"release_deploy_repo"`
	}
	if json.Unmarshal(ws.Settings, &s) != nil || s.Repo == "" {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(s.Repo), owner+"/"+name)
}

// pendingReleaseTTL is how long a receipt waits for its run before the sweep
// drops it: a run that never arrived in a week is not coming.
const pendingReleaseTTL = 7 * 24 * time.Hour

// openRelease stores the receipt, then settles it. The receipt is stored
// first so no later failure loses it: a lookup that fails leaves it for the
// sweep, and a run delivery that lists pending receipts concurrently either
// sees this row or committed its path before this row, which settle then reads.
// Only the configured release repository is stored, so other repositories
// cannot grow the table.
func (h *Handler) openRelease(ctx context.Context, ws db.Workspace, r ghrun.ReleaseReceipt) error {
	if !releaseRepoConfigured(ws, r.RepoOwner, r.RepoName) {
		slog.Warn("github: release receipt from a repository that is not the configured release repository", "repo", r.RepoOwner+"/"+r.RepoName, "run_id", r.RunID)
		return nil
	}
	if err := h.Queries.PutGitHubReleasePending(ctx, db.PutGitHubReleasePendingParams{
		WorkspaceID: ws.ID, RepoOwner: r.RepoOwner, RepoName: r.RepoName, RunID: r.RunID,
		OldSha: r.OldSHA, SourceSha: r.NewSHA, ImageDigest: r.Digest,
	}); err != nil {
		return fmt.Errorf("keep release receipt: %w", err)
	}
	_, err := h.settleRelease(ctx, ws, r)
	return err
}

// settleRelease decides a stored receipt: it opens the issue (deploy workflow)
// or discards the receipt (any other workflow), and leaves it while its run is
// unverified. The issue is created before the row is deleted, so a crash
// between them replays.
func (h *Handler) settleRelease(ctx context.Context, ws db.Workspace, r ghrun.ReleaseReceipt) (releaseCheck, error) {
	check, err := h.checkRelease(ctx, ws, r)
	if err != nil || check == releasePending {
		return check, err
	}
	if check == releaseOK {
		if _, _, err := h.IssueService.CreateRelease(ctx, service.ReleaseDeploy{
			WorkspaceID: ws.ID, RepoOwner: r.RepoOwner, RepoName: r.RepoName,
			RunID: r.RunID, OldSHA: r.OldSHA, NewSHA: r.NewSHA, Digest: r.Digest,
		}); err != nil {
			return check, err
		}
	}
	return check, h.Queries.DeleteGitHubReleasePending(ctx, db.DeleteGitHubReleasePendingParams{
		WorkspaceID: ws.ID, RepoOwner: r.RepoOwner, RepoName: r.RepoName, RunID: r.RunID,
	})
}

func receiptOf(p db.GithubReleasePending) ghrun.ReleaseReceipt {
	return ghrun.ReleaseReceipt{RepoOwner: p.RepoOwner, RepoName: p.RepoName, RunID: p.RunID, OldSHA: p.OldSha, NewSHA: p.SourceSha, Digest: p.ImageDigest}
}

// reconcileRelease settles the receipts of a run when its signed workflow_run
// delivery gives the run a workflow file.
func (h *Handler) reconcileRelease(ctx context.Context, ws db.Workspace, event ghrun.RunEvent) error {
	if event.WorkflowPath == "" {
		return nil
	}
	pending, err := h.Queries.ListGitHubReleasePendingForRun(ctx, db.ListGitHubReleasePendingForRunParams{
		WorkspaceID: ws.ID, RepoOwner: strings.ToLower(event.RepoOwner), RepoName: strings.ToLower(event.RepoName), RunID: event.RunID,
	})
	if err != nil {
		return fmt.Errorf("list pending release receipts: %w", err)
	}
	var errs []error
	for _, p := range pending {
		_, err := h.settleRelease(ctx, ws, receiptOf(p))
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// SweepReleaseReceipts is the automatic retry for stored receipts: a lookup
// that failed, or an interleaving that left a receipt behind a verified run.
// GitHub does not redeliver a failed webhook, so nothing else would settle
// them. A receipt whose run never arrives expires after pendingReleaseTTL.
func (h *Handler) SweepReleaseReceipts(ctx context.Context) error {
	pending, err := h.Queries.ListGitHubReleasePending(ctx)
	if err != nil {
		return fmt.Errorf("list pending release receipts: %w", err)
	}
	var errs []error
	for _, p := range pending {
		ws, err := h.Queries.GetWorkspace(ctx, p.WorkspaceID)
		if err != nil {
			errs = append(errs, fmt.Errorf("workspace %s: load workspace: %w", uuidToString(p.WorkspaceID), err))
			continue
		}
		check, err := h.settleRelease(ctx, ws, receiptOf(p))
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if check == releasePending && time.Since(p.CreatedAt.Time) > pendingReleaseTTL {
			errs = append(errs, h.Queries.DeleteGitHubReleasePending(ctx, db.DeleteGitHubReleasePendingParams{
				WorkspaceID: p.WorkspaceID, RepoOwner: p.RepoOwner, RepoName: p.RepoName, RunID: p.RunID,
			}))
		}
	}
	return errors.Join(errs...)
}

// fanOutWorkflowEvent runs apply once per workspace bound to the installation
// whose GitHub switch is on. One workspace failing does not stop the others;
// every failure is joined. Writes are guarded, so a replay of the whole
// delivery is safe after a partial failure.
func (h *Handler) fanOutWorkflowEvent(ctx context.Context, installationID int64, apply func(db.Workspace) error) error {
	insts, err := h.allowedGitHubInstallations(ctx, installationID)
	if err != nil {
		return fmt.Errorf("list installations by installation id: %w", err)
	}
	var errs []error
	for _, inst := range insts {
		ws, err := h.Queries.GetWorkspace(ctx, inst.WorkspaceID)
		if err != nil {
			errs = append(errs, fmt.Errorf("workspace %s: load workspace: %w", uuidToString(inst.WorkspaceID), err))
			continue
		}
		if !githubEnabledForWorkspace(ws) {
			continue
		}
		if err := apply(ws); err != nil {
			errs = append(errs, fmt.Errorf("workspace %s: %w", uuidToString(inst.WorkspaceID), err))
		}
	}
	return errors.Join(errs...)
}

type txWorkflowWriter struct{ tx txStarter }

func (w txWorkflowWriter) ApplyRunEvent(ctx context.Context, workspaceID pgtype.UUID, event ghrun.RunEvent) error {
	return ghrun.ApplyRunEvent(ctx, w.tx, workspaceID, event)
}

func (w txWorkflowWriter) ApplyJobEvent(ctx context.Context, workspaceID pgtype.UUID, event ghrun.JobEvent) error {
	return ghrun.ApplyJobEvent(ctx, w.tx, workspaceID, event)
}

func (h *Handler) defaultWorkflowWriter() workflowWriter {
	if h.workflowWriter != nil {
		return h.workflowWriter
	}
	return txWorkflowWriter{tx: h.TxStarter}
}
