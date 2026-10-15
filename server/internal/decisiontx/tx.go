package decisiontx

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func Begin(ctx context.Context, pool *pgxpool.Pool, workspaceID pgtype.UUID) (pgx.Tx, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := db.New(tx).LockWorkspaceForChatSessionCreate(ctx, workspaceID); err != nil {
		_ = tx.Rollback(ctx)
		return nil, fmt.Errorf("decision workspace fence: %w", err)
	}
	return tx, nil
}

func IsRetryable(err error) bool {
	var pgError *pgconn.PgError
	return errors.As(err, &pgError) && (pgError.Code == "55P03" || pgError.Code == "40P01")
}
