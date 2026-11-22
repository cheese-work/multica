package handler

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/multica-ai/multica/server/internal/issuestatus"
	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// A manual move is a park (CHE-6460 D1c). Under decision_ask_park=on_wait a
// request leaves its issue running, so a step text that says "open a decision
// request, then move to need_decision" would otherwise hand the issue to the
// step's handler and lose the restore. The move is applied as the park
// instead: same status, same assignee, a park record and Readiness gated-human,
// so the answer puts the issue back.

// manualParkMove reports whether this status write is a manual park: the
// setting is on_wait, the write moves the issue to its park target, the issue
// has an open decision request and no park record, and the write names no
// assignee or handoff. Other writes, including every write under "always", are
// not parks. Only a write to need_decision or blocked reads the database.
func manualParkMove(ctx context.Context, q *db.Queries, prev db.Issue, params *db.UpdateIssueParams, statusExplicit, assigneeTouched bool, caller workflowCaller) (bool, error) {
	if !statusExplicit || assigneeTouched || caller.handoffAssignee != nil || !params.Status.Valid || params.Status.String == prev.Status || prev.TriageState.Valid ||
		(params.Status.String != idleGuardNeedDecision && params.Status.String != issuestatus.Blocked) || params.ProjectID != prev.ProjectID {
		return false, nil
	}
	workspace, err := q.GetWorkspace(ctx, prev.WorkspaceID)
	if err != nil || !decisionAskParkOnWaitSet(workspace.Settings) {
		return false, err
	}
	category, err := issuestatus.CategoryWithError(ctx, q, prev.WorkspaceID, prev.Status)
	if err != nil || category == issuestatus.CategoryDone || category == issuestatus.CategoryClosed {
		return false, err
	}
	target, err := decisionParkTarget(ctx, q, prev)
	if err != nil || target != params.Status.String {
		return false, err
	}
	open, err := q.HasOpenDecisionRequestForIssue(ctx, db.HasOpenDecisionRequestForIssueParams{WorkspaceID: prev.WorkspaceID, IssueID: prev.ID})
	if err != nil || !open {
		return false, err
	}
	if _, err := q.GetIssueDecisionPark(ctx, db.GetIssueDecisionParkParams{IssueID: prev.ID, WorkspaceID: prev.WorkspaceID}); err == nil {
		return false, nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return false, err
	}
	return true, nil
}

// markManualPark makes the status row of a park carry the decision cause, as
// the request's own park does; the actor stays the one who moved the issue.
// Call it before the status write.
func (w *workflowWrite) markManualPark(ctx context.Context) {
	if rec := statusRecordFrom(ctx); w != nil && w.park && rec != nil {
		rec.trigger.Cause.Kind = service.StatusCauseDecision
	}
}

// recordManualPark writes the park record and Readiness of an issue a manual
// move just put in its park target, and returns the issue as stored. Call it
// after the status write, in the same transaction.
func (w *workflowWrite) recordManualPark(ctx context.Context, q *db.Queries, moved db.Issue) (db.Issue, error) {
	if w == nil || !w.park {
		return moved, nil
	}
	if err := writeDecisionPark(ctx, q, moved, w.parkFrom, moved.Status); err != nil {
		return moved, err
	}
	return q.GetIssue(ctx, moved.ID)
}
