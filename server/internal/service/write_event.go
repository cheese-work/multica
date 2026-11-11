package service

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// ErrWriteEventLost means the request no longer holds the claim on its event
// id: another request took it over, so this one must not write.
var ErrWriteEventLost = errors.New("write event claim lost")

type writeEventScope struct {
	workspaceID pgtype.UUID
	key         string
	owner       pgtype.UUID
}

type writeEventScopeKey struct{}

// WithWriteEventClaim marks ctx as running under the claim (workspace, key,
// owner) of an idempotent write. Every transaction the write begins calls
// ApplyWriteEvent.
func WithWriteEventClaim(ctx context.Context, workspaceID pgtype.UUID, key string, owner pgtype.UUID) context.Context {
	return context.WithValue(ctx, writeEventScopeKey{}, writeEventScope{workspaceID: workspaceID, key: key, owner: owner})
}

// HasWriteEventClaim reports whether ctx runs under an idempotent write's claim.
func HasWriteEventClaim(ctx context.Context) bool {
	_, ok := ctx.Value(writeEventScopeKey{}).(writeEventScope)
	return ok
}

// ApplyWriteEvent marks the claim applied inside the write's own transaction,
// so the marker commits with the write or not at all, and fails the write when
// the claim has been taken over. It does nothing outside an idempotent write.
func ApplyWriteEvent(ctx context.Context, q *db.Queries) error {
	scope, ok := ctx.Value(writeEventScopeKey{}).(writeEventScope)
	if !ok {
		return nil
	}
	n, err := q.ApplyWriteEvent(ctx, db.ApplyWriteEventParams{WorkspaceID: scope.workspaceID, EventKey: scope.key, Owner: scope.owner})
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrWriteEventLost
	}
	return nil
}
