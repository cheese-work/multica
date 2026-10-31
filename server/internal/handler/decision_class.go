package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/commentguard"
	"github.com/multica-ai/multica/server/internal/decisiontx"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func (handler *Handler) RaiseDecisionClass(writer http.ResponseWriter, request *http.Request) {
	switch request.Header.Get("X-Actor-Source") {
	case "interactive_jwt", "cli_jwt", "pat":
	default:
		writeError(writer, http.StatusForbidden, "decision class changes require the request's human")
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
		writeError(writer, http.StatusForbidden, "decision class changes require the request's human")
		return
	}
	var input struct {
		EscalationClass string `json:"escalation_class"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(writer, request.Body, maxCommentContentBytes)).Decode(&input); err != nil {
		writeError(writer, http.StatusBadRequest, "invalid decision class")
		return
	}
	switch input.EscalationClass {
	case "reversible", "spend", "deploy", "publish", "irreversible", "product_direction":
	default:
		writeError(writer, http.StatusBadRequest, "invalid decision class")
		return
	}
	var result db.DecisionRequest
	for attempt := 0; ; attempt++ {
		result, err = handler.raiseDecisionClassTx(request, workspaceUUID, requestID, userID, input.EscalationClass)
		if err == nil || !decisiontx.IsRetryable(err) || attempt == 1 {
			break
		}
	}
	if err != nil {
		writeDecisionError(writer, err, "decision class change")
		return
	}
	writeJSON(writer, http.StatusOK, result)
}

func (handler *Handler) raiseDecisionClassTx(request *http.Request, workspaceID, requestID, userID pgtype.UUID, class string) (db.DecisionRequest, error) {
	ctx := request.Context()
	empty := db.DecisionRequest{}
	transaction, err := handler.TxStarter.Begin(ctx)
	if err != nil {
		return empty, err
	}
	defer transaction.Rollback(ctx)
	queries := handler.Queries.WithTx(transaction)
	if _, err := queries.LockWorkspaceForChatSessionCreate(ctx, workspaceID); err != nil {
		return empty, err
	}
	// A non-owner's request above reversible ends here instead of being held, so it takes the lifecycle locks.
	toOwner, err := raisesPastNonOwner(ctx, queries, workspaceID, requestID, userID)
	if err != nil {
		return empty, err
	}
	var root *db.Comment
	if toOwner {
		located, err := queries.GetDecisionRequest(ctx, db.GetDecisionRequestParams{WorkspaceID: workspaceID, ID: requestID})
		if err != nil {
			return empty, err
		}
		if _, err := queries.LockDecisionSession(ctx, db.LockDecisionSessionParams{WorkspaceID: workspaceID, ID: located.ChatSessionID}); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return empty, err
		}
		if _, err := queries.LockDecisionAskingAgent(ctx, db.LockDecisionAskingAgentParams{WorkspaceID: workspaceID, ID: located.AskingAgentID}); err != nil {
			return empty, err
		}
		if root, err = commentguard.LockThreadForReplyAndLoadRoot(ctx, queries, workspaceID, located.IssueCommentID); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return empty, err
		}
		if _, err := queries.LockDecisionIssue(ctx, db.LockDecisionIssueParams{WorkspaceID: workspaceID, ID: located.IssueID}); err != nil {
			return empty, err
		}
	}
	current, err := queries.LockDecisionRequest(ctx, db.LockDecisionRequestParams{WorkspaceID: workspaceID, ID: requestID})
	if errors.Is(err, pgx.ErrNoRows) {
		return empty, decisionIntentRefusal{http.StatusNotFound, "decision request not found"}
	}
	if err != nil {
		return empty, err
	}
	if current.HumanUserID != userID {
		return empty, decisionIntentRefusal{http.StatusForbidden, "only the request's human can raise its class"}
	}
	if current.Status == "cancelled" || current.EscalationClass != "reversible" || class == "reversible" {
		return empty, decisionIntentRefusal{http.StatusConflict, "decision class can only be raised from reversible to an escalation class on a non-cancelled request"}
	}
	result, err := queries.RaiseDecisionClass(ctx, db.RaiseDecisionClassParams{WorkspaceID: workspaceID, ID: current.ID, EscalationClass: class})
	if err != nil {
		return empty, err
	}
	if err := queries.CreateDecisionClassChange(ctx, db.CreateDecisionClassChangeParams{WorkspaceID: workspaceID, ID: current.ID, FromClass: current.EscalationClass, ToClass: class, ChangedByUserID: userID}); err != nil {
		return empty, err
	}
	var live db.DecisionAnswer
	if current.Status == "answered" {
		answer, err := queries.LockDecisionLiveAnswer(ctx, db.LockDecisionLiveAnswerParams{WorkspaceID: workspaceID, ID: current.ID})
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return empty, err
		}
		if err == nil {
			live = answer
		}
	}
	if toOwner {
		withdrawn, err := cancelRaisedPastNonOwner(ctx, queries, result, live, root, userID)
		if err != nil {
			return empty, err
		}
		if err := transaction.Commit(ctx); err != nil {
			return empty, err
		}
		handler.publishRaisedPastNonOwner(workspaceID, userID, withdrawn)
		handler.DecisionWorker.Notify()
		return withdrawn.withdrawal.Request, nil
	}
	if live.ID.Valid && !live.WokenAt.Valid {
		if _, err := queries.HoldDecisionAnswerForClass(ctx, db.HoldDecisionAnswerForClassParams{WorkspaceID: workspaceID, ID: live.ID}); err != nil {
			return empty, err
		}
	}
	if err := transaction.Commit(ctx); err != nil {
		return empty, err
	}
	handler.DecisionWorker.Notify()
	return result, nil
}

// raisesPastNonOwner reports whether the request's human is not a workspace owner. A delegate may not
// hold any class above reversible, so the caller only asks for a raise.
func raisesPastNonOwner(ctx context.Context, queries *db.Queries, workspaceID, requestID, userID pgtype.UUID) (bool, error) {
	located, err := queries.GetDecisionRequest(ctx, db.GetDecisionRequestParams{WorkspaceID: workspaceID, ID: requestID})
	if errors.Is(err, pgx.ErrNoRows) || err == nil && located.HumanUserID != userID {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	authority, err := queries.DecisionHumanAuthority(ctx, db.DecisionHumanAuthorityParams{WorkspaceID: workspaceID, UserID: userID, ProjectID: pgtype.UUID{}})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil && !authority.IsOwner, err
}

// cancelRaisedPastNonOwner ends a request raised past reversible by a non-owner. A live answer goes
// through the normal withdrawal, which cancels the request and, once woken, queues one retraction; an
// open request gets the lifecycle cancel. Neither reopens the request.
func cancelRaisedPastNonOwner(ctx context.Context, queries *db.Queries, raised db.DecisionRequest, live db.DecisionAnswer, root *db.Comment, userID pgtype.UUID) (decisionCancelOutcome, error) {
	if !live.ID.Valid {
		return cancelLockedDecision(ctx, queries, raised, live, root, decisiontx.ClassNeedsOwner)
	}
	session, err := queries.GetChatSessionInWorkspace(ctx, db.GetChatSessionInWorkspaceParams{WorkspaceID: raised.WorkspaceID, ID: raised.ChatSessionID})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return decisionCancelOutcome{}, err
	}
	withdrawn, err := decisiontx.Withdraw(ctx, queries, raised, live, session.Status, decisiontx.ClassNeedsOwner, userID)
	if err != nil {
		return decisionCancelOutcome{}, err
	}
	if len(withdrawn.Comments) > 0 && root != nil {
		withdrawn.ClearedComments, err = queries.ClearThreadResolutionForReply(ctx, db.ClearThreadResolutionForReplyParams{ThreadRootID: root.ID, IssueID: raised.IssueID, WorkspaceID: raised.WorkspaceID})
		if err != nil {
			return decisionCancelOutcome{}, err
		}
	}
	return decisionCancelOutcome{withdrawal: withdrawn, session: session}, nil
}

func (handler *Handler) publishRaisedPastNonOwner(workspaceID, userID pgtype.UUID, out decisionCancelOutcome) {
	request := out.withdrawal.Request
	handler.publishDecisionCancelNote(workspaceID, request, out)
	if out.withdrawal.Answer.ID.Valid {
		handler.TaskService.PublishThreadUnresolvedOnReply(out.withdrawal.ClearedComments, uuidToString(workspaceID), "member", uuidToString(userID))
		handler.publishDecisionAnswerUpdated(uuidToString(workspaceID), "member", uuidToString(userID), out.withdrawal.Answer.ID, request.IssueID, decisionStateDisclaimed)
	}
}
