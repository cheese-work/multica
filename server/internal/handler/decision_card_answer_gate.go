package handler

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/passkey"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

var (
	errAssertionRequired = decisionIntentRefusal{http.StatusForbidden, "assertion_required: sign this answer with your passkey"}
	errSessionUnbound    = decisionIntentRefusal{http.StatusForbidden, "session_unbound: sign in again to answer with a passkey"}
	errStaleAnswer       = decisionIntentRefusal{http.StatusConflict, "stale_answer: the request or workspace policy changed; reload the card"}
	errNotGated          = decisionIntentRefusal{http.StatusConflict, "not_gated: this answer needs no passkey"}
)

// answerNeedsAssertion is true when the workspace gate blocks and the class is
// one a card answer may not settle on a bare session.
func answerNeedsAssertion(workspace db.Workspace, decision db.DecisionRequest) bool {
	switch decision.EscalationClass {
	case "irreversible", "deploy", "spend", "publish":
		return workspace.DecisionPasskeyGate == passkey.GateBlock
	}
	return false
}

func answerBinding(workspace db.Workspace, decision db.DecisionRequest, userID pgtype.UUID, sessionID string, input decisionCardAnswerInput) passkey.AnswerBinding {
	return passkey.AnswerBinding{
		WorkspaceID: workspace.ID, DecisionID: decision.ID, AnswerRevision: decision.AnswerRevision, PolicyRevision: workspace.PolicyRevision,
		RecipientUserID: userID, SessionID: sessionID, Option: input.Option, Text: strings.TrimSpace(input.Text),
	}
}

// verifyGatedAnswer runs inside the answer transaction, after the request is
// locked, so the revisions it compares and the proof it consumes are the ones
// the answer commits with. A refusal rolls the transaction back and leaves the
// proof unconsumed.
func (handler *Handler) verifyGatedAnswer(ctx context.Context, queries *db.Queries, workspace db.Workspace, decision db.DecisionRequest, userID pgtype.UUID, sessionID string, input decisionCardAnswerInput) error {
	if !answerNeedsAssertion(workspace, decision) {
		return nil
	}
	switch {
	case len(input.Assertion) == 0:
		return errAssertionRequired
	case handler.Passkeys == nil:
		return errApprovalUnavailable
	case input.AnswerRevision != decision.AnswerRevision || input.PolicyRevision != workspace.PolicyRevision:
		return errStaleAnswer
	case sessionID == "":
		return errSessionUnbound
	}
	_, err := handler.Passkeys.VerifyAnswer(ctx, queries, answerBinding(workspace, decision, userID, sessionID, input), input.Assertion)
	return err
}

type answerChallengeResponse struct {
	challengeResponse
	AnswerRevision int32 `json:"answer_revision"`
	PolicyRevision int32 `json:"policy_revision"`
}

// ChallengeDecisionAnswer opens the human's assertion challenge for answering
// this card with exactly this option and text. It reads without locks: the
// answer re-checks everything under its own locks.
func (handler *Handler) ChallengeDecisionAnswer(writer http.ResponseWriter, request *http.Request) {
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
	if err := decodeCardAnswer(writer, request, &input); err != nil {
		writeError(writer, http.StatusBadRequest, "invalid request body")
		return
	}
	issued, binding, err := handler.issueAnswerChallenge(request, workspaceUUID, requestID, userID, input)
	if err != nil {
		writeDecisionError(writer, err, "answer challenge")
		return
	}
	writeJSON(writer, http.StatusOK, answerChallengeResponse{
		challengeResponse: challengeResponse{ChallengeID: uuidToString(issued.ChallengeID), Options: issued.Options, ExpiresAt: issued.ExpiresAt},
		AnswerRevision:    binding.AnswerRevision, PolicyRevision: binding.PolicyRevision,
	})
}

func (handler *Handler) issueAnswerChallenge(request *http.Request, workspaceID, requestID, userID pgtype.UUID, input decisionCardAnswerInput) (passkey.Issued, passkey.AnswerBinding, error) {
	ctx := request.Context()
	sessionID := request.Header.Get("X-Session-ID")
	if sessionID == "" {
		return passkey.Issued{}, passkey.AnswerBinding{}, errSessionUnbound
	}
	decision, err := handler.Queries.GetDecisionRequest(ctx, db.GetDecisionRequestParams{WorkspaceID: workspaceID, ID: requestID})
	if errors.Is(err, pgx.ErrNoRows) {
		return passkey.Issued{}, passkey.AnswerBinding{}, decisionIntentRefusal{http.StatusNotFound, "decision request not found"}
	}
	if err != nil {
		return passkey.Issued{}, passkey.AnswerBinding{}, err
	}
	if decision.HumanUserID != userID {
		return passkey.Issued{}, passkey.AnswerBinding{}, decisionIntentRefusal{http.StatusForbidden, "only the request's human can answer"}
	}
	if decision.Origin != "asked" || decision.Status != "open" {
		return passkey.Issued{}, passkey.AnswerBinding{}, decisionIntentRefusal{http.StatusConflict, "this request cannot accept another answer"}
	}
	if _, err := cardAnswerContent(decision, input); err != nil {
		return passkey.Issued{}, passkey.AnswerBinding{}, err
	}
	workspace, err := handler.Queries.GetWorkspace(ctx, workspaceID)
	if err != nil {
		return passkey.Issued{}, passkey.AnswerBinding{}, err
	}
	if !answerNeedsAssertion(workspace, decision) {
		return passkey.Issued{}, passkey.AnswerBinding{}, errNotGated
	}
	if handler.Passkeys == nil {
		return passkey.Issued{}, passkey.AnswerBinding{}, errApprovalUnavailable
	}
	binding := answerBinding(workspace, decision, userID, sessionID, input)
	issued, err := handler.Passkeys.IssueAnswer(ctx, binding)
	switch {
	case errors.Is(err, passkey.ErrNoPasskey):
		err = errNoPasskey
	case errors.Is(err, passkey.ErrBusy):
		err = errBusyRetry
	}
	return issued, binding, err
}
