package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/integrations/ghsnapshot"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

const (
	mergeAnnouncementWorkerPollInterval = 2 * time.Second
	mergeAnnouncementWorkerMaxAttempts  = 5
	mergeAnnouncementWorkerConcurrency  = 2
)

// MergeAnnouncementWorker delivers exactly one system comment per merged,
// linked GitHub pull request (CHE-374/CHE-379). It is deliberately
// independent of issue completion: the announcement is a durable record of
// "this PR merged", not a vote on whether the issue is done, so it must never
// read or write issue.status and must run whether or not the merge carried
// close intent.
//
// Enqueueing (CreateGitHubMergeAnnouncement) happens inline in
// mirrorPullRequestForWorkspace, in the same request that persists the PR
// mirror, before the webhook handler returns 202 — so a crash after commit
// can never lose the intent to announce. This worker only owns delivery: it
// claims a due pending row (DB-leased, SKIP LOCKED — see
// ClaimPendingGitHubMergeAnnouncement), writes the comment, and marks the row
// delivered in one transaction so a crash between the two never produces a
// duplicate comment on retry.
type MergeAnnouncementWorker struct {
	h      *Handler
	notify chan struct{}
	done   chan struct{}
}

func NewMergeAnnouncementWorker(h *Handler) *MergeAnnouncementWorker {
	return &MergeAnnouncementWorker{
		h:      h,
		notify: make(chan struct{}, mergeAnnouncementWorkerConcurrency),
		done:   make(chan struct{}),
	}
}

func (w *MergeAnnouncementWorker) Notify() {
	if w == nil {
		return
	}
	select {
	case w.notify <- struct{}{}:
	default:
	}
}

func (w *MergeAnnouncementWorker) Run(ctx context.Context) {
	if w == nil {
		return
	}
	defer close(w.done)
	if w.h == nil || w.h.Queries == nil {
		return
	}

	var workers sync.WaitGroup
	workers.Add(mergeAnnouncementWorkerConcurrency)
	for range mergeAnnouncementWorkerConcurrency {
		go func() {
			defer workers.Done()
			w.runLoop(ctx)
		}()
	}
	workers.Wait()
}

func (w *MergeAnnouncementWorker) runLoop(ctx context.Context) {
	ticker := time.NewTicker(mergeAnnouncementWorkerPollInterval)
	defer ticker.Stop()
	for {
		worked, err := w.ProcessNext(ctx)
		if err != nil && !errors.Is(err, context.Canceled) {
			slog.Error("merge announcement worker: process", "error", err)
		}
		if worked {
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-w.notify:
		case <-ticker.C:
		}
	}
}

func (w *MergeAnnouncementWorker) WaitWithTimeout(timeout time.Duration) bool {
	if w == nil {
		return true
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-w.done:
		return true
	case <-timer.C:
		return false
	}
}

// ProcessNext claims and delivers one due announcement. Public so tests can
// drive the durable queue synchronously without starting a goroutine (same
// shape as WebhookDeliveryWorker.ProcessNext). The claim is an unfiltered
// "oldest pending, anywhere in the deployment" pick — the correct behavior
// for a background sweep, but NOT for a caller that wants one specific row
// delivered (see DeliverByID).
func (w *MergeAnnouncementWorker) ProcessNext(ctx context.Context) (bool, error) {
	a, err := w.h.Queries.ClaimPendingGitHubMergeAnnouncement(ctx)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("claim pending merge announcement: %w", err)
	}
	return true, w.deliverClaimed(ctx, a)
}

// DeliverByID claims and delivers exactly the named pending announcement —
// never an unrelated row (CHE-384/01-02 review fix). AnnounceMergeForIssue
// calls this instead of ProcessNext: the global claim in ProcessNext has no
// way to prefer the row a specific request just created/looked up, so under
// deployment-wide backlog it can (and does) deliver a different workspace's
// older pending row while reporting the caller's own target as still
// "pending" — correct-but-misreported from the worker's point of view, but a
// synchronous recovery caller needs "did MY row get delivered" answered
// truthfully. Returns (false, nil) when the row is no longer claimable
// (already delivered/failed/skipped by a concurrent claimant, or its lease
// hasn't expired yet) — the caller re-reads the row's current state rather
// than treating that as an error.
func (w *MergeAnnouncementWorker) DeliverByID(ctx context.Context, id pgtype.UUID) (bool, error) {
	a, err := w.h.Queries.ClaimPendingGitHubMergeAnnouncementByID(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("claim merge announcement %s: %w", uuidToString(id), err)
	}
	return true, w.deliverClaimed(ctx, a)
}

// deliverClaimed runs delivery/revalidation for an already-claimed row —
// shared by ProcessNext's global claim and DeliverByID's targeted claim, so
// the two claim strategies produce identical eligibility checks and delivery
// semantics.
func (w *MergeAnnouncementWorker) deliverClaimed(ctx context.Context, a db.GithubMergeAnnouncement) error {
	issue, err := w.h.Queries.GetIssue(ctx, a.IssueID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// The issue this announcement targeted is gone. Nothing to
			// attach a comment to and nothing to retry toward.
			return w.skip(ctx, a, "issue not found")
		}
		return w.retryOrFail(ctx, a, fmt.Errorf("load issue: %w", err))
	}

	// Revalidate eligibility at delivery time (CHE-374 review round 2, item
	// 2): the row was found eligible when it was enqueued, but delivery can
	// happen an arbitrary amount of time later — long enough for the
	// workspace to turn GitHub off, for the installation to be uninstalled,
	// or for the issue↔PR link to be removed (a post-merge body edit that
	// drops the closing/linking claim, or a manual unlink). None of those are
	// transient: they mean this specific announcement should never be
	// delivered, so they terminate via skip(), not retryOrFail(). A DB error
	// while checking is transient and goes through retryOrFail so
	// attempt_count advances instead of silently re-claiming forever.
	//
	// This must use the error-expressing check, not the fail-open
	// h.githubEnabledForWorkspace (CHE-374 review round 3, item 2): a
	// GetWorkspace failure or unparseable settings blob here is
	// indeterminate, not a pass — the previous fail-open version would
	// deliver the comment despite being unable to confirm GitHub is still on
	// for the workspace.
	enabled, err := w.h.githubEnabledForWorkspaceChecked(ctx, a.WorkspaceID)
	if err != nil {
		return w.retryOrFail(ctx, a, fmt.Errorf("check github enabled for workspace: %w", err))
	}
	if !enabled {
		return w.skip(ctx, a, "github disabled for workspace")
	}

	// Revalidate against the announcement's SOURCE PR installation, not just
	// "does the workspace have any GitHub installation at all" (CHE-374
	// review round 3, item 3). A workspace can have several bound
	// installations; removing the one that owns this PR while a different
	// installation remains bound must still block delivery, since the app
	// that originally granted access to this repository is gone.
	pr, err := w.h.Queries.GetGitHubPullRequestByID(ctx, a.PullRequestID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return w.skip(ctx, a, "source pull request no longer exists")
		}
		return w.retryOrFail(ctx, a, fmt.Errorf("load source pull request: %w", err))
	}
	installations, err := w.h.Queries.ListGitHubInstallationsByWorkspace(ctx, a.WorkspaceID)
	if err != nil {
		return w.retryOrFail(ctx, a, fmt.Errorf("list installations for workspace: %w", err))
	}
	boundToSourceInstallation := false
	for _, inst := range installations {
		if inst.InstallationID == pr.InstallationID {
			boundToSourceInstallation = true
			break
		}
	}
	if !boundToSourceInstallation {
		return w.skip(ctx, a, "source installation no longer bound to workspace")
	}

	if _, err := w.h.Queries.GetIssuePullRequestLink(ctx, db.GetIssuePullRequestLinkParams{
		IssueID:       a.IssueID,
		PullRequestID: a.PullRequestID,
	}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return w.skip(ctx, a, "issue no longer linked to pull request")
		}
		return w.retryOrFail(ctx, a, fmt.Errorf("check issue-pr link: %w", err))
	}

	content := w.h.mergeAnnouncementCommentBody(ctx, a, issue)

	tx, err := w.h.TxStarter.Begin(ctx)
	if err != nil {
		return w.retryOrFail(ctx, a, fmt.Errorf("begin tx: %w", err))
	}
	defer tx.Rollback(ctx)
	qtx := w.h.Queries.WithTx(tx)

	created, err := qtx.CreateComment(ctx, db.CreateCommentParams{
		ID:          dbid.NewV7(),
		IssueID:     issue.ID,
		WorkspaceID: issue.WorkspaceID,
		AuthorType:  "system",
		AuthorID:    pgtype.UUID{Valid: true},
		Content:     content,
		Type:        "system",
		ParentID:    pgtype.UUID{Valid: false},
	})
	if err != nil {
		return w.retryOrFail(ctx, a, fmt.Errorf("create comment: %w", err))
	}
	comment := created.Comment()

	if _, err := qtx.CompleteGitHubMergeAnnouncementDelivery(ctx, db.CompleteGitHubMergeAnnouncementDeliveryParams{
		ID:         a.ID,
		LeaseToken: a.LeaseToken,
		CommentID:  comment.ID,
	}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Lease ownership changed mid-flight (another worker's retry
			// beat us here) — abandon this attempt without emitting an
			// error; rolling back the transaction also undoes the comment
			// insert above, so no duplicate is left behind.
			slog.Debug("merge announcement worker: lease ownership changed", "id", uuidToString(a.ID))
			return nil
		}
		// CHE-374 review round 2, item 3: a non-ErrNoRows failure here is a
		// transient DB error (the row is still leased by us — no ownership
		// race), not a terminal condition, so it must go through
		// retryOrFail like every other failure branch in this function.
		// Returning the raw error directly used to bypass attempt_count
		// entirely, so a persistently failing completion write would be
		// reclaimed and reattempted forever without ever tripping the
		// mergeAnnouncementWorkerMaxAttempts terminal-fail path.
		return w.retryOrFail(ctx, a, fmt.Errorf("complete merge announcement: %w", err))
	}

	if err := tx.Commit(ctx); err != nil {
		// Same reasoning as the completion-write branch above: a commit
		// failure is transient/infrastructure-level, not a reason to treat
		// the row as ineligible, so it must count toward attempt_count via
		// retryOrFail rather than escaping it.
		return w.retryOrFail(ctx, a, fmt.Errorf("commit merge announcement delivery: %w", err))
	}

	w.h.publish(protocol.EventCommentCreated, uuidToString(issue.WorkspaceID), "system", "", map[string]any{
		"comment":             commentToResponse(comment, nil, nil),
		"issue_title":         issue.Title,
		"issue_assignee_type": textToPtr(issue.AssigneeType),
		"issue_assignee_id":   uuidToPtr(issue.AssigneeID),
		"issue_status":        issue.Status,
		"issue_revision":      created.IssueRevision,
	})
	return nil
}

func (w *MergeAnnouncementWorker) skip(ctx context.Context, a db.GithubMergeAnnouncement, reason string) error {
	_, err := w.h.Queries.SkipGitHubMergeAnnouncement(ctx, db.SkipGitHubMergeAnnouncementParams{
		ID:         a.ID,
		LeaseToken: a.LeaseToken,
		LastError:  pgtype.Text{String: reason, Valid: true},
	})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("skip merge announcement: %w", err)
	}
	return nil
}

// retryOrFail records a sanitized (message-only, no stack/query text) error
// and defers the row for bounded exponential backoff, or marks it terminally
// failed after mergeAnnouncementWorkerMaxAttempts — visible to operators via
// ListGitHubMergeAnnouncementsByIssue (review condition A: named owner for
// exhausted-retry visibility is the issue's pull-requests read path).
func (w *MergeAnnouncementWorker) retryOrFail(ctx context.Context, a db.GithubMergeAnnouncement, cause error) error {
	sanitized := cause.Error()
	if a.AttemptCount+1 >= mergeAnnouncementWorkerMaxAttempts {
		_, err := w.h.Queries.FailGitHubMergeAnnouncement(ctx, db.FailGitHubMergeAnnouncementParams{
			ID:         a.ID,
			LeaseToken: a.LeaseToken,
			LastError:  pgtype.Text{String: sanitized, Valid: true},
		})
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("fail merge announcement after %v: %w", cause, err)
		}
		slog.Error("merge announcement worker: exhausted retries",
			"id", uuidToString(a.ID), "issue_id", uuidToString(a.IssueID), "error", sanitized)
		return nil
	}
	backoff := time.Second * time.Duration(1<<min(a.AttemptCount, 6))
	_, err := w.h.Queries.RetryGitHubMergeAnnouncement(ctx, db.RetryGitHubMergeAnnouncementParams{
		ID:          a.ID,
		LeaseToken:  a.LeaseToken,
		LastError:   pgtype.Text{String: sanitized, Valid: true},
		AvailableAt: pgtype.Timestamptz{Time: time.Now().Add(backoff), Valid: true},
	})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("retry merge announcement after %v: %w", cause, err)
	}
	slog.Warn("merge announcement worker: deferred",
		"id", uuidToString(a.ID), "attempt", a.AttemptCount+1, "backoff", backoff, "error", sanitized)
	return nil
}

// mergeAnnouncementOwnerName resolves the issue's assignee to an actual
// display name for the merge-announcement comment (CHE-374 review fix T3).
// It is deliberately separate from resolveAssigneeMentionLabel
// (issue_child_done.go): that helper renders a `[@x](mention://…)` link and
// its behavior is depended on by existing mention-dispatch callers, while
// this text is plain prose (see mergeAnnouncementCommentBody's no-mention
// rationale) and additionally needs to cover the "member" assignee type,
// which resolveAssigneeMentionLabel does not. Returns "Unassigned" when the
// issue has no assignee, and falls back to the raw assignee type discriminator
// only if the referenced row can no longer be loaded (e.g. a deleted member).
func (h *Handler) mergeAnnouncementOwnerName(ctx context.Context, workspaceID pgtype.UUID, assigneeType string, assigneeID pgtype.UUID) string {
	switch assigneeType {
	case "agent":
		agent, err := h.Queries.GetAgentInWorkspace(ctx, db.GetAgentInWorkspaceParams{
			ID:          assigneeID,
			WorkspaceID: workspaceID,
		})
		if err != nil {
			return "agent"
		}
		return sanitizeMentionLabel(agent.Name)
	case "squad":
		squad, err := h.Queries.GetSquadInWorkspace(ctx, db.GetSquadInWorkspaceParams{
			ID:          assigneeID,
			WorkspaceID: workspaceID,
		})
		if err != nil {
			return "squad"
		}
		return sanitizeMentionLabel(squad.Name)
	case "member":
		member, err := h.Queries.GetMember(ctx, assigneeID)
		if err != nil {
			return "member"
		}
		user, err := h.Queries.GetUser(ctx, member.UserID)
		if err != nil {
			return "member"
		}
		return sanitizeMentionLabel(user.Name)
	}
	return "Unassigned"
}

// mergeAnnouncementCommentBody renders the deterministic next-action comment
// text (01-CONTEXT.md D-04). It never interpolates PR title/body text as a
// mention: every dynamic value here is either a system-derived identifier
// (issue identifier, PR number/URL, commit SHA, merge time) or the issue's
// own persisted status/assignee — none of it is attacker-controlled PR text,
// so no `[@x](mention://…)` substring from a malicious PR title can ever
// reach this content.
//
// CHE-374 review fix T3: previously omitted the PR URL entirely, never
// rendered merged_at even though it was already stored, printed the raw
// assignee_type discriminator ("agent"/"member") instead of a name, and
// always claimed the PR "does not declare completion" even when this exact
// merge carried closing intent (in which case the issue's own advance-to-done
// gate is the thing that will resolve it, not this comment.
func (h *Handler) mergeAnnouncementCommentBody(ctx context.Context, a db.GithubMergeAnnouncement, issue db.Issue) string {
	owner := h.mergeAnnouncementOwnerName(ctx, issue.WorkspaceID, issue.AssigneeType.String, issue.AssigneeID)

	prRef := fmt.Sprintf("%s#%d", a.RepoOwner+"/"+a.RepoName, a.PrNumber)
	if a.HtmlUrl.Valid && a.HtmlUrl.String != "" {
		prRef = fmt.Sprintf("[%s](%s)", prRef, a.HtmlUrl.String)
	}

	mergedAtText := ""
	if a.MergedAt.Valid {
		mergedAtText = fmt.Sprintf(" at %s", a.MergedAt.Time.UTC().Format(time.RFC3339))
	}

	// close_intent is captured on the announcement row at link time (frozen
	// to what was true for this specific merge — see migration 469), so a
	// later edit to the link row can't change what a past merge's comment
	// says. Pre-migration rows have no captured value (Valid=false); those
	// fall back to the original, close-intent-agnostic wording rather than
	// asserting something about a merge we don't have the answer for.
	nextAction := "Next: continue the issue's remaining work; this PR does not declare completion."
	if a.CloseIntent.Valid {
		if a.CloseIntent.Bool {
			nextAction = "Next: this PR declared closing intent for this issue; the issue will auto-advance once no linked PR is still open."
		} else {
			nextAction = "Next: this PR did not declare closing intent for this issue; continue the issue's remaining work."
		}
	}

	return fmt.Sprintf(
		"PR %s merged%s — commit `%.12s`.\n\nStatus: %s. Owner: %s. %s",
		prRef, mergedAtText, a.MergeCommitSha, issue.Status, owner, nextAction,
	)
}

// ── Selected merge-announcement recovery (CHE-384/01-02 task 1) ────────────
//
// AnnounceMergeForIssue is an explicit, narrow write for a single already-
// merged PR that never produced a durable announcement — e.g. it merged
// before the announcement feature existed, or before this workspace had
// GitHub enabled. It is deliberately NOT a bulk scan or backfill (01-CONTEXT.md
// D-03): the caller names one issue and one PR URL, and every fact the
// resulting comment states (merge SHA, merge time) comes from a fresh
// authoritative GitHub API read, never from the request body. Retrying with
// the same issue+PR is idempotent — CreateGitHubMergeAnnouncement's identity
// index makes a second call a no-op that returns the first call's outcome.
var githubPRURLRe = regexp.MustCompile(`^https://github\.com/([^/]+)/([^/]+)/pull/(\d+)/?$`)

// parseGitHubPRURL extracts (owner, repo, number) from a github.com PR URL.
// Rejects anything else so a caller cannot point recovery at a different
// host or a non-PR path.
func parseGitHubPRURL(rawURL string) (owner, repo string, number int32, ok bool) {
	m := githubPRURLRe.FindStringSubmatch(rawURL)
	if m == nil {
		return "", "", 0, false
	}
	n, err := strconv.ParseInt(m[3], 10, 32)
	if err != nil {
		return "", "", 0, false
	}
	return m[1], m[2], int32(n), true
}

type announceMergeRequest struct {
	PRURL string `json:"pr_url"`
}

// AnnounceMergeResponse reports the outcome of one recovery call. Exactly one
// of the two ID fields is set: an already-delivered identity returns its
// existing CommentID (no new comment, no re-delivery); a newly recovered one
// returns AnnouncementID for the durable record this call created/delivered.
type AnnounceMergeResponse struct {
	Status         string  `json:"status"`
	AnnouncementID *string `json:"announcement_id,omitempty"`
	CommentID      *string `json:"comment_id,omitempty"`
	MergedAt       *string `json:"merged_at,omitempty"`
	MergeCommitSHA *string `json:"merge_commit_sha,omitempty"`
}

// AnnounceMergeForIssue handles POST /api/issues/{id}/pull-requests/merge-announcements.
func (h *Handler) AnnounceMergeForIssue(w http.ResponseWriter, r *http.Request) {
	issue, ok := h.loadIssueForUser(w, r, chi.URLParam(r, "id"))
	if !ok {
		return
	}

	var req announceMergeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	owner, repo, number, ok := parseGitHubPRURL(req.PRURL)
	if !ok {
		writeError(w, http.StatusBadRequest, "pr_url must be a github.com pull request URL")
		return
	}

	ctx := r.Context()

	enabled, err := h.githubEnabledForWorkspaceChecked(ctx, issue.WorkspaceID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to check github integration status")
		return
	}
	if !enabled {
		writeError(w, http.StatusConflict, "github integration is disabled for this workspace")
		return
	}

	pr, err := h.Queries.GetGitHubPullRequest(ctx, db.GetGitHubPullRequestParams{
		WorkspaceID: issue.WorkspaceID,
		RepoOwner:   owner,
		RepoName:    repo,
		PrNumber:    number,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "pull request is not mirrored in this workspace")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to load pull request")
		return
	}

	// The existing working link is required — recovery narrates "this PR
	// merged for this issue"; there is no announcement to recover for a PR
	// this issue was never linked to. Also captures close_intent for the
	// recovered row so its rendered comment matches the same wording a live
	// webhook delivery would have produced.
	link, err := h.Queries.GetIssuePullRequestLink(ctx, db.GetIssuePullRequestLinkParams{
		IssueID:       issue.ID,
		PullRequestID: pr.ID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "issue is not linked to this pull request")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to load issue-pull request link")
		return
	}

	// Revalidate the source installation is still bound to this workspace —
	// same reasoning as MergeAnnouncementWorker.ProcessNext: an App
	// authenticated fetch must not run using an installation this workspace
	// no longer owns.
	installations, err := h.Queries.ListGitHubInstallationsByWorkspace(ctx, issue.WorkspaceID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list installations")
		return
	}
	boundToSourceInstallation := false
	for _, inst := range installations {
		if inst.InstallationID == pr.InstallationID {
			boundToSourceInstallation = true
			break
		}
	}
	if !boundToSourceInstallation {
		writeError(w, http.StatusConflict, "source installation is no longer bound to this workspace")
		return
	}

	client := h.PRRefresh.Client()
	if !client.Enabled() {
		writeError(w, http.StatusServiceUnavailable, "github api client is not configured")
		return
	}
	identity, err := ghsnapshot.FetchPRMergeIdentity(ctx, client, pr.InstallationID, owner, repo, number)
	if err != nil {
		var rl *ghsnapshot.RateLimitError
		if errors.As(err, &rl) {
			writeError(w, http.StatusTooManyRequests, "github api rate limited, retry later")
			return
		}
		slog.Error("announce-merge: fetch merge identity", "error", err, "issue_id", uuidToString(issue.ID))
		writeError(w, http.StatusBadGateway, "failed to fetch authoritative merge identity from github")
		return
	}
	if !identity.Merged {
		writeError(w, http.StatusConflict, "pull request is not merged")
		return
	}

	mergedAt := parseGHTimeRequired(identity.MergedAt)
	created, err := h.Queries.CreateGitHubMergeAnnouncement(ctx, db.CreateGitHubMergeAnnouncementParams{
		WorkspaceID:    issue.WorkspaceID,
		Provider:       "github",
		RepositoryID:   identity.RepositoryID,
		RepoOwner:      owner,
		RepoName:       repo,
		PrNumber:       number,
		PullRequestID:  pr.ID,
		IssueID:        issue.ID,
		EventKind:      "merged",
		DeliveryGuid:   pgtype.Text{}, // no webhook delivery — this is a reconciliation recovery, not a redelivery
		MergeCommitSha: identity.MergeCommitSHA,
		MergedAt:       mergedAt,
		HtmlUrl:        strToTextPtr(identity.HTMLURL),
		CloseIntent:    pgtype.Bool{Bool: link.CloseIntent, Valid: true},
	})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusInternalServerError, "failed to record merge announcement")
		return
	}

	var record db.GithubMergeAnnouncement
	if err == nil {
		record = created
	} else {
		// ON CONFLICT DO NOTHING: an announcement for this exact identity
		// already exists (an earlier webhook, or an earlier recovery call).
		// Read it back rather than treating "already have one" as an error —
		// idempotent retry per 01-02-PLAN.md task 1.
		existing, getErr := h.Queries.GetGitHubMergeAnnouncementByIdentity(ctx, db.GetGitHubMergeAnnouncementByIdentityParams{
			WorkspaceID:  issue.WorkspaceID,
			Provider:     "github",
			RepositoryID: identity.RepositoryID,
			PrNumber:     number,
			IssueID:      issue.ID,
			EventKind:    "merged",
		})
		if getErr != nil {
			writeError(w, http.StatusInternalServerError, "failed to read existing merge announcement")
			return
		}
		record = existing
	}

	if record.Status == "delivered" {
		writeJSON(w, http.StatusOK, AnnounceMergeResponse{
			Status:         "delivered",
			AnnouncementID: uuidToPtr(record.ID),
			CommentID:      uuidToPtr(record.CommentID),
			MergedAt:       timestampToPtr(record.MergedAt),
			MergeCommitSHA: mergeAnnounceStrPtr(record.MergeCommitSha),
		})
		return
	}

	// Newly created (or a pre-existing pending/failed/skipped record):
	// deliver it now via DeliverByID — never ProcessNext's unfiltered global
	// claim (CHE-384/01-02 review fix). ProcessNext claims the OLDEST pending
	// row anywhere in the deployment; under any deployment-wide backlog it
	// would deliver an unrelated workspace's announcement while leaving this
	// exact record untouched, and this handler would then read that
	// unrelated delivery back as if it were the caller's own outcome.
	// DeliverByID's WHERE id = ... makes "deliver THIS record" structural
	// rather than a race against the rest of the queue.
	if _, procErr := h.MergeAnnouncementWorker.DeliverByID(ctx, record.ID); procErr != nil {
		slog.Error("announce-merge: deliver", "error", procErr, "announcement_id", uuidToString(record.ID))
	}

	final, err := h.Queries.GetGitHubMergeAnnouncementByIdentity(ctx, db.GetGitHubMergeAnnouncementByIdentityParams{
		WorkspaceID:  issue.WorkspaceID,
		Provider:     "github",
		RepositoryID: identity.RepositoryID,
		PrNumber:     number,
		IssueID:      issue.ID,
		EventKind:    "merged",
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to read merge announcement after delivery")
		return
	}

	resp := AnnounceMergeResponse{
		Status:         final.Status,
		AnnouncementID: uuidToPtr(final.ID),
		MergedAt:       timestampToPtr(final.MergedAt),
		MergeCommitSHA: mergeAnnounceStrPtr(final.MergeCommitSha),
	}
	status := http.StatusAccepted
	if final.Status == "delivered" {
		status = http.StatusOK
		resp.CommentID = uuidToPtr(final.CommentID)
	}
	writeJSON(w, status, resp)
}

func strToTextPtr(s string) pgtype.Text {
	if s == "" {
		return pgtype.Text{}
	}
	return pgtype.Text{String: s, Valid: true}
}

func mergeAnnounceStrPtr(s string) *string { return &s }
