package handler

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/commentguard"
	"github.com/multica-ai/multica/server/internal/decisiontx"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

func (handler *Handler) DisclaimDecisionAnswer(writer http.ResponseWriter, request *http.Request) {
	switch request.Header.Get("X-Actor-Source") {
	case "interactive_jwt", "cli_jwt", "pat":
	default:
		writeError(writer, http.StatusForbidden, "decision disclaim requires the request's human")
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
	answerID, ok := parseUUIDOrBadRequest(writer, chi.URLParam(request, "id"), "id")
	if !ok {
		return
	}
	userID, err := util.ParseUUID(request.Header.Get("X-User-ID"))
	if err != nil {
		writeError(writer, http.StatusForbidden, "decision disclaim requires the request's human")
		return
	}
	var result decisiontx.Withdrawal
	for attempt := 0; ; attempt++ {
		result, err = handler.disclaimDecisionAnswerTx(request, workspaceUUID, answerID, userID)
		if err == nil || !decisiontx.IsRetryable(err) || attempt == 1 {
			break
		}
	}
	if err != nil {
		writeDecisionError(writer, err, "decision disclaim")
		return
	}
	for _, comment := range result.Comments {
		handler.publish(protocol.EventCommentCreated, workspaceID, "member", uuidToString(userID), map[string]any{"comment": commentToResponse(comment, nil, nil)})
	}
	handler.TaskService.PublishThreadUnresolvedOnReply(result.ClearedComments, workspaceID, "member", uuidToString(userID))
	handler.publishDecisionAnswerUpdated(workspaceID, "member", uuidToString(userID), result.Answer.ID, result.Request.IssueID, decisionStateDisclaimed)
	writeJSON(writer, http.StatusOK, result.Answer)
}

func (handler *Handler) disclaimDecisionAnswerTx(request *http.Request, workspaceID, answerID, userID pgtype.UUID) (decisiontx.Withdrawal, error) {
	ctx := request.Context()
	empty := decisiontx.Withdrawal{}
	answer, err := handler.Queries.GetDecisionAnswer(ctx, db.GetDecisionAnswerParams{WorkspaceID: workspaceID, ID: answerID})
	if errors.Is(err, pgx.ErrNoRows) {
		return empty, decisionIntentRefusal{http.StatusNotFound, "decision answer not found"}
	}
	if err != nil {
		return empty, err
	}
	located, err := handler.Queries.GetDecisionRequest(ctx, db.GetDecisionRequestParams{WorkspaceID: workspaceID, ID: answer.DecisionRequestID})
	if err != nil {
		return empty, err
	}
	if located.HumanUserID != userID {
		return empty, decisionIntentRefusal{http.StatusForbidden, "only the request's human can disclaim"}
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
	current, err := queries.LockDecisionRequest(ctx, db.LockDecisionRequestParams{WorkspaceID: workspaceID, ID: located.ID})
	if err != nil {
		return empty, err
	}
	if current.ChatSessionID != located.ChatSessionID || current.HumanUserID != userID || current.IssueID != located.IssueID || current.IssueCommentID != located.IssueCommentID {
		return empty, decisionIntentRefusal{http.StatusConflict, "decision context changed; retry"}
	}
	answer, err = queries.LockDecisionAnswer(ctx, db.LockDecisionAnswerParams{WorkspaceID: workspaceID, ID: answerID})
	if err != nil {
		return empty, err
	}
	if answer.DisclaimedAt.Valid {
		return empty, decisionIntentRefusal{http.StatusConflict, "decision answer is already withdrawn"}
	}
	session, err := queries.GetChatSessionInWorkspace(ctx, db.GetChatSessionInWorkspaceParams{WorkspaceID: workspaceID, ID: current.ChatSessionID})
	if err != nil {
		return empty, err
	}
	result, err := decisiontx.Withdraw(ctx, queries, current, answer, session.Status, "human", userID)
	if err != nil {
		return empty, err
	}
	if len(result.Comments) > 0 && root != nil {
		result.ClearedComments, err = queries.ClearThreadResolutionForReply(ctx, db.ClearThreadResolutionForReplyParams{ThreadRootID: root.ID, IssueID: current.IssueID, WorkspaceID: workspaceID})
		if err != nil {
			return empty, err
		}
	}
	if err := transaction.Commit(ctx); err != nil {
		return empty, err
	}
	handler.DecisionWorker.Notify()
	return result, nil
}
