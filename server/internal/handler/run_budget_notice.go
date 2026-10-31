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

var (
	// runBudgetCommitWait bounds how long a warning waits, after its transaction
	// has committed, for the committed run count to show the admission.
	runBudgetCommitWait = 10 * time.Second
	// runBudgetTxWait is a safety cap on following an admission's open
	// transaction; a transaction open this long is a leak, not a slow commit.
	runBudgetTxWait = 10 * time.Minute
)

// onRunBudgetEvent writes the 80% warning once the admission that raised it
// committed (CHE-2297 L1a).
func (h *Handler) onRunBudgetEvent(ctx context.Context, e service.RunBudgetEvent) {
	if e.Kind == service.RunBudgetKindWarn80Tokens {
		h.warnTokensAtEighty(ctx, e)
		return
	}
	if e.Kind != service.RunBudgetKindWarn80 || !h.admissionCommitted(ctx, e) {
		return
	}
	h.writeRunBudgetNotice(ctx, e, fmt.Sprintf("This issue used %d of %d runs of its run budget. When the budget is spent, nothing derived from this issue runs until a member moves its status.", e.Runs, e.Max))
}

// warnTokensAtEighty writes the token warning once the usage report that
// crossed 80% committed (CHE-2297 L2e). The report is the only event that
// spends tokens, so nothing else raises it; a report that rolled back, or that
// carries no transaction to follow, writes nothing and leaves the window's
// claim free for the real crossing.
func (h *Handler) warnTokensAtEighty(ctx context.Context, e service.RunBudgetEvent) {
	if e.Xid == 0 || !h.admissionTxFinished(ctx, e) {
		return
	}
	h.writeRunBudgetNotice(ctx, e, fmt.Sprintf("This issue has %d of %d tokens left of its token allowance. When the allowance is spent, nothing derived from this issue runs until a member moves its status.", e.Tokens, e.TokenBase))
}

// admissionCommitted reports whether the admission that raised a warning has
// committed. The guard fires before the task insert and its transaction finish,
// so the warning follows that transaction: it waits while it is open, drops the
// warning when it aborts, and then waits for the committed run count to reach
// the admission's own. A rejected or rolled-back admission never gets there: no
// false notice, and the window's warn80 claim stays free for the real 80%
// admission.
func (h *Handler) admissionCommitted(ctx context.Context, e service.RunBudgetEvent) bool {
	if e.Xid != 0 && !h.admissionTxFinished(ctx, e) {
		return false
	}
	since := pgtype.Timestamptz{Time: e.Since, Valid: true}
	deadline := time.Now().Add(runBudgetCommitWait)
	for {
		runs, err := h.Queries.CountIssueRunsSince(ctx, db.CountIssueRunsSinceParams{IssueID: e.IssueID, Since: since})
		if err != nil {
			slog.Warn("run budget: count committed runs failed", "err", err, "issue_id", uuidToString(e.IssueID))
			return false
		}
		if runs >= e.Runs {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// admissionTxFinished waits for the admission's transaction to leave "in
// progress" and reports whether it did not abort.
func (h *Handler) admissionTxFinished(ctx context.Context, e service.RunBudgetEvent) bool {
	deadline := time.Now().Add(runBudgetTxWait)
	for {
		status, err := h.Queries.XactStatus(ctx, e.Xid)
		if err != nil {
			slog.Warn("run budget: read admission transaction failed", "err", err, "issue_id", uuidToString(e.IssueID))
			return false
		}
		if status != "in progress" {
			return status != "aborted"
		}
		if !time.Now().Before(deadline) {
			slog.Warn("run budget: admission transaction still open, dropping warning", "issue_id", uuidToString(e.IssueID))
			return false
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// writeRunBudgetNotice writes one system comment and one inbox item per
// (issue, budget window, kind). The claim row, the comment and the inbox items
// share one transaction: the inserting transaction alone writes the notice, and
// a failed write rolls the claim back so the next event retries it.
func (h *Handler) writeRunBudgetNotice(ctx context.Context, e service.RunBudgetEvent, text string) {
	issue, err := h.Queries.GetIssue(ctx, e.IssueID)
	if err != nil {
		slog.Warn("run budget: load issue for notice failed", "err", err, "issue_id", uuidToString(e.IssueID))
		return
	}
	content, title := h.runBudgetNoticeText(ctx, issue, e, text)
	var comment db.Comment
	var revision int64
	var items []db.InboxItem
	var decisions []decisionRequestResult
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
		if e.Kind != service.RunBudgetKindExhausted {
			return nil
		}
		decisions, err = h.openRunBudgetDecisions(ctx, qtx, issue, e, comment, recipients)
		return err
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
	h.publishRunBudgetDecisions(issue, decisions)
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

// runBudgetNoticeText appends the token figure to the notice text, best-effort:
// a failed read leaves it as "unknown" rather than losing the notice.
func (h *Handler) runBudgetNoticeText(ctx context.Context, issue db.Issue, e service.RunBudgetEvent, text string) (content, title string) {
	tokens := "unknown"
	if u, err := h.Queries.GetIssueUsageSummary(ctx, issue.ID); err == nil {
		tokens = fmt.Sprintf("%d (%d runs have not reported usage)", u.TotalInputTokens+u.TotalOutputTokens, u.UnreportedTaskCount)
	} else {
		slog.Warn("run budget: usage summary failed", "err", err, "issue_id", uuidToString(issue.ID))
	}
	content = fmt.Sprintf("%s Tokens used so far (input and output): %s.", text, tokens)
	switch e.Kind {
	case service.RunBudgetKindWarn80:
		return content, "Run budget at 80%"
	case service.RunBudgetKindWarn80Tokens:
		return content, "Token allowance at 80%"
	}
	return content, "Run budget used up"
}
