package handler

import (
	"context"
	"encoding/json"
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

// lockedDeferredSwitchIssue is the issue row, locked in the order every issue
// writer takes (budget boundary, catalog, project, issue) inside one
// transaction. close rolls back what was not committed and releases the lock.
type lockedDeferredSwitchIssue struct {
	tx      pgx.Tx
	qtx     *db.Queries
	issue   db.Issue
	release func()
}

func (l *lockedDeferredSwitchIssue) close(ctx context.Context) {
	_ = l.tx.Rollback(ctx)
	l.release()
}

// lockDeferredSwitchIssue returns pgx.ErrNoRows when the issue is gone.
func (h *Handler) lockDeferredSwitchIssue(ctx context.Context, issueID pgtype.UUID) (*lockedDeferredSwitchIssue, error) {
	loaded, err := h.Queries.GetIssue(ctx, issueID)
	if err != nil {
		return nil, err
	}
	release, err := h.lockRunBudgetBoundary(ctx, issueID)
	if err != nil {
		return nil, fmt.Errorf("run budget boundary: %w", err)
	}
	tx, err := h.beginWakeupWrite(ctx)
	if err != nil {
		release()
		return nil, err
	}
	lk := &lockedDeferredSwitchIssue{tx: tx, qtx: h.Queries.WithTx(tx), release: release}
	fail := func(err error) (*lockedDeferredSwitchIssue, error) {
		lk.close(ctx)
		return nil, err
	}
	if err := lk.qtx.LockIssueStatusCatalogShared(ctx, loaded.WorkspaceID); err != nil {
		return fail(err)
	}
	if loaded.ProjectID.Valid {
		if _, err := lk.qtx.LockProjectForIssueWrite(ctx, db.LockProjectForIssueWriteParams{ID: loaded.ProjectID, WorkspaceID: loaded.WorkspaceID}); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fail(err)
		}
	}
	lk.issue, err = lk.qtx.LockIssueForDescriptionUpdate(ctx, db.LockIssueForDescriptionUpdateParams{ID: issueID, WorkspaceID: loaded.WorkspaceID})
	if err != nil {
		return fail(err)
	}
	if lk.issue.ProjectID != loaded.ProjectID {
		return fail(errWorkflowChanged)
	}
	return lk, nil
}

func (h *Handler) applyDeferredWorkflowSwitch(ctx context.Context, issueID pgtype.UUID) error {
	lk, err := h.lockDeferredSwitchIssue(ctx, issueID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	defer lk.close(ctx)
	tx, qtx, issue, release := lk.tx, lk.qtx, lk.issue, lk.release
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

// refuseDeferredWorkflowSwitch settles a switch the target refused, in one
// transaction: the row is consumed, the issue is blocked, blocked_reason is
// set and one system comment names the target and the cause. Any failure rolls
// all of it back, so the row stays and the next terminal event refuses again.
// A request that cannot be blocked (a workflow with no blocked step or no way
// to blocked) keeps its row, so it recovers once the workflow can block; the
// comment and blocked_reason are written once per cause, not on every event. A
// finished issue has nothing to block: its row is consumed with the comment.
func (h *Handler) refuseDeferredWorkflowSwitch(ctx context.Context, issueID pgtype.UUID, refusal *deferredSwitchRefusal) {
	err := h.settleDeferredSwitchRefusal(ctx, issueID, refusal)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		slog.Warn("deferred workflow switch: settle refusal failed, row kept", "err", err, "issue_id", uuidToString(issueID))
	}
}

func (h *Handler) settleDeferredSwitchRefusal(ctx context.Context, issueID pgtype.UUID, refusal *deferredSwitchRefusal) error {
	lk, err := h.lockDeferredSwitchIssue(ctx, issueID)
	if err != nil {
		return err
	}
	defer lk.close(ctx)
	qtx, issue := lk.qtx, lk.issue
	// The apply rolled back with the row. A concurrent settle may have consumed
	// it since; settling twice would comment twice.
	pending, err := qtx.HasIssueWorkflowSwitchPending(ctx, issueID)
	if err != nil || !pending {
		return err
	}
	reason := refusal.Error()
	blocked, finished := issue.Status == issuestatus.Blocked, service.IsFinishedStatus(ctx, qtx, issue.WorkspaceID, issue.Status)
	var move *edgeMove
	if !blocked && !finished {
		def, _, err := issueworkflow.ForIssue(ctx, qtx, issue.WorkspaceID, issue.WorkflowID, issue.ProjectID)
		if err != nil && !isWorkflowRefusal(err) {
			return err
		}
		if err == nil && def != nil && def.Has(issuestatus.Blocked) {
			m, err := h.moveIssueOnEdgeTx(ctx, qtx, issue, issuestatus.Blocked, "workflow_switch_refused", map[string]any{"reason": reason}, false, nil)
			switch {
			case err == nil:
				move, blocked = &m, true
				issue = m.Issue
			case isWorkflowRefusal(err):
				slog.Warn("deferred workflow switch: workflow refuses blocking, recording the reason only", "err", err, "issue_id", uuidToString(issueID))
			default:
				var notConfirmed *service.StopNotConfirmedError
				if errors.As(err, &notConfirmed) {
					_ = lk.tx.Rollback(ctx)
					h.recordStopNotConfirmed(ctx, issue, "system", "", notConfirmed)
				}
				return err
			}
		}
	}
	if blocked || finished {
		if _, err := qtx.DeleteIssueWorkflowSwitchPending(ctx, issueID); err != nil {
			return err
		}
	} else if recordedBlockedReason(issue.Metadata) == reason {
		// Kept for recovery and already recorded: write nothing again.
		return nil
	}
	outcome := "The issue keeps its current workflow and is blocked. Fix the target, then switch again."
	if !blocked {
		outcome = "The issue keeps its current workflow. It could not be blocked: its status or workflow has no way to blocked. The request is kept and applies again when the workflow can block or the target is fixed."
	}
	created, err := qtx.CreateComment(ctx, db.CreateCommentParams{
		ID: dbid.NewV7(), IssueID: issue.ID, WorkspaceID: issue.WorkspaceID,
		AuthorType: "system", AuthorID: pgtype.UUID{Valid: true}, Type: "system",
		Content: "Workflow switch refused (`" + blockedReasonWorkflowSwitchRefused + "`): " + reason + "\n\n" + outcome,
	})
	if err != nil {
		return err
	}
	// An unchanged value updates no row.
	if recordedBlockedReason(issue.Metadata) != reason {
		if _, err := setIssueMetadataValue(ctx, qtx, issue.WorkspaceID, issue.ID, "blocked_reason", reason); err != nil {
			return err
		}
	}
	if err := lk.tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit workflow switch refusal: %w", err)
	}
	lk.release()
	if move != nil {
		move.AfterCommit(ctx)
	}
	h.publish(protocol.EventCommentCreated, uuidToString(issue.WorkspaceID), "system", "", map[string]any{
		"comment":             commentToResponse(created.Comment(), nil, nil),
		"issue_title":         issue.Title,
		"issue_assignee_type": textToPtr(issue.AssigneeType),
		"issue_assignee_id":   uuidToPtr(issue.AssigneeID),
		"issue_status":        issue.Status,
		"issue_revision":      created.IssueRevision,
	})
	return nil
}

func recordedBlockedReason(metadata []byte) string {
	var m struct {
		BlockedReason string `json:"blocked_reason"`
	}
	_ = json.Unmarshal(metadata, &m)
	return m.BlockedReason
}
