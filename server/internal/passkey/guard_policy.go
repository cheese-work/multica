package passkey

import (
	"context"
	"crypto/sha256"
	"encoding/json"

	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// PurposeGuardPolicy signs one edit of the workspace guard policy.
const PurposeGuardPolicy Purpose = "guard_policy_change"

// guardPolicyChallenge is sha256("guard_policy_change" || workspace id || digest
// of the policy before || digest of the policy after || nonce): the signature
// names the workspace, the version it edits and the exact policy it writes.
func guardPolicyChallenge(workspaceID pgtype.UUID, baseSHA256, afterSHA256 string, nonce []byte) []byte {
	hash := sha256.New()
	hash.Write([]byte(PurposeGuardPolicy))
	hash.Write(workspaceID.Bytes[:])
	hash.Write([]byte(baseSHA256))
	hash.Write([]byte(afterSHA256))
	hash.Write(nonce)
	return hash.Sum(nil)
}

// IssueGuardPolicyChange opens the person's assertion challenge for writing
// the policy whose digest is afterSHA256 over the one whose digest is baseSHA256.
func (service *Service) IssueGuardPolicyChange(ctx context.Context, userID, workspaceID pgtype.UUID, baseSHA256, afterSHA256 string) (Issued, error) {
	usable, err := service.Queries.ListUsablePasskeys(ctx, userID)
	if err != nil {
		return Issued{}, err
	}
	return service.issueAssertion(ctx, userID, PurposeGuardPolicy, workspaceID, usable, func(nonce []byte) []byte {
		return guardPolicyChallenge(workspaceID, baseSHA256, afterSHA256, nonce)
	})
}

// SetGuardPolicy runs write once the person signed this exact edit. check runs
// first, in the same transaction, and a refusal from it (a stale base) leaves
// the challenge unused. A sign-count regression marks the signing credential
// suspect, outside the aborted transaction.
func (service *Service) SetGuardPolicy(
	ctx context.Context, userID, workspaceID pgtype.UUID, baseSHA256, afterSHA256 string, assertion json.RawMessage,
	check func(ctx context.Context, queries *db.Queries) error,
	write func(ctx context.Context, queries *db.Queries, signer pgtype.UUID) error,
) error {
	tx, err := service.Tx.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	queries := db.New(tx)
	if err := check(ctx, queries); err != nil {
		return err
	}
	challengeID, credential, err := parseEnvelope(assertion)
	if err != nil {
		return err
	}
	signer, err := service.verifyAssertion(ctx, queries, verification{
		UserID: userID, Purpose: PurposeGuardPolicy, SubjectID: workspaceID, ChallengeID: challengeID, Credential: credential,
		Expect: func(nonce []byte) []byte { return guardPolicyChallenge(workspaceID, baseSHA256, afterSHA256, nonce) },
	})
	if err != nil {
		return service.abortAssertion(ctx, tx, err)
	}
	if err := write(ctx, queries, signer); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
