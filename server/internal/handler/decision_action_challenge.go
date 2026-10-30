package handler

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/passkey"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

var (
	errNoPasskey = decisionIntentRefusal{http.StatusConflict, "no_passkey: enroll a passkey before approving"}
)

// usePasskeys makes service the approval verifier. A nil service leaves
// approvals failing closed.
func (handler *Handler) usePasskeys(service *passkey.Service) {
	handler.Passkeys = service
	if service == nil {
		handler.ApprovalVerifier = nil
		return
	}
	handler.ApprovalVerifier = func(ctx context.Context, queries *db.Queries, claim ApprovalClaim) (pgtype.UUID, error) {
		return service.VerifyApproval(ctx, queries, claim.UserID, claim.ActionID, claim.PayloadSHA256, claim.Assertion)
	}
}

// usePasskeysFromEnv turns passkey approval on when the relying party is
// configured. A bad configuration is logged and leaves approval off.
func (handler *Handler) usePasskeysFromEnv(queries *db.Queries, transactions txStarter) {
	relyingParty, err := passkey.FromEnv(os.Getenv)
	if err != nil {
		slog.Error("passkey relying party configuration is invalid; approvals stay off", "error", err)
		return
	}
	if relyingParty != nil {
		handler.usePasskeys(&passkey.Service{
			WebAuthn: relyingParty, Queries: queries, Tx: transactions,
			Mail: handler.EmailService, CodeKey: []byte(os.Getenv("MULTICA_PASSKEY_ENROLL_KEY")),
		})
	}
}

// markSuspectAfterRefusal records a cloned-authenticator signal. The
// verifier ran inside the approval transaction, which its refusal rolled
// back, so the mark is a separate write.
func (handler *Handler) markSuspectAfterRefusal(ctx context.Context, err error) {
	var refusal passkey.Refusal
	if handler.Passkeys == nil || !errors.As(err, &refusal) || !refusal.Suspect.Valid {
		return
	}
	markCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), decisionActionAcquireTimeout)
	defer cancel()
	_ = handler.Passkeys.MarkSuspect(markCtx, refusal.Suspect)
}

type challengeResponse struct {
	ChallengeID string      `json:"challenge_id"`
	Options     interface{} `json:"options"`
	ExpiresAt   time.Time   `json:"expires_at"`
}

// ChallengeDecisionAction opens the human's assertion challenge for approving
// this action's exact payload. It reads without locks: the approval re-checks
// every condition under its own locks.
func (handler *Handler) ChallengeDecisionAction(writer http.ResponseWriter, request *http.Request) {
	caller, ok := handler.authorizeDecisionActionCaller(writer, request)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), decisionActionDeadline)
	defer cancel()
	issued, err := handler.issueApprovalChallenge(ctx, caller)
	if err != nil {
		writeDecisionActionError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, challengeResponse{
		ChallengeID: uuidToString(issued.ChallengeID), Options: issued.Options, ExpiresAt: issued.ExpiresAt,
	})
}

func (handler *Handler) issueApprovalChallenge(ctx context.Context, caller decisionActionCaller) (passkey.Issued, error) {
	preview, located, err := handler.locateDecisionAction(ctx, caller)
	if err != nil {
		return passkey.Issued{}, err
	}
	if handler.Passkeys == nil {
		return passkey.Issued{}, errApprovalUnavailable
	}
	if located.Status != "open" || preview.State != "pending" {
		return passkey.Issued{}, errNotPending
	}
	if expired, err := handler.Queries.DecisionActionExpired(ctx, db.DecisionActionExpiredParams{ID: preview.ID, WorkspaceID: caller.workspaceID}); err != nil {
		return passkey.Issued{}, err
	} else if expired {
		return passkey.Issued{}, errActionExpired
	}
	issued, err := handler.Passkeys.IssueApproval(ctx, caller.userID, preview.ID, preview.PayloadSha256)
	switch {
	case errors.Is(err, passkey.ErrNoPasskey):
		return passkey.Issued{}, errNoPasskey
	case errors.Is(err, passkey.ErrBusy):
		return passkey.Issued{}, errBusyRetry
	}
	return issued, err
}
