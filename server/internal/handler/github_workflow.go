package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

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
	return h.fanOutWorkflowEvent(ctx, event.InstallationID, func(workspaceID pgtype.UUID) error {
		if err := writer.ApplyRunEvent(ctx, workspaceID, event); err != nil {
			return err
		}
		err := h.watchCompletedRun(ctx, workspaceID, event)
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
	return h.fanOutWorkflowEvent(ctx, installationID, func(workspaceID pgtype.UUID) error {
		var errs []error
		if ok {
			errs = append(errs, writer.ApplyJobEvent(ctx, workspaceID, event))
		}
		if deployed && h.releaseAuthorized(ctx, workspaceID, receipt) {
			_, _, err := h.IssueService.CreateRelease(ctx, service.ReleaseDeploy{
				WorkspaceID: workspaceID, RepoOwner: receipt.RepoOwner, RepoName: receipt.RepoName,
				RunID: receipt.RunID, OldSHA: receipt.OldSHA, NewSHA: receipt.NewSHA, Digest: receipt.Digest,
			})
			errs = append(errs, err)
		}
		return errors.Join(errs...)
	})
}

// deployWorkflowPath is the workflow file of the C00 deploy.
const deployWorkflowPath = ".github/workflows/cd-deploy.yml"

// releaseAuthorized binds a receipt to this workspace. The job's own names are
// not proof: any repository the App is installed on can run a job called
// `release-receipt` that prints any SHA and digest. A receipt counts only when
// its repository is one the workspace declared, and its run was mirrored from
// the signed workflow_run delivery as the C00 deploy workflow file. A refusal is
// final for this delivery (nil), not an error to retry.
func (h *Handler) releaseAuthorized(ctx context.Context, workspaceID pgtype.UUID, r ghrun.ReleaseReceipt) bool {
	ws, err := h.Queries.GetWorkspace(ctx, workspaceID)
	if err != nil || !workspaceDeclaresRepo(ws, r.RepoOwner, r.RepoName) {
		slog.Warn("github: release receipt from an undeclared repository", "repo", r.RepoOwner+"/"+r.RepoName, "run_id", r.RunID)
		return false
	}
	run, err := h.Queries.GetGitHubWorkflowRun(ctx, db.GetGitHubWorkflowRunParams{
		WorkspaceID: workspaceID, RepoOwner: r.RepoOwner, RepoName: r.RepoName, RunID: r.RunID,
	})
	if err != nil || run.WorkflowPath != deployWorkflowPath {
		slog.Warn("github: release receipt from a run that is not the C00 deploy workflow", "repo", r.RepoOwner+"/"+r.RepoName, "run_id", r.RunID)
		return false
	}
	return true
}

// workspaceDeclaresRepo reports whether the workspace's repo list names
// owner/name on github.com (https or ssh URL, case-insensitive).
func workspaceDeclaresRepo(ws db.Workspace, owner, name string) bool {
	var repos []workspaceRepoRef
	if json.Unmarshal(ws.Repos, &repos) != nil {
		return false
	}
	want := owner + "/" + name
	for _, repo := range repos {
		u := strings.ToLower(strings.TrimSpace(repo.URL))
		u = strings.TrimSuffix(strings.TrimSuffix(u, "/"), ".git")
		for _, prefix := range []string{"https://github.com/", "http://github.com/", "git@github.com:", "ssh://git@github.com/"} {
			if strings.TrimPrefix(u, prefix) == want && u != want {
				return true
			}
		}
	}
	return false
}

// fanOutWorkflowEvent runs apply once per workspace bound to the installation
// whose GitHub switch is on. One workspace failing does not stop the others;
// every failure is joined. Writes are guarded, so a replay of the whole
// delivery is safe after a partial failure.
func (h *Handler) fanOutWorkflowEvent(ctx context.Context, installationID int64, apply func(pgtype.UUID) error) error {
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
		if err := apply(inst.WorkspaceID); err != nil {
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
