package browserlease

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"
)

// Both denials are ErrGateDenied, so the handler answers 403 and the reason reaches the caller.
var (
	ErrUserNotPermitted = fmt.Errorf("%w: the accountable user has no browser permission for the profile", ErrGateDenied)
	ErrAgentNotAllowed  = fmt.Errorf("%w: the agent is not on the profile allowlist", ErrGateDenied)
)

// Permissions reads the user gate lists. W3-8 (CHE-4737) supplies the table-backed store.
type Permissions interface {
	UserPermitted(ctx context.Context, ws, user, profile pgtype.UUID) (bool, error)
	AgentAllowed(ctx context.Context, ws, profile, agent pgtype.UUID) (bool, error)
}

// UserGate enforces the user gate (plan F3): the task's accountable user holds browser
// permission for the profile and the agent is on its allowlist. Default deny: a missing store,
// a task without a user or an unlisted agent is denied; a store error is returned as is, so the
// mint or renew fails closed without ending a lease on a transient fault.
type UserGate struct{ Permissions Permissions }

func (g UserGate) Check(ctx context.Context, s Subject) error {
	if g.Permissions == nil {
		return ErrGateDenied
	}
	if !s.UserID.Valid {
		return ErrUserNotPermitted
	}
	ok, err := g.Permissions.UserPermitted(ctx, s.WorkspaceID, s.UserID, s.Profile.ID)
	if err != nil {
		return err
	}
	if !ok {
		return ErrUserNotPermitted
	}
	ok, err = g.Permissions.AgentAllowed(ctx, s.WorkspaceID, s.Profile.ID, s.AgentID)
	if err != nil {
		return err
	}
	if !ok {
		return ErrAgentNotAllowed
	}
	return nil
}
