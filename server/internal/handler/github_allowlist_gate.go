package handler

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

// githubPolicyRun caches the allowed bindings of each installation for one
// sweep and logs a fully denied installation once. A lookup error is returned,
// never read as a denial: the caller retries instead of settling the work.
type githubPolicyRun struct {
	h       *Handler
	allowed map[int64]map[pgtype.UUID]bool
	failed  map[int64]error
}

func (h *Handler) newGitHubPolicyRun() *githubPolicyRun {
	return &githubPolicyRun{h: h, allowed: map[int64]map[pgtype.UUID]bool{}, failed: map[int64]error{}}
}

func (run *githubPolicyRun) bindings(ctx context.Context, installationID int64) (map[pgtype.UUID]bool, error) {
	if set, seen := run.allowed[installationID]; seen {
		return set, nil
	}
	if err, seen := run.failed[installationID]; seen {
		return nil, err
	}
	rows, err := run.h.allowedGitHubInstallations(ctx, installationID)
	if err != nil {
		slog.Warn("github: allowlist lookup failed; will retry", "installation_id", installationID, "error", err)
		run.failed[installationID] = err
		return nil, err
	}
	set := map[pgtype.UUID]bool{}
	for _, row := range rows {
		set[row.WorkspaceID] = true
	}
	if len(set) == 0 {
		slog.Warn("github: installation denied by allowlist; skipped", "installation_id", installationID)
	}
	run.allowed[installationID] = set
	return set, nil
}

// installationAllowed is true when any workspace binding passes the policy.
func (run *githubPolicyRun) installationAllowed(ctx context.Context, installationID int64) (bool, error) {
	if run.h.githubAllowlist.ids == nil {
		return true, nil
	}
	set, err := run.bindings(ctx, installationID)
	return len(set) > 0, err
}

// workspaceAllowed is true when this workspace's binding passes the policy.
func (run *githubPolicyRun) workspaceAllowed(ctx context.Context, installationID int64, workspaceID pgtype.UUID) (bool, error) {
	if run.h.githubAllowlist.ids == nil {
		return true, nil
	}
	set, err := run.bindings(ctx, installationID)
	return set[workspaceID], err
}

// githubRefreshAllowed is the PR refresh manager's policy hook. The manager has
// no request context, so the lookup gets its own short one. Enqueue is
// best-effort and the sweep retries, so a lookup error skips this call only.
func (h *Handler) githubRefreshAllowed(installationID int64) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	allowed, err := h.newGitHubPolicyRun().installationAllowed(ctx, installationID)
	return allowed && err == nil
}
