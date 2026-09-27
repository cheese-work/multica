package db

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

type transactionBeginner interface {
	Begin(context.Context) (pgx.Tx, error)
}

// CreateOrResolveGovernanceCase returns the prior case for the same material
// fingerprint, or atomically allocates the next generation for this identity.
// The identity lock must precede reads in separate statements so a waiter gets
// a post-lock Read Committed snapshot rather than a stale CTE snapshot.
func (q *Queries) CreateOrResolveGovernanceCase(ctx context.Context, arg InsertNextGovernanceCaseParams) (GovernanceCase, error) {
	beginner, ok := q.db.(transactionBeginner)
	if !ok {
		return GovernanceCase{}, fmt.Errorf("governance case create-or-resolve requires a transaction-capable database")
	}
	tx, err := beginner.Begin(ctx)
	if err != nil {
		return GovernanceCase{}, fmt.Errorf("begin governance case create-or-resolve: %w", err)
	}
	defer tx.Rollback(ctx)

	txq := q.WithTx(tx)
	identity := LockGovernanceCaseIdentityParams{
		WorkspaceID:     arg.WorkspaceID,
		SubjectType:     pgtype.Text{String: arg.SubjectType, Valid: true},
		SubjectID:       arg.SubjectID,
		SubjectRevision: arg.SubjectRevision,
		RuleID:          arg.RuleID,
	}
	if err := txq.LockGovernanceCaseIdentity(ctx, identity); err != nil {
		return GovernanceCase{}, fmt.Errorf("lock governance case identity: %w", err)
	}

	existing, err := txq.FindGovernanceCaseByMaterialFingerprint(ctx, FindGovernanceCaseByMaterialFingerprintParams{
		WorkspaceID:         arg.WorkspaceID,
		SubjectType:         arg.SubjectType,
		SubjectID:           arg.SubjectID,
		SubjectRevision:     arg.SubjectRevision,
		RuleID:              arg.RuleID,
		MaterialFingerprint: arg.MaterialFingerprint,
	})
	if err == nil {
		if err := tx.Commit(ctx); err != nil {
			return GovernanceCase{}, fmt.Errorf("commit resolved governance case: %w", err)
		}
		return existing, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return GovernanceCase{}, fmt.Errorf("find governance case by material fingerprint: %w", err)
	}

	created, err := txq.InsertNextGovernanceCase(ctx, arg)
	if err != nil {
		return GovernanceCase{}, fmt.Errorf("insert next governance case: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return GovernanceCase{}, fmt.Errorf("commit created governance case: %w", err)
	}
	return created, nil
}
