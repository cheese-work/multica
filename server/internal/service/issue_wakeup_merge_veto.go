package service

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

var errMergeVetoed = errors.New("a person vetoed this PR in Multica; clear the veto to release it")

// SetMergeVeto records a veto made in Multica only. The caller must already
// have checked that the credential is a person who may veto. It drops the PR's
// stored receipt in the same transaction.
func SetMergeVeto(ctx context.Context, txs TxStarter, workspaceID pgtype.UUID, owner, repo string, number int32, by pgtype.UUID, reason string, now time.Time) error {
	tx, err := txs.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `INSERT INTO merge_veto(workspace_id,repo_owner,repo_name,pr_number,vetoed_by,reason,created_at)
		VALUES($1,$2,$3,$4,$5,$6,$7)
		ON CONFLICT (workspace_id,repo_owner,repo_name,pr_number) DO UPDATE SET vetoed_by=EXCLUDED.vetoed_by, reason=EXCLUDED.reason, created_at=EXCLUDED.created_at`,
		workspaceID, owner, repo, number, by, reason, now); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM merge_receipt_fact WHERE workspace_id=$1 AND repo_owner=$2 AND repo_name=$3 AND pr_number=$4`,
		workspaceID, owner, repo, number); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ClearMergeVeto lifts a veto. The receipt it dropped stays gone: a producer
// must write a new one, so nothing recorded before the veto comes back.
func ClearMergeVeto(ctx context.Context, txs TxStarter, workspaceID pgtype.UUID, owner, repo string, number int32) error {
	tx, err := txs.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `DELETE FROM merge_veto WHERE workspace_id=$1 AND repo_owner=$2 AND repo_name=$3 AND pr_number=$4`,
		workspaceID, owner, repo, number); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
