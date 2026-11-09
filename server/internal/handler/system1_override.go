package handler

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

type memberRouteOverrideKey struct{}

// withMemberRouteOverride arms a member's issue write to mark the issue's latest
// route label overridden (CHE-4439 S2).
func withMemberRouteOverride(ctx context.Context) context.Context {
	return context.WithValue(ctx, memberRouteOverrideKey{}, true)
}

func memberRouteOverrideArmed(ctx context.Context) bool {
	armed, _ := ctx.Value(memberRouteOverrideKey{}).(bool)
	return armed
}

// markMemberRouteOverride runs in the write transaction, under the issue row
// lock and before the write's own route label. Routes that commit later wait on
// that lock, so the mark can only land on the route that was current when the
// person decided. Strict: an error fails the write.
func markMemberRouteOverride(ctx context.Context, q *db.Queries, workspaceID pgtype.UUID, params db.UpdateIssueParams) error {
	if !memberRouteOverrideArmed(ctx) {
		return nil
	}
	prior, err := q.LockIssueForDescriptionUpdate(ctx, db.LockIssueForDescriptionUpdateParams{ID: params.ID, WorkspaceID: workspaceID})
	if err != nil {
		return fmt.Errorf("lock issue for route override: %w", err)
	}
	statusChanges := params.Status.Valid && params.Status.String != prior.Status
	ownerChanges := (params.AssigneeType.Valid || params.AssigneeID.Valid) &&
		(params.AssigneeType.String != prior.AssigneeType.String || params.AssigneeID != prior.AssigneeID)
	if !statusChanges && !ownerChanges {
		return nil
	}
	if err := q.MarkSystem1RouteOverridden(ctx, params.ID); err != nil {
		return fmt.Errorf("mark route overridden: %w", err)
	}
	return nil
}
