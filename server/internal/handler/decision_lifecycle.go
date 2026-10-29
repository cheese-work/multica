package handler

import (
	"context"
	"errors"
	"slices"
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
	issue, err := queries.GetIssue(ctx, current.IssueID)
	if err != nil {
		return result, err
	}
	workspace, err := queries.GetWorkspace(ctx, workspaceID)
	if err != nil {
		return result, err
	}
	closedAt, err := queries.DecisionIssueClosedAt(ctx, db.DecisionIssueClosedAtParams{WorkspaceID: workspaceID, ID: current.IssueID})
	if err != nil {
		return result, err
	}
	cancelled := decisionCancelIssueFor(issue, workspace, closedAt.Time)
	content := decisionCancelComment(reason, cancelled, current)
	actionPending, err := decisiontx.EndAction(ctx, queries, current, reason)
	if err != nil {
		return result, err
	}
	if reason == "expired" {
		content = decisionExpiryExplanation
		if actionPending {
			content = decisionActionExpiryExplanation
		}
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
	if err == nil && session.Status != "archived" && reason != "expired" {
		noteRequests, err := decisionCancelNoteRequests(ctx, queries, reason, current, closedAt)
		if err != nil {
			return result, err
		}
		if len(noteRequests) > 0 {
			note, err = queries.CreateChatMessage(ctx, db.CreateChatMessageParams{ID: dbid.NewV7(), ChatSessionID: session.ID, Role: "assistant", Content: decisionCancelNote(reason, cancelled, noteRequests), MessageKind: pgtype.Text{String: protocol.ChatMessageKindDecisionCancelNotice, Valid: true}, SentVia: pgtype.Text{String: "task_token", Valid: true}})
			if err != nil {
				return result, err
			}
			requestIDs := make([]pgtype.UUID, len(noteRequests))
			for index, noted := range noteRequests {
				requestIDs[index] = noted.ID
			}
			if err := queries.StampDecisionCancelMessageRequests(ctx, db.StampDecisionCancelMessageRequestsParams{ID: note.ID, ChatSessionID: session.ID, RequestIds: requestIDs}); err != nil {
				return result, err
			}
			if err := queries.TouchChatSession(ctx, session.ID); err != nil {
				return result, err
			}
		} else {
			note, err = queries.GetDecisionCancelMessageForRequest(ctx, db.GetDecisionCancelMessageForRequestParams{ChatSessionID: session.ID, RequestID: current.ID})
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return result, err
			}
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
		handler.publishChat(protocol.EventChatMessage, uuidToString(workspaceID), "agent", uuidToString(current.AskingAgentID), uuidToString(session.ID), protocol.ChatMessagePayload{ChatSessionID: uuidToString(session.ID), MessageID: uuidToString(note.ID), Role: note.Role, Content: note.Content, CreatedAt: note.CreatedAt.Time.Format(time.RFC3339Nano), MessageKind: note.MessageKind})
	}
	return result, nil
}

// decisionCancelNoteRequests returns the requests the DM note covers. A close groups the open requests of
// the session at once, so the first request cancelled by that close writes one note for all of them
// and later ones write none. Other reasons stay one note per request.
func decisionCancelNoteRequests(ctx context.Context, queries *db.Queries, reason string, current db.DecisionRequest, closedAt pgtype.Timestamptz) ([]db.DecisionRequest, error) {
	if reason != "issue_closed" {
		return []db.DecisionRequest{current}, nil
	}
	params := db.HasDecisionCancelNoticeSinceCloseParams{WorkspaceID: current.WorkspaceID, IssueID: current.IssueID, ChatSessionID: current.ChatSessionID, ID: current.ID, ClosedAt: closedAt}
	posted, err := queries.HasDecisionCancelNoticeSinceClose(ctx, params)
	if err != nil || posted {
		return nil, err
	}
	requests, err := queries.ListDecisionCancelSiblings(ctx, db.ListDecisionCancelSiblingsParams{WorkspaceID: current.WorkspaceID, IssueID: current.IssueID, ChatSessionID: current.ChatSessionID})
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(requests, func(request db.DecisionRequest) bool {
		return request.Status != "open"
	}), nil
}
