package handler

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

// githubPolicyRun caches the allowed bindings of each installation for one
// sweep and logs a fully denied installation once. A lookup error fails closed.
type githubPolicyRun struct {
	h       *Handler
	allowed map[int64]map[pgtype.UUID]bool
}

func (h *Handler) newGitHubPolicyRun() *githubPolicyRun {
	return &githubPolicyRun{h: h, allowed: map[int64]map[pgtype.UUID]bool{}}
}

func (run *githubPolicyRun) bindings(ctx context.Context, installationID int64) map[pgtype.UUID]bool {
	if set, seen := run.allowed[installationID]; seen {
		return set
	}
	rows, err := run.h.allowedGitHubInstallations(ctx, installationID)
	set := map[pgtype.UUID]bool{}
	for _, row := range rows {
		set[row.WorkspaceID] = true
	}
	if err != nil || len(set) == 0 {
		slog.Warn("github: installation denied by allowlist; skipped", "installation_id", installationID)
	}
	run.allowed[installationID] = set
	return set
}

// installationAllowed is true when any workspace binding passes the policy.
func (run *githubPolicyRun) installationAllowed(ctx context.Context, installationID int64) bool {
	if run.h.githubAllowlist.ids == nil {
		return true
	}
	return len(run.bindings(ctx, installationID)) > 0
}

// workspaceAllowed is true when this workspace's binding passes the policy.
func (run *githubPolicyRun) workspaceAllowed(ctx context.Context, installationID int64, workspaceID pgtype.UUID) bool {
	if run.h.githubAllowlist.ids == nil {
		return true
	}
	return run.bindings(ctx, installationID)[workspaceID]
}

// githubRefreshAllowed is the PR refresh manager's policy hook. The manager has
// no request context, so the lookup gets its own short one.
func (h *Handler) githubRefreshAllowed(installationID int64) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return h.newGitHubPolicyRun().installationAllowed(ctx, installationID)
}
