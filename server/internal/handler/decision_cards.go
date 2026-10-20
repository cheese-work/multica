package handler

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

const (
	decisionStatePending    = "pending"
	decisionStateEffective  = "effective"
	decisionStateDisclaimed = "disclaimed"
)

// DecisionCard is the display state of one decision_answer. The DM bubble that
// was quoted and the reference message both carry it, so the DM can render the
// "quoted in <issue>" card without parsing message text.
type DecisionCard struct {
	AnswerID        string  `json:"answer_id"`
	IssueID         string  `json:"issue_id"`
	IssueIdentifier string  `json:"issue_identifier"`
	IssueCommentID  string  `json:"issue_comment_id"`
	Class           string  `json:"class"`
	State           string  `json:"state"`
	EffectiveAt     string  `json:"effective_at"`
	DisclaimedAt    *string `json:"disclaimed_at,omitempty"`
}

// DecisionCommentFields is the display state a decision-stamped issue comment
// carries. The DM pointers are set only for the request's human: a DM is
// private, so other members must not learn which session or message was quoted.
type DecisionCommentFields struct {
	DecisionHumanUserID     string  `json:"decision_human_user_id,omitempty"`
	DecisionRecorderAgentID string  `json:"decision_recorder_agent_id,omitempty"`
	DecisionClass           string  `json:"decision_class,omitempty"`
	DecisionState           string  `json:"decision_state,omitempty"`
	DecisionIssueCommentID  string  `json:"decision_issue_comment_id,omitempty"`
	EffectiveAt             *string `json:"effective_at,omitempty"`
	DisclaimedAt            *string `json:"disclaimed_at,omitempty"`
	DecisionSourceSessionID string  `json:"decision_source_session_id,omitempty"`
	DecisionSourceMessageID string  `json:"decision_source_message_id,omitempty"`
}

// decisionAnswerState is pending until the wake delivers it, effective after,
// and disclaimed once withdrawn by anyone.
func decisionAnswerState(wokenAt, disclaimedAt pgtype.Timestamptz) string {
	switch {
	case disclaimedAt.Valid:
		return decisionStateDisclaimed
	case wokenAt.Valid:
		return decisionStateEffective
	default:
		return decisionStatePending
	}
}

func decisionCardFromRow(row db.ListDecisionCardsRow) DecisionCard {
	return DecisionCard{
		AnswerID:        uuidToString(row.AnswerID),
		IssueID:         uuidToString(row.IssueID),
		IssueIdentifier: fmt.Sprintf("%s-%d", row.IssuePrefix, row.IssueNumber),
		IssueCommentID:  uuidToString(row.IssueCommentID),
		Class:           row.EscalationClass,
		State:           decisionAnswerState(row.WokenAt, row.DisclaimedAt),
		EffectiveAt:     timestampToString(row.EffectiveAt),
		DisclaimedAt:    timestampToPtr(row.DisclaimedAt),
	}
}

func (handler *Handler) listDecisionCards(ctx context.Context, workspaceID pgtype.UUID, answerIDs, messageIDs []pgtype.UUID) []db.ListDecisionCardsRow {
	if len(answerIDs) == 0 && len(messageIDs) == 0 {
		return nil
	}
	rows, err := handler.Queries.ListDecisionCards(ctx, db.ListDecisionCardsParams{WorkspaceID: workspaceID, AnswerIds: answerIDs, MessageIds: messageIDs})
	if err != nil {
		// Cards are display-only: the timeline and the DM stay readable without them.
		slog.Warn("decision cards lookup failed", "error", err)
		return nil
	}
	return rows
}

// decisionCommentFields hydrates the card fields of every comment stamped with
// a decision answer, keyed by comment id. viewer is the authenticated member,
// or the zero UUID for a broadcast payload that has no single reader.
func (handler *Handler) decisionCommentFields(ctx context.Context, workspaceID pgtype.UUID, comments []db.Comment, viewer pgtype.UUID) map[string]DecisionCommentFields {
	var answerIDs []pgtype.UUID
	for _, comment := range comments {
		if comment.DecisionAnswerID.Valid {
			answerIDs = append(answerIDs, comment.DecisionAnswerID)
		}
	}
	rows := handler.listDecisionCards(ctx, workspaceID, answerIDs, nil)
	byAnswer := make(map[pgtype.UUID]db.ListDecisionCardsRow, len(rows))
	for _, row := range rows {
		byAnswer[row.AnswerID] = row
	}
	fields := make(map[string]DecisionCommentFields, len(answerIDs))
	for _, comment := range comments {
		row, ok := byAnswer[comment.DecisionAnswerID]
		if !ok {
			continue
		}
		entry := DecisionCommentFields{
			DecisionHumanUserID:     uuidToString(row.HumanUserID),
			DecisionRecorderAgentID: uuidToString(row.AskingAgentID),
			DecisionClass:           row.EscalationClass,
			DecisionState:           decisionAnswerState(row.WokenAt, row.DisclaimedAt),
			DecisionIssueCommentID:  uuidToString(row.IssueCommentID),
			EffectiveAt:             timestampToPtr(row.EffectiveAt),
			DisclaimedAt:            timestampToPtr(row.DisclaimedAt),
		}
		if viewer.Valid && viewer == row.HumanUserID && len(row.ChatMessageIds) > 0 {
			entry.DecisionSourceSessionID = uuidToString(row.SourceChatSessionID)
			entry.DecisionSourceMessageID = uuidToString(row.ChatMessageIds[0])
		}
		fields[uuidToString(comment.ID)] = entry
	}
	return fields
}

// withDecisionFields adds the card fields to a comment snapshot that is
// broadcast or returned after a mutation, so a snapshot never erases them from
// a client's timeline. Broadcast payloads have no single reader, so the DM
// pointers stay out; clients keep the ones they already hold.
func (handler *Handler) withDecisionFields(ctx context.Context, comment db.Comment, response CommentResponse) CommentResponse {
	response.DecisionCommentFields = handler.decisionCommentFields(ctx, comment.WorkspaceID, []db.Comment{comment}, pgtype.UUID{})[response.ID]
	return response
}

// decisionCardsForMessages returns, for a page of one DM, the cards that quote
// each user bubble and the card a reference message announces. The caller has
// already proven the reader owns the session.
func (handler *Handler) decisionCardsForMessages(ctx context.Context, workspaceID string, messages []db.ChatMessage) (quotes map[string][]DecisionCard, refs map[string]DecisionCard) {
	workspaceUUID, err := util.ParseUUID(workspaceID)
	if err != nil {
		return nil, nil
	}
	var answerIDs, messageIDs []pgtype.UUID
	for _, message := range messages {
		messageIDs = append(messageIDs, message.ID)
		if message.DecisionAnswerID.Valid {
			answerIDs = append(answerIDs, message.DecisionAnswerID)
		}
	}
	quotes, refs = map[string][]DecisionCard{}, map[string]DecisionCard{}
	rows := handler.listDecisionCards(ctx, workspaceUUID, answerIDs, messageIDs)
	onPage := make(map[pgtype.UUID]bool, len(messages))
	for _, message := range messages {
		onPage[message.ID] = true
	}
	for _, row := range rows {
		card := decisionCardFromRow(row)
		refs[card.AnswerID] = card
		for _, messageID := range row.ChatMessageIds {
			if onPage[messageID] {
				quotes[uuidToString(messageID)] = append(quotes[uuidToString(messageID)], card)
			}
		}
	}
	return quotes, refs
}

// publishDecisionAnswerUpdated tells open clients a decision card changed
// state. The payload carries ids and state only; clients refetch the cards.
func (handler *Handler) publishDecisionAnswerUpdated(workspaceID, actorType, actorID string, answerID, issueID pgtype.UUID, state string) {
	handler.publish(protocol.EventDecisionAnswerUpdated, workspaceID, actorType, actorID, map[string]any{
		"answer_id": uuidToString(answerID),
		"issue_id":  uuidToString(issueID),
		"state":     state,
	})
}
