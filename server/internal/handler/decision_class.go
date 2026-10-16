package handler

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
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
	if current.Status == "answered" {
		answer, err := queries.LockDecisionLiveAnswer(ctx, db.LockDecisionLiveAnswerParams{WorkspaceID: workspaceID, ID: current.ID})
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return empty, err
		}
		if err == nil && !answer.WokenAt.Valid {
			if _, err := queries.HoldDecisionAnswerForClass(ctx, db.HoldDecisionAnswerForClassParams{WorkspaceID: workspaceID, ID: answer.ID}); err != nil {
				return empty, err
			}
		}
	}
	if err := transaction.Commit(ctx); err != nil {
		return empty, err
	}
	handler.DecisionWorker.Notify()
	return result, nil
}
