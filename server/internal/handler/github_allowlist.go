package handler

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const githubAllowedAccountIDsEnv = "GITHUB_ALLOWED_ACCOUNT_IDS"

// githubAllowlist is the parsed GITHUB_ALLOWED_ACCOUNT_IDS policy. A nil ids
// map means the policy is off.
type githubAllowlist struct {
	ids map[int64]struct{}
}

// parseGitHubAllowedAccountIDs reads a comma-separated list of numeric GitHub
// account ids. Only a blank value turns the policy off. Any other malformed
// list, including an empty entry ("11,," or ","), is an error so a typo cannot
// silently disable or narrow the policy.
func parseGitHubAllowedAccountIDs(raw string) (githubAllowlist, error) {
	var list githubAllowlist
	if strings.TrimSpace(raw) == "" {
		return list, nil
	}
	list.ids = map[int64]struct{}{}
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		id, err := strconv.ParseInt(entry, 10, 64)
		if err != nil || id <= 0 {
			return githubAllowlist{}, fmt.Errorf("%s: %q is not a numeric GitHub account id", githubAllowedAccountIDsEnv, entry)
		}
		list.ids[id] = struct{}{}
	}
	return list, nil
}

// ConfigureGitHubAllowlist installs the policy at startup. A malformed list is
// returned as an error for the caller to treat as fatal. An empty list turns
// the policy off and warns once when GitHub is configured.
func (h *Handler) ConfigureGitHubAllowlist(raw string) error {
	list, err := parseGitHubAllowedAccountIDs(raw)
	if err != nil {
		return err
	}
	h.githubAllowlist = list
	h.PRRefresh.SetInstallationFilter(h.githubRefreshAllowed)
	if list.ids == nil {
		slog.Warn(githubAllowedAccountIDsEnv + " is unset; every GitHub App installation is allowed")
	}
	// The refresh pipeline also selects rows by address; keep it off denied
	// bindings that share an installation with an allowed one.
	h.PRRefresh.SetWorkspaceFilter(func(ctx context.Context, q *db.Queries, installationID int64) (map[pgtype.UUID]bool, error) {
		rows, err := q.ListGitHubInstallationsByInstallationIDForShare(ctx, installationID)
		if err != nil {
			return nil, err
		}
		set := make(map[pgtype.UUID]bool, len(rows))
		for _, row := range rows {
			if h.githubInstallationAllowed(row) {
				set[row.WorkspaceID] = true
			}
		}
		return set, nil
	})
	return nil
}

// githubInstallationAllowed applies the policy to one binding. With the policy
// on, only a stored numeric account_id in the list passes; a null id never
// does. The login is display data and is never consulted.
func (h *Handler) githubInstallationAllowed(row db.GithubInstallation) bool {
	if h.githubAllowlist.ids == nil {
		return true
	}
	if !row.AccountID.Valid {
		slog.Warn("github: installation has no trusted account_id; denied by allowlist",
			"installation_id", row.InstallationID, "workspace_id", uuidToString(row.WorkspaceID))
		return false
	}
	_, ok := h.githubAllowlist.ids[row.AccountID.Int64]
	return ok
}

// allowedGitHubInstallations lists the workspace bindings of an installation
// that pass the policy.
func (h *Handler) allowedGitHubInstallations(ctx context.Context, installationID int64) ([]db.GithubInstallation, error) {
	rows, err := h.Queries.ListGitHubInstallationsByInstallationID(ctx, installationID)
	if err != nil {
		return nil, err
	}
	allowed := rows[:0:0]
	for _, row := range rows {
		if h.githubInstallationAllowed(row) {
			allowed = append(allowed, row)
		}
	}
	return allowed, nil
}

// anyGitHubInstallationAllowed reports whether at least one binding passes the
// policy.
func (h *Handler) anyGitHubInstallationAllowed(rows []db.GithubInstallation) bool {
	for _, row := range rows {
		if h.githubInstallationAllowed(row) {
			return true
		}
	}
	return false
}

// workspaceIDsOf lists the workspace of each binding.
func workspaceIDsOf(rows []db.GithubInstallation) []pgtype.UUID {
	ids := make([]pgtype.UUID, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.WorkspaceID)
	}
	return ids
}

// allowedWorkspaceIDs lists the workspaces of the bindings that pass the policy.
func (h *Handler) allowedWorkspaceIDs(rows []db.GithubInstallation) []pgtype.UUID {
	ids := make([]pgtype.UUID, 0, len(rows))
	for _, row := range rows {
		if h.githubInstallationAllowed(row) {
			ids = append(ids, row.WorkspaceID)
		}
	}
	return ids
}

// githubAccountIDParam maps a trusted id (0 = none) to a nullable column value.
func githubAccountIDParam(id int64) pgtype.Int8 {
	return pgtype.Int8{Int64: id, Valid: id > 0}
}

// BackfillGitHubAccountIDs fills account_id for rows bound before the column
// existed, from the App API (GET /app/installations/{id}). Failures leave the
// row null, which the policy denies. Returns the resolved and failed counts.
func (h *Handler) BackfillGitHubAccountIDs(ctx context.Context) (resolved, failed int) {
	rows, err := h.Queries.ListGitHubInstallationsMissingAccountID(ctx)
	if err != nil {
		slog.Warn("github: account_id backfill list failed", "err", err)
		return 0, 0
	}
	// One App lookup per installation, shared by its workspace bindings.
	seen := map[int64]bool{}
	client := &http.Client{Timeout: 15 * time.Second}
	for _, row := range rows {
		if seen[row.InstallationID] {
			continue
		}
		seen[row.InstallationID] = true
		lookupCtx, cancel := context.WithTimeout(ctx, githubUserAuthorizationTimeout)
		inst, err := fetchGitHubInstallationForBinding(lookupCtx, client, row.InstallationID)
		cancel()
		if err != nil {
			failed++
			slog.Warn("github: account_id backfill lookup failed", "err", err, "installation_id", row.InstallationID)
			continue
		}
		if _, err := h.Queries.SetGitHubInstallationAccountIDByInstallationID(ctx,
			db.SetGitHubInstallationAccountIDByInstallationIDParams{
				InstallationID: row.InstallationID,
				AccountID:      githubAccountIDParam(inst.Account.ID),
			}); err != nil {
			failed++
			slog.Warn("github: account_id backfill write failed", "err", err, "installation_id", row.InstallationID)
			continue
		}
		resolved++
	}
	slog.Info("github: account_id backfill finished", "resolved", resolved, "failed", failed)
	return resolved, failed
}

// ConfigureGitHubAllowlistFromEnv installs the policy from the environment.
func (h *Handler) ConfigureGitHubAllowlistFromEnv() error {
	return h.ConfigureGitHubAllowlist(os.Getenv(githubAllowedAccountIDsEnv))
}
