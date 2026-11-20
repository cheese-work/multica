package browserlease

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
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
// permission for the profile and, for an agent lease, the agent is on its allowlist (a class H
// member login has no agent). Default deny: a missing store,
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
	// A member's class H login lease has no agent: the profile member grant alone decides it.
	if s.Profile.Class == ClassH {
		return nil
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

// StoreGate is the table-backed gate wired into the server: UserGate over the W3-8 tables.
func StoreGate(q *db.Queries) Gate { return UserGate{Permissions: tablePermissions{q}} }

// tablePermissions reads browser_profile_member and browser_profile_agent (CHE-4737). A grant is a
// row, scoped by workspace and profile; no row means no access.
type tablePermissions struct{ q *db.Queries }

func (t tablePermissions) UserPermitted(ctx context.Context, ws, user, profile pgtype.UUID) (bool, error) {
	return t.q.BrowserProfileHasMember(ctx, db.BrowserProfileHasMemberParams{WorkspaceID: ws, ProfileID: profile, UserID: user})
}

func (t tablePermissions) AgentAllowed(ctx context.Context, ws, profile, agent pgtype.UUID) (bool, error) {
	return t.q.BrowserProfileHasAgent(ctx, db.BrowserProfileHasAgentParams{WorkspaceID: ws, ProfileID: profile, AgentID: agent})
}
