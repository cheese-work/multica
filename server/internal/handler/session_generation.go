package handler

import (
	"errors"
	"net/http"
	"strconv"

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
	claimed, err := strconv.Atoi(generation)
	if err != nil {
		return err
	}
	ctx := r.Context()
	tx, err := h.TxStarter.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout = '5s'`); err != nil {
		return err
	}
	queries := h.Queries.WithTx(tx)
	stored, err := queries.LockUserSessionGeneration(ctx, parseUUID(userID))
	if err != nil {
		return err
	}
	if int(stored) != claimed {
		return errSessionRevoked
	}
	if err := write(queries); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
