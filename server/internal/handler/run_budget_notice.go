package handler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/inbox"
	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// onRunBudgetEvent blocks the issue when the budget is spent, then writes the
// run-budget notice for the admission or refusal (CHE-2297 L1a).
func (h *Handler) onRunBudgetEvent(ctx context.Context, e service.RunBudgetEvent) {
	if e.Kind == service.RunBudgetKindExhausted {
		h.blockExhaustedIssue(ctx, e)
	}
	h.writeRunBudgetNotice(ctx, e)
}

// writeRunBudgetNotice writes one system comment and one inbox item per
// (issue, budget window, kind). The claim row, the comment and the inbox items
// share one transaction: the inserting transaction alone writes the notice, and
// a failed write rolls the claim back so the next event retries it.
func (h *Handler) writeRunBudgetNotice(ctx context.Context, e service.RunBudgetEvent) {
	issue, err := h.Queries.GetIssue(ctx, e.IssueID)
	if err != nil {
		slog.Warn("run budget: load issue for notice failed", "err", err, "issue_id", uuidToString(e.IssueID))
		return
	}
	content, title := h.runBudgetNoticeText(ctx, issue, e)
	var comment db.Comment
	var revision int64
	var items []db.InboxItem
	err = h.withTx(ctx, func(qtx *db.Queries) error {
		noticeID, err := qtx.ClaimRunBudgetNotice(ctx, db.ClaimRunBudgetNoticeParams{
			ID: dbid.NewV7(), WorkspaceID: issue.WorkspaceID, IssueID: issue.ID,
			WindowID: e.Since.UTC().Format(time.RFC3339Nano), Kind: e.Kind,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("claim notice: %w", err)
		}
		created, err := qtx.CreateComment(ctx, db.CreateCommentParams{
			ID: dbid.NewV7(), IssueID: issue.ID, WorkspaceID: issue.WorkspaceID,
			AuthorType: "system", AuthorID: pgtype.UUID{Valid: true}, Content: content, Type: "system",
		})
		if err != nil {
			return fmt.Errorf("create comment: %w", err)
		}
		comment, revision = created.Comment(), created.IssueRevision
		if err := qtx.SetRunBudgetNoticeComment(ctx, db.SetRunBudgetNoticeCommentParams{ID: noticeID, CommentID: comment.ID}); err != nil {
			return fmt.Errorf("set notice comment: %w", err)
		}
		recipients, err := runBudgetRecipients(ctx, qtx, issue)
		if err != nil {
			return fmt.Errorf("recipients: %w", err)
		}
		for _, userID := range recipients {
			item, inserted, err := inbox.CreateIfEligible(ctx, qtx, db.CreateInboxItemParams{
				ID: dbid.NewV7(), WorkspaceID: issue.WorkspaceID, RecipientType: "member", RecipientID: userID,
				Type: "run_budget", Severity: "action_required", IssueID: issue.ID, Title: title,
				Body:      pgtype.Text{String: content, Valid: true},
				ActorType: pgtype.Text{String: "system", Valid: true},
				Details:   []byte(fmt.Sprintf(`{"kind":"run_budget","notice":%q}`, e.Kind)),
			})
			if err != nil {
				return fmt.Errorf("inbox item: %w", err)
			}
			if inserted {
				items = append(items, item)
			}
		}
		return nil
	})
	if err != nil {
		slog.Warn("run budget: write notice failed", "err", err, "issue_id", uuidToString(issue.ID), "kind", e.Kind)
		return
	}
	if !comment.ID.Valid {
		return
	}
	workspaceID := uuidToString(issue.WorkspaceID)
	h.publish(protocol.EventCommentCreated, workspaceID, "system", "", map[string]any{
		"comment":             commentToResponse(comment, nil, nil),
		"issue_title":         issue.Title,
		"issue_assignee_type": textToPtr(issue.AssigneeType),
		"issue_assignee_id":   uuidToPtr(issue.AssigneeID),
		"issue_status":        issue.Status,
		"issue_revision":      revision,
	})
	for _, item := range items {
		h.publish(protocol.EventInboxNew, workspaceID, "system", "", map[string]any{"item": inboxToResponse(item)})
	}
}

// runBudgetRecipients is the requester, else the member creator, else the
// workspace owners.
func runBudgetRecipients(ctx context.Context, q *db.Queries, issue db.Issue) ([]pgtype.UUID, error) {
	if issue.RequestedByID.Valid {
		return []pgtype.UUID{issue.RequestedByID}, nil
	}
	if issue.CreatorType == "member" && issue.CreatorID.Valid {
		return []pgtype.UUID{issue.CreatorID}, nil
	}
	return q.ListWorkspaceOwnerUserIDs(ctx, issue.WorkspaceID)
}

// runBudgetNoticeText reads the token figure best-effort: a failed read leaves
// it out rather than losing the notice.
func (h *Handler) runBudgetNoticeText(ctx context.Context, issue db.Issue, e service.RunBudgetEvent) (content, title string) {
	tokens := "unknown"
	if u, err := h.Queries.GetIssueUsageSummary(ctx, issue.ID); err == nil {
		tokens = fmt.Sprintf("%d (%d runs have not reported usage)", u.TotalInputTokens+u.TotalOutputTokens, u.UnreportedTaskCount)
	} else {
		slog.Warn("run budget: usage summary failed", "err", err, "issue_id", uuidToString(issue.ID))
	}
	if e.Kind == service.RunBudgetKindWarn80 {
		return fmt.Sprintf("This issue used %d of %d runs of its run budget. Tokens used so far (input and output): %s. When the budget is spent, nothing derived from this issue runs until a member moves its status.", e.Runs, e.Max, tokens),
			"Run budget at 80%"
	}
	return fmt.Sprintf("This issue used its run budget: %d of %d runs. Nothing derived from this issue runs now. Tokens used so far (input and output): %s. To reopen it, a member moves its status.", e.Runs, e.Max, tokens),
		"Run budget used up"
}
