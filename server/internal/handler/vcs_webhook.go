package handler

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/integrations/vcs"
	"github.com/multica-ai/multica/server/internal/issuestatus"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// ── Response mappers ────────────────────────────────────────────────────────

// vcsPullRequestToResponse maps a stored VCS PR onto the shared PR response
// shape for single-PR webhook broadcasts (no aggregated check counts; the
// frontend re-queries the issue's PR list for fresh counts).
func vcsPullRequestToResponse(p db.VcsPullRequest) GitHubPullRequestResponse {
	return GitHubPullRequestResponse{
		ID:               uuidToString(p.ID),
		Provider:         p.Provider,
		WorkspaceID:      uuidToString(p.WorkspaceID),
		RepoOwner:        p.RepoOwner,
		RepoName:         p.RepoName,
		Number:           p.PrNumber,
		Title:            p.Title,
		State:            p.State,
		HtmlURL:          p.HtmlUrl,
		Branch:           textToPtr(p.Branch),
		AuthorLogin:      textToPtr(p.AuthorLogin),
		AuthorAvatarURL:  textToPtr(p.AuthorAvatarUrl),
		MergedAt:         timestampToPtr(p.MergedAt),
		ClosedAt:         timestampToPtr(p.ClosedAt),
		PRCreatedAt:      timestampToString(p.PrCreatedAt),
		PRUpdatedAt:      timestampToString(p.PrUpdatedAt),
		MergeableState:   nil,
		ChecksConclusion: nil,
		Additions:        p.Additions,
		Deletions:        p.Deletions,
		ChangedFiles:     p.ChangedFiles,
	}
}

// vcsPullRequestRowToResponse maps an issue's PR-list row, which carries the
// aggregated commit-status counts, onto the shared response shape.
func vcsPullRequestRowToResponse(p db.ListVCSPullRequestsByIssueRow) GitHubPullRequestResponse {
	return GitHubPullRequestResponse{
		ID:               uuidToString(p.ID),
		Provider:         p.Provider,
		WorkspaceID:      uuidToString(p.WorkspaceID),
		RepoOwner:        p.RepoOwner,
		RepoName:         p.RepoName,
		Number:           p.PrNumber,
		Title:            p.Title,
		State:            p.State,
		HtmlURL:          p.HtmlUrl,
		Branch:           textToPtr(p.Branch),
		AuthorLogin:      textToPtr(p.AuthorLogin),
		AuthorAvatarURL:  textToPtr(p.AuthorAvatarUrl),
		MergedAt:         timestampToPtr(p.MergedAt),
		ClosedAt:         timestampToPtr(p.ClosedAt),
		PRCreatedAt:      timestampToString(p.PrCreatedAt),
		PRUpdatedAt:      timestampToString(p.PrUpdatedAt),
		MergeableState:   nil,
		ChecksConclusion: aggregateChecksConclusion(p.ChecksFailed, p.ChecksPassed, p.ChecksPending, p.ChecksTotal),
		ChecksTotal:      p.ChecksTotal,
		ChecksPassed:     p.ChecksPassed,
		ChecksFailed:     p.ChecksFailed,
		ChecksPending:    p.ChecksPending,
		ChecksRunning:    p.ChecksPending,
		FailedCheckNames: []string{},
		Additions:        p.Additions,
		Deletions:        p.Deletions,
		ChangedFiles:     p.ChangedFiles,
	}
}

// ── Webhook ─────────────────────────────────────────────────────────────────

// HandleVCSWebhook (POST /api/webhooks/vcs/{connectionId}) authenticates and
// mirrors webhooks from any token-based Git provider. The connection id in the path
// selects the workspace, the provider, and the decryption secret; the provider
// adapter handles the provider-specific signature scheme, event header, and
// payload shape, returning normalized events to the shared mirror logic below.
func (h *Handler) HandleVCSWebhook(w http.ResponseWriter, r *http.Request) {
	// Where the integration is off (the managed cloud) the endpoint behaves as
	// if it does not exist — a bare 404 that reveals nothing about config, the
	// same response a genuinely unknown connection id gets below.
	if !h.isVCSAvailable() {
		writeError(w, http.StatusNotFound, "unknown connection")
		return
	}
	if !h.isVCSConfigured() {
		writeError(w, http.StatusNotFound, "unknown connection")
		return
	}
	connUUID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "connectionId"), "connection id")
	if !ok {
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 10<<20)) // 10 MiB cap
	if err != nil {
		writeError(w, http.StatusBadRequest, "read body failed")
		return
	}

	conn, err := h.Queries.GetVCSConnectionByID(r.Context(), connUUID)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			slog.Warn("vcs: lookup connection failed", "err", err)
		}
		writeError(w, http.StatusNotFound, "unknown connection")
		return
	}
	provider, ok := vcs.For(conn.Provider)
	if !ok {
		slog.Error("vcs: connection has unknown provider", "provider", conn.Provider)
		writeError(w, http.StatusInternalServerError, "unknown provider")
		return
	}

	secret, err := h.openVCSSecret(conn.WebhookSecretEncrypted)
	if err != nil {
		slog.Error("vcs: decrypt webhook secret failed", "err", err)
		writeError(w, http.StatusInternalServerError, "secret error")
		return
	}
	if !provider.VerifySignature(secret, r.Header, body) {
		writeError(w, http.StatusUnauthorized, "invalid signature")
		return
	}

	switch provider.EventKind(r.Header) {
	case vcs.EventPullRequest:
		if pr, err := provider.ParsePullRequest(body); err != nil {
			slog.Warn("vcs: bad pull_request payload", "provider", conn.Provider, "err", err)
		} else {
			if err := h.mirrorVCSPullRequest(r.Context(), conn, pr); err != nil {
				slog.Error("vcs: pull_request event failed", "provider", conn.Provider, "err", err)
				writeError(w, http.StatusInternalServerError, "failed to process pull_request event")
				return
			}
		}
	case vcs.EventCIStatus:
		if st, err := provider.ParseCIStatus(body); err != nil {
			slog.Warn("vcs: bad status payload", "provider", conn.Provider, "err", err)
		} else {
			h.mirrorVCSCIStatus(r.Context(), conn, st)
		}
	default:
		// Acknowledge unmodelled events so the provider doesn't flag the hook.
	}
	w.WriteHeader(http.StatusAccepted)
}

func (h *Handler) mirrorVCSPullRequest(ctx context.Context, conn db.VcsConnection, ev vcs.PullRequestEvent) error {
	if ev.RepoOwner == "" || ev.RepoName == "" || ev.Number == 0 {
		slog.Warn("vcs: pull_request missing repo identity", "provider", conn.Provider)
		return nil
	}
	ws, err := h.Queries.GetWorkspace(ctx, conn.WorkspaceID)
	if err != nil {
		return fmt.Errorf("vcs: load workspace: %w", err)
	}
	tx, err := h.TxStarter.Begin(ctx)
	if err != nil {
		return fmt.Errorf("vcs: begin mirror transaction: %w", err)
	}
	defer tx.Rollback(ctx)
	qtx := h.Queries.WithTx(tx)
	// The stored state before this event, so a merge completes issues once —
	// when it happens — and not again on a later event of a merged PR.
	prevState := ""
	if prev, err := qtx.GetVCSPullRequestByKey(ctx, db.GetVCSPullRequestByKeyParams{
		ConnectionID: conn.ID,
		RepoOwner:    ev.RepoOwner,
		RepoName:     ev.RepoName,
		PrNumber:     ev.Number,
	}); err == nil {
		prevState = prev.State
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("vcs: load previous pr state: %w", err)
	}

	pr, err := qtx.UpsertVCSPullRequest(ctx, db.UpsertVCSPullRequestParams{
		WorkspaceID:     conn.WorkspaceID,
		ConnectionID:    conn.ID,
		Provider:        conn.Provider,
		RepoOwner:       ev.RepoOwner,
		RepoName:        ev.RepoName,
		PrNumber:        ev.Number,
		Title:           ev.Title,
		State:           ev.State,
		HtmlUrl:         ev.HTMLURL,
		Branch:          ptrToText(strPtrOrNil(ev.Branch)),
		AuthorLogin:     ptrToText(strPtrOrNil(ev.AuthorLogin)),
		AuthorAvatarUrl: ptrToText(strPtrOrNil(ev.AuthorAvatarURL)),
		MergedAt:        parseGHTime(ev.MergedAt),
		ClosedAt:        parseGHTime(ev.ClosedAt),
		PrCreatedAt:     parseGHTimeRequired(ev.CreatedAt),
		PrUpdatedAt:     parseGHTimeRequired(ev.UpdatedAt),
		Additions:       ev.Additions,
		Deletions:       ev.Deletions,
		ChangedFiles:    ev.ChangedFiles,
		HeadSha:         ev.HeadSHA,
	})
	if err != nil {
		return fmt.Errorf("vcs: upsert pr: %w", err)
	}

	// Out-of-order guard for the link write. UpsertVCSPullRequest keeps the
	// newer persisted row on a stale redelivery, so `pr` may reflect a newer
	// event than this `ev`. Everything the link pass decides below comes from
	// `ev`, so acting on a stale event would undo what the newer one already
	// recorded (e.g. a redelivered older event dropping a link the newer title
	// carries). If the persisted row is strictly newer than this event, the
	// newer event already linked and published — stop here. (An event with no
	// usable timestamp falls back to now(), which is never strictly after the
	// stored value, so it proceeds.)
	evUpdatedAt := parseGHTimeRequired(ev.UpdatedAt)
	if pr.PrUpdatedAt.Valid && evUpdatedAt.Valid && pr.PrUpdatedAt.Time.After(evUpdatedAt.Time) {
		return nil
	}

	workspaceID := uuidToString(conn.WorkspaceID)
	resp := vcsPullRequestToResponse(pr)

	// Auto-link to issues by identifiers in the title, branch, and closing
	// keywords. Connecting a provider is the opt-in, so there is no separate
	// per-workspace flag. The issue-side machinery is shared with GitHub
	// (reconcileAutoLinks, maybeAutoCompleteIssue). A connection belongs to
	// exactly one workspace, so there is no cross-workspace ambiguity to settle.
	idents, closing := prClaimedIdentifiers(ev.Title, ev.Body, ev.Branch)
	permits := func(string) bool { return true }
	linkedIssueIDs, touched, err := h.reconcileAutoLinks(ctx, qtx, ws, pr.ID, ev.State, prAutoLinkInput{
		idents:    idents,
		permits:   permits,
		ambiguous: func(string) bool { return false },
		link: func(issueID pgtype.UUID) (int64, error) {
			return qtx.LinkIssueToVCSPullRequest(ctx, db.LinkIssueToVCSPullRequestParams{IssueID: issueID, PullRequestID: pr.ID})
		},
		unlink: func(issueID pgtype.UUID) (int64, error) {
			return qtx.UnlinkIssueFromVCSPullRequest(ctx, db.UnlinkIssueFromVCSPullRequestParams{IssueID: issueID, PullRequestID: pr.ID})
		},
		listAuto: func() ([]pgtype.UUID, error) {
			return qtx.ListAutoLinkedIssueIDsForVCSPullRequest(ctx, pr.ID)
		},
	})
	if err != nil {
		return fmt.Errorf("vcs: reconcile links: %w", err)
	}
	// Close intent follows the PR text up to and including the merge/close
	// event, on every link of the PR (see closingIssueIDs).
	if ev.Terminal() || (ev.State != "merged" && ev.State != "closed") {
		closingIDs, err := h.closingIssueIDs(ctx, qtx, ws, closing, permits)
		if err != nil {
			return fmt.Errorf("vcs: resolve close intent: %w", err)
		}
		if err := qtx.SyncVCSPullRequestCloseIntent(ctx, db.SyncVCSPullRequestCloseIntentParams{
			PullRequestID:   pr.ID,
			ClosingIssueIds: closingIDs,
		}); err != nil {
			return fmt.Errorf("vcs: sync close intent: %w", err)
		}
	}
	if ev.State == "merged" && prevState != "merged" {
		issueIDs, err := qtx.ListIssueIDsForVCSPullRequest(ctx, pr.ID)
		if err != nil {
			return fmt.Errorf("vcs: list linked issues: %w", err)
		}
		for _, id := range issueIDs {
			touched[id] = struct{}{}
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("vcs: commit mirror transaction: %w", err)
	}
	resolver := issuestatus.NewResolver(conn.WorkspaceID)
	for issueID := range touched {
		h.maybeAutoCompleteIssue(ctx, conn.WorkspaceID, issueID, resolver)
	}

	h.publish(protocol.EventPullRequestUpdated, workspaceID, "system", "", map[string]any{
		"pull_request":     resp,
		"linked_issue_ids": linkedIssueIDs,
	})
	return nil
}

func (h *Handler) mirrorVCSCIStatus(ctx context.Context, conn db.VcsConnection, ev vcs.CIStatusEvent) {
	if ev.SHA == "" || ev.State == "" {
		return
	}
	// Use the provider's own event timestamp so UpsertVCSCommitStatus's
	// monotonic guard has something real to compare — writing time.Now() here
	// made the guard always true, so an out-of-order redelivery could regress a
	// status. Falls back to now() only when the payload carried no timestamp.
	if err := h.Queries.UpsertVCSCommitStatus(ctx, db.UpsertVCSCommitStatusParams{
		ConnectionID: conn.ID,
		Sha:          ev.SHA,
		Context:      ev.Context,
		State:        ev.State,
		TargetUrl:    ptrToText(strPtrOrNil(ev.TargetURL)),
		Description:  ptrToText(strPtrOrNil(ev.Description)),
		UpdatedAt:    parseGHTimeRequired(ev.UpdatedAt),
	}); err != nil {
		slog.Warn("vcs: upsert commit status failed", "err", err)
		return
	}

	issueIDs, err := h.Queries.ListIssueIDsForVCSPRHead(ctx, db.ListIssueIDsForVCSPRHeadParams{
		ConnectionID: conn.ID,
		HeadSha:      ev.SHA,
	})
	if err != nil {
		slog.Warn("vcs: lookup issues for status failed", "err", err)
		return
	}
	workspaceID := uuidToString(conn.WorkspaceID)
	for _, issueID := range issueIDs {
		h.publish(protocol.EventPullRequestUpdated, workspaceID, "system", "", map[string]any{
			"issue_id": uuidToString(issueID),
		})
	}
}
