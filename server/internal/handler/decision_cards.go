package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/decisiontx"
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
	// Delivered is true once the asking agent was woken with the answer.
	Delivered bool `json:"delivered"`
	// ChosenOptionIndex is 0-based into the card's options, absent for a free-text answer.
	ChosenOptionIndex *int   `json:"chosen_option_index,omitempty"`
	OptionLabel       string `json:"option_label,omitempty"`
	// The request pointers name the ask card in its own session, so the receipt can jump back to it.
	RequestSessionID string `json:"request_session_id,omitempty"`
	RequestMessageID string `json:"request_message_id,omitempty"`
	// Inherited marks a card shown on a forked copy: read-only provenance that
	// still points at the original issue comment. Clients show no Disclaim on it.
	Inherited bool `json:"inherited,omitempty"`
}

// DecisionCommentFields is the display state a decision-stamped issue comment
// carries. The DM pointers are set only for the request's human: a DM is
// private, so other members must not learn which session or message was quoted.
type DecisionCommentFields struct {
	DecisionHumanUserID     string `json:"decision_human_user_id,omitempty"`
	DecisionRecorderAgentID string `json:"decision_recorder_agent_id,omitempty"`
	DecisionClass           string `json:"decision_class,omitempty"`
	DecisionState           string `json:"decision_state,omitempty"`
	DecisionDelivered       bool   `json:"decision_delivered,omitempty"`
	// ChosenOptionIndex is 0-based into the ask's options, absent for a free-text answer.
	DecisionChosenOptionIndex *int   `json:"decision_chosen_option_index,omitempty"`
	DecisionOptionLabel       string `json:"decision_option_label,omitempty"`
	DecisionIssueCommentID    string `json:"decision_issue_comment_id,omitempty"`
	// DecisionReceiptAnswerID marks a human's own comment that a task recorded as
	// the answer: the comment carries the receipt, the agent quote stays separate.
	DecisionReceiptAnswerID string `json:"decision_receipt_answer_id,omitempty"`
	// The issue that accepted the answer, so a receipt on a comment elsewhere
	// can say which issue it was recorded for.
	DecisionIssueID         string  `json:"decision_issue_id,omitempty"`
	DecisionIssueIdentifier string  `json:"decision_issue_identifier,omitempty"`
	EffectiveAt             *string `json:"effective_at,omitempty"`
	DisclaimedAt            *string `json:"disclaimed_at,omitempty"`
	DecisionSourceSessionID string  `json:"decision_source_session_id,omitempty"`
	DecisionSourceMessageID string  `json:"decision_source_message_id,omitempty"`
	// The request pointers name the DM card that asked, in the request's own
	// session: a continued DM answers in a newer session than the one that asked.
	DecisionRequestSessionID string `json:"decision_request_session_id,omitempty"`
	DecisionRequestMessageID string `json:"decision_request_message_id,omitempty"`
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

// decisionAnswerDelivered is true once the wake reached the asking agent: woken
// and not skipped as ineligible, and not withdrawn since.
func decisionAnswerDelivered(wokenAt pgtype.Timestamptz, wakeOutcome pgtype.Text, disclaimedAt pgtype.Timestamptz) bool {
	return wokenAt.Valid && !disclaimedAt.Valid && wakeOutcome.String != "skipped_ineligible"
}

// chosenOption resolves a stored 0-based option index against the request's
// options, so a receipt names the option the human picked.
func chosenOption(options []byte, index pgtype.Int4) (*int, string) {
	if !index.Valid {
		return nil, ""
	}
	chosen := int(index.Int32)
	var parsed []decisiontx.AskOption
	if json.Unmarshal(options, &parsed) != nil || chosen >= len(parsed) {
		return &chosen, ""
	}
	return &chosen, parsed[chosen].Label
}

func decisionCardFromRow(row db.ListDecisionCardsRow) DecisionCard {
	index, label := chosenOption(row.RequestOptions, row.ChosenOptionIndex)
	return DecisionCard{
		Delivered:         decisionAnswerDelivered(row.WokenAt, row.WakeOutcome, row.DisclaimedAt),
		ChosenOptionIndex: index,
		OptionLabel:       label,
		RequestSessionID:  uuidToString(row.RequestChatSessionID),
		RequestMessageID:  uuidToString(row.RequestPingMessageID),
		AnswerID:          uuidToString(row.AnswerID),
		IssueID:           uuidToString(row.IssueID),
		IssueIdentifier:   fmt.Sprintf("%s-%d", row.IssuePrefix, row.IssueNumber),
		IssueCommentID:    uuidToString(row.IssueCommentID),
		Class:             row.EscalationClass,
		State:             decisionAnswerState(row.WokenAt, row.DisclaimedAt),
		EffectiveAt:       timestampToString(row.EffectiveAt),
		DisclaimedAt:      timestampToPtr(row.DisclaimedAt),
	}
}

func (handler *Handler) listDecisionCards(ctx context.Context, workspaceID pgtype.UUID, answerIDs, messageIDs, sourceCommentIDs []pgtype.UUID) []db.ListDecisionCardsRow {
	if len(answerIDs) == 0 && len(messageIDs) == 0 && len(sourceCommentIDs) == 0 {
		return nil
	}
	rows, err := handler.Queries.ListDecisionCards(ctx, db.ListDecisionCardsParams{WorkspaceID: workspaceID, AnswerIds: answerIDs, MessageIds: messageIDs, SourceCommentIds: sourceCommentIDs})
	if err != nil {
		// Cards are display-only: the timeline and the DM stay readable without them.
		slog.Warn("decision cards lookup failed", "error", err)
		return nil
	}
	return rows
}

// decisionCommentFields hydrates the card fields of every comment stamped with
// a decision answer, keyed by comment id. A member comment that a task recorded
// as the answer carries the same fields plus DecisionReceiptAnswerID, so the
// receipt sits on the human's own words. viewer is the authenticated member, or
// the zero UUID for a broadcast payload that has no single reader.
func (handler *Handler) decisionCommentFields(ctx context.Context, workspaceID pgtype.UUID, comments []db.Comment, viewer pgtype.UUID) map[string]DecisionCommentFields {
	var answerIDs, sourceIDs []pgtype.UUID
	for _, comment := range comments {
		switch {
		case comment.DecisionAnswerID.Valid:
			answerIDs = append(answerIDs, comment.DecisionAnswerID)
		case comment.AuthorType == "member":
			sourceIDs = append(sourceIDs, comment.ID)
		}
	}
	rows := handler.listDecisionCards(ctx, workspaceID, answerIDs, nil, sourceIDs)
	byAnswer := make(map[pgtype.UUID]db.ListDecisionCardsRow, len(rows))
	bySource := make(map[pgtype.UUID]db.ListDecisionCardsRow, len(sourceIDs))
	for _, row := range rows {
		byAnswer[row.AnswerID] = row
		if row.SourceCommentID.Valid {
			// Rows come oldest first, so the newest answer to a comment wins.
			bySource[row.SourceCommentID] = row
		}
	}
	fields := make(map[string]DecisionCommentFields, len(answerIDs)+len(sourceIDs))
	for _, comment := range comments {
		row, ok := byAnswer[comment.DecisionAnswerID]
		receipt := false
		if !ok && !comment.DecisionAnswerID.Valid && comment.AuthorType == "member" {
			row, ok = bySource[comment.ID]
			receipt = ok
		}
		if !ok {
			continue
		}
		entry := decisionCommentEntry(row, viewer)
		if receipt {
			entry.DecisionReceiptAnswerID = uuidToString(row.AnswerID)
		}
		fields[uuidToString(comment.ID)] = entry
	}
	return fields
}

func decisionCommentEntry(row db.ListDecisionCardsRow, viewer pgtype.UUID) DecisionCommentFields {
	index, label := chosenOption(row.RequestOptions, row.ChosenOptionIndex)
	entry := DecisionCommentFields{
		DecisionDelivered:         decisionAnswerDelivered(row.WokenAt, row.WakeOutcome, row.DisclaimedAt),
		DecisionChosenOptionIndex: index,
		DecisionOptionLabel:       label,
		DecisionHumanUserID:       uuidToString(row.HumanUserID),
		DecisionRecorderAgentID:   uuidToString(row.AskingAgentID),
		DecisionClass:             row.EscalationClass,
		DecisionState:             decisionAnswerState(row.WokenAt, row.DisclaimedAt),
		DecisionIssueCommentID:    uuidToString(row.IssueCommentID),
		DecisionIssueID:           uuidToString(row.IssueID),
		DecisionIssueIdentifier:   fmt.Sprintf("%s-%d", row.IssuePrefix, row.IssueNumber),
		EffectiveAt:               timestampToPtr(row.EffectiveAt),
		DisclaimedAt:              timestampToPtr(row.DisclaimedAt),
	}
	if viewer.Valid && viewer == row.HumanUserID && len(row.ChatMessageIds) > 0 {
		entry.DecisionSourceSessionID = uuidToString(row.SourceChatSessionID)
		entry.DecisionSourceMessageID = uuidToString(row.ChatMessageIds[0])
	}
	if viewer.Valid && viewer == row.HumanUserID && row.RequestPingMessageID.Valid {
		entry.DecisionRequestSessionID = uuidToString(row.RequestChatSessionID)
		entry.DecisionRequestMessageID = uuidToString(row.RequestPingMessageID)
	}
	return entry
}

// decisionViewer is the member who may see the owner-only DM pointers, or the
// zero UUID. Task-token auth sets X-User-ID to the task's user, so the user id
// alone does not make a human: only the actor sources DisclaimDecisionAnswer
// accepts for the request's human count.
func decisionViewer(request *http.Request) pgtype.UUID {
	switch request.Header.Get("X-Actor-Source") {
	case "interactive_jwt", "cli_jwt", "pat":
		viewer, _ := util.ParseUUID(requestUserID(request))
		return viewer
	}
	return pgtype.UUID{}
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
	// A forked copy is matched through every message it descends from, because
	// a decision's source ids are those of the DM it was recorded in.
	pageByLookup := make(map[pgtype.UUID][]db.ChatMessage, len(messages))
	for _, message := range messages {
		for _, lookup := range append([]pgtype.UUID{message.ID}, message.LineageMessageIds...) {
			messageIDs = append(messageIDs, lookup)
			pageByLookup[lookup] = append(pageByLookup[lookup], message)
		}
		if message.DecisionAnswerID.Valid {
			answerIDs = append(answerIDs, message.DecisionAnswerID)
		}
	}
	quotes, refs = map[string][]DecisionCard{}, map[string]DecisionCard{}
	rows := handler.listDecisionCards(ctx, workspaceUUID, answerIDs, messageIDs, nil)
	for _, row := range rows {
		card := decisionCardFromRow(row)
		refs[card.AnswerID] = card
		for _, sourceID := range row.ChatMessageIds {
			for _, message := range pageByLookup[sourceID] {
				shown := card
				shown.Inherited = sourceID != message.ID
				quotes[uuidToString(message.ID)] = append(quotes[uuidToString(message.ID)], shown)
			}
		}
	}
	return quotes, refs
}

// publishDecisionAnswerUpdated tells open clients a decision card changed
// state. The payload carries ids and state only; clients refetch the cards.
// A receipt can sit on the human's comment in another issue, so the payload
// names that issue too and its open timeline refreshes.
func (handler *Handler) publishDecisionAnswerUpdated(workspaceID, actorType, actorID string, answerID, issueID pgtype.UUID, state string) {
	payload := map[string]any{
		"answer_id": uuidToString(answerID),
		"issue_id":  uuidToString(issueID),
		"state":     state,
	}
	if sourceIssueID := handler.decisionSourceIssueID(workspaceID, answerID); sourceIssueID.Valid && sourceIssueID != issueID {
		payload["source_issue_id"] = uuidToString(sourceIssueID)
	}
	handler.publish(protocol.EventDecisionAnswerUpdated, workspaceID, actorType, actorID, payload)
}

// decisionSourceIssueID is the issue holding the member comment an answer was
// recorded from, or the zero UUID for a DM answer. The event is a hint, so a
// failed lookup only skips the extra refresh.
func (handler *Handler) decisionSourceIssueID(workspaceID string, answerID pgtype.UUID) pgtype.UUID {
	workspaceUUID, err := util.ParseUUID(workspaceID)
	if err != nil {
		return pgtype.UUID{}
	}
	ctx := context.Background()
	answer, err := handler.Queries.GetDecisionAnswer(ctx, db.GetDecisionAnswerParams{WorkspaceID: workspaceUUID, ID: answerID})
	if err != nil || !answer.SourceCommentID.Valid {
		return pgtype.UUID{}
	}
	comment, err := handler.Queries.GetComment(ctx, answer.SourceCommentID)
	if err != nil {
		return pgtype.UUID{}
	}
	return comment.IssueID
}
