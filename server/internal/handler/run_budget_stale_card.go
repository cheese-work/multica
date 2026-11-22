package handler

import (
	"context"
	"errors"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/commentguard"
	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// A raise or top-up card names the run window it was asked in. When the issue
// leaves that window, the card can neither preview nor apply, and the client
// shows "Details unavailable" for it (CHE-6882). After a run-budget action
// lands, the issue's other open run-budget cards of an older window are
// withdrawn. Best effort: the approval has committed and never depends on it.
// A window that a status move rolls does not trigger this sweep.
func (handler *Handler) withdrawStaleRunBudgetCards(ctx context.Context, action db.DecisionAction) {
	if action.ActionType != runBudgetRaiseType && action.ActionType != runBudgetTopUpType {
		return
	}
	var withdrawn []decisionRequestResult
	var workspaceID pgtype.UUID
	err := handler.withPgxTx(ctx, func(tx pgx.Tx) error {
		q := handler.Queries.WithTx(tx)
		own, err := q.GetDecisionRequest(ctx, db.GetDecisionRequestParams{WorkspaceID: action.WorkspaceID, ID: action.DecisionRequestID})
		if err != nil {
			return err
		}
		workspaceID = own.WorkspaceID
		issue, err := q.GetIssueInWorkspace(ctx, db.GetIssueInWorkspaceParams{WorkspaceID: own.WorkspaceID, ID: own.IssueID})
		if err != nil {
			return err
		}
		snapshot, err := service.LoadRunBudgetSnapshot(ctx, q, issue)
		if err != nil {
			return err
		}
		if !snapshot.Budgeted {
			return nil
		}
		rows, err := tx.Query(ctx, `
SELECT request.id FROM decision_request request
JOIN decision_action card_action ON card_action.decision_request_id = request.id
WHERE request.workspace_id = $1 AND request.issue_id = $2 AND request.id <> $3
  AND request.status = 'open' AND card_action.state = 'pending'
  AND card_action.action_type IN ($4, $5)
  AND card_action.params->>'window_id' IS DISTINCT FROM $6`,
			own.WorkspaceID, own.IssueID, own.ID, runBudgetRaiseType, runBudgetTopUpType, snapshot.WindowID)
		if err != nil {
			return err
		}
		var stale []pgtype.UUID
		for rows.Next() {
			var id pgtype.UUID
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			stale = append(stale, id)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, id := range stale {
			done, err := withdrawStaleRunBudgetCard(ctx, q, own.WorkspaceID, id)
			if err != nil {
				return err
			}
			withdrawn = append(withdrawn, done)
		}
		return nil
	})
	if err != nil {
		slog.Warn("run budget: withdrawing stale cards failed", "action_id", uuidToString(action.ID), "error", err)
		return
	}
	for _, done := range withdrawn {
		if !done.Comment.ID.Valid {
			continue
		}
		handler.publishDecisionWithdrawal(uuidToString(workspaceID), done)
	}
}

// withdrawStaleRunBudgetCard takes the locks a withdrawal takes, then cancels
// the card as the agent that asked for it.
func withdrawStaleRunBudgetCard(ctx context.Context, q *db.Queries, workspaceID, id pgtype.UUID) (decisionRequestResult, error) {
	empty := decisionRequestResult{}
	card, err := q.GetDecisionRequest(ctx, db.GetDecisionRequestParams{WorkspaceID: workspaceID, ID: id})
	if err != nil {
		return empty, err
	}
	if _, err := q.LockDecisionIssue(ctx, db.LockDecisionIssueParams{WorkspaceID: card.WorkspaceID, ID: card.IssueID}); err != nil {
		return empty, err
	}
	if _, err := q.LockDecisionSession(ctx, db.LockDecisionSessionParams{WorkspaceID: card.WorkspaceID, ID: card.ChatSessionID}); err != nil {
		return empty, err
	}
	root, err := commentguard.LockThreadForReplyAndLoadRoot(ctx, q, card.WorkspaceID, card.IssueCommentID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return empty, err
	}
	current, err := q.LockDecisionRequest(ctx, db.LockDecisionRequestParams{WorkspaceID: card.WorkspaceID, ID: card.ID})
	if err != nil {
		return empty, err
	}
	if current.Status != "open" {
		return empty, nil
	}
	return withdrawOpenDecision(ctx, q, current, root, "The run window it named is no longer open.", "agent", current.AskingAgentID, pgtype.UUID{}, pgtype.UUID{})
}
