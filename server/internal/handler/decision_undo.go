package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/decisiontx"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const (
	decisionUndoGraceKey     = "decision_undo_grace"
	decisionScreenKey        = "decision_screen"
	defaultDecisionUndoGrace = 5
	maxDecisionUndoGrace     = 30
	// undoReason is the disclaim_reason of an answer taken back inside its grace.
	undoReason = "undo"
)

var errUndoExpired = decisionIntentRefusal{http.StatusConflict, "undo_expired: the undo window has passed"}

// decisionUndoGrace is the workspace's undo grace (plan r4, section 3.11): 0 to 30 s. An explicit
// setting wins; without one the grace is 5 s when decision_screen is on and off otherwise, so a
// workspace that has not opted in keeps today's behaviour.
func decisionUndoGrace(settings []byte) time.Duration {
	var values map[string]json.RawMessage
	if json.Unmarshal(settings, &values) != nil {
		return 0
	}
	var seconds *float64
	if raw, ok := values[decisionUndoGraceKey]; ok && json.Unmarshal(raw, &seconds) == nil && seconds != nil {
		return time.Duration(min(max(*seconds, 0), maxDecisionUndoGrace) * float64(time.Second))
	}
	var screen string
	if json.Unmarshal(values[decisionScreenKey], &screen) == nil && screen == "on" {
		return defaultDecisionUndoGrace * time.Second
	}
	return 0
}

// UndoDecisionRequest takes a card answer back before its grace ends: the answer is disclaimed with
// reason undo and the request reopens, so the wake never runs.
func (handler *Handler) UndoDecisionRequest(writer http.ResponseWriter, request *http.Request) {
	switch request.Header.Get("X-Actor-Source") {
	case "interactive_jwt", "cli_jwt", "pat":
	default:
		writeError(writer, http.StatusForbidden, "undo requires the answering member")
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
		writeError(writer, http.StatusForbidden, "undo requires the answering member")
		return
	}
	var result decisiontx.Withdrawal
	for attempt := 0; ; attempt++ {
		result, err = handler.undoDecisionAnswer(request, workspaceUUID, requestID, userID)
		if err == nil || !decisiontx.IsRetryable(err) || attempt == 1 {
			break
		}
	}
	if err != nil {
		writeDecisionError(writer, err, "decision undo")
		return
	}
	handler.publishWithdrawalEvents(workspaceID, "member", uuidToString(userID), result, nil)
	writeJSON(writer, http.StatusOK, result.Answer)
}

func (handler *Handler) undoDecisionAnswer(request *http.Request, workspaceID, requestID, userID pgtype.UUID) (decisiontx.Withdrawal, error) {
	ctx := request.Context()
	located, err := handler.Queries.GetDecisionRequest(ctx, db.GetDecisionRequestParams{WorkspaceID: workspaceID, ID: requestID})
	if errors.Is(err, pgx.ErrNoRows) {
		return decisiontx.Withdrawal{}, decisionIntentRefusal{http.StatusNotFound, "decision request not found"}
	}
	if err != nil {
		return decisiontx.Withdrawal{}, err
	}
	answer, err := handler.Queries.GetLiveDecisionAnswer(ctx, db.GetLiveDecisionAnswerParams{WorkspaceID: workspaceID, ID: requestID})
	if errors.Is(err, pgx.ErrNoRows) {
		return decisiontx.Withdrawal{}, decisionIntentRefusal{http.StatusConflict, "this request has no answer to undo"}
	}
	if err != nil {
		return decisiontx.Withdrawal{}, err
	}
	if answer.AnsweredByUserID != userID {
		return decisiontx.Withdrawal{}, decisionIntentRefusal{http.StatusForbidden, "only the answering member can undo"}
	}
	return handler.withdrawAnswerTx(request, workspaceID, located, answer.ID, userID, undoReason, func(locked db.DecisionAnswer) error {
		if !locked.UndoUntil.Valid || !time.Now().Before(locked.UndoUntil.Time) {
			return errUndoExpired
		}
		return nil
	})
}

// runningUndoUntil is undo_until while the grace still runs on a live answer, nil otherwise.
func runningUndoUntil(undoUntil, disclaimedAt pgtype.Timestamptz) *string {
	if !undoUntil.Valid || disclaimedAt.Valid || !time.Now().Before(undoUntil.Time) {
		return nil
	}
	return timestampToPtr(undoUntil)
}
