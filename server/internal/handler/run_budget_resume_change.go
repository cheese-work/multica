package handler

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/decisionaction"
	"github.com/multica-ai/multica/server/internal/issueworkflow"
	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// resumeRefusalsAfterBudgetChange runs after an executed action committed. A
// workflow budget change or an issue rebinding gives refused issues room again
// without a second card (CHE-5508): the refusals of the issues it reaches that
// now have balance are linked to the action, then the P3 worker resumes them
// once each (keyed by refusal id). Every other action only resumes what it
// linked in its own transaction. The link is idempotent, so a retry of the
// post-commit work links nothing twice.
func (h *Handler) resumeRefusalsAfterBudgetChange(ctx context.Context, action db.DecisionAction) error {
	if err := h.linkRefusalsAfterBudgetChange(ctx, action); err != nil {
		return err
	}
	return h.notifyRunBudgetResume(ctx, action.ID)
}

func (h *Handler) linkRefusalsAfterBudgetChange(ctx context.Context, action db.DecisionAction) error {
	var issueID, workflowID pgtype.UUID
	switch action.ActionType {
	case decisionaction.WorkflowBudgetSet:
		var params decisionaction.WorkflowBudgetSetParams
		if err := json.Unmarshal(action.Params, &params); err != nil {
			return err
		}
		id, err := parseUUIDString(params.WorkflowID)
		if err != nil {
			return err
		}
		workflowID = id
	case issueWorkflowSetType:
		var params decisionaction.IssueWorkflowSetParams
		if err := json.Unmarshal(action.Params, &params); err != nil {
			return err
		}
		id, err := parseUUIDString(params.IssueID)
		if err != nil {
			return err
		}
		issueID = id
	default:
		return nil
	}
	waiting, err := h.Queries.ListWaitingRunBudgetRefusals(ctx, db.ListWaitingRunBudgetRefusalsParams{WorkspaceID: action.WorkspaceID, IssueID: issueID})
	if err != nil {
		return err
	}
	for _, refusal := range waiting {
		if err := h.linkRefusalIfFunded(ctx, action, refusal, workflowID); err != nil {
			return err
		}
	}
	return nil
}

// linkRefusalIfFunded links one refusal when its issue follows workflowID (any
// workflow when unset) and now has a balance above 0; otherwise it leaves the
// refusal as it is. The boundary lock comes first, the order a top-up takes.
func (h *Handler) linkRefusalIfFunded(ctx context.Context, action db.DecisionAction, refusal db.ListWaitingRunBudgetRefusalsRow, workflowID pgtype.UUID) error {
	return h.withPgxTx(ctx, func(tx pgx.Tx) error {
		q := h.Queries.WithTx(tx)
		if err := q.LockIssueRunBudgetBoundary(ctx, refusal.IssueID); err != nil {
			return err
		}
		if workflowID.Valid {
			issue, err := q.GetIssue(ctx, refusal.IssueID)
			if errors.Is(err, pgx.ErrNoRows) { // the issue is gone; its refusal waits for nothing
				return nil
			}
			if err != nil {
				return err
			}
			def, _, err := issueworkflow.ForIssue(ctx, q, issue.WorkspaceID, issue.WorkflowID, issue.ProjectID)
			if errors.Is(err, issueworkflow.ErrOverrideMissing) || (err == nil && (def == nil || def.ID != workflowID)) {
				return nil
			}
			if err != nil {
				return err
			}
		}
		balance, window, err := service.OpenRunBudgetBalance(ctx, q, refusal.IssueID)
		if err != nil || window == "" || balance <= 0 {
			return err
		}
		return linkRunBudgetRefusal(ctx, q, refusal.IssueID, uuidToString(refusal.ID), action.ID, window)
	})
}
