package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/passkey"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const (
	decisionActionDeadline       = 20 * time.Second
	decisionActionAcquireTimeout = 3 * time.Second
)

// Readback lock timeouts. Variables so tests can shorten the wait.
var (
	commitReadbackTimeout  = 15 * time.Second
	outcomeReadbackTimeout = 5 * time.Second
)

// uncertainCommit marks an error raised while COMMIT was in flight: the
// transaction may or may not have been applied.
type uncertainCommit struct{ error }

func (err uncertainCommit) Unwrap() error { return err.error }

// actionTxBusy reports an error after which the whole transaction is rolled
// back and the human may retry: a lock or statement timeout, a deadlock, a
// serialization failure or a lost connection (plan 3.2 rule 4).
func actionTxBusy(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var pgError *pgconn.PgError
	if !errors.As(err, &pgError) {
		return false
	}
	switch pgError.Code {
	case "55P03", "40P01", "40001", "57014":
		return true
	}
	return strings.HasPrefix(pgError.Code, "08")
}

var (
	errBusyRetry      = decisionIntentRefusal{http.StatusConflict, "busy_retry: another change holds this decision; retry"}
	errNotPending     = decisionIntentRefusal{http.StatusConflict, "not_pending: this action is no longer waiting for a decision"}
	errOutcomeUnknown = decisionIntentRefusal{http.StatusServiceUnavailable, "outcome_unknown: check the issue before deciding again"}
	errOutcomeRetry   = decisionIntentRefusal{http.StatusServiceUnavailable, "outcome_unknown_retry: the decision was not applied; decide again"}
	errNotTheHuman    = decisionIntentRefusal{http.StatusForbidden, "only the request's human can decide this action"}
	errActionNotFound = decisionIntentRefusal{http.StatusNotFound, "decision action not found"}
)

// decisionActionOutcome is the card's read of an action's final state.
type decisionActionOutcome struct {
	ID           string          `json:"id"`
	State        string          `json:"state"`
	Result       json.RawMessage `json:"result,omitempty"`
	ErrorCode    string          `json:"error_code,omitempty"`
	ErrorMessage string          `json:"error_message,omitempty"`
}

func newDecisionActionOutcome(action db.DecisionAction) decisionActionOutcome {
	return decisionActionOutcome{
		ID: uuidToString(action.ID), State: action.State, Result: action.Result,
		ErrorCode: action.ErrorCode.String, ErrorMessage: action.ErrorMessage.String,
	}
}

// decisionActionCaller is the authenticated human behind a card click.
type decisionActionCaller struct {
	workspaceID, actionID, userID pgtype.UUID
}

// authorizeDecisionActionCaller admits only a human actor who is a member of
// the workspace. Machine credentials never decide an action.
func (handler *Handler) authorizeDecisionActionCaller(writer http.ResponseWriter, request *http.Request) (decisionActionCaller, bool) {
	var caller decisionActionCaller
	switch request.Header.Get("X-Actor-Source") {
	case "interactive_jwt", "cli_jwt", "pat":
	default:
		writeError(writer, http.StatusForbidden, errNotTheHuman.message)
		return caller, false
	}
	workspaceID := handler.resolveWorkspaceID(request)
	if _, ok := handler.requireWorkspaceMember(writer, request, workspaceID, "workspace not found"); !ok {
		return caller, false
	}
	var ok bool
	if caller.workspaceID, ok = parseUUIDOrBadRequest(writer, workspaceID, "workspace_id"); !ok {
		return caller, false
	}
	if caller.actionID, ok = parseUUIDOrBadRequest(writer, chi.URLParam(request, "id"), "id"); !ok {
		return caller, false
	}
	userID, err := util.ParseUUID(request.Header.Get("X-User-ID"))
	if err != nil {
		writeError(writer, http.StatusForbidden, errNotTheHuman.message)
		return caller, false
	}
	caller.userID = userID
	return caller, true
}

// RejectDecisionAction ends a pending action as rejected and records the
// human's click as a card answer.
func (handler *Handler) RejectDecisionAction(writer http.ResponseWriter, request *http.Request) {
	caller, ok := handler.authorizeDecisionActionCaller(writer, request)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), decisionActionDeadline)
	defer cancel()
	action, err := handler.rejectDecisionActionTx(ctx, caller)
	var uncertain uncertainCommit
	if errors.As(err, &uncertain) {
		action, err = handler.readbackAfterUncertainCommit(ctx, caller)
	}
	if err != nil {
		writeDecisionActionError(writer, err)
		return
	}
	handler.DecisionWorker.Notify()
	writeJSON(writer, http.StatusOK, newDecisionActionResponse(action))
}

// GetDecisionActionOutcome reads an action's state under its row lock, so a
// pending read proves no earlier transaction is still deciding it.
func (handler *Handler) GetDecisionActionOutcome(writer http.ResponseWriter, request *http.Request) {
	caller, ok := handler.authorizeDecisionActionCaller(writer, request)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), decisionActionDeadline)
	defer cancel()
	action, err := handler.readDecisionActionUnderLock(ctx, caller.workspaceID, caller.actionID, outcomeReadbackTimeout)
	if err == nil && !handler.isActionsHuman(ctx, caller, action) {
		err = errNotTheHuman
	}
	if err != nil {
		writeDecisionActionError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, newDecisionActionOutcome(action))
}

func (handler *Handler) isActionsHuman(ctx context.Context, caller decisionActionCaller, action db.DecisionAction) bool {
	decisionRequest, err := handler.Queries.GetDecisionRequest(ctx, db.GetDecisionRequestParams{WorkspaceID: caller.workspaceID, ID: action.DecisionRequestID})
	return err == nil && decisionRequest.HumanUserID == caller.userID
}

func writeDecisionActionError(writer http.ResponseWriter, err error) {
	var refusal decisionIntentRefusal
	var proofRefusal passkey.Refusal
	switch {
	case errors.As(err, &refusal):
		writeError(writer, refusal.status, refusal.message)
	case errors.As(err, &proofRefusal):
		writeError(writer, proofRefusal.Status, proofRefusal.Message)
	case actionTxBusy(err):
		writer.Header().Set("Retry-After", "1")
		writeError(writer, errBusyRetry.status, errBusyRetry.message)
	default:
		writeError(writer, http.StatusInternalServerError, "failed to decide the action")
	}
}

// beginActionTx takes one pooled connection (bounded acquire) and bounds every
// implicit wait inside the transaction.
func (handler *Handler) beginActionTx(ctx context.Context, lockTimeout time.Duration) (pgx.Tx, error) {
	acquire, cancel := context.WithTimeout(ctx, decisionActionAcquireTimeout)
	defer cancel()
	transaction, err := handler.TxStarter.Begin(acquire)
	if err != nil {
		return nil, errBusyRetry
	}
	settings := fmt.Sprintf(`SET LOCAL lock_timeout = '%dms'; SET LOCAL statement_timeout = '10s'`, lockTimeout.Milliseconds())
	if _, err := transaction.Exec(ctx, settings); err != nil {
		_ = transaction.Rollback(context.WithoutCancel(ctx))
		return nil, err
	}
	return transaction, nil
}

// rejectDecisionActionTx runs the reject lock sequence (plan 3.2, rows 1, 3, 4,
// 7, 8, 9, 11 — the rows a reject writes or must fence) in one transaction.
func (handler *Handler) rejectDecisionActionTx(ctx context.Context, caller decisionActionCaller) (db.DecisionAction, error) {
	empty := db.DecisionAction{}
	_, located, err := handler.locateDecisionAction(ctx, caller)
	if err != nil {
		return empty, err
	}
	transaction, err := handler.beginActionTx(ctx, 2*time.Second)
	if err != nil {
		return empty, err
	}
	defer transaction.Rollback(context.WithoutCancel(ctx))
	queries := handler.Queries.WithTx(transaction)
	if _, err := queries.LockWorkspaceKeyShareNowait(ctx, caller.workspaceID); err != nil {
		return empty, err
	}
	if _, err := queries.LockDecisionSessionNowait(ctx, db.LockDecisionSessionNowaitParams{WorkspaceID: caller.workspaceID, ID: located.ChatSessionID}); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return empty, err
	}
	if _, err := queries.LockDecisionAskingAgent(ctx, db.LockDecisionAskingAgentParams{WorkspaceID: caller.workspaceID, ID: located.AskingAgentID}); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return empty, err
	}
	if err := lockDecisionThreadAndIssue(ctx, queries, caller.workspaceID, located); err != nil {
		return empty, err
	}
	current, err := queries.LockDecisionRequestNowait(ctx, db.LockDecisionRequestNowaitParams{WorkspaceID: caller.workspaceID, ID: located.ID})
	if err != nil {
		return empty, err
	}
	if _, err := queries.LockDecisionLiveAnswerNowait(ctx, db.LockDecisionLiveAnswerNowaitParams{WorkspaceID: caller.workspaceID, ID: current.ID}); err == nil {
		return empty, errNotPending
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return empty, err
	}
	action, err := queries.LockDecisionActionNowait(ctx, db.LockDecisionActionNowaitParams{ID: caller.actionID, WorkspaceID: caller.workspaceID})
	if err != nil {
		return empty, err
	}
	if _, err := queries.LockDecisionActionMemberNowait(ctx, db.LockDecisionActionMemberNowaitParams{WorkspaceID: caller.workspaceID, UserID: caller.userID}); errors.Is(err, pgx.ErrNoRows) {
		return empty, errNotTheHuman
	} else if err != nil {
		return empty, err
	}
	if current.HumanUserID != caller.userID {
		return empty, errNotTheHuman
	}
	if current.Status != "open" || action.State != "pending" {
		return empty, errNotPending
	}
	rejected, err := queries.RejectDecisionActionRow(ctx, db.RejectDecisionActionRowParams{ID: action.ID, WorkspaceID: caller.workspaceID})
	if err != nil {
		return empty, err
	}
	if err := finishCardDecision(ctx, queries, caller, current, rejected, "rejected", "Rejected "+action.ActionType+"."); err != nil {
		return empty, err
	}
	if err := transaction.Commit(ctx); err != nil {
		return empty, uncertainCommit{err}
	}
	return rejected, nil
}

// readbackAfterUncertainCommit learns what an interrupted COMMIT did. It runs
// on a fresh connection: the broken one is already back with the pool.
func (handler *Handler) readbackAfterUncertainCommit(ctx context.Context, caller decisionActionCaller) (db.DecisionAction, error) {
	timeout := commitReadbackTimeout
	if deadline, ok := ctx.Deadline(); ok {
		timeout = min(timeout, time.Until(deadline))
	}
	action, err := handler.readDecisionActionUnderLock(ctx, caller.workspaceID, caller.actionID, timeout)
	if err != nil {
		return action, err
	}
	if action.State == "pending" {
		return db.DecisionAction{}, errOutcomeRetry
	}
	return action, nil
}

// readDecisionActionUnderLock reads the action FOR SHARE. The deciding
// transaction holds the row FOR UPDATE until it ends, so the read returns only
// after that transaction has committed or aborted. Anything that stops the
// read from completing is "outcome unknown": the caller must not assume the
// action is still pending.
func (handler *Handler) readDecisionActionUnderLock(ctx context.Context, workspaceID, actionID pgtype.UUID, timeout time.Duration) (db.DecisionAction, error) {
	empty := db.DecisionAction{}
	if timeout <= 0 {
		return empty, errOutcomeUnknown
	}
	transaction, err := handler.beginActionTx(ctx, timeout)
	if err != nil {
		return empty, errOutcomeUnknown
	}
	defer transaction.Rollback(context.WithoutCancel(ctx))
	action, err := handler.Queries.WithTx(transaction).ReadDecisionActionForShare(ctx, db.ReadDecisionActionForShareParams{ID: actionID, WorkspaceID: workspaceID})
	if errors.Is(err, pgx.ErrNoRows) {
		return empty, errActionNotFound
	}
	if err != nil {
		if actionTxBusy(err) {
			return empty, errOutcomeUnknown
		}
		return empty, err
	}
	return action, nil
}

// locateDecisionAction reads the action and its request without locks, to
// pick the rows the deciding transaction must lock, and checks the caller is
// the request's human.
func (handler *Handler) locateDecisionAction(ctx context.Context, caller decisionActionCaller) (db.DecisionAction, db.DecisionRequest, error) {
	preview, err := handler.Queries.GetDecisionAction(ctx, db.GetDecisionActionParams{ID: caller.actionID, WorkspaceID: caller.workspaceID})
	if errors.Is(err, pgx.ErrNoRows) {
		return preview, db.DecisionRequest{}, errActionNotFound
	}
	if err != nil {
		return preview, db.DecisionRequest{}, err
	}
	located, err := handler.Queries.GetDecisionRequest(ctx, db.GetDecisionRequestParams{WorkspaceID: caller.workspaceID, ID: preview.DecisionRequestID})
	if err != nil {
		return preview, located, err
	}
	if located.HumanUserID != caller.userID {
		return preview, located, errNotTheHuman
	}
	return preview, located, nil
}
