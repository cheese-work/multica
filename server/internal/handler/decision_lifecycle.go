package handler

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/commentguard"
	"github.com/multica-ai/multica/server/internal/decisiontx"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

func (handler *Handler) CancelDecisionLifecycle(ctx context.Context, workspaceID, requestID pgtype.UUID) (decisiontx.Withdrawal, error) {
	var result decisiontx.Withdrawal
	located, err := handler.Queries.GetDecisionRequest(ctx, db.GetDecisionRequestParams{WorkspaceID: workspaceID, ID: requestID})
	if errors.Is(err, pgx.ErrNoRows) {
		return result, nil
	}
	if err != nil {
		return result, err
	}
	transaction, err := handler.TxStarter.Begin(ctx)
	if err != nil {
		return result, err
	}
	defer transaction.Rollback(ctx)
	queries := handler.Queries.WithTx(transaction)
	if _, err := queries.LockWorkspaceForChatSessionCreate(ctx, workspaceID); err != nil {
		return result, err
	}
	if _, err := queries.LockDecisionSession(ctx, db.LockDecisionSessionParams{WorkspaceID: workspaceID, ID: located.ChatSessionID}); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return result, err
	}
	if _, err := queries.LockDecisionAskingAgent(ctx, db.LockDecisionAskingAgentParams{WorkspaceID: workspaceID, ID: located.AskingAgentID}); err != nil {
		return result, err
	}
	root, err := commentguard.LockThreadForReplyAndLoadRoot(ctx, queries, workspaceID, located.IssueCommentID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return result, err
	}
	if _, err := queries.LockDecisionIssue(ctx, db.LockDecisionIssueParams{WorkspaceID: workspaceID, ID: located.IssueID}); errors.Is(err, pgx.ErrNoRows) {
		return result, nil
	} else if err != nil {
		return result, err
	}
	current, err := queries.LockDecisionRequest(ctx, db.LockDecisionRequestParams{WorkspaceID: workspaceID, ID: requestID})
	if errors.Is(err, pgx.ErrNoRows) {
		return result, nil
	}
	if err != nil {
		return result, err
	}
	if current.ChatSessionID != located.ChatSessionID || current.AskingAgentID != located.AskingAgentID || current.IssueID != located.IssueID || current.IssueCommentID != located.IssueCommentID {
		return result, errors.New("decision lifecycle context changed; retry")
	}
	if current.Status != "open" && current.Status != "answered" {
		return result, nil
	}
	answer, err := queries.LockDecisionLiveAnswer(ctx, db.LockDecisionLiveAnswerParams{WorkspaceID: workspaceID, ID: requestID})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return result, err
	}
	reason, err := queries.DecisionLifecycleReason(ctx, db.DecisionLifecycleReasonParams{WorkspaceID: workspaceID, ID: requestID})
	if err != nil || reason == "" {
		return result, err
	}
	parentID := pgtype.UUID{}
	if root != nil {
		parentID = current.IssueCommentID
	}
	content := "Decision request cancelled: " + reason + "."
	if reason == "human_removed" {
		content += " The person asked is no longer a workspace member. Ask again with a new decision request."
	}
	created, err := queries.CreateComment(ctx, db.CreateCommentParams{ID: dbid.NewV7(), WorkspaceID: workspaceID, IssueID: current.IssueID, ParentID: parentID, AuthorType: "agent", AuthorID: current.AskingAgentID, Type: "comment", Content: content, DecisionRequestID: current.ID, DecisionCommentKind: pgtype.Text{String: "cancel", Valid: true}})
	if err != nil {
		return result, err
	}
	result.Comments = append(result.Comments, created.Comment())
	var note db.ChatMessage
	session, err := queries.GetChatSessionInWorkspace(ctx, db.GetChatSessionInWorkspaceParams{WorkspaceID: workspaceID, ID: current.ChatSessionID})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return result, err
	}
	if err == nil && session.Status != "archived" {
		note, err = queries.CreateChatMessage(ctx, db.CreateChatMessageParams{ID: dbid.NewV7(), ChatSessionID: session.ID, Role: "assistant", Content: content, MessageKind: pgtype.Text{String: "decision_reference", Valid: true}, SentVia: pgtype.Text{String: "task_token", Valid: true}})
		if err != nil {
			return result, err
		}
		if err := queries.TouchChatSession(ctx, session.ID); err != nil {
			return result, err
		}
	}
	if answer.ID.Valid && !answer.WokenAt.Valid {
		rows, err := queries.AcknowledgeDecisionWake(ctx, db.AcknowledgeDecisionWakeParams{WorkspaceID: workspaceID, ID: answer.ID, Outcome: pgtype.Text{String: "skipped_ineligible", Valid: true}})
		if err != nil {
			return result, err
		}
		if rows != 1 {
			return result, errors.New("decision cancellation acknowledgement changed")
		}
	}
	result.Request, err = queries.CancelDecisionLifecycleRequest(ctx, db.CancelDecisionLifecycleRequestParams{WorkspaceID: workspaceID, ID: requestID, Reason: pgtype.Text{String: reason, Valid: true}})
	if err != nil {
		return result, err
	}
	if root != nil {
		result.ClearedComments, err = queries.ClearThreadResolutionForReply(ctx, db.ClearThreadResolutionForReplyParams{WorkspaceID: workspaceID, IssueID: current.IssueID, ThreadRootID: root.ID})
		if err != nil {
			return result, err
		}
	}
	if err := transaction.Commit(ctx); err != nil {
		return decisiontx.Withdrawal{}, err
	}
	if note.ID.Valid {
		handler.publishChat(protocol.EventChatMessage, uuidToString(workspaceID), "agent", uuidToString(current.AskingAgentID), uuidToString(session.ID), protocol.ChatMessagePayload{ChatSessionID: uuidToString(session.ID), MessageID: uuidToString(note.ID), Role: note.Role, Content: note.Content, CreatedAt: note.CreatedAt.Time.Format(time.RFC3339Nano)})
	}
	return result, nil
}
