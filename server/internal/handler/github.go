package handler

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/issuestatus"
	"github.com/multica-ai/multica/server/internal/middleware"
	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// githubAPIBase is the base URL for GitHub's REST API. Mutable so tests can
// point App-authenticated calls at an httptest server without touching GitHub.
var githubAPIBase = "https://api.github.com"

// githubOAuthBase is separate because GitHub's OAuth exchange uses github.com,
// while all identity and installation authority checks use api.github.com.
// Tests point both at the same local server.
var githubOAuthBase = "https://github.com"

const (
	githubReturnToGitHub       = "github"
	githubReturnToRepositories = "repositories"
	githubAPIResponseLimit     = 4 << 20
)

// ── Response shapes ─────────────────────────────────────────────────────────

// GitHubInstallationResponse is the JSON shape returned by the installation
// list endpoint and broadcast on installation-related WS events.
//
// InstallationID is admin-only: the numeric GitHub installation_id is the
// management handle used by the Connect/Disconnect flows, so non-admin
// members receive responses with the field omitted. The list handler gates
// it by role; realtime broadcasts always omit it because the WS fanout has
// no per-recipient view (admins re-query the list endpoint on invalidation
// to recover the management handle).
type GitHubInstallationResponse struct {
	ID               string  `json:"id"`
	WorkspaceID      string  `json:"workspace_id"`
	InstallationID   *int64  `json:"installation_id,omitempty"`
	AccountLogin     string  `json:"account_login"`
	AccountType      string  `json:"account_type"`
	AccountAvatarURL *string `json:"account_avatar_url"`
	CreatedAt        string  `json:"created_at"`
}

type GitHubPullRequestResponse struct {
	ID string `json:"id"`
	// Provider is the Git provider this PR was mirrored from: "github", "forgejo",
	// "gitea", or "gitlab". The frontend uses it to pick the host icon and
	// label (e.g. GitLab "merge request").
	Provider        string  `json:"provider"`
	WorkspaceID     string  `json:"workspace_id"`
	RepoOwner       string  `json:"repo_owner"`
	RepoName        string  `json:"repo_name"`
	Number          int32   `json:"number"`
	Title           string  `json:"title"`
	State           string  `json:"state"`
	HtmlURL         string  `json:"html_url"`
	Branch          *string `json:"branch"`
	AuthorLogin     *string `json:"author_login"`
	AuthorAvatarURL *string `json:"author_avatar_url"`
	MergedAt        *string `json:"merged_at"`
	ClosedAt        *string `json:"closed_at"`
	PRCreatedAt     string  `json:"pr_created_at"`
	PRUpdatedAt     string  `json:"pr_updated_at"`
	// LinkSource explains why the PR is on the issue: "manual", "title",
	// "branch", or "auto" (an older rule). Only set on the issue PR list.
	LinkSource string `json:"link_source,omitempty"`
	// Mergeable state mirrors GitHub's REST `mergeable_state` field, retained
	// for compatibility. The card now reads the richer GraphQL fields below.
	MergeableState *string `json:"mergeable_state"`
	// ── GitHub API snapshot (MUL-5265, Plan C) ──────────────────────────────
	// These come from an authenticated GraphQL query, the single source of
	// truth. All are null / empty / 0 when no snapshot has landed (or the
	// GitHub App private key is unconfigured), so the card hides the CI / merge
	// region and degrades cleanly.
	//
	// Mergeable answers ONLY "is there a conflict": "mergeable" | "conflicting"
	// | "unknown" | null. A false "conflicting" must never be reported as
	// "not mergeable" — that verdict is MergeStateStatus's job.
	Mergeable *string `json:"mergeable"`
	// MergeStateStatus is GitHub's merge-state verdict, lowercased: "clean" |
	// "dirty" | "blocked" | "behind" | "unstable" | "draft" | "has_hooks" |
	// "unknown" | null. "Ready to merge" is derived ONLY from "clean".
	MergeStateStatus *string `json:"merge_state_status"`
	// SnapshotAvailable distinguishes a current API snapshot from both
	// "feature disabled / not fetched yet" and "a current snapshot whose
	// statusCheckRollup is null". Only the last case may render "no checks".
	// It is omitted for non-GitHub providers, which keep their webhook-derived
	// checks_conclusion compatibility path.
	SnapshotAvailable *bool `json:"snapshot_available,omitempty"`
	// ChecksRollup is GitHub's overall CI verdict, lowercased: "success" |
	// "failure" | "pending" | "error" | "expected" | null. null means
	// statusCheckRollup was null (no checks yet) and must NEVER render as
	// passed.
	ChecksRollup *string `json:"checks_rollup"`
	// ChecksConclusion is a coarse compat alias derived from the snapshot:
	// "passed" | "failed" | "pending" | null.
	ChecksConclusion *string `json:"checks_conclusion"`
	// Run-level counts for the PR's snapshot head. ChecksPending mirrors
	// ChecksRunning for older clients that still read the old key.
	ChecksTotal   int64 `json:"checks_total"`
	ChecksPassed  int64 `json:"checks_passed"`
	ChecksFailed  int64 `json:"checks_failed"`
	ChecksRunning int64 `json:"checks_running"`
	ChecksPending int64 `json:"checks_pending"`
	// FailedCheckNames names the failing checks so the card can point at them
	// (e.g. "✗ 2/7 · backend, e2e").
	FailedCheckNames []string `json:"failed_check_names"`
	// SnapshotStale is true when an open PR's last successful fetch is older
	// than the stale threshold (GitHub outage / revoked key): the card shows
	// last-known data greyed out rather than blank.
	SnapshotStale bool `json:"snapshot_stale"`
	// SnapshotFetchedAt is when the snapshot was last fetched (RFC3339), or null.
	SnapshotFetchedAt *string `json:"snapshot_fetched_at"`
	// Diff stats (lines added/removed and file count) sourced from the
	// `pull_request` webhook payload. Legacy rows that pre-date this
	// field default to 0; the frontend treats total == 0 as "unknown"
	// and hides the stats row.
	Additions    int32 `json:"additions"`
	Deletions    int32 `json:"deletions"`
	ChangedFiles int32 `json:"changed_files"`
}

type GitHubConnectResponse struct {
	URL        string `json:"url"`
	Configured bool   `json:"configured"`
}

type GitHubRepositoryResponse struct {
	ID            int64   `json:"id"`
	FullName      string  `json:"full_name"`
	HTMLURL       string  `json:"html_url"`
	CloneURL      string  `json:"clone_url"`
	Description   *string `json:"description"`
	Private       bool    `json:"private"`
	Archived      bool    `json:"archived"`
	DefaultBranch string  `json:"default_branch"`
}

type GitHubRepositoriesResponse struct {
	Repositories []GitHubRepositoryResponse `json:"repositories"`
	TotalCount   int64                      `json:"total_count"`
	NextPage     *int                       `json:"next_page"`
}

func githubInstallationToResponse(i db.GithubInstallation) GitHubInstallationResponse {
	instID := i.InstallationID
	return GitHubInstallationResponse{
		ID:               uuidToString(i.ID),
		WorkspaceID:      uuidToString(i.WorkspaceID),
		InstallationID:   &instID,
		AccountLogin:     i.AccountLogin,
		AccountType:      i.AccountType,
		AccountAvatarURL: textToPtr(i.AccountAvatarUrl),
		CreatedAt:        timestampToString(i.CreatedAt),
	}
}

// githubInstallationToBroadcast returns the same shape as the list endpoint's
// per-role response with the numeric `installation_id` stripped. Realtime
// events fan out to every WS client subscribed to the workspace, so the
// payload must match the weakest-role view — admin/owner clients re-query
// the list endpoint to recover the management handle. The frontend uses
// these events only to invalidate the installations query, so it does not
// read `installation_id` off the broadcast.
func githubInstallationToBroadcast(i db.GithubInstallation) GitHubInstallationResponse {
	resp := githubInstallationToResponse(i)
	resp.InstallationID = nil
	return resp
}

func githubPullRequestToResponse(p db.GithubPullRequest, snapshotEnabled bool) GitHubPullRequestResponse {
	snapshotAvailable := currentGitHubSnapshotAvailable(
		snapshotEnabled, p.HeadSha, p.SnapshotHeadSha, p.SnapshotFetchedAt,
	)
	return GitHubPullRequestResponse{
		ID:                uuidToString(p.ID),
		Provider:          "github",
		WorkspaceID:       uuidToString(p.WorkspaceID),
		RepoOwner:         p.RepoOwner,
		RepoName:          p.RepoName,
		Number:            p.PrNumber,
		Title:             p.Title,
		State:             p.State,
		HtmlURL:           p.HtmlUrl,
		Branch:            textToPtr(p.Branch),
		AuthorLogin:       textToPtr(p.AuthorLogin),
		AuthorAvatarURL:   textToPtr(p.AuthorAvatarUrl),
		MergedAt:          timestampToPtr(p.MergedAt),
		ClosedAt:          timestampToPtr(p.ClosedAt),
		PRCreatedAt:       timestampToString(p.PrCreatedAt),
		PRUpdatedAt:       timestampToString(p.PrUpdatedAt),
		MergeableState:    textToPtr(p.MergeableState),
		SnapshotAvailable: &snapshotAvailable,
		// A bare PR row has no aggregated check counts — webhook broadcasts of a
		// single PR fall through here and the frontend re-queries the list for
		// the full snapshot (mergeable / rollup / counts).
		ChecksConclusion: nil,
		FailedCheckNames: []string{},
		Additions:        p.Additions,
		Deletions:        p.Deletions,
		ChangedFiles:     p.ChangedFiles,
	}
}

// prSnapshotStaleThreshold is how old an open PR's last successful fetch may be
// before the card greys it out as stale. Healthy pipelines refresh open PRs at
// least every sweep interval (~10m), so crossing 30m means refreshes are not
// landing (GitHub outage, revoked key) and the shown data is last-known.
const prSnapshotStaleThreshold = 30 * time.Minute

func issuePullRequestRowToResponse(p db.ListPullRequestsByIssueRow, snapshotEnabled bool) GitHubPullRequestResponse {
	snapshotAvailable := currentGitHubSnapshotAvailable(
		snapshotEnabled, p.HeadSha, p.SnapshotHeadSha, p.SnapshotFetchedAt,
	)
	stale := false
	if snapshotAvailable && (p.State == "open" || p.State == "draft") {
		stale = time.Since(p.SnapshotFetchedAt.Time) > prSnapshotStaleThreshold
	}
	failedNames := p.FailedCheckNames
	if failedNames == nil {
		failedNames = []string{}
	}
	resp := GitHubPullRequestResponse{
		ID:                uuidToString(p.ID),
		Provider:          "github",
		WorkspaceID:       uuidToString(p.WorkspaceID),
		RepoOwner:         p.RepoOwner,
		RepoName:          p.RepoName,
		Number:            p.PrNumber,
		Title:             p.Title,
		State:             p.State,
		HtmlURL:           p.HtmlUrl,
		Branch:            textToPtr(p.Branch),
		AuthorLogin:       textToPtr(p.AuthorLogin),
		AuthorAvatarURL:   textToPtr(p.AuthorAvatarUrl),
		MergedAt:          timestampToPtr(p.MergedAt),
		ClosedAt:          timestampToPtr(p.ClosedAt),
		PRCreatedAt:       timestampToString(p.PrCreatedAt),
		PRUpdatedAt:       timestampToString(p.PrUpdatedAt),
		MergeableState:    textToPtr(p.MergeableState),
		SnapshotAvailable: &snapshotAvailable,
		FailedCheckNames:  []string{},
		SnapshotStale:     stale,
		Additions:         p.Additions,
		Deletions:         p.Deletions,
		ChangedFiles:      p.ChangedFiles,
	}
	if snapshotAvailable {
		resp.Mergeable = lowerTextPtr(p.ApiMergeable)
		resp.MergeStateStatus = lowerTextPtr(p.ApiMergeStateStatus)
		resp.ChecksRollup = lowerTextPtr(p.ChecksRollupState)
		resp.ChecksConclusion = rollupToConclusion(p.ChecksRollupState, p.ChecksFailed, p.ChecksRunning, p.ChecksPassed)
		resp.ChecksTotal = p.ChecksTotal
		resp.ChecksPassed = p.ChecksPassed
		resp.ChecksFailed = p.ChecksFailed
		resp.ChecksRunning = p.ChecksRunning
		resp.ChecksPending = p.ChecksRunning
		resp.FailedCheckNames = failedNames
		resp.SnapshotFetchedAt = timestampToPtr(p.SnapshotFetchedAt)
	}
	return resp
}

func currentGitHubSnapshotAvailable(
	enabled bool,
	headSHA string,
	snapshotHeadSHA string,
	fetchedAt pgtype.Timestamptz,
) bool {
	return enabled && fetchedAt.Valid && snapshotHeadSHA != "" && snapshotHeadSHA == headSHA
}

// aggregateChecksConclusion collapses per-PR commit-status counts into a
// single coarse status. Still used by the self-hosted VCS provider path
// (Forgejo / Gitea / GitLab), which mirrors commit statuses via webhook rather
// than fetching a GitHub-style API snapshot:
//   - any failed status wins ("failed");
//   - any not-yet-completed status makes the PR "pending";
//   - all completed and passed is "passed";
//   - no observed status at all is nil (rendered as "no checks" / hidden).
func aggregateChecksConclusion(failed, passed, pending, total int64) *string {
	if total == 0 {
		return nil
	}
	var v string
	switch {
	case failed > 0:
		v = "failed"
	case pending > 0:
		v = "pending"
	case passed > 0:
		v = "passed"
	default:
		return nil
	}
	return &v
}

// lowerTextPtr returns a lowercased *string for a non-empty pgtype.Text, else
// nil. Used to expose GraphQL enums (MERGEABLE / CLEAN / SUCCESS …) to the API
// in the project's lowercase convention.
func lowerTextPtr(t pgtype.Text) *string {
	if !t.Valid || t.String == "" {
		return nil
	}
	v := strings.ToLower(t.String)
	return &v
}

// rollupToConclusion derives the coarse compat "checks_conclusion" from the
// GraphQL rollup, falling back to the run counts when the rollup enum is
// unfamiliar. A null/empty rollup means "no checks yet" → nil (never "passed").
func rollupToConclusion(rollup pgtype.Text, failed, running, passed int64) *string {
	if !rollup.Valid || rollup.String == "" {
		return nil
	}
	var v string
	switch strings.ToUpper(rollup.String) {
	case "FAILURE", "ERROR":
		v = "failed"
	case "PENDING", "EXPECTED":
		v = "pending"
	case "SUCCESS":
		v = "passed"
	default:
		switch {
		case failed > 0:
			v = "failed"
		case running > 0:
			v = "pending"
		case passed > 0:
			v = "passed"
		default:
			return nil
		}
	}
	return &v
}

// ── Connect / state token ───────────────────────────────────────────────────

// githubAppSlug returns the GitHub App slug used to build the install URL.
// Empty when the integration is not configured for this deployment.
func githubAppSlug() string { return strings.TrimSpace(os.Getenv("GITHUB_APP_SLUG")) }

// githubWebhookSecret is shared by webhook verification and state-token signing.
// We reuse the webhook secret as the state HMAC key so operators only need to
// configure one value.
func githubWebhookSecret() string { return strings.TrimSpace(os.Getenv("GITHUB_WEBHOOK_SECRET")) }

func githubAppClientID() string { return strings.TrimSpace(os.Getenv("GITHUB_APP_CLIENT_ID")) }

func githubAppClientSecret() string {
	return strings.TrimSpace(os.Getenv("GITHUB_APP_CLIENT_SECRET"))
}

// A fresh binding is accepted only after both App-authenticated installation
// lookup and user-authority verification. Hide Connect when either credential
// set is incomplete instead of offering a callback that must fail closed.
func isGitHubConfigured() bool {
	return githubAppSlug() != "" &&
		githubWebhookSecret() != "" &&
		githubAppClientID() != "" &&
		githubAppClientSecret() != "" &&
		isGitHubRepositoryBrowseConfigured()
}

// isGitHubRepositoryBrowseConfigured is deliberately separate from the
// install-flow flag because clients expose repository browsing as its own
// capability. App JWT credentials are also part of the stronger connect gate.
func isGitHubRepositoryBrowseConfigured() bool {
	return strings.TrimSpace(os.Getenv("GITHUB_APP_ID")) != "" &&
		strings.TrimSpace(os.Getenv("GITHUB_APP_PRIVATE_KEY")) != ""
}

const (
	githubStateMaxAge    = 15 * time.Minute
	githubStateClockSkew = time.Minute
)

// signState produces an opaque token that binds a workspace and return target
// to a short-lived install flow.
// Format: "<workspaceID>.<returnTo>.<issuedAtUnix>.<nonce>.<sigHex>".
func signState(workspaceID string) (string, error) {
	return signStateForReturn(workspaceID, githubReturnToGitHub)
}

func signStateForReturn(workspaceID, returnTo string) (string, error) {
	return signStateForReturnAt(workspaceID, returnTo, time.Now())
}

func signStateForReturnAt(workspaceID, returnTo string, now time.Time) (string, error) {
	secret := githubWebhookSecret()
	if secret == "" {
		return "", errors.New("github integration is not configured")
	}
	if !isAllowedGitHubReturnTo(returnTo) {
		return "", errors.New("invalid github return target")
	}
	nonceBytes := make([]byte, 12)
	if _, err := rand.Read(nonceBytes); err != nil {
		return "", err
	}
	payload := strings.Join([]string{
		workspaceID,
		returnTo,
		strconv.FormatInt(now.Unix(), 10),
		hex.EncodeToString(nonceBytes),
	}, ".")
	return payload + "." + signGitHubState(secret, payload), nil
}

func signGitHubState(secret, payload string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(payload))
	return hex.EncodeToString(mac.Sum(nil))
}

func verifyState(token string) (string, bool) {
	workspaceID, _, ok := verifyStateWithReturn(token)
	return workspaceID, ok
}

func verifyStateWithReturn(token string) (workspaceID, returnTo string, ok bool) {
	return verifyStateWithReturnAt(token, time.Now())
}

func verifyStateWithReturnAt(token string, now time.Time) (workspaceID, returnTo string, ok bool) {
	secret := githubWebhookSecret()
	if secret == "" {
		return "", "", false
	}
	parts := strings.Split(token, ".")
	if len(parts) != 5 {
		return "", "", false
	}
	workspaceID, returnTo = parts[0], parts[1]
	if !isAllowedGitHubReturnTo(returnTo) {
		return "", "", false
	}
	payload := strings.Join(parts[:4], ".")
	if !hmac.Equal([]byte(signGitHubState(secret, payload)), []byte(parts[4])) {
		return "", "", false
	}
	issuedAt, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		return "", "", false
	}
	age := now.Sub(time.Unix(issuedAt, 0))
	if age > githubStateMaxAge || age < -githubStateClockSkew {
		return "", "", false
	}
	return workspaceID, returnTo, true
}

func isAllowedGitHubReturnTo(returnTo string) bool {
	return returnTo == githubReturnToGitHub || returnTo == githubReturnToRepositories
}

func githubSettingsURL(frontend, returnTo string) string {
	if !isAllowedGitHubReturnTo(returnTo) {
		returnTo = githubReturnToGitHub
	}
	return strings.TrimRight(frontend, "/") + "/settings?tab=" + url.QueryEscape(returnTo)
}

// GitHubConnect (GET /api/workspaces/{id}/github/connect) returns the URL the
// browser should open to install the Multica GitHub App against the caller's
// repos. The state token binds the resulting setup callback to this workspace.
func (h *Handler) GitHubConnect(w http.ResponseWriter, r *http.Request) {
	workspaceID := chi.URLParam(r, "id")
	if _, ok := parseUUIDOrBadRequest(w, workspaceID, "workspace id"); !ok {
		return
	}
	if !isGitHubConfigured() {
		writeJSON(w, http.StatusOK, GitHubConnectResponse{Configured: false})
		return
	}
	returnTo := strings.TrimSpace(r.URL.Query().Get("return_to"))
	if returnTo == "" {
		returnTo = githubReturnToGitHub
	}
	if !isAllowedGitHubReturnTo(returnTo) {
		writeError(w, http.StatusBadRequest, "invalid return target")
		return
	}
	slug := githubAppSlug()
	state, err := signStateForReturn(workspaceID, returnTo)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to sign state")
		return
	}
	installURL := fmt.Sprintf(
		"https://github.com/apps/%s/installations/new?state=%s",
		url.PathEscape(slug),
		url.QueryEscape(state),
	)
	writeJSON(w, http.StatusOK, GitHubConnectResponse{URL: installURL, Configured: true})
}

// GitHubSetupCallback (GET /api/github/setup) handles the redirect GitHub
// sends after a user installs (or re-authorizes) the App. A new binding needs
// GitHub's user-authorization code; an exact existing binding may be refreshed
// without one only for setup_action=update.
func (h *Handler) GitHubSetupCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	installationIDStr := q.Get("installation_id")
	state := q.Get("state")
	frontend := strings.TrimSpace(os.Getenv("FRONTEND_ORIGIN"))
	if frontend == "" {
		frontend = "http://localhost:3000"
	}
	settingsURL := githubSettingsURL(frontend, githubReturnToGitHub)

	if state == "" {
		http.Redirect(w, r, settingsURL+"&github_error=missing_params", http.StatusFound)
		return
	}
	workspaceID, returnTo, ok := verifyStateWithReturn(state)
	if !ok {
		http.Redirect(w, r, settingsURL+"&github_error=invalid_state", http.StatusFound)
		return
	}
	settingsURL = githubSettingsURL(frontend, returnTo)
	if installationIDStr == "" {
		http.Redirect(w, r, settingsURL+"&github_error=missing_params", http.StatusFound)
		return
	}
	installationID, err := strconv.ParseInt(installationIDStr, 10, 64)
	if err != nil {
		http.Redirect(w, r, settingsURL+"&github_error=bad_installation_id", http.StatusFound)
		return
	}
	wsUUID, err := parseStrictUUID(workspaceID)
	if err != nil {
		http.Redirect(w, r, settingsURL+"&github_error=bad_workspace", http.StatusFound)
		return
	}
	existing, alreadyBound := h.workspaceGitHubInstallation(r.Context(), wsUUID, installationID)
	code := strings.TrimSpace(q.Get("code"))
	var verified githubUserInstallation
	if code == "" {
		if q.Get("setup_action") != "update" || !alreadyBound {
			http.Redirect(w, r, settingsURL+"&github_error=missing_authorization", http.StatusFound)
			return
		}
		verified = githubUserInstallationFromRow(existing)
	} else {
		verified, err = verifyGitHubInstallationOwnership(r.Context(), code, installationID)
		if err != nil {
			slog.Warn("github: refused unverified installation bind",
				"err", err,
				"installation_id", installationID,
				"workspace_id", workspaceID,
			)
			http.Redirect(w, r, settingsURL+"&github_error="+githubOwnershipErrorCode(err), http.StatusFound)
			return
		}
	}

	connectedBy := pgtype.UUID{}
	if code == "" {
		connectedBy = existing.ConnectedByID
	} else if userID := requestUserID(r); userID != "" {
		if u, err := parseStrictUUID(userID); err == nil {
			connectedBy = u
		}
	}
	avatar := ptrToText(nil)
	if verified.Account.AvatarURL != "" {
		avatar = ptrToText(&verified.Account.AvatarURL)
	}

	inst, err := h.Queries.CreateGitHubInstallation(r.Context(), db.CreateGitHubInstallationParams{
		WorkspaceID:      wsUUID,
		InstallationID:   installationID,
		AccountLogin:     verified.Account.Login,
		AccountType:      verified.Account.Type,
		AccountAvatarUrl: avatar,
		ConnectedByID:    connectedBy,
	})
	if err != nil {
		slog.Error("github: failed to persist installation", "err", err, "installation_id", installationID)
		http.Redirect(w, r, settingsURL+"&github_error=persist_failed", http.StatusFound)
		return
	}
	// The App-authenticated lookup above is authoritative. A signed webhook may
	// have arrived first, but its pending display snapshot must not replace the
	// account that was used for this authority decision.
	if err := h.Queries.DeletePendingGitHubInstallation(r.Context(), installationID); err != nil {
		slog.Warn("github: failed to clear pending installation metadata", "err", err, "installation_id", installationID)
	}
	h.publish(protocol.EventGitHubInstallationCreated, workspaceID, "system", "", map[string]any{
		"installation": githubInstallationToBroadcast(inst),
	})
	redirectURL := settingsURL + "&github_connected=1"
	if returnTo == githubReturnToRepositories {
		redirectURL += "&github_installation=" + url.QueryEscape(uuidToString(inst.ID))
	}
	http.Redirect(w, r, redirectURL, http.StatusFound)
}

// ── Install authority proof ────────────────────────────────────────────────

var (
	errGitHubUserAuthorizationMissing  = errors.New("github: setup callback carried no user authorization code")
	errGitHubInstallationNotAuthorized = errors.New("github: installation account is not controlled by the authorizing user")
)

const githubUserAuthorizationTimeout = 30 * time.Second

type githubUserInstallation struct {
	ID      int64 `json:"id"`
	Account struct {
		ID        int64  `json:"id"`
		Login     string `json:"login"`
		Type      string `json:"type"`
		AvatarURL string `json:"avatar_url"`
	} `json:"account"`
}

type githubOAuthUser struct {
	ID    int64  `json:"id"`
	Login string `json:"login"`
}

func githubUserInstallationFromRow(row db.GithubInstallation) githubUserInstallation {
	installation := githubUserInstallation{ID: row.InstallationID}
	installation.Account.Login = row.AccountLogin
	installation.Account.Type = row.AccountType
	if row.AccountAvatarUrl.Valid {
		installation.Account.AvatarURL = row.AccountAvatarUrl.String
	}
	return installation
}

// workspaceGitHubInstallation returns only the exact workspace/installation
// pair. A database error is treated as absent so code-less callbacks fail
// closed and cannot manufacture a new binding.
func (h *Handler) workspaceGitHubInstallation(
	ctx context.Context,
	workspaceID pgtype.UUID,
	installationID int64,
) (db.GithubInstallation, bool) {
	rows, err := h.Queries.ListGitHubInstallationsByInstallationID(ctx, installationID)
	if err != nil {
		slog.Warn("github: existing installation lookup failed", "err", err, "installation_id", installationID)
		return db.GithubInstallation{}, false
	}
	for _, row := range rows {
		if row.WorkspaceID == workspaceID {
			return row, true
		}
	}
	return db.GithubInstallation{}, false
}

// verifyGitHubInstallationOwnership proves authority over the installation's
// account. Merely seeing an installation in /user/installations is not enough:
// an ordinary organization member can have that access. Personal installs
// require the OAuth user's exact id and login; organization installs require
// an active membership whose role is admin.
func verifyGitHubInstallationOwnership(
	ctx context.Context,
	code string,
	installationID int64,
) (githubUserInstallation, error) {
	clientID, clientSecret := githubAppClientID(), githubAppClientSecret()
	if clientID == "" || clientSecret == "" {
		return githubUserInstallation{}, errors.New("github: user authorization credentials are not configured")
	}
	if strings.TrimSpace(code) == "" {
		return githubUserInstallation{}, errGitHubUserAuthorizationMissing
	}

	ctx, cancel := context.WithTimeout(ctx, githubUserAuthorizationTimeout)
	defer cancel()
	client := &http.Client{Timeout: 15 * time.Second}
	userToken, err := exchangeGitHubUserCode(ctx, client, clientID, clientSecret, code)
	if err != nil {
		return githubUserInstallation{}, err
	}
	defer revokeGitHubUserToken(client, clientID, clientSecret, userToken)

	installation, err := fetchGitHubInstallationForBinding(ctx, client, installationID)
	if err != nil {
		return githubUserInstallation{}, err
	}
	if err := authorizeGitHubInstallationAccount(ctx, client, userToken, installation); err != nil {
		return githubUserInstallation{}, err
	}
	return installation, nil
}

func fetchGitHubInstallationForBinding(
	ctx context.Context,
	client *http.Client,
	installationID int64,
) (githubUserInstallation, error) {
	appJWT, err := signGitHubAppJWT(time.Now())
	if err != nil {
		return githubUserInstallation{}, fmt.Errorf("sign github App JWT: %w", err)
	}
	if appJWT == "" {
		return githubUserInstallation{}, errors.New("github: App credentials are not configured")
	}
	endpoint := fmt.Sprintf("%s/app/installations/%d", strings.TrimRight(githubAPIBase, "/"), installationID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return githubUserInstallation{}, err
	}
	setGitHubAPIHeaders(req, appJWT)
	resp, err := client.Do(req)
	if err != nil {
		return githubUserInstallation{}, fmt.Errorf("get github App installation: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, githubAPIResponseLimit))
		return githubUserInstallation{}, fmt.Errorf("get github App installation: github status %d", resp.StatusCode)
	}
	var installation githubUserInstallation
	if err := json.NewDecoder(io.LimitReader(resp.Body, githubAPIResponseLimit)).Decode(&installation); err != nil {
		return githubUserInstallation{}, fmt.Errorf("decode github App installation: %w", err)
	}
	if installation.ID != installationID || installation.Account.ID == 0 ||
		strings.TrimSpace(installation.Account.Login) == "" ||
		(installation.Account.Type != "User" && installation.Account.Type != "Organization") {
		return githubUserInstallation{}, errors.New("github returned an invalid App installation")
	}
	return installation, nil
}

func authorizeGitHubInstallationAccount(
	ctx context.Context,
	client *http.Client,
	userToken string,
	installation githubUserInstallation,
) error {
	user, err := fetchGitHubOAuthUser(ctx, client, userToken)
	if err != nil {
		return err
	}
	switch installation.Account.Type {
	case "User":
		if user.ID != installation.Account.ID || !strings.EqualFold(user.Login, installation.Account.Login) {
			return errGitHubInstallationNotAuthorized
		}
		return nil
	case "Organization":
		return requireGitHubOrganizationAdmin(ctx, client, userToken, installation.Account.Login)
	default:
		return errGitHubInstallationNotAuthorized
	}
}

func fetchGitHubOAuthUser(ctx context.Context, client *http.Client, userToken string) (githubOAuthUser, error) {
	endpoint := strings.TrimRight(githubAPIBase, "/") + "/user"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return githubOAuthUser{}, err
	}
	setGitHubAPIHeaders(req, userToken)
	resp, err := client.Do(req)
	if err != nil {
		return githubOAuthUser{}, fmt.Errorf("get github OAuth user: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, githubAPIResponseLimit))
		return githubOAuthUser{}, fmt.Errorf("get github OAuth user: github status %d", resp.StatusCode)
	}
	var user githubOAuthUser
	if err := json.NewDecoder(io.LimitReader(resp.Body, githubAPIResponseLimit)).Decode(&user); err != nil {
		return githubOAuthUser{}, fmt.Errorf("decode github OAuth user: %w", err)
	}
	if user.ID == 0 || strings.TrimSpace(user.Login) == "" {
		return githubOAuthUser{}, errors.New("github returned an invalid OAuth user")
	}
	return user, nil
}

func requireGitHubOrganizationAdmin(
	ctx context.Context,
	client *http.Client,
	userToken, organization string,
) error {
	endpoint := fmt.Sprintf(
		"%s/user/memberships/orgs/%s",
		strings.TrimRight(githubAPIBase, "/"),
		url.PathEscape(organization),
	)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	setGitHubAPIHeaders(req, userToken)
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("get github organization membership: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, githubAPIResponseLimit))
		return fmt.Errorf("get github organization membership: github status %d", resp.StatusCode)
	}
	var membership struct {
		State string `json:"state"`
		Role  string `json:"role"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, githubAPIResponseLimit)).Decode(&membership); err != nil {
		return fmt.Errorf("decode github organization membership: %w", err)
	}
	if membership.State != "active" || membership.Role != "admin" {
		return errGitHubInstallationNotAuthorized
	}
	return nil
}

func githubOwnershipErrorCode(err error) string {
	switch {
	case errors.Is(err, errGitHubInstallationNotAuthorized):
		return "installation_not_authorized"
	case errors.Is(err, errGitHubUserAuthorizationMissing):
		return "missing_authorization"
	default:
		return "verification_failed"
	}
}

func exchangeGitHubUserCode(
	ctx context.Context,
	client *http.Client,
	clientID, clientSecret, code string,
) (string, error) {
	form := url.Values{}
	form.Set("client_id", clientID)
	form.Set("client_secret", clientSecret)
	form.Set("code", code)
	endpoint := strings.TrimRight(githubOAuthBase, "/") + "/login/oauth/access_token"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("exchange github user code: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, githubAPIResponseLimit))
		return "", fmt.Errorf("exchange github user code: github status %d", resp.StatusCode)
	}
	var body struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, githubAPIResponseLimit)).Decode(&body); err != nil {
		return "", fmt.Errorf("decode github user token: %w", err)
	}
	if body.Error != "" {
		return "", fmt.Errorf("exchange github user code: %s", body.Error)
	}
	if body.AccessToken == "" {
		return "", errors.New("github returned an empty user access token")
	}
	return body.AccessToken, nil
}

func revokeGitHubUserToken(client *http.Client, clientID, clientSecret, token string) {
	if token == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	endpoint := fmt.Sprintf(
		"%s/applications/%s/token",
		strings.TrimRight(githubAPIBase, "/"),
		url.PathEscape(clientID),
	)
	payload, err := json.Marshal(map[string]string{"access_token": token})
	if err != nil {
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, endpoint, bytes.NewReader(payload))
	if err != nil {
		return
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("Content-Type", "application/json")
	req.SetBasicAuth(clientID, clientSecret)
	resp, err := client.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, githubAPIResponseLimit))
}

// fetchInstallationAccount tries to enrich the installation row with the
// account name + avatar from GitHub.
//
// GitHub's `GET /app/installations/{id}` endpoint requires GitHub App
// authentication (a JWT signed with the App's RSA private key). When the
// operator has configured GITHUB_APP_ID and GITHUB_APP_PRIVATE_KEY, we
// sign a short-lived JWT and use it; on any failure (env not set, key
// malformed, GitHub returns non-200) we fall back to the "unknown"
// placeholder. The next `installation` webhook delivery from GitHub will
// upsert the row with the real account info — see handleInstallationEvent.
//
// The HTTP call is synchronous (no independent timeout — that's a pre-
// existing wart of the install path), but we deliberately do NOT let a
// failure abort the setup callback: a network blip here just leaves the
// "unknown" placeholder in place, and the frontend re-queries on the
// realtime broadcast emitted by the webhook handler, so the UI converges
// without a manual refresh.
func fetchInstallationAccount(ctx context.Context, installationID int64) (login, accountType string, avatar *string) {
	login = "unknown"
	accountType = "User"
	avatar = nil
	endpoint := fmt.Sprintf("%s/app/installations/%d", strings.TrimRight(githubAPIBase, "/"), installationID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	if token, err := signGitHubAppJWT(time.Now()); err != nil {
		// Misconfigured private key is operator-actionable — log so the
		// install path doesn't silently fall back to "unknown" forever
		// without leaving a breadcrumb.
		slog.Warn("github: sign App JWT failed", "err", err)
	} else if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return
	}
	var body struct {
		Account struct {
			Login     string `json:"login"`
			Type      string `json:"type"`
			AvatarURL string `json:"avatar_url"`
		} `json:"account"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return
	}
	if body.Account.Login != "" {
		login = body.Account.Login
	}
	if body.Account.Type != "" {
		accountType = body.Account.Type
	}
	if body.Account.AvatarURL != "" {
		v := body.Account.AvatarURL
		avatar = &v
	}
	return
}

// signGitHubAppJWT mints the short-lived RS256 JWT GitHub requires for
// App-authenticated REST calls (see fetchInstallationAccount). Returns
// ("", nil) when the operator hasn't configured the App identity — that's
// a soft "App auth not available" signal, not an error, so callers can
// fall through to their unauthenticated path. A malformed
// GITHUB_APP_PRIVATE_KEY surfaces as an error so the operator notices.
//
// `now` is injected for deterministic tests; production callers pass
// time.Now().
func signGitHubAppJWT(now time.Time) (string, error) {
	appID := strings.TrimSpace(os.Getenv("GITHUB_APP_ID"))
	pemKey := strings.TrimSpace(os.Getenv("GITHUB_APP_PRIVATE_KEY"))
	if appID == "" || pemKey == "" {
		return "", nil
	}
	key, err := jwt.ParseRSAPrivateKeyFromPEM([]byte(pemKey))
	if err != nil {
		return "", fmt.Errorf("parse GITHUB_APP_PRIVATE_KEY: %w", err)
	}
	// GitHub allows JWTs valid for up to 10 minutes. We back-date `iat`
	// by 60 seconds to absorb modest clock skew between us and GitHub
	// (otherwise an "iat in the future" verdict from GitHub fails the
	// request) and cap `exp` at 9 minutes ahead to stay inside the cap
	// even with the same skew applied.
	claims := jwt.MapClaims{
		"iat": now.Add(-60 * time.Second).Unix(),
		"exp": now.Add(9 * time.Minute).Unix(),
		"iss": appID,
	}
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	signed, err := token.SignedString(key)
	if err != nil {
		return "", fmt.Errorf("sign App JWT: %w", err)
	}
	return signed, nil
}

// ── Listing / disconnect ────────────────────────────────────────────────────

// ListGitHubInstallations returns the workspace's connected GitHub
// installations to any workspace member. Connect/disconnect remain
// admin-only at the router level, so the response carries a `can_manage`
// hint and strips the numeric `installation_id` for non-admin callers —
// they get visibility into "is GitHub wired up, and by whom?" without the
// management handle.
func (h *Handler) ListGitHubInstallations(w http.ResponseWriter, r *http.Request) {
	workspaceID := chi.URLParam(r, "id")
	wsUUID, ok := parseUUIDOrBadRequest(w, workspaceID, "workspace id")
	if !ok {
		return
	}
	member, _ := middleware.MemberFromContext(r.Context())
	canManage := roleAllowed(member.Role, "owner", "admin")

	rows, err := h.Queries.ListGitHubInstallationsByWorkspace(r.Context(), wsUUID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list installations")
		return
	}
	out := make([]GitHubInstallationResponse, 0, len(rows))
	for _, row := range rows {
		resp := githubInstallationToResponse(row)
		if !canManage {
			resp.InstallationID = nil
		}
		out = append(out, resp)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"installations":                out,
		"configured":                   isGitHubConfigured(),
		"repository_browse_configured": isGitHubRepositoryBrowseConfigured(),
		"can_manage":                   canManage,
	})
}

// ListGitHubInstallationRepositories returns the repositories accessible to a
// workspace-bound GitHub App installation. The route is admin-only because
// private repository names are sensitive. The path takes our installation row
// UUID (not GitHub's numeric installation id), and the workspace ownership
// check happens before any GitHub API call.
func (h *Handler) ListGitHubInstallationRepositories(w http.ResponseWriter, r *http.Request) {
	workspaceID := chi.URLParam(r, "id")
	if _, ok := parseUUIDOrBadRequest(w, workspaceID, "workspace id"); !ok {
		return
	}
	installationRowID := chi.URLParam(r, "installationId")
	rowUUID, ok := parseUUIDOrBadRequest(w, installationRowID, "installation id")
	if !ok {
		return
	}
	row, err := h.Queries.GetGitHubInstallationByID(r.Context(), rowUUID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "github installation not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to load github installation")
		return
	}
	if uuidToString(row.WorkspaceID) != workspaceID {
		writeError(w, http.StatusNotFound, "github installation not found")
		return
	}
	if !isGitHubRepositoryBrowseConfigured() {
		writeFeatureDisabled(w, "github_repository_browsing_not_configured", "github repository browsing is not configured")
		return
	}
	page, ok := parseGitHubPageParam(w, r, "page", 1, 1, 100000)
	if !ok {
		return
	}
	perPage, ok := parseGitHubPageParam(w, r, "per_page", 100, 1, 100)
	if !ok {
		return
	}

	repositories, err := fetchGitHubInstallationRepositories(
		r.Context(),
		row.InstallationID,
		page,
		perPage,
	)
	if err != nil {
		slog.Warn("github: list installation repositories failed", "err", err)
		writeError(w, http.StatusBadGateway, "failed to list github repositories")
		return
	}
	writeJSON(w, http.StatusOK, repositories)
}

func parseGitHubPageParam(
	w http.ResponseWriter,
	r *http.Request,
	name string,
	defaultValue, minValue, maxValue int,
) (int, bool) {
	raw := strings.TrimSpace(r.URL.Query().Get(name))
	if raw == "" {
		return defaultValue, true
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < minValue || value > maxValue {
		writeError(w, http.StatusBadRequest, "invalid "+name)
		return 0, false
	}
	return value, true
}

func fetchGitHubInstallationRepositories(
	ctx context.Context,
	installationID int64,
	page, perPage int,
) (GitHubRepositoriesResponse, error) {
	appJWT, err := signGitHubAppJWT(time.Now())
	if err != nil {
		return GitHubRepositoriesResponse{}, err
	}
	if appJWT == "" {
		return GitHubRepositoriesResponse{}, errors.New("github App JWT credentials unavailable")
	}

	client := &http.Client{Timeout: 15 * time.Second}
	tokenEndpoint := fmt.Sprintf(
		"%s/app/installations/%d/access_tokens",
		strings.TrimRight(githubAPIBase, "/"),
		installationID,
	)
	tokenReq, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		tokenEndpoint,
		strings.NewReader(`{"permissions":{"metadata":"read"}}`),
	)
	if err != nil {
		return GitHubRepositoriesResponse{}, err
	}
	setGitHubAPIHeaders(tokenReq, appJWT)
	tokenReq.Header.Set("Content-Type", "application/json")
	tokenResp, err := client.Do(tokenReq)
	if err != nil {
		return GitHubRepositoriesResponse{}, fmt.Errorf("create installation token: %w", err)
	}
	defer tokenResp.Body.Close()
	if tokenResp.StatusCode != http.StatusCreated {
		_, _ = io.Copy(io.Discard, io.LimitReader(tokenResp.Body, githubAPIResponseLimit))
		return GitHubRepositoriesResponse{}, fmt.Errorf("create installation token: github status %d", tokenResp.StatusCode)
	}
	var tokenBody struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(io.LimitReader(tokenResp.Body, githubAPIResponseLimit)).Decode(&tokenBody); err != nil {
		return GitHubRepositoriesResponse{}, fmt.Errorf("decode installation token: %w", err)
	}
	if tokenBody.Token == "" {
		return GitHubRepositoriesResponse{}, errors.New("github returned an empty installation token")
	}
	defer revokeGitHubInstallationToken(client, tokenBody.Token)

	repositoriesEndpoint := fmt.Sprintf(
		"%s/installation/repositories?page=%d&per_page=%d",
		strings.TrimRight(githubAPIBase, "/"),
		page,
		perPage,
	)
	repositoriesReq, err := http.NewRequestWithContext(ctx, http.MethodGet, repositoriesEndpoint, nil)
	if err != nil {
		return GitHubRepositoriesResponse{}, err
	}
	setGitHubAPIHeaders(repositoriesReq, tokenBody.Token)
	repositoriesResp, err := client.Do(repositoriesReq)
	if err != nil {
		return GitHubRepositoriesResponse{}, fmt.Errorf("list installation repositories: %w", err)
	}
	defer repositoriesResp.Body.Close()
	if repositoriesResp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(repositoriesResp.Body, githubAPIResponseLimit))
		return GitHubRepositoriesResponse{}, fmt.Errorf("list installation repositories: github status %d", repositoriesResp.StatusCode)
	}
	var body struct {
		TotalCount   int64 `json:"total_count"`
		Repositories []struct {
			ID            int64   `json:"id"`
			FullName      string  `json:"full_name"`
			HTMLURL       string  `json:"html_url"`
			CloneURL      string  `json:"clone_url"`
			Description   *string `json:"description"`
			Private       bool    `json:"private"`
			Archived      bool    `json:"archived"`
			DefaultBranch string  `json:"default_branch"`
		} `json:"repositories"`
	}
	if err := json.NewDecoder(io.LimitReader(repositoriesResp.Body, githubAPIResponseLimit)).Decode(&body); err != nil {
		return GitHubRepositoriesResponse{}, fmt.Errorf("decode installation repositories: %w", err)
	}
	out := GitHubRepositoriesResponse{
		Repositories: make([]GitHubRepositoryResponse, 0, len(body.Repositories)),
		TotalCount:   body.TotalCount,
	}
	for _, repository := range body.Repositories {
		out.Repositories = append(out.Repositories, GitHubRepositoryResponse(repository))
	}
	if int64(page*perPage) < body.TotalCount {
		nextPage := page + 1
		out.NextPage = &nextPage
	}
	return out, nil
}

func setGitHubAPIHeaders(req *http.Request, token string) {
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
}

func revokeGitHubInstallationToken(client *http.Client, token string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	endpoint := strings.TrimRight(githubAPIBase, "/") + "/installation/token"
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, endpoint, nil)
	if err != nil {
		return
	}
	setGitHubAPIHeaders(req, token)
	resp, err := client.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, githubAPIResponseLimit))
}

func (h *Handler) DeleteGitHubInstallation(w http.ResponseWriter, r *http.Request) {
	workspaceID := chi.URLParam(r, "id")
	wsUUID, ok := parseUUIDOrBadRequest(w, workspaceID, "workspace id")
	if !ok {
		return
	}
	id := chi.URLParam(r, "installationId")
	idUUID, ok := parseUUIDOrBadRequest(w, id, "installation id")
	if !ok {
		return
	}
	if err := h.Queries.DeleteGitHubInstallation(r.Context(), db.DeleteGitHubInstallationParams{
		ID:          idUUID,
		WorkspaceID: wsUUID,
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to remove installation")
		return
	}
	h.publish(protocol.EventGitHubInstallationDeleted, workspaceID, "system", "", map[string]any{
		"id": id,
	})
	w.WriteHeader(http.StatusNoContent)
}

// ── List PRs for an issue ───────────────────────────────────────────────────

func (h *Handler) ListPullRequestsForIssue(w http.ResponseWriter, r *http.Request) {
	issue, ok := h.loadIssueForUser(w, r, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	ws, err := h.Queries.GetWorkspace(r.Context(), issue.WorkspaceID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list pull requests")
		return
	}
	identifier := fmt.Sprintf("%s-%d", issuePrefixForWorkspace(ws), issue.Number)
	rows, err := h.Queries.ListPullRequestsByIssue(r.Context(), issue.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list pull requests")
		return
	}
	out := make([]GitHubPullRequestResponse, 0, len(rows))
	for _, row := range rows {
		resp := issuePullRequestRowToResponse(row, h.PRRefresh.Enabled())
		resp.LinkSource = prLinkSource(row.LinkedByType, identifier, row.Title, row.Branch.String)
		out = append(out, resp)
		// Page-visit trigger (MUL-5265): if this card's snapshot is missing or
		// older than the view TTL, kick an async refresh. Non-blocking — the
		// current (possibly stale) response is returned immediately and the
		// fresh snapshot arrives via the pull_request:updated realtime event.
		h.PRRefresh.MaybeEnqueueOnView(
			row.InstallationID, row.RepoOwner, row.RepoName, row.PrNumber,
			row.SnapshotFetchedAt.Time,
			row.SnapshotFetchedAt.Valid &&
				row.SnapshotHeadSha != "" &&
				row.SnapshotHeadSha == row.HeadSha,
		)
	}
	// PRs from token-based providers (Forgejo / Gitea / GitLab) share the same
	// card list. They live in their own provider-tagged tables, so they merge
	// in here mapped to the same response shape; the combined list is re-sorted
	// newest-first.
	vcsRows, err := h.Queries.ListVCSPullRequestsByIssue(r.Context(), issue.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list pull requests")
		return
	}
	for _, row := range vcsRows {
		resp := vcsPullRequestRowToResponse(row)
		resp.LinkSource = prLinkSource(row.LinkedByType, identifier, row.Title, row.Branch.String)
		out = append(out, resp)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].PRCreatedAt > out[j].PRCreatedAt
	})
	// The one auto-complete decision the issue page renders, so the UI never
	// re-derives the rule on its own.
	decision, err := h.decidePRAutoComplete(r.Context(), ws, issue, nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list pull requests")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"pull_requests": out,
		"auto_complete": prAutoCompleteToResponse(decision),
	})
}

// broadcastPRSnapshotApplied is the ghsnapshot pipeline's onApplied callback:
// once an API snapshot is written to a PR row, re-broadcast the PR so every
// open issue detail page re-queries its PR list and picks up the fresh CI /
// mergeability state. Runs on a background pipeline goroutine.
func (h *Handler) broadcastPRSnapshotApplied(ctx context.Context, prID pgtype.UUID) {
	pr, err := h.Queries.GetGitHubPullRequestByID(ctx, prID)
	if err != nil {
		return
	}
	issueIDs, err := h.Queries.ListIssueIDsForPullRequest(ctx, prID)
	if err != nil {
		return
	}
	linked := make([]string, 0, len(issueIDs))
	for _, id := range issueIDs {
		linked = append(linked, uuidToString(id))
	}
	if pr.SnapshotHeadSha != "" && pr.SnapshotHeadSha == pr.HeadSha && pr.ChecksRollupState.Valid &&
		(pr.ChecksRollupState.String == "FAILURE" || pr.ChecksRollupState.String == "ERROR") {
		wakeup := service.IssueWakeupService{Tasks: h.TaskService}
		for _, issueID := range issueIDs {
			if err := wakeup.TriggerPullRequestWakeup(ctx, issueID, service.PullRequestWakeupInput{
				Rule: service.SystemRulePRChecksFailed, RepoOwner: pr.RepoOwner, RepoName: pr.RepoName,
				Number: pr.PrNumber, URL: pr.HtmlUrl, HeadSHA: pr.SnapshotHeadSha, Conclusion: pr.ChecksRollupState.String,
				// The mirror keeps the head branch only; a base-branch filter
				// never matches a failing-checks event until it is stored.
				HeadBranch: pr.Branch.String,
			}); err != nil {
				slog.Warn("github: failed to dispatch pull request check wakeup", "err", err, "pr_id", uuidToString(pr.ID), "issue_id", uuidToString(issueID))
			}
		}
	}
	h.publish(protocol.EventPullRequestUpdated, uuidToString(pr.WorkspaceID), "system", "", map[string]any{
		"pull_request":     githubPullRequestToResponse(pr, h.PRRefresh.Enabled()),
		"linked_issue_ids": linked,
	})
}

// ── Webhook ─────────────────────────────────────────────────────────────────

// identifierRe extracts identifiers like "MUL-1510" from text. Case-insensitive
// because branch names are conventionally lowercase but issue prefixes are
// uppercase. Word boundary on the left prevents matching inside email-style
// strings (e.g. "abc@MUL-1") and the digit anchor on the right rules out
// version numbers like "v1.2-3".
var identifierRe = regexp.MustCompile(`(?i)\b([a-z][a-z0-9]{0,9})-(\d+)\b`)

// closingIdentifierRe extracts identifiers that appear immediately after a
// GitHub-style closing keyword ("close[sd]?", "fix(e[sd])?", "resolve[sd]?"),
// optionally separated by a colon and whitespace. Matching is intentionally
// strict on adjacency — "Fix MUL-1" names MUL-1, but "Fix login MUL-1" does
// not. It is the one way a PR body links an issue; a bare body mention links
// nothing. Since MUL-7726 the keyword only links: what a merge does is the
// workspace's choice, not the PR text's.
var closingIdentifierRe = regexp.MustCompile(
	`(?i)\b(?:close[sd]?|fix(?:e[sd])?|resolve[sd]?)[:\s]+([a-z][a-z0-9]{0,9})-(\d+)\b`,
)

// HandleGitHubWebhook (POST /api/webhooks/github) is GitHub's destination for
// every event from a connected installation. We verify HMAC signature, route
// on X-GitHub-Event, and either upsert PR rows + auto-link to issues or
// remove the installation on uninstall.
func (h *Handler) HandleGitHubWebhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 10<<20)) // 10 MiB cap
	if err != nil {
		writeError(w, http.StatusBadRequest, "read body failed")
		return
	}
	secret := githubWebhookSecret()
	if secret == "" {
		// Refusing to process webhooks at all is safer than treating an
		// unconfigured deployment as "all signatures valid".
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	sigHeader := r.Header.Get("X-Hub-Signature-256")
	if !verifyWebhookSignature(secret, sigHeader, body) {
		writeError(w, http.StatusUnauthorized, "invalid signature")
		return
	}
	event := r.Header.Get("X-GitHub-Event")
	deliveryGUID := r.Header.Get("X-GitHub-Delivery")
	ctx := r.Context()
	switch event {
	case "ping":
		writeJSON(w, http.StatusOK, map[string]string{"ok": "pong"})
		return
	case "installation":
		h.handleInstallationEvent(ctx, body)
	case "pull_request":
		if err := h.handlePullRequestEvent(ctx, body); err != nil {
			// Returning 5xx makes GitHub redeliver when mirroring or link writes fail.
			slog.Error("github: pull_request event failed", "err", err, "delivery_guid", deliveryGUID)
			writeError(w, http.StatusInternalServerError, "failed to process pull_request event")
			return
		}
	case "check_suite", "check_run", "status":
		// CI events are pure triggers under Plan C (MUL-5265): their payload is
		// never read for display. Each just asks the API pipeline to re-fetch
		// the authoritative snapshot for the PR(s) it concerns.
		h.triggerPRRefreshFromCIEvent(ctx, body)
	default:
		// Acknowledge every event so GitHub doesn't mark the endpoint failing,
		// but ignore types we don't model.
	}
	w.WriteHeader(http.StatusAccepted)
}

func verifyWebhookSignature(secret, header string, body []byte) bool {
	const prefix = "sha256="
	if !strings.HasPrefix(header, prefix) {
		return false
	}
	want, err := hex.DecodeString(strings.TrimPrefix(header, prefix))
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hmac.Equal(mac.Sum(nil), want)
}

type ghInstallationPayload struct {
	Action       string `json:"action"`
	Installation struct {
		ID      int64 `json:"id"`
		Account struct {
			Login     string `json:"login"`
			Type      string `json:"type"`
			AvatarURL string `json:"avatar_url"`
		} `json:"account"`
	} `json:"installation"`
}

func githubInstallationAccountFromPayload(p ghInstallationPayload) (login, accountType string, avatar *string, ok bool) {
	login = strings.TrimSpace(p.Installation.Account.Login)
	if login == "" {
		return "", "", nil, false
	}
	accountType = coalesce(p.Installation.Account.Type, "User")
	avatar = strPtrOrNil(p.Installation.Account.AvatarURL)
	return login, accountType, avatar, true
}

func (h *Handler) handleInstallationEvent(ctx context.Context, body []byte) {
	var p ghInstallationPayload
	if err := json.Unmarshal(body, &p); err != nil {
		slog.Warn("github: bad installation payload", "err", err)
		return
	}
	switch p.Action {
	case "deleted", "suspend":
		// User removed/suspended the App on GitHub — trust in this
		// installation_id is gone entirely, so drop every workspace binding.
		// We DELETE … RETURNING so each broadcast can be scoped to its
		// workspace; events without WorkspaceID are dropped by the realtime
		// listener and would leave already-open Settings tabs stale.
		deleted, err := h.Queries.DeleteGitHubInstallationByInstallationID(ctx, p.Installation.ID)
		if err != nil {
			slog.Warn("github: delete installation failed", "err", err, "installation_id", p.Installation.ID)
			return
		}
		if err := h.Queries.DeletePendingGitHubInstallation(ctx, p.Installation.ID); err != nil {
			slog.Warn("github: delete pending installation failed", "err", err, "installation_id", p.Installation.ID)
		}
		// Broadcast the internal row id only — the numeric installation_id is
		// a management handle that non-admin members are not allowed to see.
		// The frontend invalidates the installations query on this event and
		// does not read the broadcast payload directly. One broadcast per
		// deleted binding so every affected workspace's Settings tab refreshes.
		for _, row := range deleted {
			h.publish(protocol.EventGitHubInstallationDeleted, uuidToString(row.WorkspaceID), "system", "", map[string]any{
				"id": uuidToString(row.ID),
			})
		}
	case "created", "new_permissions_accepted", "unsuspend":
		login, accountType, avatar, ok := githubInstallationAccountFromPayload(p)
		if !ok {
			slog.Warn("github: installation payload missing account login", "installation_id", p.Installation.ID)
			return
		}

		// We don't know which workspace(s) this maps to from the webhook
		// alone. If no setup callback has created a workspace binding yet,
		// keep the account metadata and let the callback consume it after it
		// creates github_installation.
		existing, err := h.Queries.ListGitHubInstallationsByInstallationID(ctx, p.Installation.ID)
		if err != nil {
			slog.Warn("github: lookup installation failed", "err", err, "installation_id", p.Installation.ID)
			return
		}
		if len(existing) == 0 {
			if _, err := h.Queries.UpsertPendingGitHubInstallation(ctx, db.UpsertPendingGitHubInstallationParams{
				InstallationID:   p.Installation.ID,
				AccountLogin:     login,
				AccountType:      accountType,
				AccountAvatarUrl: ptrToText(avatar),
			}); err != nil {
				slog.Warn("github: store pending installation failed", "err", err, "installation_id", p.Installation.ID)
			}
			return
		}
		// Refresh the account display metadata across every workspace binding;
		// workspace_id and connected_by_id are left untouched.
		refreshed, err := h.Queries.UpdateGitHubInstallationAccountByInstallationID(ctx, db.UpdateGitHubInstallationAccountByInstallationIDParams{
			InstallationID:   p.Installation.ID,
			AccountLogin:     login,
			AccountType:      accountType,
			AccountAvatarUrl: ptrToText(avatar),
		})
		if err != nil {
			slog.Warn("github: refresh installation failed", "err", err)
			return
		}
		if err := h.Queries.DeletePendingGitHubInstallation(ctx, p.Installation.ID); err != nil {
			slog.Warn("github: delete pending installation failed", "err", err, "installation_id", p.Installation.ID)
		}
		// Broadcast so any open Settings → GitHub tab re-queries the
		// installations list. Without this, a row created by the setup
		// callback with the "unknown" placeholder (e.g. because GitHub
		// App JWT auth wasn't configured, or this webhook arrived after
		// the user already loaded the page) would stay visibly stale
		// until the user manually refreshes. One broadcast per bound workspace.
		for _, inst := range refreshed {
			h.publish(protocol.EventGitHubInstallationCreated, uuidToString(inst.WorkspaceID), "system", "", map[string]any{
				"installation": githubInstallationToBroadcast(inst),
			})
		}
	}
}

type ghPullRequestPayload struct {
	Action      string `json:"action"`
	PullRequest struct {
		Number         int32  `json:"number"`
		HTMLURL        string `json:"html_url"`
		Title          string `json:"title"`
		Body           string `json:"body"`
		State          string `json:"state"`
		Draft          bool   `json:"draft"`
		Merged         bool   `json:"merged"`
		MergedAt       string `json:"merged_at"`
		ClosedAt       string `json:"closed_at"`
		CreatedAt      string `json:"created_at"`
		UpdatedAt      string `json:"updated_at"`
		MergeableState string `json:"mergeable_state"`
		Additions      int32  `json:"additions"`
		Deletions      int32  `json:"deletions"`
		ChangedFiles   int32  `json:"changed_files"`
		MergeCommitSHA string `json:"merge_commit_sha"`
		Head           struct {
			Ref string `json:"ref"`
			SHA string `json:"sha"`
		} `json:"head"`
		Base struct {
			Ref string `json:"ref"`
		} `json:"base"`
		User struct {
			Login     string `json:"login"`
			AvatarURL string `json:"avatar_url"`
		} `json:"user"`
	} `json:"pull_request"`
	Changes    *ghPRChanges `json:"changes"`
	Repository struct {
		ID    int64  `json:"id"`
		Name  string `json:"name"`
		Owner struct {
			Login string `json:"login"`
		} `json:"owner"`
	} `json:"repository"`
	Installation struct {
		ID int64 `json:"id"`
	} `json:"installation"`
}

func (h *Handler) handlePullRequestEvent(ctx context.Context, body []byte) error {
	var p ghPullRequestPayload
	if err := json.Unmarshal(body, &p); err != nil {
		// A malformed payload is not something redelivery would fix, so this
		// is intentionally not surfaced as a failure to the caller (which
		// would make HandleGitHubWebhook ask GitHub to redeliver the same
		// bad body forever) — log and drop, same as before this fix.
		slog.Warn("github: bad pull_request payload", "err", err)
		return nil
	}
	if p.Installation.ID == 0 {
		return nil
	}
	insts, err := h.Queries.ListGitHubInstallationsByInstallationID(ctx, p.Installation.ID)
	if err != nil {
		// CHE-374 review round 2, item 5: a real DB error here (as opposed to
		// "no installation row exists", which is len(insts)==0 below and stays
		// a silent drop) is transient and indistinguishable from a fan-out
		// write failure elsewhere in this function — those already return the
		// error so the caller surfaces 5xx and GitHub redelivers (see the T1
		// comment at the call site). Warn-and-swallow here used to make a
		// blip in this one lookup silently drop the whole event with a 202,
		// which is exactly the outcome T1 exists to prevent for every other
		// failure in this path.
		return fmt.Errorf("list installations by installation id: %w", err)
	}
	if len(insts) == 0 {
		// Webhook from an installation we never wired up — nothing we
		// can attribute to a workspace, so drop it silently.
		return nil
	}
	// #4855 lets one GitHub App installation bind to several workspaces. A
	// repo's events belong to every bound workspace, so fan the delivery out:
	// each workspace independently mirrors the PR and auto-links it against its
	// own issues (its own prefix + github toggle). Repo scope is whatever GitHub
	// authorized the installation for; we deliberately don't gate on the
	// workspace.repos registry — that list is "code the agent clones", not a
	// webhook subscription (MUL-4343).
	//
	// Fanning out means an identifier can resolve in more than one workspace at
	// once (issue prefixes are not globally unique and issue numbers restart at
	// 1 per workspace, so two bound workspaces can both own a real "ABC-100").
	// Settle which workspace may link each claimed identifier before writing any
	// workspace links, and treat that verdict as authoritative for the delivery.
	closePolicy := h.resolvePRLinkPolicy(ctx, insts, &p)
	// Fan out to every bound workspace and keep going on a per-workspace
	// failure — one workspace's transaction failing must not stop another
	// bound workspace's mirror from being attempted. All failures are
	// aggregated and returned so the caller can still ask GitHub to
	// redeliver the whole event (CHE-374 review fix T1); a partial success
	// is safe to redeliver because every write below is upsert/ON CONFLICT
	// idempotent.
	var errs []error
	for _, inst := range insts {
		if err := h.mirrorPullRequestForWorkspace(ctx, inst.WorkspaceID, inst.InstallationID, &p, closePolicy); err != nil {
			errs = append(errs, fmt.Errorf("workspace %s: %w", uuidToString(inst.WorkspaceID), err))
		}
	}
	// The PR row(s) now carry the new head; ask the API pipeline for the
	// authoritative CI + mergeability snapshot for that head. The webhook is
	// only the doorbell — its own mergeable/checks payload is not used for
	// display anymore (MUL-5265).
	h.PRRefresh.Enqueue(p.Installation.ID, p.Repository.Owner.Login, p.Repository.Name, p.PullRequest.Number)
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}

// prLinkPolicy decides which (identifier, workspace) pairs this delivery may
// link, and is the single authority for that decision: the per-workspace mirror
// pass consults it instead of re-deriving the answer from its own reads.
//
// A link is a claim that the PR delivers the issue. So when one installation
// is bound to several workspaces, an identifier that resolves in more than one
// of them must not link anywhere — otherwise merging the PR could change the
// status of an issue in a workspace that has nothing to do with it (#6804). A
// member can still link the PR by hand.
type prLinkPolicy struct {
	// unrestricted marks the single-binding case: cross-workspace ambiguity is
	// impossible with one bound workspace, so the scan does no reads at all.
	unrestricted bool
	// indeterminate means a read failed, so we cannot tell ambiguous from
	// unique. The mirror pass then leaves links and statuses untouched for this
	// delivery rather than guessing either way.
	indeterminate bool
	// owner maps an identifier to the one auto-linking workspace proven to
	// resolve it.
	owner map[string]string
	// ambiguous lists identifiers that resolved in more than one auto-linking
	// workspace.
	ambiguous map[string]bool
}

// permits reports whether workspaceID may link identifier.
func (c prLinkPolicy) permits(identifier, workspaceID string) bool {
	if c.unrestricted {
		return true
	}
	owner, ok := c.owner[identifier]
	return ok && owner == workspaceID
}

// resolvePRLinkPolicy determines, before any workspace writes, which claimed
// identifiers on this PR may link, and in which workspace. An identifier is
// allowed only when exactly one auto-linking bound workspace was proven to
// resolve it; misjudging "unique" as "ambiguous" costs a link a person can add
// by hand, while the reverse would move someone else's issue on merge.
func (h *Handler) resolvePRLinkPolicy(ctx context.Context, insts []db.GithubInstallation, p *ghPullRequestPayload) prLinkPolicy {
	if len(insts) < 2 {
		return prLinkPolicy{unrestricted: true}
	}
	idents := prClaimedIdentifiers(p.PullRequest.Title, p.PullRequest.Body, p.PullRequest.Head.Ref)
	policy := prLinkPolicy{owner: map[string]string{}, ambiguous: map[string]bool{}}
	if len(idents) == 0 {
		return policy
	}
	prNumber := p.PullRequest.Number
	repo := p.Repository.Owner.Login + "/" + p.Repository.Name
	indeterminate := func(msg string, args ...any) prLinkPolicy {
		slog.Warn(msg, append(args, "installation_id", p.Installation.ID, "repo", repo, "pr_number", prNumber)...)
		return prLinkPolicy{indeterminate: true}
	}

	type resolver struct {
		workspaceID string
		autoLink    bool
	}
	resolvers := make(map[string][]resolver, len(idents))
	for _, inst := range insts {
		ws, err := h.Queries.GetWorkspace(ctx, inst.WorkspaceID)
		if err != nil {
			return indeterminate("github: cannot load bound workspace, leaving links unchanged for this delivery", "err", err)
		}
		// A settings blob we cannot parse is not evidence either way, so it
		// fails closed.
		autoLink, err := autoLinkPRsEnabledForWorkspace(ws)
		if err != nil {
			return indeterminate("github: cannot read workspace auto-link setting, leaving links unchanged for this delivery",
				"err", err, "workspace_id", uuidToString(inst.WorkspaceID))
		}
		// With GitHub off a workspace acts on nothing, so it claims nothing.
		if !githubFeaturesEnabled(ws) {
			continue
		}
		prefix := issuePrefixForWorkspace(ws)
		for _, id := range idents {
			number, ok := issueNumberForPrefix(id, prefix)
			if !ok {
				continue
			}
			if _, err := h.Queries.GetIssueByNumber(ctx, db.GetIssueByNumberParams{
				WorkspaceID: inst.WorkspaceID,
				Number:      number,
			}); err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					continue
				}
				return indeterminate("github: cannot resolve identifier in bound workspace, leaving links unchanged for this delivery",
					"err", err, "identifier", id, "workspace_id", uuidToString(inst.WorkspaceID))
			}
			resolvers[id] = append(resolvers[id], resolver{workspaceID: uuidToString(inst.WorkspaceID), autoLink: autoLink})
		}
	}
	for id, all := range resolvers {
		// A workspace with auto-link off never writes a link row, so it is not
		// a competing claimant for the link.
		var wss []string
		for _, r := range all {
			if r.autoLink {
				wss = append(wss, r.workspaceID)
			}
		}
		if len(wss) == 0 {
			continue
		}
		if len(wss) == 1 {
			policy.owner[id] = wss[0]
			continue
		}
		policy.ambiguous[id] = true
		// An issue that never auto-links is otherwise untraceable back to
		// GitHub, so leave a breadcrumb naming the collision.
		slog.Warn("github: identifier resolves in several bound workspaces, not auto-linking it",
			"identifier", id, "workspaces", len(wss),
			"installation_id", p.Installation.ID, "repo", repo, "pr_number", prNumber)
	}
	return policy
}

// ghCIEventPayload captures the shared shape of the check_suite / check_run /
// status webhooks — enough to resolve which PR to refresh. These events are
// pure triggers under Plan C: their payload data is never read for display.
type ghCIEventPayload struct {
	Installation struct {
		ID int64 `json:"id"`
	} `json:"installation"`
	Repository struct {
		Name  string `json:"name"`
		Owner struct {
			Login string `json:"login"`
		} `json:"owner"`
	} `json:"repository"`
	// status events: top-level commit SHA, no PR number.
	SHA        string `json:"sha"`
	CheckSuite struct {
		HeadSHA      string `json:"head_sha"`
		PullRequests []struct {
			Number int32 `json:"number"`
		} `json:"pull_requests"`
	} `json:"check_suite"`
	CheckRun struct {
		PullRequests []struct {
			Number int32 `json:"number"`
		} `json:"pull_requests"`
		CheckSuite struct {
			HeadSHA string `json:"head_sha"`
		} `json:"check_suite"`
	} `json:"check_run"`
}

// triggerPRRefreshFromCIEvent enqueues an API refresh for the PR(s) a
// check_suite / check_run / status webhook concerns. check_suite/check_run
// carry the PR numbers directly; status events carry only a commit SHA, so we
// map it back to the mirrored head_sha to find the PR(s).
func (h *Handler) triggerPRRefreshFromCIEvent(ctx context.Context, body []byte) {
	if !h.PRRefresh.Enabled() {
		return
	}
	var p ghCIEventPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return
	}
	if p.Installation.ID == 0 || p.Repository.Name == "" {
		return
	}
	owner, repo := p.Repository.Owner.Login, p.Repository.Name

	seen := map[int32]struct{}{}
	enqueue := func(number int32) {
		if number == 0 {
			return
		}
		if _, ok := seen[number]; ok {
			return
		}
		seen[number] = struct{}{}
		h.PRRefresh.Enqueue(p.Installation.ID, owner, repo, number)
	}
	for _, pr := range p.CheckSuite.PullRequests {
		enqueue(pr.Number)
	}
	for _, pr := range p.CheckRun.PullRequests {
		enqueue(pr.Number)
	}
	if len(seen) > 0 {
		return
	}
	// No PR number in the payload (status event, or a check event whose
	// pull_requests array was empty) — resolve by head SHA.
	sha := p.SHA
	if sha == "" {
		sha = coalesce(p.CheckSuite.HeadSHA, p.CheckRun.CheckSuite.HeadSHA)
	}
	if sha == "" {
		return
	}
	numbers, err := h.Queries.ListGitHubPRNumbersByHeadSHA(ctx, db.ListGitHubPRNumbersByHeadSHAParams{
		InstallationID: p.Installation.ID,
		RepoOwner:      owner,
		RepoName:       repo,
		HeadSha:        sha,
	})
	if err != nil {
		return
	}
	for _, number := range numbers {
		enqueue(number)
	}
}

// mirrorPullRequestForWorkspace mirrors a pull_request webhook into a single
// workspace: it upserts the PR row, replays any check_suite events that
// arrived before the PR was mirrored, auto-links referenced issues (gated by
// the workspace's GitHub toggles), applies the workspace's merge-status choice,
// and broadcasts the change. Invoked once per bound workspace.
//
// linkPolicy is the delivery-wide verdict on which claimed identifiers this
// workspace may link. It cannot choose the workspace's merge-status target.
// mirrorPullRequestForWorkspace persists the PR mirror row and its issue
// link/unlink rows in one DB transaction.
func (h *Handler) mirrorPullRequestForWorkspace(ctx context.Context, wsID pgtype.UUID, installationID int64, p *ghPullRequestPayload, linkPolicy prLinkPolicy) error {
	ws, err := h.Queries.GetWorkspace(ctx, wsID)
	if err != nil {
		return fmt.Errorf("github: load workspace: %w", err)
	}
	if !githubEnabledForWorkspace(ws) {
		return nil
	}
	tx, err := h.TxStarter.Begin(ctx)
	if err != nil {
		return fmt.Errorf("github: begin mirror transaction: %w", err)
	}
	defer tx.Rollback(ctx)
	qtx := h.Queries.WithTx(tx)
	prevState := ""
	prev, err := qtx.GetGitHubPullRequest(ctx, db.GetGitHubPullRequestParams{
		WorkspaceID: wsID, RepoOwner: p.Repository.Owner.Login,
		RepoName: p.Repository.Name, PrNumber: p.PullRequest.Number,
	})
	if err == nil {
		prevState = prev.State
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("github: load previous pr state: %w", err)
	}
	state := derivePRState(p.PullRequest.State, p.PullRequest.Draft, p.PullRequest.Merged)
	mergeable, clearMergeable := derivePRMergeableState(p.Action, p.PullRequest.MergeableState, baseRefChanged(p.Changes))
	pr, err := qtx.UpsertGitHubPullRequest(ctx, db.UpsertGitHubPullRequestParams{
		WorkspaceID: wsID, InstallationID: installationID,
		RepoOwner: p.Repository.Owner.Login, RepoName: p.Repository.Name,
		PrNumber: p.PullRequest.Number, Title: p.PullRequest.Title, State: state,
		HtmlUrl: p.PullRequest.HTMLURL, Branch: ptrToText(strPtrOrNil(p.PullRequest.Head.Ref)),
		AuthorLogin:     ptrToText(strPtrOrNil(p.PullRequest.User.Login)),
		AuthorAvatarUrl: ptrToText(strPtrOrNil(p.PullRequest.User.AvatarURL)),
		MergedAt:        parseGHTime(p.PullRequest.MergedAt), ClosedAt: parseGHTime(p.PullRequest.ClosedAt),
		PrCreatedAt: parseGHTimeRequired(p.PullRequest.CreatedAt), PrUpdatedAt: parseGHTimeRequired(p.PullRequest.UpdatedAt),
		HeadSha: p.PullRequest.Head.SHA, MergeableState: mergeable,
		ClearMergeableState: pgtype.Bool{Bool: clearMergeable, Valid: true},
		Additions:           p.PullRequest.Additions, Deletions: p.PullRequest.Deletions,
		ChangedFiles: p.PullRequest.ChangedFiles, IsReopen: p.Action == "reopened",
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("github: upsert pr: %w", err)
	}
	state = pr.State
	workspaceID := uuidToString(wsID)
	linkedIssueIDs := make([]string, 0)
	touched := map[pgtype.UUID]struct{}{}
	if githubFeaturesEnabled(ws) && !linkPolicy.indeterminate {
		idents := prClaimedIdentifiers(p.PullRequest.Title, p.PullRequest.Body, p.PullRequest.Head.Ref)
		autoLink, err := autoLinkPRsEnabledForWorkspace(ws)
		if err != nil {
			return fmt.Errorf("github: read auto-link setting: %w", err)
		}
		if autoLink {
			linkedIssueIDs, touched, err = h.reconcileAutoLinks(ctx, qtx, ws, pr.ID, state, prAutoLinkInput{
				idents:    idents,
				permits:   func(id string) bool { return linkPolicy.permits(id, workspaceID) },
				ambiguous: func(id string) bool { return linkPolicy.ambiguous[id] },
				link: func(issueID pgtype.UUID) (int64, error) {
					return qtx.LinkIssueToPullRequest(ctx, db.LinkIssueToPullRequestParams{IssueID: issueID, PullRequestID: pr.ID})
				},
				unlink: func(issueID pgtype.UUID) (int64, error) {
					return qtx.UnlinkIssueFromPullRequest(ctx, db.UnlinkIssueFromPullRequestParams{IssueID: issueID, PullRequestID: pr.ID})
				},
				listAuto: func() ([]pgtype.UUID, error) { return qtx.ListAutoLinkedIssueIDsForPullRequest(ctx, pr.ID) },
			})
			if err != nil {
				return fmt.Errorf("github: reconcile links: %w", err)
			}
		}
	}
	issueIDs, err := qtx.ListIssueIDsForPullRequest(ctx, pr.ID)
	if err != nil {
		return fmt.Errorf("github: list persisted links: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("github: commit mirror transaction: %w", err)
	}
	if githubFeaturesEnabled(ws) && !linkPolicy.indeterminate {
		if state == "merged" && prevState != "merged" {
			for _, issueID := range issueIDs {
				touched[issueID] = struct{}{}
			}
		}
		resolver := issuestatus.NewResolver(wsID)
		for issueID := range touched {
			h.maybeAutoCompleteIssue(ctx, wsID, issueID, resolver)
		}
	}
	if state == "merged" {
		wakeup := service.IssueWakeupService{Tasks: h.TaskService}
		return dispatchMergedPRWakeups(issueIDs, func(issueID pgtype.UUID) error {
			return wakeup.TriggerPullRequestWakeup(ctx, issueID, service.PullRequestWakeupInput{
				Rule: service.SystemRulePRMerged, RepoOwner: p.Repository.Owner.Login, RepoName: p.Repository.Name,
				Number: p.PullRequest.Number, URL: p.PullRequest.HTMLURL, MergeCommit: p.PullRequest.MergeCommitSHA,
				HeadSHA: p.PullRequest.Head.SHA, BaseBranch: p.PullRequest.Base.Ref, HeadBranch: p.PullRequest.Head.Ref,
			})
		}, func() {
			h.publish(protocol.EventPullRequestUpdated, workspaceID, "system", "", map[string]any{
				"pull_request":     githubPullRequestToResponse(pr, h.PRRefresh.Enabled()),
				"linked_issue_ids": linkedIssueIDs,
			})
		})
	}
	h.publish(protocol.EventPullRequestUpdated, workspaceID, "system", "", map[string]any{
		"pull_request":     githubPullRequestToResponse(pr, h.PRRefresh.Enabled()),
		"linked_issue_ids": linkedIssueIDs,
	})
	return nil
}

func dispatchMergedPRWakeups(issueIDs []pgtype.UUID, dispatch func(pgtype.UUID) error, publish func()) error {
	var dispatchErrors []error
	for _, issueID := range issueIDs {
		if err := dispatch(issueID); err != nil {
			dispatchErrors = append(dispatchErrors, fmt.Errorf("github: dispatch pull request merge wakeup for issue %s: %w", uuidToString(issueID), err))
		}
	}
	publish()
	return errors.Join(dispatchErrors...)
}

// prAutoLinkInput is what reconcileAutoLinks needs from one provider.
type prAutoLinkInput struct {
	idents    []string                         // identifiers the PR claims (see prClaimedIdentifiers)
	permits   func(identifier string) bool     // cross-workspace verdict (always true for VCS)
	ambiguous func(identifier string) bool     // resolved in several workspaces
	link      func(pgtype.UUID) (int64, error) // automatic link; 1 when new
	unlink    func(pgtype.UUID) (int64, error) // drop a link; 1 when removed
	listAuto  func() ([]pgtype.UUID, error)    // issues currently auto-linked to the PR
}

// reconcileAutoLinks makes the PR's automatic links match its claims. It
// returns the issue ids the PR is linked to after the pass, and the issues
// whose link set changed (a PR event for auto-complete).
//
//   - A claimed identifier that resolves here links, unless a person removed
//     that PR from that issue before.
//   - An automatic link the PR no longer claims is dropped while the PR is still
//     open. After merge/close it is kept: a later title edit must not take the
//     delivered work off the issue.
//   - An automatic link for an identifier that turned out to be ambiguous
//     across bound workspaces is always dropped (see prLinkPolicy).
//
// Manual links are never touched here, and close intent is not either (see
// closingIssueIDs).
func (h *Handler) reconcileAutoLinks(ctx context.Context, queries *db.Queries, ws db.Workspace, prID pgtype.UUID, state string, in prAutoLinkInput) ([]string, map[pgtype.UUID]struct{}, error) {
	touched := map[pgtype.UUID]struct{}{}
	linked := make([]string, 0)
	claimed := map[pgtype.UUID]struct{}{}
	ambiguousIssues := map[pgtype.UUID]struct{}{}
	prefix := issuePrefixForWorkspace(ws)
	for _, id := range in.idents {
		issue, ok, err := h.lookupIssueByIdentifier(ctx, queries, ws.ID, prefix, id)
		if err != nil {
			return linked, touched, err
		}
		if !ok {
			continue
		}
		if !in.permits(id) {
			if in.ambiguous(id) {
				ambiguousIssues[issue.ID] = struct{}{}
			}
			continue
		}
		excluded, err := queries.IsPullRequestExcludedFromIssue(ctx, db.IsPullRequestExcludedFromIssueParams{IssueID: issue.ID, PullRequestID: prID})
		if err != nil {
			return linked, touched, err
		}
		if excluded {
			continue
		}
		claimed[issue.ID] = struct{}{}
		rows, err := in.link(issue.ID)
		if err != nil {
			return linked, touched, err
		}
		linked = append(linked, uuidToString(issue.ID))
		if rows > 0 {
			touched[issue.ID] = struct{}{}
		}
	}

	terminal := state == "merged" || state == "closed"
	current, err := in.listAuto()
	if err != nil {
		return linked, touched, err
	}
	for _, issueID := range current {
		if _, ok := claimed[issueID]; ok {
			continue
		}
		_, ambiguous := ambiguousIssues[issueID]
		if terminal && !ambiguous {
			continue
		}
		rows, err := in.unlink(issueID)
		if err != nil {
			return linked, touched, err
		}
		if rows > 0 {
			// Dropping an unmerged PR can leave only merged ones behind.
			touched[issueID] = struct{}{}
		}
	}
	return linked, touched, nil
}

// derivePRMergeableState resolves the upsert behaviour for the PR row's
// mergeable_state column on a `pull_request` webhook. It returns three
// states encoded as (value, clear):
//
//   - clear=true → force the column to NULL. State-changing actions (`opened`,
//     `synchronize`, `reopened`, or a base-branch swap) must blank the value
//     because GitHub re-computes mergeability asynchronously; the payload may
//     still carry the previous head's clean/dirty answer, and trusting it
//     would surface a stale verdict against the new head.
//   - clear=false, value valid → write the value. The event carried a
//     concrete verdict we should persist.
//   - clear=false, value invalid → preserve the existing column. Metadata
//     events (labeled/assigned/edited-without-base-swap) ship pull_request
//     payloads with mergeable_state empty even when the previous verdict is
//     still accurate, and silently overwriting clean/dirty with NULL would
//     drop information GitHub only refreshes lazily.
func derivePRMergeableState(action, payload string, baseRefChanged bool) (pgtype.Text, bool) {
	if action == "opened" || action == "synchronize" || action == "reopened" {
		return pgtype.Text{}, true
	}
	if action == "edited" && baseRefChanged {
		return pgtype.Text{}, true
	}
	if payload == "" {
		return pgtype.Text{}, false
	}
	return pgtype.Text{String: payload, Valid: true}, false
}

// ghPRChanges captures the only field of `pull_request.edited`'s `changes`
// payload we care about: a base-branch swap. Everything else (title, body)
// leaves mergeability intact.
type ghPRChanges struct {
	Base *struct {
		Ref *struct {
			From string `json:"from"`
		} `json:"ref"`
	} `json:"base"`
}

// baseRefChanged returns true when a pull_request.edited event indicates the
// PR's base branch was swapped. Only this kind of edit invalidates the
// existing mergeable_state.
func baseRefChanged(c *ghPRChanges) bool {
	return c != nil && c.Base != nil && c.Base.Ref != nil && c.Base.Ref.From != ""
}

func derivePRState(state string, draft, merged bool) string {
	if merged {
		return "merged"
	}
	if state == "closed" {
		return "closed"
	}
	if draft {
		return "draft"
	}
	return "open"
}

func parseGHTime(s string) pgtype.Timestamptz {
	if s == "" {
		return pgtype.Timestamptz{}
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return pgtype.Timestamptz{}
	}
	return pgtype.Timestamptz{Time: t, Valid: true}
}

func parseGHTimeRequired(s string) pgtype.Timestamptz {
	t := parseGHTime(s)
	if !t.Valid {
		return pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true}
	}
	return t
}

// extractIdentifiers pulls every "PREFIX-NUMBER" match across the supplied
// fields, deduplicating in input order.
func extractIdentifiers(parts ...string) []string {
	return extractMatchedIdentifiers(identifierRe, parts...)
}

func extractMatchedIdentifiers(re *regexp.Regexp, parts ...string) []string {
	seen := map[string]struct{}{}
	out := []string{}
	for _, src := range parts {
		for _, m := range re.FindAllStringSubmatch(src, -1) {
			ident := strings.ToUpper(m[1]) + "-" + m[2]
			if _, dup := seen[ident]; dup {
				continue
			}
			seen[ident] = struct{}{}
			out = append(out, ident)
		}
	}
	return out
}

// mdFenceLineRe matches a line consisting solely (aside from up to 3 leading
// spaces of indent, per CommonMark) of a fence delimiter run of 3+ backticks
// or 3+ tildes, capturing the delimiter character and run length, so the
// scanner in stripMarkdownCodeSpans can find the matching close fence.
//
// The info string (anything after the delimiter run) is validated
// delimiter-specifically, not with one shared class: CommonMark forbids a
// backtick anywhere in a backtick fence's info string (it would be
// ambiguous with an inline code span), but a tilde fence's info string may
// contain backticks freely. A shared `[^`\n]*` class rejected valid tilde
// fences such as `~~~ lang`example` and left their body's closing keyword
// exposed to the close parser — caught in independent review (CHE-520).
var mdFenceLineRe = regexp.MustCompile("(?m)^ {0,3}(?:(`{3,})[^`\n]*|(~{3,})[^\n]*)$")

// stripMarkdownCodeSpans blanks out fenced code blocks and inline code spans,
// replacing each with a single space so surrounding word boundaries and
// adjacency checks still behave as if the span were absent. This keeps a PR
// body that merely quotes a closing keyword as documentation — e.g. a body
// describing what another PR wrote in a code span reading "Closes CHE-380"
// — from being treated as a live closing declaration (CHE-520).
//
// Both fence and inline-span matching follow CommonMark's actual delimiter
// rules rather than a fixed-width regex, because a naive “ `...` “ /
// ```` ```...``` ```` pattern misses two valid forms a PR author can use to
// quote text containing a backtick: a longer backtick run as the inline-span
// delimiter (“ “Closes CHE-380“ “), and a tilde fence (`~~~`). Both left
// a live closing keyword unstripped and re-triggered the CHE-520 false
// auto-close (caught in independent review of the initial fixed-width fix).
//
//   - Fenced blocks: a line of 3+ backticks or 3+ tildes opens a fence; it is
//     closed by the next line consisting of a run of the same character at
//     least as long (CommonMark fenced-code-block rule). An unterminated
//     fence extends to end of input.
//   - Inline spans: a run of N backticks opens a span; it is closed by the
//     next run of exactly N backticks (CommonMark code-span rule). A run
//     with no matching close of the same length is left as plain text.
func stripMarkdownCodeSpans(s string) string {
	// Pass 1: fenced code blocks, since a fence's own delimiter run must
	// never be mistaken for inline-span backticks.
	var out strings.Builder
	rest := s
	for {
		loc := mdFenceLineRe.FindStringSubmatchIndex(rest)
		if loc == nil {
			out.WriteString(rest)
			break
		}
		openStart, openEnd := loc[0], loc[1]
		var delim string
		if loc[2] != -1 {
			delim = rest[loc[2]:loc[3]] // backtick fence
		} else {
			delim = rest[loc[4]:loc[5]] // tilde fence
		}
		out.WriteString(rest[:openStart])
		out.WriteString(" ")

		afterOpen := rest[openEnd:]
		closeRe := regexp.MustCompile("(?m)^ {0,3}" + regexp.QuoteMeta(string(delim[0])) + "{" + strconv.Itoa(len(delim)) + ",}[ \t]*$")
		if cLoc := closeRe.FindStringIndex(afterOpen); cLoc != nil {
			rest = afterOpen[cLoc[1]:]
		} else {
			rest = ""
		}
	}
	s = out.String()

	// Pass 2: inline code spans via CommonMark's equal-length-run rule.
	out.Reset()
	i := 0
	for i < len(s) {
		if s[i] != '`' {
			out.WriteByte(s[i])
			i++
			continue
		}
		start := i
		for i < len(s) && s[i] == '`' {
			i++
		}
		runLen := i - start
		closeIdx := -1
		closeEnd := -1
		j := i
		for j < len(s) {
			if s[j] != '`' {
				j++
				continue
			}
			k := j
			for k < len(s) && s[k] == '`' {
				k++
			}
			if k-j == runLen {
				closeIdx, closeEnd = j, k
				break
			}
			j = k
		}
		if closeIdx == -1 {
			out.WriteString(s[start:i])
			continue
		}
		out.WriteString(" ")
		i = closeEnd
	}
	return out.String()
}

// extractClosingIdentifiers pulls every "PREFIX-NUMBER" identifier that
// appears immediately after a GitHub-style closing keyword in the supplied
// fields, deduplicating in input order. Identifiers in branch names are
// intentionally excluded — callers should pass only title and body — because
// branch names are not natural-language fields and treating "mul-1/fix-login"
// as a close declaration would silently re-open the bug this gate is meant
// to fix. Markdown code spans are stripped first so a quoted closing keyword
// in documentation prose is never mistaken for a live closing declaration.
func extractClosingIdentifiers(parts ...string) []string {
	seen := map[string]struct{}{}
	out := []string{}
	for _, src := range parts {
		src = stripMarkdownCodeSpans(src)
		for _, m := range closingIdentifierRe.FindAllStringSubmatch(src, -1) {
			ident := strings.ToUpper(m[1]) + "-" + m[2]
			if _, dup := seen[ident]; dup {
				continue
			}
			seen[ident] = struct{}{}
			out = append(out, ident)
		}
	}
	return out
}

// autoLinkPRsEnabledForWorkspace reports whether the workspace allows the
// GitHub webhook to create issue ↔ PR link rows. Defaults to true so that
// workspaces predating RFC MUL-2414 keep the historical "auto-link on"
// behavior, and short-circuits to false whenever the master GitHub switch
// is explicitly off — mirroring the precedence used on the client side.
//
// An unparseable settings blob is surfaced as an error instead of being folded
// into the permissive default. Callers deciding whether to write a link row
// keep taking the default, but the cross-workspace scan has to tell "auto-link
// is on" apart from "we could not find out" — see prLinkPolicy.
func prClaimedIdentifiers(title, body, branch string) []string {
	idents := extractIdentifiers(title, branch)
	for _, id := range extractClosingIdentifiers(title, body) {
		if !slices.Contains(idents, id) {
			idents = append(idents, id)
		}
	}
	return idents
}

func autoLinkPRsEnabledForWorkspace(ws db.Workspace) (bool, error) {
	if len(ws.Settings) == 0 {
		return true, nil
	}
	var s struct {
		GitHubEnabled            *bool `json:"github_enabled"`
		GitHubAutoLinkPRsEnabled *bool `json:"github_auto_link_prs_enabled"`
	}
	if err := json.Unmarshal(ws.Settings, &s); err != nil {
		return true, err
	}
	if s.GitHubEnabled != nil && !*s.GitHubEnabled {
		return false, nil
	}
	if s.GitHubAutoLinkPRsEnabled == nil {
		return true, nil
	}
	return *s.GitHubAutoLinkPRsEnabled, nil
}

// githubFeaturesEnabled reads the GitHub master switch. Absent means on.
func githubFeaturesEnabled(ws db.Workspace) bool {
	if len(ws.Settings) == 0 {
		return true
	}
	var s struct {
		GitHubEnabled *bool `json:"github_enabled"`
	}
	if err := json.Unmarshal(ws.Settings, &s); err != nil {
		return true
	}
	return s.GitHubEnabled == nil || *s.GitHubEnabled
}

// githubEnabledForWorkspace reports whether the workspace's master GitHub
// switch (`github_enabled`) is on, independent of the auto-link toggle
// (CHE-374 review round 2, item 1). autoLinkPRsEnabledForWorkspace conflates
// the two: it returns false both when github_enabled is explicitly off AND
// when github_auto_link_prs_enabled is off, so it cannot answer "is GitHub
// itself still on for this workspace". This helper answers only that
// narrower question, so callers can still drive GitHub side effects (merge
// announcements, advance-to-done re-evaluation) off a PERSISTED link row even
// when auto-linking new identifiers is disabled.
//
// Defaults to true when unset/unparseable, matching the same permissive
// default autoLinkPRsEnabledForWorkspace uses.
func githubEnabledForWorkspace(ws db.Workspace) bool {
	if len(ws.Settings) == 0 {
		return true
	}
	var s struct {
		GitHubEnabled *bool `json:"github_enabled"`
	}
	if err := json.Unmarshal(ws.Settings, &s); err != nil {
		return true
	}
	if s.GitHubEnabled == nil {
		return true
	}
	return *s.GitHubEnabled
}

func (h *Handler) githubEnabledForWorkspace(ctx context.Context, workspaceID pgtype.UUID) bool {
	ws, err := h.Queries.GetWorkspace(ctx, workspaceID)
	if err != nil {
		return true
	}
	return githubEnabledForWorkspace(ws)
}

// issueNumberForPrefix returns the issue number encoded in a "PREFIX-NUMBER"
// identifier, and false when the identifier is malformed or carries a prefix
// that is not the workspace's. Case-insensitive: branch names are
// conventionally lowercase while issue prefixes are uppercase.
func issueNumberForPrefix(identifier, prefix string) (int32, bool) {
	idx := strings.LastIndex(identifier, "-")
	if idx < 0 {
		return 0, false
	}
	gotPrefix, numStr := identifier[:idx], identifier[idx+1:]
	if !strings.EqualFold(gotPrefix, prefix) {
		return 0, false
	}
	n, err := strconv.Atoi(numStr)
	if err != nil {
		return 0, false
	}
	return int32(n), true
}

// lookupIssueByIdentifier looks up an issue in the given workspace by its
// "PREFIX-NUMBER" identifier. Returns the row + true if the prefix matches
// the workspace's configured prefix and the number resolves to a real issue.
func (h *Handler) lookupIssueByIdentifier(ctx context.Context, queries *db.Queries, workspaceID pgtype.UUID, prefix, identifier string) (db.Issue, bool, error) {
	number, ok := issueNumberForPrefix(identifier, prefix)
	if !ok {
		return db.Issue{}, false, nil
	}
	issue, err := queries.GetIssueByNumber(ctx, db.GetIssueByNumberParams{
		WorkspaceID: workspaceID,
		Number:      number,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return db.Issue{}, false, nil
	}
	if err != nil {
		return db.Issue{}, false, fmt.Errorf("lookup issue %s: %w", identifier, err)
	}
	return issue, true, nil
}

// advanceIssueToDone auto-completes an issue whose linked PR(s) merged with
// closing intent. It refuses the transition when the issue still has an
// open child: aggregating only linked-PR state let a parent flip to `done`
// while its own sub-issues were still `in_progress` or `todo`, silently
// hiding unfinished work (CHE-520). Children already in a terminal status
// category (done/cancelled, including custom statuses that resolve to
// either) do not block the parent.
//
// This deliberately does NOT reuse resolveTerminalChildren: that helper
// skips unstaged siblings whenever any sibling in the set is staged (it
// implements the stage-BARRIER rule for the parent-notification path, where
// an unstaged sibling is genuinely irrelevant to closing a specific stage).
// The auto-done gate here has no stage concept — it is "every direct child
// must be terminal, full stop" — so skipping any child would let a mixed
// staged/unstaged, all-terminal child set wrongly block auto-completion
// forever, or (worse) let a skipped non-terminal unstaged child through.
// Every direct child's effective status is resolved and checked here.
func (h *Handler) advanceIssueToDone(ctx context.Context, issue db.Issue, workspaceID string) {
	// An issue leaves Triage only by being accepted; a merged "Closes" PR
	// links to it but must not move it out. (MUL-7189 §2.2)
	//
	// Checked before the child scan below: this is a field read on an issue
	// already in hand, so a triaged issue short-circuits without spending a
	// ListChildIssues round-trip.
	if issue.TriageState.Valid {
		return
	}

	children, err := h.Queries.ListChildIssues(ctx, issue.ID)
	if err != nil {
		slog.Warn("github: advance issue to done: list children failed", "err", err, "issue_id", uuidToString(issue.ID))
		return
	}
	if len(children) > 0 {
		effective := h.childStatusResolver(ctx)
		for _, child := range children {
			status, err := effective(child)
			if err != nil {
				slog.Warn("github: advance issue to done: resolve child status failed", "err", err, "issue_id", uuidToString(issue.ID), "child_id", uuidToString(child.ID))
				return
			}
			if !isTerminalChildStatus(status) {
				return
			}
		}
	}

	updated, err := h.Queries.UpdateIssueStatus(ctx, db.UpdateIssueStatusParams{
		ID:          issue.ID,
		Status:      "done",
		WorkspaceID: issue.WorkspaceID,
	})
	if err != nil {
		slog.Warn("github: advance issue to done failed", "err", err)
		return
	}

	// The status write records a child event; process it after the write commits.
	h.processChildEvents(ctx, updated.ParentIssueID)

	prefix := h.getIssuePrefix(ctx, issue.WorkspaceID)
	resp := issueToResponse(updated, prefix)
	h.fillStatusCategory(ctx, updated.WorkspaceID, &resp)
	h.publish(protocol.EventIssueUpdated, workspaceID, "system", "", map[string]any{
		"issue":          resp,
		"status_changed": true,
		"prev_status":    issue.Status,
		"creator_type":   issue.CreatorType,
		"creator_id":     uuidToString(issue.CreatorID),
		"source":         "github_pr_merged",
	})
}

// ── Helpers ─────────────────────────────────────────────────────────────────

func parseStrictUUID(s string) (pgtype.UUID, error) {
	var u pgtype.UUID
	if err := u.Scan(s); err != nil {
		return pgtype.UUID{}, err
	}
	return u, nil
}

func coalesce(a, fallback string) string {
	if strings.TrimSpace(a) == "" {
		return fallback
	}
	return a
}

func strPtrOrNil(s string) *string {
	if s == "" {
		return nil
	}
	v := s
	return &v
}
