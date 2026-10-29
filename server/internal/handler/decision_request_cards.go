package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/decisiontx"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

type DecisionRequestCard struct {
	ID              string  `json:"id"`
	IssueID         string  `json:"issue_id"`
	IssueIdentifier string  `json:"issue_identifier"`
	SourceCommentID *string `json:"source_comment_id,omitempty"`
	Ask             string  `json:"ask"`
	// Options are the answer buttons; empty for a card with no structured options.
	Options     []decisiontx.AskOption `json:"options"`
	State       string                 `json:"state"`
	CreatedAt   string                 `json:"created_at"`
	WithdrawnAt *string                `json:"withdrawn_at,omitempty"`
	Reason      string                 `json:"reason,omitempty"`
	// RecordCommentID is the issue comment that recorded the answer, set only while the request is answered.
	RecordCommentID *string `json:"record_comment_id,omitempty"`
}

func (handler *Handler) decisionRequestsForMessages(ctx context.Context, workspaceID string, messages []db.ChatMessage) map[string][]DecisionRequestCard {
	workspaceUUID, err := util.ParseUUID(workspaceID)
	if err != nil || len(messages) == 0 {
		return nil
	}
	var messageIDs []pgtype.UUID
	pageByLookup := make(map[pgtype.UUID][]string, len(messages))
	for _, message := range messages {
		for _, lookup := range append([]pgtype.UUID{message.ID}, message.LineageMessageIds...) {
			messageIDs = append(messageIDs, lookup)
			pageByLookup[lookup] = append(pageByLookup[lookup], uuidToString(message.ID))
		}
	}
	rows, err := handler.Queries.ListDecisionRequestCards(ctx, db.ListDecisionRequestCardsParams{WorkspaceID: workspaceUUID, MessageIds: messageIDs})
	if err != nil {
		slog.ErrorContext(ctx, "list decision request cards", "error", err)
		return nil
	}
	cards := make(map[string][]DecisionRequestCard, len(rows))
	for _, row := range rows {
		card := DecisionRequestCard{
			ID: uuidToString(row.RequestID), IssueID: uuidToString(row.IssueID),
			IssueIdentifier: fmt.Sprintf("%s-%d", row.IssuePrefix, row.IssueNumber),
			Ask:             row.Ask, State: row.Status, CreatedAt: timestampToString(row.CreatedAt),
		}
		if err := json.Unmarshal(row.Options, &card.Options); err != nil {
			slog.WarnContext(ctx, "decision card options unreadable", "request_id", card.ID, "error", err)
		}
		if row.SourceCommentID.Valid {
			sourceID := uuidToString(row.SourceCommentID)
			card.SourceCommentID = &sourceID
		}
		if row.RecordCommentID.Valid {
			recordID := uuidToString(row.RecordCommentID)
			card.RecordCommentID = &recordID
		}
		if row.Status == "cancelled" {
			card.State = "withdrawn"
			card.Reason = row.WithdrawReason
			if row.CancelReason.String == "expired" {
				card.Reason = decisionExpiryExplanation
			}
			if card.Reason == "" {
				card.Reason = strings.ReplaceAll(row.CancelReason.String, "_", " ")
			}
			if row.WithdrawnAt.Valid {
				withdrawnAt := timestampToString(row.WithdrawnAt)
				card.WithdrawnAt = &withdrawnAt
			}
		}
		for _, messageID := range pageByLookup[row.MessageID] {
			cards[messageID] = append(cards[messageID], card)
		}
	}
	return cards
}
