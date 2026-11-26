package service

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// CommentProtection reports whether agent runs are barred from editing another
// agent's comment. The server wires the real check; a nil one reads as false.
type CommentProtection func() bool

// VerifiedAgent is an agent identity with its lab from LabOf ("" when unknown).
type VerifiedAgent struct {
	AgentID   string `json:"agent_id"`
	AgentName string `json:"agent_name"`
	Lab       string `json:"lab"`
}

// VerifiedSigner is an agent whose trusted PASS stands on the exact head.
type VerifiedSigner struct {
	VerifiedAgent
	CommentID string    `json:"comment_id"`
	VerdictAt time.Time `json:"verdict_at"`
}

// VerifiedSignersResult holds facts only. Any merge rule lives in the consumer.
type VerifiedSignersResult struct {
	CommentProtection bool             `json:"comment_protection"`
	PRAuthor          *VerifiedAgent   `json:"pr_author"`
	Signers           []VerifiedSigner `json:"signers"`
}

// VerifiedSigners reads the distinct agents whose trusted PASS stands on c's
// exact head, the PR author, and whether comment protection is on.
func VerifiedSigners(ctx context.Context, tx pgx.Tx, q *db.Queries, c MergeCandidate, protection CommentProtection) (VerifiedSignersResult, error) {
	return VerifiedSignersResult{}, nil
}
