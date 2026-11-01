package passkey

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const (
	PurposeGateChange Purpose = "gate_change"
	PurposeEnroll     Purpose = "passkey_enroll"
)

const (
	GateOff   = "off"
	GateBlock = "block"

	// minGatedPasskeys is the floor a gated user keeps: one lost device must
	// not lock them out of approving.
	minGatedPasskeys = 2

	eventGateEnabled  = "decision_gate_enabled"
	eventGateDisabled = "decision_gate_disabled"
)

var errTwoPasskeysRequired = conflict("two_passkeys_required: enroll a second passkey before turning the gate on")

// gateChangeChallenge is sha256("gate_change" || workspace id || target state
// || nonce): the signature names the workspace and the state it moves to.
func gateChangeChallenge(workspaceID pgtype.UUID, target string, nonce []byte) []byte {
	hash := sha256.New()
	hash.Write([]byte(PurposeGateChange))
	hash.Write(workspaceID.Bytes[:])
	hash.Write([]byte(target))
	hash.Write(nonce)
	return hash.Sum(nil)
}

// enrollChallenge is sha256("passkey_enroll" || ceremony id || nonce): the
// signature names the one enrollment ceremony it lets through.
func enrollChallenge(ceremonyID pgtype.UUID, nonce []byte) []byte {
	hash := sha256.New()
	hash.Write([]byte(PurposeEnroll))
	hash.Write(ceremonyID.Bytes[:])
	hash.Write(nonce)
	return hash.Sum(nil)
}

// gateTransition refuses a state that is unknown or already in force, and an
// enable without two usable passkeys. Removing passkeys never flips the gate;
// only this path does.
func gateTransition(current, target string, usable []db.UserPasskey) error {
	switch {
	case target != GateOff && target != GateBlock:
		return Refusal{Status: 400, Message: "invalid_state: state must be off or block"}
	case current == target:
		return conflict("gate_unchanged: the gate is already " + target)
	case target == GateBlock && len(usable) < minGatedPasskeys:
		return errTwoPasskeysRequired
	}
	return nil
}

// IssueGateChange opens the owner's assertion challenge for moving the
// workspace's decision gate to target, signed by any of their usable passkeys.
func (service *Service) IssueGateChange(ctx context.Context, userID, workspaceID pgtype.UUID, target string) (Issued, error) {
	workspace, err := service.Queries.GetWorkspace(ctx, workspaceID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Issued{}, forbidden("workspace_not_found: no such workspace")
	}
	if err != nil {
		return Issued{}, err
	}
	usable, err := service.Queries.ListUsablePasskeys(ctx, userID)
	if err != nil {
		return Issued{}, err
	}
	if err := gateTransition(workspace.DecisionPasskeyGate, target, usable); err != nil {
		return Issued{}, err
	}
	return service.issueAssertion(ctx, userID, PurposeGateChange, workspaceID, usable, func(nonce []byte) []byte {
		return gateChangeChallenge(workspaceID, target, nonce)
	})
}

// SetDecisionGate moves the workspace's decision gate to target once the owner
// signed that exact change, and bumps policy_revision. A sign-count regression
// marks the signing credential suspect, outside the aborted transaction.
func (service *Service) SetDecisionGate(ctx context.Context, userID, workspaceID pgtype.UUID, target string, assertion json.RawMessage) error {
	tx, err := service.Tx.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	queries := db.New(tx)
	current, err := queries.LockWorkspaceDecisionGate(ctx, workspaceID)
	if errors.Is(err, pgx.ErrNoRows) {
		return forbidden("workspace_not_found: no such workspace")
	}
	if err != nil {
		return busyOr(err)
	}
	usable, err := queries.ListUsablePasskeys(ctx, userID)
	if err != nil {
		return err
	}
	if err := gateTransition(current.DecisionPasskeyGate, target, usable); err != nil {
		return err
	}
	challengeID, credential, err := parseEnvelope(assertion)
	if err != nil {
		return err
	}
	signer, err := service.verifyAssertion(ctx, queries, verification{
		UserID: userID, Purpose: PurposeGateChange, SubjectID: workspaceID, ChallengeID: challengeID, Credential: credential,
		Expect: func(nonce []byte) []byte { return gateChangeChallenge(workspaceID, target, nonce) },
	})
	if err != nil {
		return service.abortAssertion(ctx, tx, err)
	}
	changed, err := queries.SetWorkspaceDecisionGate(ctx, db.SetWorkspaceDecisionGateParams{ID: workspaceID, DecisionPasskeyGate: target})
	if err != nil {
		return err
	}
	action := eventGateDisabled
	if target == GateBlock {
		action = eventGateEnabled
	}
	if err := writeEvent(ctx, queries, userID, action, map[string]any{
		"workspace_id": uuid.UUID(workspaceID.Bytes).String(), "policy_revision": changed.PolicyRevision,
		"signed_by": uuid.UUID(signer.Bytes).String(),
	}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// abortAssertion rolls the transaction back after a refused assertion and, for
// a sign-count regression, flags the credential in its own statement.
func (service *Service) abortAssertion(ctx context.Context, tx pgx.Tx, err error) error {
	tx.Rollback(ctx)
	var refusal Refusal
	if errors.As(err, &refusal) && refusal.Suspect.Valid {
		if markErr := service.MarkSuspect(ctx, refusal.Suspect); markErr != nil {
			return markErr
		}
	}
	return busyOr(err)
}

// gatedWithPasskeys reports whether the user sits under a blocking gate and
// already holds a usable passkey, and returns those passkeys. The first
// passkey of a gated user needs no assertion: there is nothing to sign with.
func gatedWithPasskeys(ctx context.Context, queries *db.Queries, userID pgtype.UUID) (bool, []db.UserPasskey, error) {
	gated, err := queries.UserUnderDecisionGate(ctx, userID)
	if err != nil || !gated {
		return false, nil, err
	}
	usable, err := queries.ListUsablePasskeys(ctx, userID)
	return len(usable) > 0, usable, err
}

// requireEnrollAssertion demands, for a gated user who already has a passkey,
// that an existing passkey signed this enrollment ceremony.
func (service *Service) requireEnrollAssertion(ctx context.Context, tx pgx.Tx, queries *db.Queries, userID, ceremonyID pgtype.UUID, assertion json.RawMessage) error {
	required, _, err := gatedWithPasskeys(ctx, queries, userID)
	if err != nil || !required {
		return err
	}
	challengeID, credential, err := parseEnvelope(assertion)
	if err != nil {
		return err
	}
	if _, err := service.verifyAssertion(ctx, queries, verification{
		UserID: userID, Purpose: PurposeEnroll, SubjectID: ceremonyID, ChallengeID: challengeID, Credential: credential,
		Expect: func(nonce []byte) []byte { return enrollChallenge(ceremonyID, nonce) },
	}); err != nil {
		return service.abortAssertion(ctx, tx, err)
	}
	return nil
}

// issueEnrollAssertion opens the assertion a gated user must sign to finish the
// ceremony, or returns nil when none is needed.
func (service *Service) issueEnrollAssertion(ctx context.Context, userID, ceremonyID pgtype.UUID) (*Issued, error) {
	required, usable, err := gatedWithPasskeys(ctx, service.Queries, userID)
	if err != nil || !required {
		return nil, err
	}
	issued, err := service.issueAssertion(ctx, userID, PurposeEnroll, ceremonyID, usable, func(nonce []byte) []byte {
		return enrollChallenge(ceremonyID, nonce)
	})
	if err != nil {
		return nil, err
	}
	return &issued, nil
}

// enforceFloor refuses removing a usable passkey from a gated user when fewer
// than two would stay.
func enforceFloor(ctx context.Context, queries *db.Queries, userID, targetID pgtype.UUID) error {
	gated, usable, err := gatedWithPasskeys(ctx, queries, userID)
	if err != nil || !gated {
		return err
	}
	remaining, targetUsable := 0, false
	for _, key := range usable {
		if key.ID == targetID {
			targetUsable = true
		} else {
			remaining++
		}
	}
	if targetUsable && remaining < minGatedPasskeys {
		return conflict("passkey_floor: the decision gate needs at least two active passkeys")
	}
	return nil
}
