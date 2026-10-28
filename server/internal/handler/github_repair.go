package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/multica-ai/multica/server/internal/integrations/ghsnapshot"
)

// RepairUnverifiedHeadPullRequests replays PRs stored with head_repo_same=false
// whose head repository GitHub reports as the base repository (CHE-2301). The
// old check rejected every same-repository PR in a repository that is itself a
// fork, so those rows never auto-linked, auto-completed or woke anyone. The row
// keeps no head repository id, so identity comes from the live REST pull request:
// only a PR whose head repository id and full name equal its base repository's
// is replayed, through the normal pull_request path. That path keeps member
// links and exclusions, and runs auto-complete only for newly linked issues.
// Real fork PRs stay unchanged. The sweep is idempotent.
func (h *Handler) RepairUnverifiedHeadPullRequests(ctx context.Context) (replayed int, err error) {
	fetch := h.fetchPullRequestJSON
	if fetch == nil && h.PRRefresh.Enabled() {
		fetch = h.PRRefresh.FetchPullRequest
	}
	if fetch == nil {
		slog.Warn("github repair: unverified head sweep skipped, GitHub App API client not configured")
		return 0, nil
	}
	rows, err := h.Queries.ListGitHubPRsWithUnverifiedHead(ctx)
	if err != nil {
		return 0, fmt.Errorf("github repair: list rows: %w", err)
	}
	var errs []error
	gone := map[int64]bool{}
	for _, row := range rows {
		if gone[row.InstallationID] {
			continue
		}
		raw, err := fetch(ctx, row.InstallationID, row.RepoOwner, row.RepoName, row.PrNumber)
		var rateLimit *ghsnapshot.RateLimitError
		if errors.As(err, &rateLimit) {
			return replayed, errors.Join(append(errs, err)...)
		}
		var installationGone *ghsnapshot.InstallationGoneError
		if errors.As(err, &installationGone) {
			// Every row of this installation fails the same way and burns the rate-limit budget.
			slog.Warn("github repair: installation unavailable, skipping its remaining rows", "installation_id", row.InstallationID, "err", err)
			gone[row.InstallationID] = true
			errs = append(errs, err)
			continue
		}
		if err != nil {
			slog.Warn("github repair: fetch pull request failed", "repo", row.RepoOwner+"/"+row.RepoName, "pr_number", row.PrNumber, "err", err)
			errs = append(errs, err)
			continue
		}
		body, ok := replayableSameRepositoryPR(raw, row.InstallationID, row.RepoOwner, row.RepoName)
		if !ok {
			continue
		}
		if err := h.handlePullRequestEvent(ctx, body); err != nil {
			errs = append(errs, err)
			continue
		}
		replayed++
	}
	slog.Info("github repair: unverified head sweep done", "candidates", len(rows), "replayed", replayed, "errors", len(errs))
	return replayed, errors.Join(errs...)
}

// replayableSameRepositoryPR wraps a REST pull request as a synchronize event
// when its base repository is the row's repository and its head repository is
// that same repository by id and full name.
func replayableSameRepositoryPR(raw json.RawMessage, installationID int64, owner, name string) ([]byte, bool) {
	var pull struct {
		Base struct {
			Repo struct {
				ID            int64  `json:"id"`
				Name          string `json:"name"`
				FullName      string `json:"full_name"`
				DefaultBranch string `json:"default_branch"`
				Owner         struct {
					Login string `json:"login"`
				} `json:"owner"`
			} `json:"repo"`
		} `json:"base"`
		Head struct {
			Repo *ghPRHeadRepository `json:"repo"`
		} `json:"head"`
	}
	if json.Unmarshal(raw, &pull) != nil {
		return nil, false
	}
	base := pull.Base.Repo
	if !strings.EqualFold(base.FullName, owner+"/"+name) || !pull.Head.Repo.sameRepository(base.ID, base.Owner.Login, base.Name) {
		return nil, false
	}
	body, err := json.Marshal(map[string]any{
		"action":       "synchronize",
		"pull_request": raw,
		"repository": map[string]any{
			"id": base.ID, "name": base.Name, "default_branch": base.DefaultBranch, "owner": map[string]any{"login": base.Owner.Login},
		},
		"installation": map[string]any{"id": installationID},
	})
	return body, err == nil
}
