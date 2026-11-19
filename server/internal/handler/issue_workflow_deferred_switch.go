package handler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/intakehold"
	"github.com/multica-ai/multica/server/internal/issuestatus"
	"github.com/multica-ai/multica/server/internal/issueworkflow"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// Deferred workflow switch (CHE-7669). An issue with a live task cannot change
// workflow (SetIssueWorkflow refuses it), so a caller that wants the switch
// parks it in issue_workflow_switch_pending, one row per issue. Every terminal
// task event tries to apply it; only the event that finds no other live task
// does.

// blockedReasonWorkflowSwitchRefused prefixes the blocked_reason of an issue
// whose deferred switch the target workflow refused.
const blockedReasonWorkflowSwitchRefused = "workflow-switch-refused"

// RegisterDeferredWorkflowSwitch applies a pending workflow switch on every
// terminal task event: completed, failed and cancelled.
func (h *Handler) RegisterDeferredWorkflowSwitch(bus *events.Bus) {
	for _, eventType := range []string{protocol.EventTaskCompleted, protocol.EventTaskFailed, protocol.EventTaskCancelled} {
		bus.Subscribe(eventType, func(e events.Event) {
			payload, _ := e.Payload.(map[string]any)
			issueID, _ := payload["issue_id"].(string)
			id, err := util.ParseUUID(issueID)
			if err != nil || !id.Valid {
				return
			}
			h.ApplyDeferredWorkflowSwitch(context.Background(), id)
		})
	}
}

// deferredSwitchRefusal is a switch the target workflow cannot take: the issue
// is blocked with the reason, instead of retrying on every later event.
type deferredSwitchRefusal struct {
	target pgtype.UUID
	cause  error
}

func (e *deferredSwitchRefusal) Error() string {
	return fmt.Sprintf("%s: target %s: %v", blockedReasonWorkflowSwitchRefused, uuidToString(e.target), e.cause)
}

func (e *deferredSwitchRefusal) Unwrap() error { return e.cause }

// isWorkflowRefusal reports whether err is the workflow saying no to a move, as
// opposed to a failure that a retry can clear.
func isWorkflowRefusal(err error) bool {
	var notIn *issueworkflow.NotInWorkflowError
	var notAllowed *issueworkflow.MoveNotAllowedError
	var held *intakehold.HoldError
	var gated *EntryGateError
	return errors.Is(err, issueworkflow.ErrOverrideMissing) || errors.As(err, &notIn) ||
		errors.As(err, &notAllowed) || errors.As(err, &held) || errors.As(err, &gated)
}

// ApplyDeferredWorkflowSwitch applies the issue's pending switch when no task
// of the issue can still run. It writes nothing when there is no row (a
// duplicate or unrelated event) or another live task remains; a failure that a
// retry can clear keeps the row for the next terminal event.
func (h *Handler) ApplyDeferredWorkflowSwitch(ctx context.Context, issueID pgtype.UUID) {
	pending, err := h.Queries.HasIssueWorkflowSwitchPending(ctx, issueID)
	if err != nil || !pending {
		if err != nil {
			slog.Warn("deferred workflow switch: read pending row failed", "err", err, "issue_id", uuidToString(issueID))
		}
		return
	}
	err = h.applyDeferredWorkflowSwitch(ctx, issueID)
	var refusal *deferredSwitchRefusal
	switch {
	case err == nil:
	case errors.As(err, &refusal):
		h.refuseDeferredWorkflowSwitch(ctx, issueID, refusal)
	default:
		slog.Warn("deferred workflow switch: apply failed, row kept", "err", err, "issue_id", uuidToString(issueID))
	}
}

func (h *Handler) applyDeferredWorkflowSwitch(ctx context.Context, issueID pgtype.UUID) error {
	loaded, err := h.Queries.GetIssue(ctx, issueID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	release, err := h.lockRunBudgetBoundary(ctx, issueID)
	if err != nil {
		return fmt.Errorf("run budget boundary: %w", err)
	}
	defer release()
	tx, err := h.beginWakeupWrite(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	qtx := h.Queries.WithTx(tx)
	// The lock order every issue writer takes: catalog, project, issue.
	if err := qtx.LockIssueStatusCatalogShared(ctx, loaded.WorkspaceID); err != nil {
		return err
	}
	if loaded.ProjectID.Valid {
		if _, err := qtx.LockProjectForIssueWrite(ctx, db.LockProjectForIssueWriteParams{ID: loaded.ProjectID, WorkspaceID: loaded.WorkspaceID}); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
	}
	issue, err := qtx.LockIssueForDescriptionUpdate(ctx, db.LockIssueForDescriptionUpdateParams{ID: issueID, WorkspaceID: loaded.WorkspaceID})
	if err != nil {
		return err
	}
	if issue.ProjectID != loaded.ProjectID {
		return errWorkflowChanged
	}
	// The live check comes first: while any task can still run, the row stays.
	live, err := qtx.HasLiveTaskForIssue(ctx, issue.ID)
	if err != nil || live {
		return err
	}
	row, err := qtx.DeleteIssueWorkflowSwitchPending(ctx, issue.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	refuse := func(cause error) error { return &deferredSwitchRefusal{target: row.WorkflowID, cause: cause} }

	def, _, err := issueworkflow.ForIssue(ctx, qtx, issue.WorkspaceID, row.WorkflowID, issue.ProjectID)
	if isWorkflowRefusal(err) {
		return refuse(err)
	}
	if err != nil {
		return err
	}
	if def == nil || issue.WorkflowID == row.WorkflowID {
		return tx.Commit(ctx)
	}
	to := issueworkflow.MoveTarget(*def, issue.Status, func(key string) string {
		return issuestatus.Category(ctx, qtx, issue.WorkspaceID, key)
	})
	if err := issueworkflow.CheckStatus(def, to); err != nil {
		return refuse(err)
	}
	rebound, err := qtx.SetIssueWorkflow(ctx, db.SetIssueWorkflowParams{ID: issue.ID, WorkspaceID: issue.WorkspaceID, WorkflowID: row.WorkflowID})
	if err != nil {
		return err
	}
	if service.IsFinishedStatus(ctx, qtx, issue.WorkspaceID, issue.Status) {
		// A finished issue follows the new workflow but is not handed to anyone.
		if err := tx.Commit(ctx); err != nil {
			return err
		}
		release()
		h.publishIssueSnapshots(ctx, []db.Issue{rebound}, "system", "")
		return nil
	}
	extra := map[string]any{"workflow_id": uuidToString(row.WorkflowID), "reason": row.Reason}
	move, err := h.moveIssueOnEdgeTxAs(ctx, qtx, rebound, to, "workflow_switch", extra, true, nil, workflowCaller{enterCurrent: true})
	if isWorkflowRefusal(err) {
		return refuse(err)
	}
	if err != nil {
		var notConfirmed *service.StopNotConfirmedError
		if errors.As(err, &notConfirmed) {
			_ = tx.Rollback(ctx)
			h.recordStopNotConfirmed(ctx, issue, "system", "", notConfirmed)
		}
		return err
	}
	if move.RunRefused != nil && !errors.Is(move.RunRefused, service.ErrDuplicatePendingTask) {
		return refuse(move.RunRefused)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit deferred workflow switch: %w", err)
	}
	release()
	move.AfterCommit(ctx)
	return nil
}

// refuseDeferredWorkflowSwitch settles a switch the target refused: the row is
// gone, the issue is blocked with workflow-switch-refused, and one system
// comment names the target and the cause.
func (h *Handler) refuseDeferredWorkflowSwitch(ctx context.Context, issueID pgtype.UUID, refusal *deferredSwitchRefusal) {
	issue, err := h.Queries.GetIssue(ctx, issueID)
	if err != nil {
		slog.Warn("deferred workflow switch: load issue failed", "err", err, "issue_id", uuidToString(issueID))
		return
	}
	reason := refusal.Error()
	var comment db.Comment
	var revision int64
	err = h.withPgxTx(ctx, func(tx pgx.Tx) error {
		qtx := h.Queries.WithTx(tx)
		// The apply rolled back with the row; consuming it again keeps a concurrent
		// settle from commenting twice.
		if _, err := qtx.DeleteIssueWorkflowSwitchPending(ctx, issueID); err != nil {
			return err
		}
		created, err := qtx.CreateComment(ctx, db.CreateCommentParams{
			ID: dbid.NewV7(), IssueID: issue.ID, WorkspaceID: issue.WorkspaceID,
			AuthorType: "system", AuthorID: pgtype.UUID{Valid: true}, Type: "system",
			Content: "Workflow switch refused (`" + blockedReasonWorkflowSwitchRefused + "`): " + reason +
				"\n\nThe issue keeps its current workflow and is blocked. Fix the target, then switch again.",
		})
		if err != nil {
			return err
		}
		comment, revision = created.Comment(), created.IssueRevision
		return nil
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return
	}
	if err != nil {
		slog.Warn("deferred workflow switch: settle refusal failed", "err", err, "issue_id", uuidToString(issueID))
		return
	}
	h.publish(protocol.EventCommentCreated, uuidToString(issue.WorkspaceID), "system", "", map[string]any{
		"comment":             commentToResponse(comment, nil, nil),
		"issue_title":         issue.Title,
		"issue_assignee_type": textToPtr(issue.AssigneeType),
		"issue_assignee_id":   uuidToPtr(issue.AssigneeID),
		"issue_status":        issue.Status,
		"issue_revision":      revision,
	})
	// The comment bumped the issue's revision: the move needs the row as it is now.
	issue, err = h.Queries.GetIssue(ctx, issueID)
	if err != nil {
		slog.Warn("deferred workflow switch: reload issue failed", "err", err, "issue_id", uuidToString(issueID))
		return
	}
	if issue.Status != issuestatus.Blocked && !service.IsFinishedStatus(ctx, h.Queries, issue.WorkspaceID, issue.Status) {
		def, _, err := issueworkflow.ForIssue(ctx, h.Queries, issue.WorkspaceID, issue.WorkflowID, issue.ProjectID)
		if err != nil || def == nil || !def.Has(issuestatus.Blocked) {
			slog.Warn("deferred workflow switch: cannot block issue", "err", err, "issue_id", uuidToString(issueID))
			return
		}
		moved, _, err := h.tryMoveIssueOnEdge(ctx, issue, issuestatus.Blocked, "workflow_switch_refused", map[string]any{"reason": reason}, false)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			slog.Warn("deferred workflow switch: block failed", "err", err, "issue_id", uuidToString(issueID))
		}
		if !moved {
			return
		}
	}
	if _, err := setIssueMetadataValue(ctx, h.Queries, issue.WorkspaceID, issue.ID, "blocked_reason", reason); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		slog.Warn("deferred workflow switch: set blocked_reason failed", "err", err, "issue_id", uuidToString(issueID))
	}
}
