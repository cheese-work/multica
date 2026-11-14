package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
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
	IssueTitle      string  `json:"issue_title,omitempty"`
	SourceCommentID *string `json:"source_comment_id,omitempty"`
	Ask             string  `json:"ask"`
	// AskingAgentName is the asking agent's current name; empty when the agent is gone.
	AskingAgentName string `json:"asking_agent_name,omitempty"`
	// Options are the answer buttons; empty for a card with no structured options.
	Options []decisiontx.AskOption `json:"options"`
	// RecommendedOptionIndex is the 0-based option the ask's recommendation names; absent when none matches.
	RecommendedOptionIndex *int    `json:"recommended_option_index,omitempty"`
	State                  string  `json:"state"`
	CreatedAt              string  `json:"created_at"`
	WithdrawnAt            *string `json:"withdrawn_at,omitempty"`
	Reason                 string  `json:"reason,omitempty"`
	// RecordCommentID is the issue comment that recorded the answer, set only while the request is answered.
	RecordCommentID *string `json:"record_comment_id,omitempty"`
	// Answer is the latest answer to the request, kept after a disclaim so the card can say so.
	Answer *DecisionRequestAnswer `json:"answer,omitempty"`
	// Action is the pending action an open request asks to approve, with the server's preview of it.
	Action *DecisionRequestCardAction `json:"action,omitempty"`
	// CoversRequestIDs are the per-issue cards a batch card answers for; the DM folds them under it.
	CoversRequestIDs []string `json:"covers_request_ids,omitempty"`
	// CoveredCards lists what a batch card's answer also decides, so the human sees every covered decision.
	CoveredCards []DecisionRequestCoveredCard `json:"covered_cards,omitempty"`
	// AnsweredByBatchRequestID is the batch card whose answer closed this card.
	AnsweredByBatchRequestID *string `json:"answered_by_batch_request_id,omitempty"`
	// BatchID is set on every card of a proposal batch (CHE-5412); BatchItems then lists one entry per item.
	BatchID    string                     `json:"batch_id,omitempty"`
	BatchItems []DecisionRequestBatchItem `json:"batch_items,omitempty"`
}

// DecisionRequestBatchItem is one action of a proposal batch as the card reads it.
type DecisionRequestBatchItem struct {
	ActionID        string          `json:"action_id"`
	IssueID         string          `json:"issue_id"`
	IssueIdentifier string          `json:"issue_identifier"`
	IssueTitle      string          `json:"issue_title,omitempty"`
	State           string          `json:"state"`
	Result          json.RawMessage `json:"result,omitempty"`
	ErrorCode       string          `json:"error_code,omitempty"`
}

// DecisionRequestCoveredCard names one per-issue card a batch card covers.
type DecisionRequestCoveredCard struct {
	ID              string `json:"id"`
	IssueID         string `json:"issue_id"`
	IssueIdentifier string `json:"issue_identifier"`
	IssueTitle      string `json:"issue_title,omitempty"`
	Decision        string `json:"decision"`
	// Options are the covered card's own options with their meanings, so the human sees what each answer does there.
	Options []decisiontx.AskOption `json:"options"`
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
	batches := map[pgtype.UUID][]DecisionRequestBatchItem{}
	for _, row := range rows {
		card := DecisionRequestCard{
			ID: uuidToString(row.RequestID), IssueID: uuidToString(row.IssueID),
			IssueIdentifier: fmt.Sprintf("%s-%d", row.IssuePrefix, row.IssueNumber), IssueTitle: row.IssueTitle,
			Ask: row.Ask, AskingAgentName: row.AskingAgentName, State: row.Status, CreatedAt: timestampToString(row.CreatedAt),
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
		for _, covered := range row.CoversRequestIds {
			card.CoversRequestIDs = append(card.CoversRequestIDs, uuidToString(covered))
		}
		// On a read failure the card keeps its covers_request_ids but lists no covered
		// cards; the client then refuses to offer the answer form (fail closed).
		covered, err := handler.coveredCardSummaries(ctx, workspaceUUID, row.CoversRequestIds)
		if err != nil {
			slog.ErrorContext(ctx, "list covered decision cards", "request_id", card.ID, "error", err)
		}
		card.CoveredCards = covered
		if row.AnsweredByBatchRequestID.Valid {
			batchID := uuidToString(row.AnsweredByBatchRequestID)
			card.AnsweredByBatchRequestID = &batchID
		}
		if row.BatchID.Valid {
			card.BatchID = uuidToString(row.BatchID)
			if _, read := batches[row.BatchID]; !read {
				batches[row.BatchID] = handler.batchItems(ctx, workspaceUUID, row.BatchID)
			}
			card.BatchItems = batches[row.BatchID]
		}
		if row.RecordCommentID.Valid {
			recordID := uuidToString(row.RecordCommentID)
			card.RecordCommentID = &recordID
		}
		// An answered request keeps its action so the card can show the outcome after approval.
		if row.Status == "open" || row.Status == "answered" {
			card.Action = handler.decisionCardAction(ctx, workspaceUUID, row.RequestID)
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

// batchItems reads one entry per action of a proposal batch. A read failure
// lists none, and the client then offers no per-item choice (fail closed).
func (handler *Handler) batchItems(ctx context.Context, workspaceUUID, batchID pgtype.UUID) []DecisionRequestBatchItem {
	rows, err := handler.Queries.ListDecisionBatchItems(ctx, db.ListDecisionBatchItemsParams{WorkspaceID: workspaceUUID, BatchID: batchID})
	if err != nil {
		slog.ErrorContext(ctx, "list decision batch items", "batch_id", uuidToString(batchID), "error", err)
		return nil
	}
	items := make([]DecisionRequestBatchItem, len(rows))
	for index, row := range rows {
		items[index] = DecisionRequestBatchItem{
			ActionID: uuidToString(row.ActionID), IssueID: uuidToString(row.IssueID),
			IssueIdentifier: fmt.Sprintf("%s-%d", row.IssuePrefix, row.IssueNumber), IssueTitle: row.IssueTitle,
			State: row.State, Result: row.Result, ErrorCode: row.ErrorCode,
		}
	}
	return items
}

// recommendedOptionIndex is the one option whose label is the whole
// recommendation, trimmed and case-folded. The text is free-form, so anything
// else (a reason, a negation, two labels, duplicate labels) marks nothing: a
// wrong mark would steer the click.
func recommendedOptionIndex(options []decisiontx.AskOption, recommendation string) *int {
	want := strings.ToLower(strings.TrimSpace(recommendation))
	if want == "" {
		return nil
	}
	match := -1
	for index, option := range options {
		if strings.ToLower(strings.TrimSpace(option.Label)) != want {
			continue
		}
		if match >= 0 {
			return nil
		}
		match = index
	}
	if match < 0 {
		return nil
	}
	return &match
}

// coveredCardSummaries reads the decisions a batch card covers from the
// covered cards themselves, never from the batch card's own text, so the
// human sees what the one answer decides. A read failure is returned, never
// turned into an empty list.
func (handler *Handler) coveredCardSummaries(ctx context.Context, workspaceUUID pgtype.UUID, ids []pgtype.UUID) ([]DecisionRequestCoveredCard, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := handler.Queries.ListCoveredCardSummaries(ctx, db.ListCoveredCardSummariesParams{WorkspaceID: workspaceUUID, Ids: ids})
	if err != nil {
		return nil, err
	}
	covered := make([]DecisionRequestCoveredCard, 0, len(rows))
	for _, row := range rows {
		summary := DecisionRequestCoveredCard{ID: uuidToString(row.ID), IssueID: uuidToString(row.IssueID), IssueIdentifier: fmt.Sprintf("%s-%d", row.IssuePrefix, row.IssueNumber), IssueTitle: row.IssueTitle, Decision: row.Decision}
		if err := json.Unmarshal(row.Options, &summary.Options); err != nil {
			return nil, fmt.Errorf("covered card %s options: %w", summary.ID, err)
		}
		covered = append(covered, summary)
	}
	return covered, nil
}

// ListIssueDecisionCards returns the viewer's used (answered or withdrawn)
// request cards on one issue, oldest first. The issue page folds them; the open
// ones come from ListOpenDecisions. Only the viewer's own requests are listed,
// as in the DM.
func (handler *Handler) ListIssueDecisionCards(writer http.ResponseWriter, request *http.Request) {
	issue, ok := handler.loadIssueForUser(writer, request, request.URL.Query().Get("issue"))
	if !ok {
		return
	}
	member, ok := handler.requireWorkspaceMember(writer, request, uuidToString(issue.WorkspaceID), "workspace not found")
	if !ok {
		return
	}
	requests, err := handler.Queries.ListDecisionRequestsByIssue(request.Context(), db.ListDecisionRequestsByIssueParams{WorkspaceID: issue.WorkspaceID, IssueID: issue.ID})
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "failed to list decision requests")
		return
	}
	var messages []db.ChatMessage
	for _, stored := range requests {
		if stored.Status != "open" && stored.HumanUserID == member.UserID && stored.PingMessageID.Valid {
			messages = append(messages, db.ChatMessage{ID: stored.PingMessageID})
		}
	}
	cards := []DecisionRequestCard{}
	byMessage := handler.decisionRequestsForMessages(request.Context(), uuidToString(issue.WorkspaceID), messages)
	for _, stored := range requests {
		for _, card := range byMessage[uuidToString(stored.PingMessageID)] {
			if card.ID == uuidToString(stored.ID) && stored.Status != "open" && stored.HumanUserID == member.UserID {
				cards = append(cards, card)
			}
		}
	}
	writeJSON(writer, http.StatusOK, cards)
}
