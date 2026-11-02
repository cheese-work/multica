package handler

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// humanInChargeDeps are the lookups the resolver needs, injected so the order
// and fail-closed rules test without a database.
type humanInChargeDeps struct {
	isMember       func(ctx context.Context, userID pgtype.UUID) (bool, error)
	canInvoke      func(ctx context.Context, userID pgtype.UUID) (bool, error)
	workspaceOwner func(ctx context.Context) (pgtype.UUID, error)
}

// resolveHumanInCharge picks the member who answers for an issue (CHE-2591
// design §4.1): creator, requested_by, project lead, workspace owner, each only
// if an active member who can invoke the asking agent. It returns an invalid
// UUID for "none" and an error, never a fallback, when any lookup fails. The
// result is never an agent or a squad.
func resolveHumanInCharge(ctx context.Context, issue db.Issue, project db.Project, deps humanInChargeDeps) (pgtype.UUID, error) {
	var candidates []pgtype.UUID
	if issue.CreatorType == "member" {
		candidates = append(candidates, issue.CreatorID)
	}
	candidates = append(candidates, issue.RequestedByID)
	if project.LeadType.String == "member" {
		candidates = append(candidates, project.LeadID)
	}
	for _, candidate := range candidates {
		if !candidate.Valid {
			continue
		}
		eligible, err := humanInChargeEligible(ctx, candidate, deps)
		if err != nil {
			return pgtype.UUID{}, err
		}
		if eligible {
			return candidate, nil
		}
	}
	owner, err := deps.workspaceOwner(ctx)
	if errors.Is(err, pgx.ErrNoRows) {
		return pgtype.UUID{}, nil
	}
	if err != nil {
		return pgtype.UUID{}, fmt.Errorf("look up workspace owner: %w", err)
	}
	eligible, err := humanInChargeEligible(ctx, owner, deps)
	if err != nil || !eligible {
		return pgtype.UUID{}, err
	}
	return owner, nil
}

func humanInChargeEligible(ctx context.Context, userID pgtype.UUID, deps humanInChargeDeps) (bool, error) {
	member, err := deps.isMember(ctx, userID)
	if err != nil {
		return false, fmt.Errorf("look up membership: %w", err)
	}
	if !member {
		return false, nil
	}
	invokable, err := deps.canInvoke(ctx, userID)
	if err != nil {
		return false, fmt.Errorf("check invoke access: %w", err)
	}
	return invokable, nil
}

// humanInChargeFor resolves the human in charge of issue for askingAgent using
// the same membership and invoke checks a decision target passes.
func (h *Handler) humanInChargeFor(ctx context.Context, issue db.Issue, project db.Project, askingAgent db.Agent) (pgtype.UUID, error) {
	return resolveHumanInCharge(ctx, issue, project, humanInChargeDeps{
		isMember: func(ctx context.Context, userID pgtype.UUID) (bool, error) {
			return h.isWorkspaceMember(ctx, uuidToString(userID), uuidToString(issue.WorkspaceID))
		},
		canInvoke: func(ctx context.Context, userID pgtype.UUID) (bool, error) {
			return h.invokeAgentDecision(ctx, askingAgent, "member", uuidToString(userID), "", uuidToString(issue.WorkspaceID))
		},
		workspaceOwner: func(ctx context.Context) (pgtype.UUID, error) {
			return h.Queries.GetWorkspaceOwnerUserID(ctx, issue.WorkspaceID)
		},
	})
}
