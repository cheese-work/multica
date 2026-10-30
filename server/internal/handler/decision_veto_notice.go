package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/inbox"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

func (handler *Handler) deliverDecisionVetoNotice(ctx context.Context, workspaceID, answerID pgtype.UUID) error {
	answer, err := handler.Queries.GetDecisionAnswer(ctx, db.GetDecisionAnswerParams{WorkspaceID: workspaceID, ID: answerID})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	request, err := handler.Queries.GetDecisionRequest(ctx, db.GetDecisionRequestParams{WorkspaceID: workspaceID, ID: answer.DecisionRequestID})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if done, err := handler.decisionVetoNoticeStep(ctx, request, answerID, answer.SourceChatSessionID, true); err != nil || done {
		return err
	}
	for attempt := 0; attempt < decisionMaxAttempts; attempt++ {
		request, err = handler.Queries.GetDecisionRequest(ctx, db.GetDecisionRequestParams{WorkspaceID: workspaceID, ID: answer.DecisionRequestID})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if done, err := handler.decisionVetoNoticeStep(ctx, request, answerID, request.ChatSessionID, false); err != nil || done {
			return err
		}
	}
	return errors.New("decision veto lane changed; retry")
}

func (handler *Handler) decisionVetoNoticeStep(ctx context.Context, located db.DecisionRequest, answerID, destinationID pgtype.UUID, source bool) (bool, error) {
	transaction, err := handler.TxStarter.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer transaction.Rollback(ctx)
	queries := handler.Queries.WithTx(transaction)
	_, err = queries.LockWorkspaceForChatSessionCreate(ctx, located.WorkspaceID)
	if errors.Is(err, pgx.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	var session db.ChatSession
	if _, err := queries.LockDecisionSession(ctx, db.LockDecisionSessionParams{WorkspaceID: located.WorkspaceID, ID: destinationID}); err == nil {
		session, err = queries.GetChatSessionInWorkspace(ctx, db.GetChatSessionInWorkspaceParams{WorkspaceID: located.WorkspaceID, ID: destinationID})
		if err != nil {
			return false, err
		}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return false, err
	}
	if _, err := queries.LockDecisionIssue(ctx, db.LockDecisionIssueParams{WorkspaceID: located.WorkspaceID, ID: located.IssueID}); errors.Is(err, pgx.ErrNoRows) {
		return true, nil
	} else if err != nil {
		return false, err
	}
	request, err := queries.LockDecisionRequest(ctx, db.LockDecisionRequestParams{WorkspaceID: located.WorkspaceID, ID: located.ID})
	if errors.Is(err, pgx.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	answer, err := queries.LockDecisionAnswer(ctx, db.LockDecisionAnswerParams{WorkspaceID: located.WorkspaceID, ID: answerID})
	if errors.Is(err, pgx.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if answer.VetoNotePostedAt.Valid || !answer.DisclaimedAt.Valid || answer.DisclaimReason.String != "agent_veto" && answer.DisclaimReason.String != "record_retracted" {
		return true, nil
	}
	if request.IssueID != located.IssueID || answer.DecisionRequestID != request.ID || source && answer.SourceChatSessionID != destinationID {
		return false, errors.New("decision veto notice context changed; retry")
	}
	if !source && request.ChatSessionID != destinationID {
		return false, nil
	}
	active := session.ID.Valid && session.Status == "active"
	if source && !active {
		return false, nil
	}
	issue, err := queries.GetIssueInWorkspace(ctx, db.GetIssueInWorkspaceParams{WorkspaceID: request.WorkspaceID, ID: request.IssueID})
	if err != nil {
		return false, err
	}
	workspace, err := queries.GetWorkspace(ctx, request.WorkspaceID)
	if err != nil {
		return false, err
	}
	actor := uuidToString(answer.DisclaimedByAgentID)
	if agent, err := queries.GetAgentInWorkspace(ctx, db.GetAgentInWorkspaceParams{WorkspaceID: request.WorkspaceID, ID: answer.DisclaimedByAgentID}); err == nil {
		actor = fmt.Sprintf("%s (%s)", agent.Name, actor)
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return false, err
	}
	content := fmt.Sprintf("Decision vetoed for [%s-%d](mention://issue/%s) by %s.\nReason: %s\nOpen issue: %s", workspace.IssuePrefix, issue.Number, uuidToString(issue.ID), actor, answer.VetoReason.String, util.WorkspaceIssuePath(workspace.Slug, uuidToString(issue.ID)))
	var message db.ChatMessage
	if active {
		message, err = queries.CreateChatMessage(ctx, db.CreateChatMessageParams{ID: dbid.NewV7(), ChatSessionID: session.ID, Role: "assistant", Content: content, MessageKind: pgtype.Text{String: "decision_veto", Valid: true}, SentVia: pgtype.Text{String: "system", Valid: true}})
		if err != nil {
			return false, err
		}
		if err := queries.TouchChatSession(ctx, session.ID); err != nil {
			return false, err
		}
	}
	details, err := json.Marshal(map[string]pgtype.UUID{"comment_id": answer.WithdrawalCommentID, "decision_request_id": request.ID, "decision_answer_id": answer.ID})
	if err != nil {
		return false, err
	}
	item, inserted, err := inbox.CreateIfEligible(ctx, queries, db.CreateInboxItemParams{ID: dbid.NewV7(), WorkspaceID: request.WorkspaceID, RecipientType: "member", RecipientID: request.HumanUserID, Type: "mentioned", Severity: "info", IssueID: issue.ID, Title: issue.Title, Body: pgtype.Text{String: content, Valid: true}, ActorType: pgtype.Text{String: "agent", Valid: true}, ActorID: answer.DisclaimedByAgentID, Details: details})
	if err != nil {
		return false, err
	}
	if err := queries.MarkDecisionVetoNotified(ctx, db.MarkDecisionVetoNotifiedParams{WorkspaceID: request.WorkspaceID, ID: answer.ID}); err != nil {
		return false, err
	}
	if err := transaction.Commit(ctx); err != nil {
		return false, err
	}
	workspaceID, agentID := uuidToString(request.WorkspaceID), uuidToString(answer.DisclaimedByAgentID)
	if message.ID.Valid {
		handler.publishChat(protocol.EventChatMessage, workspaceID, "agent", agentID, uuidToString(message.ChatSessionID), protocol.ChatMessagePayload{ChatSessionID: uuidToString(message.ChatSessionID), MessageID: uuidToString(message.ID), Role: message.Role, Content: message.Content, CreatedAt: timestampToString(message.CreatedAt), MessageKind: message.MessageKind})
	}
	if inserted {
		handler.publish(protocol.EventInboxNew, workspaceID, "agent", agentID, map[string]any{"item": inboxToResponse(item)})
	}
	return true, nil
}
