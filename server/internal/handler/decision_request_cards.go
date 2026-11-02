package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
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
	// AskingAgentName is the asking agent's current name; empty when the agent is gone.
	AskingAgentName string `json:"asking_agent_name,omitempty"`
	// Options are the answer buttons; empty for a card with no structured options.
	Options []decisiontx.AskOption `json:"options"`
	// RecommendedOptionIndex is the 0-based option the ask's recommendation names; absent when none matches.
	RecommendedOptionIndex *int `json:"recommended_option_index,omitempty"`
	State       string                 `json:"state"`
	CreatedAt   string                 `json:"created_at"`
	WithdrawnAt *string                `json:"withdrawn_at,omitempty"`
	Reason      string                 `json:"reason,omitempty"`
	// RecordCommentID is the issue comment that recorded the answer, set only while the request is answered.
	RecordCommentID *string `json:"record_comment_id,omitempty"`
	// Answer is the latest answer to the request, kept after a disclaim so the card can say so.
	Answer *DecisionRequestAnswer `json:"answer,omitempty"`
}

// DecisionRequestAnswer is the receipt half of a request card.
type DecisionRequestAnswer struct {
	// State is recorded, delivered, picked_up or disclaimed.
	State string `json:"state"`
	// Run is the run that resumed the issue after the record, set with picked_up.
	Run *DecisionRequestRun `json:"run,omitempty"`
	// ChosenOptionIndex is 0-based into the card's options, absent for a free-text answer.
	ChosenOptionIndex *int   `json:"chosen_option_index,omitempty"`
	OptionLabel       string `json:"option_label,omitempty"`
	AnsweredAt        string `json:"answered_at"`
}

// DecisionRequestRun names the run that picked an answer up.
type DecisionRequestRun struct {
	ID        string `json:"id"`
	AgentName string `json:"agent_name"`
}

func decisionRequestAnswerFromRow(row db.ListDecisionRequestCardsRow) *DecisionRequestAnswer {
	if !row.AnswerID.Valid {
		return nil
	}
	answer := &DecisionRequestAnswer{State: "recorded", AnsweredAt: timestampToString(row.AnswerCreatedAt)}
	answer.ChosenOptionIndex, answer.OptionLabel = chosenOption(row.Options, row.AnswerChosenOptionIndex)
	switch {
	case row.AnswerDisclaimedAt.Valid:
		answer.State = "disclaimed"
	case decisionAnswerDelivered(row.AnswerWokenAt, row.AnswerWakeOutcome, row.AnswerDisclaimedAt):
		answer.State = "delivered"
	}
	if row.PickupTaskID.Valid && answer.State != "disclaimed" {
		answer.State = "picked_up"
		answer.Run = &DecisionRequestRun{ID: uuidToString(row.PickupTaskID), AgentName: row.PickupAgentName}
	}
	return answer
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
			Ask:             row.Ask, AskingAgentName: row.AskingAgentName, State: row.Status, CreatedAt: timestampToString(row.CreatedAt),
			Answer: decisionRequestAnswerFromRow(row),
		}
		// Buttons need the structured ask; an older request stays text-only.
		if row.HasAskJson {
			if err := json.Unmarshal(row.Options, &card.Options); err != nil {
				slog.WarnContext(ctx, "decision card options unreadable", "request_id", card.ID, "error", err)
			}
			card.RecommendedOptionIndex = recommendedOptionIndex(card.Options, row.Recommendation)
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

// recommendedOptionIndex is the option the recommendation's first clause names
// as a whole word. The text is free-form, so a clause that names no option or
// more than one marks nothing: a wrong mark would steer the click. Longer labels
// are matched first and their text is blanked, so "Ship" inside "Ship now" is
// not a second choice, while "Ship rather than Ship now" still names two.
func recommendedOptionIndex(options []decisiontx.AskOption, recommendation string) *int {
	clause := strings.ToLower(recommendation)
	if cut := strings.IndexAny(clause, ",.;:\n"); cut >= 0 {
		clause = clause[:cut]
	}
	byLength := make([]int, len(options))
	for index := range byLength {
		byLength[index] = index
	}
	slices.SortStableFunc(byLength, func(a, b int) int {
		return len(strings.TrimSpace(options[b].Label)) - len(strings.TrimSpace(options[a].Label))
	})
	var named []int
	for _, index := range byLength {
		label := strings.ToLower(strings.TrimSpace(options[index].Label))
		if label == "" {
			continue
		}
		whole := regexp.MustCompile(`(?:^|[^\p{L}\p{N}_])(` + regexp.QuoteMeta(label) + `)(?:$|[^\p{L}\p{N}_])`)
		for found := whole.FindStringSubmatchIndex(clause); found != nil; found = whole.FindStringSubmatchIndex(clause) {
			if !slices.Contains(named, index) {
				named = append(named, index)
			}
			clause = clause[:found[2]] + strings.Repeat(" ", found[3]-found[2]) + clause[found[3]:]
		}
	}
	if len(named) != 1 {
		return nil
	}
	return &named[0]
}
