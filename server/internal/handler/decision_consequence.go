package handler

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/commentguard"
	"github.com/multica-ai/multica/server/internal/decisiontx"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func (handler *Handler) CheckDecisionSource(ctx context.Context, workspaceID, requestID pgtype.UUID) (decisiontx.Withdrawal, error) {
	return handler.checkDecisionAnswer(ctx, workspaceID, requestID, false)
}

func (handler *Handler) FinalizeDecisionAnswer(ctx context.Context, workspaceID, requestID pgtype.UUID) (decisiontx.Withdrawal, error) {
	return handler.checkDecisionAnswer(ctx, workspaceID, requestID, true)
}

func (handler *Handler) checkDecisionAnswer(ctx context.Context, workspaceID, requestID pgtype.UUID, finalize bool) (decisiontx.Withdrawal, error) {
	empty := decisiontx.Withdrawal{}
	located, err := handler.Queries.GetDecisionRequest(ctx, db.GetDecisionRequestParams{WorkspaceID: workspaceID, ID: requestID})
	if errors.Is(err, pgx.ErrNoRows) {
		return empty, nil
	}
	if err != nil {
		return empty, err
	}
	transaction, err := handler.TxStarter.Begin(ctx)
	if err != nil {
		return empty, err
	}
	defer transaction.Rollback(ctx)
	queries := handler.Queries.WithTx(transaction)
	if _, err := queries.LockWorkspaceForChatSessionCreate(ctx, workspaceID); err != nil {
		return empty, err
	}
	if _, err := queries.LockDecisionSession(ctx, db.LockDecisionSessionParams{WorkspaceID: workspaceID, ID: located.ChatSessionID}); errors.Is(err, pgx.ErrNoRows) {
		return empty, nil
	} else if err != nil {
		return empty, err
	}
	root, err := commentguard.LockThreadForReplyAndLoadRoot(ctx, queries, workspaceID, located.IssueCommentID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return empty, err
	}
	if _, err := queries.LockDecisionIssue(ctx, db.LockDecisionIssueParams{WorkspaceID: workspaceID, ID: located.IssueID}); errors.Is(err, pgx.ErrNoRows) {
		return empty, nil
	} else if err != nil {
		return empty, err
	}
	current, err := queries.LockDecisionRequest(ctx, db.LockDecisionRequestParams{WorkspaceID: workspaceID, ID: requestID})
	if errors.Is(err, pgx.ErrNoRows) {
		return empty, nil
	}
	if err != nil {
		return empty, err
	}
	if current.ChatSessionID != located.ChatSessionID || current.IssueID != located.IssueID || current.IssueCommentID != located.IssueCommentID {
		return empty, errors.New("decision source context changed; retry")
	}
	answer, err := queries.LockDecisionLiveAnswer(ctx, db.LockDecisionLiveAnswerParams{WorkspaceID: workspaceID, ID: requestID})
	if errors.Is(err, pgx.ErrNoRows) {
		return empty, nil
	}
	if err != nil {
		return empty, err
	}
	reason := "source_removed"
	if finalize {
		if answer.WakeAt.Valid {
			return empty, nil
		}
		task, err := queries.GetAgentTask(ctx, answer.AnsweredByTaskID)
		if errors.Is(err, pgx.ErrNoRows) {
			return empty, nil
		}
		if err != nil {
			return empty, err
		}
		switch task.Status {
		case "completed":
			if current.Status != "answered" {
				return empty, nil
			}
			answer, err = queries.ScheduleDecisionAnswer(ctx, db.ScheduleDecisionAnswerParams{WorkspaceID: workspaceID, ID: answer.ID})
			if err != nil {
				return empty, err
			}
			if err := transaction.Commit(ctx); err != nil {
				return empty, err
			}
			return decisiontx.Withdrawal{Request: current, Answer: answer}, nil
		case "failed", "cancelled":
			reason = "answering_turn_" + task.Status
		default:
			return empty, nil
		}
	} else {
		removed, err := queries.DecisionAnswerSourceRemoved(ctx, db.DecisionAnswerSourceRemovedParams{WorkspaceID: workspaceID, ID: answer.ID})
		if err != nil {
			return empty, err
		}
		if !removed {
			return decisiontx.Withdrawal{Request: current, Answer: answer}, nil
		}
	}
	session, err := queries.GetChatSessionInWorkspace(ctx, db.GetChatSessionInWorkspaceParams{WorkspaceID: workspaceID, ID: current.ChatSessionID})
	if err != nil {
		return empty, err
	}
	result, err := decisiontx.Withdraw(ctx, queries, current, answer, session.Status, reason, pgtype.UUID{})
	if err != nil {
		return empty, err
	}
	if root != nil {
		result.ClearedComments, err = queries.ClearThreadResolutionForReply(ctx, db.ClearThreadResolutionForReplyParams{ThreadRootID: root.ID, IssueID: current.IssueID, WorkspaceID: workspaceID})
		if err != nil {
			return empty, err
		}
	}
	if err := transaction.Commit(ctx); err != nil {
		return empty, err
	}
	return result, nil
}

func (handler *Handler) SealDecisionIntent(ctx context.Context, workspaceID, intentID pgtype.UUID) (db.DecisionIntent, error) {
	empty := db.DecisionIntent{}
	located, err := handler.Queries.GetDecisionIntent(ctx, db.GetDecisionIntentParams{WorkspaceID: workspaceID, ID: intentID})
	if errors.Is(err, pgx.ErrNoRows) {
		return empty, nil
	}
	if err != nil {
		return empty, err
	}
	transaction, err := handler.TxStarter.Begin(ctx)
	if err != nil {
		return empty, err
	}
	defer transaction.Rollback(ctx)
	queries := handler.Queries.WithTx(transaction)
	if _, err := queries.LockWorkspaceForChatSessionCreate(ctx, workspaceID); err != nil {
		return empty, err
	}
	if _, err := queries.LockDecisionIssue(ctx, db.LockDecisionIssueParams{WorkspaceID: workspaceID, ID: located.IssueID}); errors.Is(err, pgx.ErrNoRows) {
		return empty, nil
	} else if err != nil {
		return empty, err
	}
	current, err := queries.LockDecisionIntent(ctx, db.LockDecisionIntentParams{WorkspaceID: workspaceID, ID: intentID})
	if errors.Is(err, pgx.ErrNoRows) {
		return empty, nil
	}
	if err != nil {
		return empty, err
	}
	if current.IssueID != located.IssueID {
		return empty, errors.New("decision intent context changed; retry")
	}
	if current.SealedAt.Valid {
		return current, nil
	}
	reason, err := decisiontx.SealInvalidIntent(ctx, queries, current)
	if err != nil {
		return empty, err
	}
	if reason == "" {
		return current, nil
	}
	current, err = queries.GetDecisionIntent(ctx, db.GetDecisionIntentParams{WorkspaceID: workspaceID, ID: intentID})
	if err != nil {
		return empty, err
	}
	if err := transaction.Commit(ctx); err != nil {
		return empty, err
	}
	return current, nil
}
