package handler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
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
// shape as WebhookDeliveryWorker.ProcessNext).
func (w *MergeAnnouncementWorker) ProcessNext(ctx context.Context) (bool, error) {
	a, err := w.h.Queries.ClaimPendingGitHubMergeAnnouncement(ctx)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("claim pending merge announcement: %w", err)
	}

	issue, err := w.h.Queries.GetIssue(ctx, a.IssueID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// The issue this announcement targeted is gone. Nothing to
			// attach a comment to and nothing to retry toward.
			return true, w.skip(ctx, a, "issue not found")
		}
		return true, w.retryOrFail(ctx, a, fmt.Errorf("load issue: %w", err))
	}

	content := w.h.mergeAnnouncementCommentBody(ctx, a, issue)

	tx, err := w.h.TxStarter.Begin(ctx)
	if err != nil {
		return true, w.retryOrFail(ctx, a, fmt.Errorf("begin tx: %w", err))
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
		return true, w.retryOrFail(ctx, a, fmt.Errorf("create comment: %w", err))
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
			return true, nil
		}
		return true, fmt.Errorf("complete merge announcement: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return true, fmt.Errorf("commit merge announcement delivery: %w", err)
	}

	w.h.publish(protocol.EventCommentCreated, uuidToString(issue.WorkspaceID), "system", "", map[string]any{
		"comment":             commentToResponse(comment, nil, nil),
		"issue_title":         issue.Title,
		"issue_assignee_type": textToPtr(issue.AssigneeType),
		"issue_assignee_id":   uuidToPtr(issue.AssigneeID),
		"issue_status":        issue.Status,
		"issue_revision":      created.IssueRevision,
	})
	return true, nil
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
