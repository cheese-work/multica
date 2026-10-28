package handler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/integrations/ghrun"
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
		return writer.ApplyRunEvent(ctx, workspaceID, event)
	})
}

func (h *Handler) handleWorkflowJobEvent(ctx context.Context, body []byte) error {
	event, ok := ghrun.ParseJobEvent(body)
	if !ok {
		return nil
	}
	writer := h.defaultWorkflowWriter()
	return h.fanOutWorkflowEvent(ctx, event.InstallationID, func(workspaceID pgtype.UUID) error {
		return writer.ApplyJobEvent(ctx, workspaceID, event)
	})
}

// fanOutWorkflowEvent runs apply once per workspace bound to the installation
// whose GitHub switch is on. One workspace failing does not stop the others;
// every failure is joined. Writes are guarded, so a replay of the whole
// delivery is safe after a partial failure.
func (h *Handler) fanOutWorkflowEvent(ctx context.Context, installationID int64, apply func(pgtype.UUID) error) error {
	insts, err := h.Queries.ListGitHubInstallationsByInstallationID(ctx, installationID)
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
