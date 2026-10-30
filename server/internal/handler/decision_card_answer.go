package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/commentguard"
	"github.com/multica-ai/multica/server/internal/decisiontx"
	"github.com/multica-ai/multica/server/internal/issuestatus"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

const maxDecisionCardTextBytes = 8 << 10

// decisionCardAnswerInput is one card answer: a chosen option label, free text,
// or both. The label must match one the card offers.
type decisionCardAnswerInput struct {
	Option string `json:"option"`
	Text   string `json:"text"`
}

// AnswerDecisionRequest records the request's human answering on the card. The
// server writes the answer, the quote comment on the issue and the card state in
// one transaction, and schedules the issue wake itself. No DM-agent run is
// enqueued: that is what separates it from typing the answer in the composer.
func (handler *Handler) AnswerDecisionRequest(writer http.ResponseWriter, request *http.Request) {
	if request.Header.Get("X-Actor-Source") != "interactive_jwt" {
		writeError(writer, http.StatusForbidden, "a card answer requires the request's human in the web app")
		return
	}
	workspaceID := handler.resolveWorkspaceID(request)
	if _, ok := handler.requireWorkspaceMember(writer, request, workspaceID, "workspace not found"); !ok {
		return
	}
	workspaceUUID, ok := parseUUIDOrBadRequest(writer, workspaceID, "workspace_id")
	if !ok {
		return
	}
	requestID, ok := parseUUIDOrBadRequest(writer, chi.URLParam(request, "id"), "id")
	if !ok {
		return
	}
	userID, err := util.ParseUUID(request.Header.Get("X-User-ID"))
	if err != nil {
		writeError(writer, http.StatusForbidden, "a card answer requires the request's human in the web app")
		return
	}
	var input decisionCardAnswerInput
	if err := json.NewDecoder(http.MaxBytesReader(writer, request.Body, 2*maxDecisionCardTextBytes)).Decode(&input); err != nil {
		writeError(writer, http.StatusBadRequest, "invalid request body")
		return
	}
	var result decisionRecordResult
	for attempt := 0; ; attempt++ {
		result, err = handler.answerDecisionRequestTx(request, workspaceUUID, requestID, userID, input)
		if err == nil || !decisiontx.IsRetryable(err) || attempt == 1 {
			break
		}
	}
	if err != nil {
		writeDecisionError(writer, err, "decision answer")
		return
	}
	actorID := uuidToString(userID)
	response := commentToResponse(result.Comment, nil, nil)
	response.DecisionCommentFields = handler.decisionCommentFields(request.Context(), workspaceUUID, []db.Comment{result.Comment}, pgtype.UUID{})[response.ID]
	handler.publish(protocol.EventCommentCreated, workspaceID, "member", actorID, map[string]any{"comment": response})
	handler.TaskService.PublishThreadUnresolvedOnReply(result.ClearedComments, workspaceID, "member", actorID)
	handler.publishDecisionAnswerUpdated(workspaceID, "member", actorID, result.Answer.ID, result.Request.IssueID, decisionAnswerState(result.Answer.WokenAt, result.Answer.DisclaimedAt))
	for _, message := range []db.ChatMessage{result.AnswerMessage, result.Message} {
		handler.publishChatFor(protocol.EventChatMessage, workspaceID, "member", actorID, uuidToString(result.Request.HumanUserID), uuidToString(message.ChatSessionID), protocol.ChatMessagePayload{
			MessageKind:   message.MessageKind,
			ChatSessionID: uuidToString(message.ChatSessionID), MessageID: uuidToString(message.ID), Role: message.Role, Content: message.Content, CreatedAt: timestampToString(message.CreatedAt),
		})
	}
	writeJSON(writer, http.StatusCreated, response)
}

func (handler *Handler) answerDecisionRequestTx(request *http.Request, workspaceID, requestID, userID pgtype.UUID, input decisionCardAnswerInput) (decisionRecordResult, error) {
	ctx := request.Context()
	empty := decisionRecordResult{}
	located, err := handler.Queries.GetDecisionRequest(ctx, db.GetDecisionRequestParams{WorkspaceID: workspaceID, ID: requestID})
	if errors.Is(err, pgx.ErrNoRows) {
		return empty, decisionIntentRefusal{http.StatusNotFound, "decision request not found"}
	}
	if err != nil {
		return empty, err
	}
	if located.HumanUserID != userID {
		return empty, decisionIntentRefusal{http.StatusForbidden, "only the request's human can answer"}
	}
	content, err := cardAnswerContent(located, input)
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
		return empty, decisionIntentRefusal{http.StatusNotFound, "decision lane not found"}
	} else if err != nil {
		return empty, err
	}
	root, err := commentguard.LockThreadForReplyAndLoadRoot(ctx, queries, workspaceID, located.IssueCommentID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return empty, err
	}
	if _, err := queries.LockDecisionIssue(ctx, db.LockDecisionIssueParams{WorkspaceID: workspaceID, ID: located.IssueID}); errors.Is(err, pgx.ErrNoRows) {
		return empty, decisionIntentRefusal{http.StatusNotFound, "decision issue not found"}
	} else if err != nil {
		return empty, err
	}
	issue, err := queries.GetIssueInWorkspace(ctx, db.GetIssueInWorkspaceParams{WorkspaceID: workspaceID, ID: located.IssueID})
	if err != nil {
		return empty, err
	}
	decision, err := queries.LockDecisionRequest(ctx, db.LockDecisionRequestParams{WorkspaceID: workspaceID, ID: requestID})
	if err != nil {
		return empty, err
	}
	if decision.ChatSessionID != located.ChatSessionID || decision.HumanUserID != userID || decision.IssueCommentID != located.IssueCommentID {
		return empty, decisionIntentRefusal{http.StatusConflict, "decision context changed; retry"}
	}
	if decision.Origin != "asked" || decision.Status != "open" {
		return empty, decisionIntentRefusal{http.StatusConflict, "this request cannot accept another answer"}
	}
	category, err := issuestatus.CategoryWithError(ctx, queries, workspaceID, issue.Status)
	if err != nil {
		return empty, err
	}
	if category == issuestatus.CategoryDone || category == issuestatus.CategoryClosed {
		return empty, decisionIntentRefusal{http.StatusConflict, "the issue is closed"}
	}
	session, err := queries.GetChatSessionInWorkspace(ctx, db.GetChatSessionInWorkspaceParams{WorkspaceID: workspaceID, ID: decision.ChatSessionID})
	if err != nil {
		return empty, err
	}
	if session.CreatorID != userID || session.Status != "active" || session.ChannelOriginAt.Valid || session.IsAgentIntro {
		return empty, decisionIntentRefusal{http.StatusConflict, "the decision lane is not available"}
	}
	workspace, err := queries.GetWorkspace(ctx, workspaceID)
	if err != nil {
		return empty, err
	}
	effectiveAt, err := queries.DecisionRecordEffectiveAt(ctx, db.DecisionRecordEffectiveAtParams{WorkspaceID: workspaceID, ID: decision.ID})
	if err != nil {
		return empty, err
	}
	// A task-less user row: the card answer is the human's own message in the
	// DM, so the answer's source rules and the delivery lock hold unchanged.
	answerMessage, err := queries.CreateChatMessage(ctx, db.CreateChatMessageParams{ID: dbid.NewV7(), ChatSessionID: session.ID, Role: "user", Content: content, MessageKind: pgtype.Text{String: "message", Valid: true}, SentVia: pgtype.Text{String: "interactive_jwt", Valid: true}})
	if err != nil {
		return empty, err
	}
	commentID := dbid.NewV7()
	chosen, err := cardOptionIndex(decision, input)
	if err != nil {
		return empty, err
	}
	answer, err := queries.CreateDecisionAnswer(ctx, db.CreateDecisionAnswerParams{DecisionRequestID: decision.ID, SourceChatSessionID: session.ID, ChatMessageIds: []pgtype.UUID{answerMessage.ID}, ContentSnapshot: content, EffectiveAt: effectiveAt, IssueCommentID: commentID, WakeAt: effectiveAt, ChosenOptionIndex: chosen, ReplyText: pgtype.Text{String: strings.TrimSpace(input.Text), Valid: true}})
	if err != nil {
		return empty, err
	}
	var cleared []db.Comment
	if root != nil {
		cleared, err = queries.ClearThreadResolutionForReply(ctx, db.ClearThreadResolutionForReplyParams{ThreadRootID: root.ID, IssueID: issue.ID, WorkspaceID: workspaceID})
		if err != nil {
			return empty, err
		}
	}
	card := fmt.Sprintf("Answered on the decision card: [open the card](/%s/chat?session=%s&message=%s)", workspace.Slug, uuidToString(session.ID), uuidToString(decision.PingMessageID))
	comment, err := queries.CreateComment(ctx, db.CreateCommentParams{ID: commentID, IssueID: issue.ID, WorkspaceID: workspaceID, AuthorType: "agent", AuthorID: decision.AskingAgentID, ParentID: decision.IssueCommentID, Content: blockQuote(content) + "\n\n" + card, Type: "comment", DecisionAnswerID: answer.ID, DecisionCommentKind: pgtype.Text{String: "quote", Valid: true}})
	if err != nil {
		return empty, err
	}
	note := fmt.Sprintf("Decision recorded for [%s-%d](mention://issue/%s).\nClass: %s.\nOpen issue: /issue/%s", workspace.IssuePrefix, issue.Number, uuidToString(issue.ID), decision.EscalationClass, uuidToString(issue.ID))
	message, err := queries.CreateChatMessage(ctx, db.CreateChatMessageParams{ID: dbid.NewV7(), ChatSessionID: session.ID, Role: "assistant", Content: note, MessageKind: pgtype.Text{String: "decision_reference", Valid: true}, SentVia: pgtype.Text{String: "interactive_jwt", Valid: true}, DecisionAnswerID: answer.ID})
	if err != nil {
		return empty, err
	}
	if err := queries.TouchChatSession(ctx, session.ID); err != nil {
		return empty, err
	}
	decision, err = queries.MarkDecisionRequestAnswered(ctx, db.MarkDecisionRequestAnsweredParams{WorkspaceID: workspaceID, ID: decision.ID})
	if err != nil {
		return empty, err
	}
	if _, err := queries.MarkInboxReadByDecisionRequest(ctx, db.MarkInboxReadByDecisionRequestParams{WorkspaceID: workspaceID, DecisionRequestID: uuidToString(decision.ID)}); err != nil {
		return empty, err
	}
	if err := transaction.Commit(ctx); err != nil {
		return empty, err
	}
	handler.DecisionWorker.Notify()
	return decisionRecordResult{Request: decision, Answer: answer, Comment: comment.Comment(), Message: message, AnswerMessage: answerMessage, ClearedComments: cleared}, nil
}

// cardAnswerContent joins the chosen option label and the free text into the
// one snapshot the answer, the DM row and the issue quote all carry.
func cardAnswerContent(decision db.DecisionRequest, input decisionCardAnswerInput) (string, error) {
	text := strings.TrimSpace(input.Text)
	if len(text) > maxDecisionCardTextBytes {
		return "", decisionIntentRefusal{http.StatusBadRequest, "answer text is too long"}
	}
	var options []decisiontx.AskOption
	if err := json.Unmarshal(decision.Options, &options); err != nil {
		return "", err
	}
	if input.Option != "" && !slices.ContainsFunc(options, func(option decisiontx.AskOption) bool { return option.Label == input.Option }) {
		return "", decisionIntentRefusal{http.StatusBadRequest, "option is not on this card"}
	}
	switch {
	case input.Option != "" && text != "":
		return input.Option + "\n\n" + text, nil
	case input.Option != "":
		return input.Option, nil
	case text != "":
		return text, nil
	}
	return "", decisionIntentRefusal{http.StatusBadRequest, "an answer needs an option or text"}
}

// cardOptionIndex is the 0-based position of the chosen option on the card, or
// NULL for a text-only answer.
func cardOptionIndex(decision db.DecisionRequest, input decisionCardAnswerInput) (pgtype.Int4, error) {
	if input.Option == "" {
		return pgtype.Int4{}, nil
	}
	var options []decisiontx.AskOption
	if err := json.Unmarshal(decision.Options, &options); err != nil {
		return pgtype.Int4{}, err
	}
	index := slices.IndexFunc(options, func(option decisiontx.AskOption) bool { return option.Label == input.Option })
	return pgtype.Int4{Int32: int32(index), Valid: index >= 0}, nil
}
