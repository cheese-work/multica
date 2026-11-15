package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/multica-ai/multica/server/internal/integrations/ghsnapshot"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
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
	policy := h.newGitHubPolicyRun()
	for _, row := range rows {
		if gone[row.InstallationID] {
			continue
		}
		permitted, err := policy.installationAllowed(ctx, row.InstallationID)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if !permitted {
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
				ID    int64  `json:"id"`
				Name  string `json:"name"`
				Owner struct {
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
	if !pull.Head.Repo.sameRepository(base.ID, base.Owner.Login, base.Name) {
		return nil, false
	}
	return pullRequestReplayEvent(raw, "synchronize", installationID, owner, name)
}

// pullRequestReplayEvent wraps a REST pull request as a pull_request webhook
// body with action when its base repository is the row's repository.
func pullRequestReplayEvent(raw json.RawMessage, action string, installationID int64, owner, name string) ([]byte, bool) {
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
	}
	if json.Unmarshal(raw, &pull) != nil {
		return nil, false
	}
	base := pull.Base.Repo
	if !strings.EqualFold(base.FullName, owner+"/"+name) {
		return nil, false
	}
	body, err := json.Marshal(map[string]any{
		"action":       action,
		"pull_request": raw,
		"repository": map[string]any{
			"id": base.ID, "name": base.Name, "default_branch": base.DefaultBranch, "owner": map[string]any{"login": base.Owner.Login},
		},
		"installation": map[string]any{"id": installationID},
	})
	return body, err == nil
}

type owedClosedReplayKey struct{}

// isOwedClosedReplay reports whether the event is an owed closed-PR replay. Its
// retry re-offers the closing issues to auto-completion, which a failed earlier
// attempt may not have reached.
func isOwedClosedReplay(ctx context.Context) bool {
	owed, _ := ctx.Value(owedClosedReplayKey{}).(bool)
	return owed
}

// replayClosedPullRequest is the refresh pipeline's onClosed callback
// (CHE-3402). A PR closed on GitHub whose closed delivery never landed stays
// open in the mirror, and GitHub does not redeliver it on its own. The current
// REST pull request is replayed as a closed event through the normal
// pull_request path, so links, wakeups and auto-complete run as for a real
// delivery. The fork check stays in that path; only the base repository must
// match the row.
func (h *Handler) replayClosedPullRequest(ctx context.Context, installationID int64, owner, name string, number int32) error {
	// The refresh selected its rows before the fetch; a binding revoked or
	// denied since then leaves nothing to replay into (CHE-3414). Skip before
	// any GitHub call or owed marker.
	allowed, err := h.allowedGitHubInstallations(ctx, installationID)
	if err != nil {
		return fmt.Errorf("github: list allowed bindings: %w", err)
	}
	if len(allowed) == 0 {
		return nil
	}
	fetch := h.fetchPullRequestJSON
	if fetch == nil {
		fetch = h.PRRefresh.FetchPullRequest
	}
	raw, err := fetch(ctx, installationID, owner, name, number)
	if err != nil {
		return fmt.Errorf("github: fetch closed pull request: %w", err)
	}
	body, ok := pullRequestReplayEvent(raw, "closed", installationID, owner, name)
	if !ok {
		return errors.New("github: closed pull request is not from the mirrored base repository")
	}
	// The mirror row commits before lifecycle and wakeup work. Record the replay
	// as owed first, so a failure after that commit is retried by the sweep.
	params := db.MarkGitHubPRClosedReplayPendingParams{InstallationID: installationID, RepoOwner: owner, RepoName: name, PrNumber: number}
	if err := h.Queries.MarkGitHubPRClosedReplayPending(ctx, params); err != nil {
		return fmt.Errorf("github: record owed closed pull request replay: %w", err)
	}
	if err := h.handlePullRequestEvent(context.WithValue(ctx, owedClosedReplayKey{}, true), body); err != nil {
		return err
	}
	if err := h.Queries.ClearGitHubPRClosedReplayPending(ctx, db.ClearGitHubPRClosedReplayPendingParams(params)); err != nil {
		return fmt.Errorf("github: clear owed closed pull request replay: %w", err)
	}
	return nil
}
