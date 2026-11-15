package passkey

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const PurposeDecisionAnswer Purpose = "decision_answer"

// AnswerBinding is everything a gated card answer signs. Text is the trimmed
// text as stored; Option is the exact label or "".
type AnswerBinding struct {
	WorkspaceID     pgtype.UUID
	DecisionID      pgtype.UUID
	AnswerRevision  int32
	PolicyRevision  int32
	RecipientUserID pgtype.UUID
	SessionID       string
	Option          string
	Text            string
}

// Digest is sha256 (lowercase hex) of the canonical JSON of the binding: keys
// sorted bytewise (declaration order below), no whitespace, no HTML escaping.
func (binding AnswerBinding) Digest() string {
	var canonical bytes.Buffer
	encoder := json.NewEncoder(&canonical)
	encoder.SetEscapeHTML(false)
	err := encoder.Encode(struct {
		AnswerRevision  int32  `json:"answer_revision"`
		DecisionID      string `json:"decision_id"`
		Option          string `json:"option"`
		PolicyRevision  int32  `json:"policy_revision"`
		RecipientUserID string `json:"recipient_user_id"`
		SessionID       string `json:"sid"`
		Text            string `json:"text"`
		V               int    `json:"v"`
		WorkspaceID     string `json:"workspace_id"`
	}{
		binding.AnswerRevision, uuid.UUID(binding.DecisionID.Bytes).String(), binding.Option, binding.PolicyRevision,
		uuid.UUID(binding.RecipientUserID.Bytes).String(), binding.SessionID, binding.Text, 1, uuid.UUID(binding.WorkspaceID.Bytes).String(),
	})
	if err != nil {
		// A fixed struct of strings and ints into a bytes.Buffer cannot fail.
		panic(fmt.Sprintf("passkey: encode answer binding: %v", err))
	}
	sum := sha256.Sum256(bytes.TrimSuffix(canonical.Bytes(), []byte("\n")))
	return hex.EncodeToString(sum[:])
}

// answerChallenge is sha256("decision_answer" || decision id || digest ||
// nonce): the signature names the one decision and the exact content.
func answerChallenge(decisionID pgtype.UUID, digest string, nonce []byte) []byte {
	hash := sha256.New()
	hash.Write([]byte(PurposeDecisionAnswer))
	hash.Write(decisionID.Bytes[:])
	hash.Write([]byte(digest))
	hash.Write(nonce)
	return hash.Sum(nil)
}

// IssueAnswer opens the recipient's assertion challenge for answering this
// decision with exactly this content, superseding any challenge they had open.
func (service *Service) IssueAnswer(ctx context.Context, binding AnswerBinding) (Issued, error) {
	rows, err := service.Queries.ListUsablePasskeys(ctx, binding.RecipientUserID)
	if err != nil {
		return Issued{}, err
	}
	digest := binding.Digest()
	return service.issueAssertion(ctx, binding.RecipientUserID, PurposeDecisionAnswer, binding.DecisionID, rows, func(nonce []byte) []byte {
		return answerChallenge(binding.DecisionID, digest, nonce)
	})
}

// VerifyAnswer proves the recipient signed this exact answer. Call it inside
// the answer transaction after the request is locked and both revisions
// matched; it locks the challenge and credential (NOWAIT) and consumes the
// challenge in that transaction, so a rollback leaves it usable until its TTL.
func (service *Service) VerifyAnswer(ctx context.Context, queries *db.Queries, binding AnswerBinding, assertion json.RawMessage) (pgtype.UUID, error) {
	challengeID, credential, err := parseEnvelope(assertion)
	if err != nil {
		return pgtype.UUID{}, err
	}
	digest := binding.Digest()
	return service.verifyAssertion(ctx, queries, verification{
		UserID: binding.RecipientUserID, Purpose: PurposeDecisionAnswer, SubjectID: binding.DecisionID, ChallengeID: challengeID, Credential: credential,
		Expect: func(nonce []byte) []byte { return answerChallenge(binding.DecisionID, digest, nonce) },
	})
}
