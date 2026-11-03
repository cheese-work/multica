package handler

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/auth"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

var errSessionRevoked = errors.New("session revoked")

// withCurrentSession runs write in one transaction that holds a share lock on
// the user row. A JWT caller's generation is compared with the stored one
// first. Passkey recovery holds that row FOR UPDATE, so a request that outlived
// the middleware check cannot mint a credential across the recovery commit.
// A caller that is not a JWT (no generation header) runs write unchecked.
func (h *Handler) withCurrentSession(r *http.Request, userID string, write func(*db.Queries) error) error {
	generation := r.Header.Get(auth.SessionGenerationHeader)
	if generation == "" {
		return write(h.Queries)
	}
	ctx := r.Context()
	tx, err := h.TxStarter.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	queries := h.Queries.WithTx(tx)
	if err := holdCurrentSession(ctx, tx, queries, parseUUID(userID), generation); err != nil {
		return err
	}
	if err := write(queries); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// holdCurrentSession takes the share lock on the user row inside tx and
// compares a JWT's generation (the header value) with the stored one. An empty
// generation is not a JWT and passes. The lock lasts to the end of tx, so
// recovery cannot commit between this check and the caller's writes.
func holdCurrentSession(ctx context.Context, tx pgx.Tx, queries *db.Queries, userID pgtype.UUID, generation string) error {
	if generation == "" {
		return nil
	}
	claimed, err := strconv.Atoi(generation)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout = '5s'`); err != nil {
		return err
	}
	stored, err := queries.LockUserSessionGeneration(ctx, userID)
	if err != nil {
		return err
	}
	if int(stored) != claimed {
		return errSessionRevoked
	}
	return nil
}
