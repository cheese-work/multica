package handler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/commentguard"
	"github.com/multica-ai/multica/server/internal/decisiontx"
	"github.com/multica-ai/multica/server/internal/issuestatus"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// askOptions reads the options a card offers; an unreadable list offers none.
func askOptions(raw []byte) []decisiontx.AskOption {
	var options []decisiontx.AskOption
	if err := json.Unmarshal(raw, &options); err != nil {
		return nil
	}
	return options
}

// offersOption reports whether options hold an option with the same label and
// the same meaning (what_happens). A label alone is not consent: two cards can
// both offer "Approve" and mean different actions.
func offersOption(options []decisiontx.AskOption, want decisiontx.AskOption) bool {
	return slices.ContainsFunc(options, func(option decisiontx.AskOption) bool {
		return option.Label == want.Label && option.WhatHappens == want.WhatHappens
	})
}

// coveredCardOffers reports whether the covered card offers the exact option
// (label and meaning) chosen on the batch card. A free-text batch answer names
// no option, so it covers nothing: the human never agreed to an unnamed answer
// for another card's question.
func coveredCardOffers(batch, card db.DecisionRequest, label string) bool {
	if label == "" {
		return false
	}
	for _, chosen := range askOptions(batch.Options) {
		if chosen.Label == label && offersOption(askOptions(card.Options), chosen) {
			return true
		}
	}
	return false
}

// batchCoverAck is the acknowledgment a batch option answer must carry: the
// SHA-256 hex of the batch id, the sorted covered ids (comma joined) and the
// chosen option label, each on its own line. A client computes it from the
// covered decisions the card disclosed, so an answer from a client that never
// showed them cannot propagate to the covered cards.
func batchCoverAck(batch db.DecisionRequest, option string) string {
	ids := make([]string, 0, len(batch.CoversRequestIds))
	for _, id := range batch.CoversRequestIds {
		ids = append(ids, uuidToString(id))
	}
	slices.Sort(ids)
	sum := sha256.Sum256([]byte(uuidToString(batch.ID) + "\n" + strings.Join(ids, ",") + "\n" + option))
	return hex.EncodeToString(sum[:])
}

// maxBatchCovers bounds how many open cards one batch card may cover.
const maxBatchCovers = 25

// coveredCardAnswer is one per-issue card a batch answer closed.
type coveredCardAnswer struct {
	Request db.DecisionRequest
	Answer  db.DecisionAnswer
	Comment db.Comment
	Cleared []db.Comment
}

// answerCoveredCards stamps the batch answer on every open card the batch
// covers, inside the batch answer's own transaction (CHE-4453). A covered card
// that is no longer open, is not the batch human's, has a gated class or an
// action, or sits on a closed issue is left as it is: it keeps its own answer
// path. Each closed card gets one answer, so the wake worker wakes its asking
// agent once, and one quote on its issue.
func (handler *Handler) answerCoveredCards(ctx context.Context, queries *db.Queries, workspace db.Workspace, batch db.DecisionRequest, input decisionCardAnswerInput, content string) ([]coveredCardAnswer, error) {
	if len(batch.CoversRequestIds) == 0 {
		return nil, nil
	}
	covered, err := queries.LockBatchCoveredRequests(ctx, db.LockBatchCoveredRequestsParams{WorkspaceID: batch.WorkspaceID, Ids: batch.CoversRequestIds, HumanUserID: batch.HumanUserID})
	if err != nil {
		return nil, err
	}
	var closed []coveredCardAnswer
	for _, card := range covered {
		if card.ID == batch.ID {
			continue
		}
		answer, err := handler.answerCoveredCard(ctx, queries, workspace, batch, card, input, content)
		if err != nil {
			return nil, err
		}
		if answer != nil {
			closed = append(closed, *answer)
		}
	}
	return closed, nil
}

func (handler *Handler) answerCoveredCard(ctx context.Context, queries *db.Queries, workspace db.Workspace, batch, card db.DecisionRequest, input decisionCardAnswerInput, content string) (*coveredCardAnswer, error) {
	if !coveredCardOffers(batch, card, input.Option) {
		return nil, nil
	}
	// A card's own answer is refused while its person is in recovery; the batch
	// answer must not settle it around that.
	if err := refuseGatedAnswerWhileRecovering(ctx, queries, workspace, card, batch.HumanUserID); err != nil {
		return nil, err
	}
	root, err := commentguard.LockThreadForReplyAndLoadRoot(ctx, queries, card.WorkspaceID, card.IssueCommentID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	if _, err := queries.LockDecisionIssue(ctx, db.LockDecisionIssueParams{WorkspaceID: card.WorkspaceID, ID: card.IssueID}); errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	issue, err := queries.GetIssueInWorkspace(ctx, db.GetIssueInWorkspaceParams{WorkspaceID: card.WorkspaceID, ID: card.IssueID})
	if err != nil {
		return nil, err
	}
	category, err := issuestatus.CategoryWithError(ctx, queries, card.WorkspaceID, issue.Status)
	if err != nil {
		return nil, err
	}
	if category == issuestatus.CategoryDone || category == issuestatus.CategoryClosed {
		return nil, nil
	}
	effectiveAt, err := queries.DecisionRecordEffectiveAt(ctx, db.DecisionRecordEffectiveAtParams{WorkspaceID: card.WorkspaceID, ID: card.ID})
	if err != nil {
		return nil, err
	}
	commentID := dbid.NewV7()
	answer, err := queries.CreateDecisionAnswer(ctx, db.CreateDecisionAnswerParams{DecisionRequestID: card.ID, AnsweredByUserID: batch.HumanUserID, AnswerSource: pgtype.Text{String: "card", Valid: true}, ChatMessageIds: []pgtype.UUID{}, ContentSnapshot: content, EffectiveAt: effectiveAt, IssueCommentID: commentID, WakeAt: effectiveAt, ReplyText: pgtype.Text{String: strings.TrimSpace(input.Text), Valid: true}})
	if err != nil {
		return nil, err
	}
	var cleared []db.Comment
	if root != nil {
		if cleared, err = queries.ClearThreadResolutionForReply(ctx, db.ClearThreadResolutionForReplyParams{ThreadRootID: root.ID, IssueID: issue.ID, WorkspaceID: card.WorkspaceID}); err != nil {
			return nil, err
		}
	}
	link := fmt.Sprintf("Answered once on the batch card: [open the batch card](/%s/chat?session=%s&message=%s)", workspace.Slug, uuidToString(batch.ChatSessionID), uuidToString(batch.PingMessageID))
	comment, err := queries.CreateComment(ctx, db.CreateCommentParams{ID: commentID, IssueID: issue.ID, WorkspaceID: card.WorkspaceID, AuthorType: "agent", AuthorID: card.AskingAgentID, ParentID: card.IssueCommentID, Content: blockQuote(cardAnswerQuoteText(input)) + "\n\n" + link, Type: "comment", DecisionAnswerID: answer.ID, DecisionCommentKind: pgtype.Text{String: "quote", Valid: true}})
	if err != nil {
		return nil, err
	}
	marked, err := queries.MarkDecisionRequestAnsweredByBatch(ctx, db.MarkDecisionRequestAnsweredByBatchParams{WorkspaceID: card.WorkspaceID, ID: card.ID, BatchRequestID: batch.ID})
	if err != nil {
		return nil, err
	}
	if _, err := queries.MarkInboxReadByDecisionRequest(ctx, db.MarkInboxReadByDecisionRequestParams{WorkspaceID: card.WorkspaceID, DecisionRequestID: uuidToString(card.ID)}); err != nil {
		return nil, err
	}
	return &coveredCardAnswer{Request: marked, Answer: answer, Comment: comment.Comment(), Cleared: cleared}, nil
}

// publishCoveredCards fans the realtime events for every card a batch answer
// closed: the issue quote, the reopened thread and the answer state.
func (handler *Handler) publishCoveredCards(ctx context.Context, workspaceUUID pgtype.UUID, workspaceID, actorID string, covered []coveredCardAnswer) {
	for _, item := range covered {
		response := commentToResponse(item.Comment, nil, nil)
		response.DecisionCommentFields = handler.decisionCommentFields(ctx, workspaceUUID, []db.Comment{item.Comment}, pgtype.UUID{})[response.ID]
		handler.publish(protocol.EventCommentCreated, workspaceID, "member", actorID, map[string]any{"comment": response})
		handler.TaskService.PublishThreadUnresolvedOnReply(item.Cleared, workspaceID, "member", actorID)
		handler.publishDecisionAnswerUpdated(workspaceID, "member", actorID, item.Answer.ID, item.Request.IssueID, decisionAnswerState(item.Answer.WokenAt, item.Answer.DisclaimedAt))
	}
}

// parseBatchCovers decodes covers_request_ids. An action request cannot be a
// batch card: its answer is a gated authorization, not a shared choice.
func parseBatchCovers(writer http.ResponseWriter, raw []string, hasAction bool) ([]pgtype.UUID, bool) {
	if len(raw) == 0 {
		return nil, true
	}
	if hasAction {
		writeError(writer, http.StatusBadRequest, "an action request cannot cover other cards")
		return nil, false
	}
	if len(raw) > maxBatchCovers {
		writeError(writer, http.StatusBadRequest, fmt.Sprintf("a batch card covers at most %d cards", maxBatchCovers))
		return nil, false
	}
	seen := map[string]bool{}
	ids := make([]pgtype.UUID, 0, len(raw))
	for _, value := range raw {
		id, ok := parseUUIDOrBadRequest(writer, value, "covers_request_ids")
		if !ok {
			return nil, false
		}
		if !seen[value] {
			seen[value] = true
			ids = append(ids, id)
		}
	}
	return ids, true
}

// coverBatchRequests stores the covered ids on a new batch card. Every id must
// be an open, ungated card of the same workspace and human, else the whole
// request is refused: a silent drop would leave the human answering twice.
func coverBatchRequests(ctx context.Context, queries *db.Queries, batch db.DecisionRequest, covers []pgtype.UUID) (db.DecisionRequest, error) {
	if len(covers) == 0 {
		return batch, nil
	}
	if batch.EscalationClass != "reversible" && batch.EscalationClass != "product_direction" {
		return batch, decisionIntentRefusal{http.StatusBadRequest, "a batch card covers only reversible or product_direction cards"}
	}
	found, err := queries.LockBatchCoveredRequests(ctx, db.LockBatchCoveredRequestsParams{WorkspaceID: batch.WorkspaceID, Ids: covers, HumanUserID: batch.HumanUserID})
	if err != nil {
		return batch, err
	}
	if len(found) != len(covers) {
		return batch, decisionIntentRefusal{http.StatusBadRequest, "covers_request_ids must name open plain choice cards (reversible or product_direction) addressed to the same human; a card with an action, a governed write, an artifact or a gated class cannot be covered"}
	}
	batchOptions := askOptions(batch.Options)
	for _, card := range found {
		cardOptions := askOptions(card.Options)
		if len(batchOptions) == 0 || slices.ContainsFunc(batchOptions, func(option decisiontx.AskOption) bool { return !offersOption(cardOptions, option) }) {
			return batch, decisionIntentRefusal{http.StatusBadRequest, "every covered card must offer each option of the batch card with the same label and the same meaning, so each batch answer is an answer to that card's own question"}
		}
	}
	return queries.SetDecisionRequestCovers(ctx, db.SetDecisionRequestCoversParams{WorkspaceID: batch.WorkspaceID, ID: batch.ID, CoversRequestIds: covers})
}

// withdrawCoveredAnswers withdraws what a withdrawn batch answer decided on the
// covered cards: each reopens, loses its batch fold and its live answer is
// disclaimed, so no covered decision stays settled by an answer that is gone.
// Every path that withdraws or cancels a batch answer calls it, setting result.Covered.
func withdrawCoveredAnswers(ctx context.Context, queries *db.Queries, batch db.DecisionRequest, result *decisiontx.Withdrawal, userID pgtype.UUID) error {
	if len(batch.CoversRequestIds) == 0 {
		return nil
	}
	covered, err := queries.LockBatchAnsweredRequests(ctx, db.LockBatchAnsweredRequestsParams{WorkspaceID: batch.WorkspaceID, BatchRequestID: batch.ID})
	if err != nil {
		return err
	}
	for _, card := range covered {
		root, err := commentguard.LockThreadForReplyAndLoadRoot(ctx, queries, card.WorkspaceID, card.IssueCommentID)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if _, err := queries.LockDecisionIssue(ctx, db.LockDecisionIssueParams{WorkspaceID: card.WorkspaceID, ID: card.IssueID}); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		answer, err := queries.LockDecisionLiveAnswer(ctx, db.LockDecisionLiveAnswerParams{WorkspaceID: card.WorkspaceID, ID: card.ID})
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return err
		}
		// A card whose DM is gone cannot reopen there: withdraw it as an archived lane
		// (cancelled) so the batch and every covered answer still end in one transaction.
		laneStatus := "archived"
		session, err := queries.GetChatSessionInWorkspace(ctx, db.GetChatSessionInWorkspaceParams{WorkspaceID: card.WorkspaceID, ID: card.ChatSessionID})
		if err == nil {
			laneStatus = session.Status
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		withdrawn, err := decisiontx.Withdraw(ctx, queries, card, answer, laneStatus, "human", userID)
		if err != nil {
			return err
		}
		if root != nil && len(withdrawn.Comments) > 0 {
			if withdrawn.ClearedComments, err = queries.ClearThreadResolutionForReply(ctx, db.ClearThreadResolutionForReplyParams{ThreadRootID: root.ID, IssueID: card.IssueID, WorkspaceID: card.WorkspaceID}); err != nil {
				return err
			}
		}
		result.Covered = append(result.Covered, withdrawn)
	}
	return nil
}

// publishWithdrawalEvents publishes a withdrawal's comments, cleared threads and answer state,
// and those of every covered card it took back.
func (handler *Handler) publishWithdrawalEvents(workspaceID, actorType, actorID string, result decisiontx.Withdrawal, extra map[string]any) {
	for _, comment := range result.Comments {
		payload := map[string]any{"comment": commentToResponse(comment, nil, nil)}
		for key, value := range extra {
			payload[key] = value
		}
		handler.publish(protocol.EventCommentCreated, workspaceID, actorType, actorID, payload)
	}
	handler.TaskService.PublishThreadUnresolvedOnReply(result.ClearedComments, workspaceID, actorType, actorID)
	handler.publishDecisionAnswerUpdated(workspaceID, actorType, actorID, result.Answer.ID, result.Request.IssueID, decisionStateDisclaimed)
	for _, item := range result.Covered {
		handler.publishWithdrawalEvents(workspaceID, actorType, actorID, item, nil)
	}
}
