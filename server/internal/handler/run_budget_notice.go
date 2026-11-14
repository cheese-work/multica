package handler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/inbox"
	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
	"github.com/multica-ai/multica/server/pkg/protocol"
	"github.com/multica-ai/multica/server/pkg/redact"
)

// runBudgetCommitWait bounds how long a warning waits, after its transaction
// has committed, for the committed run count to show the admission. Atomic:
// tests shorten it while hook goroutines from earlier admissions still read it.
var runBudgetCommitWait atomic.Int64

func init() { runBudgetCommitWait.Store(int64(10 * time.Second)) }

var (
	// runBudgetTxWait is a safety cap on following an admission's open
	// transaction; a transaction open this long is a leak, not a slow commit.
	runBudgetTxWait = 10 * time.Minute
)

// onRunBudgetEvent writes the 80% warning once the admission that raised it
// committed (CHE-2297 L1a).
func (h *Handler) onRunBudgetEvent(ctx context.Context, e service.RunBudgetEvent) {
	if e.Kind != service.RunBudgetKindWarn80 || !h.admissionCommitted(ctx, e) {
		return
	}
	_ = h.writeRunBudgetNoticeFrom(ctx, e, hookRef{}, nil, func(ctx context.Context, q *db.Queries) string { return h.warn80Text(ctx, q, e) })
}

// warn80Text words the 80% warning with the numbers and the one command that
// asks for more (CHE-5503). Without a readable budget it states the numbers only.
func (h *Handler) warn80Text(ctx context.Context, q *db.Queries, e service.RunBudgetEvent) string {
	plain := fmt.Sprintf("Run budget: %d of %d runs used.", e.Runs, e.Max)
	issue, err := q.GetIssue(ctx, e.IssueID)
	if err != nil {
		return plain
	}
	snap, err := service.LoadRunBudgetSnapshot(ctx, q, issue)
	if err != nil {
		return plain
	}
	next, err := h.loadRunBudgetNextStep(ctx, q, issue, snap)
	if err != nil || next.NextCommand == "" {
		return plain
	}
	return fmt.Sprintf("Run budget: %d of %d runs used on %s (workflow %s, cap %d). Ask for more: %s", e.Runs, e.Max, next.Identifier, next.WorkflowName, e.Max, next.NextCommand)
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
	deadline := time.Now().Add(time.Duration(runBudgetCommitWait.Load()))
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
// runBudgetBeforeNotice runs after the notice's reads and before its
// transaction. Tests use it to land a budget write in that window.
var runBudgetBeforeNotice = func() {}

func (h *Handler) writeRunBudgetNotice(ctx context.Context, e service.RunBudgetEvent, text string) {
	_ = h.writeRunBudgetNoticeFrom(ctx, e, hookRef{}, nil, func(context.Context, *db.Queries) string { return text })
}

// writeRunBudgetNoticeFrom is writeRunBudgetNotice with the text worded inside
// the transaction, under the budget lock, by a function of the transaction's
// queries. A non-nil recovery run is claimed in the same transaction.
//
// A valid hook id is the refusal whose hook this notice finishes (tx N): the
// hook advances to done in the notice's transaction, and a refusal that no
// longer applies returns *hookIneligible, with nothing written. Without one, a
// lifted refusal is dropped silently.
func (h *Handler) writeRunBudgetNoticeFrom(ctx context.Context, e service.RunBudgetEvent, hook hookRef, recovery *service.BudgetRecoveryRun, text func(context.Context, *db.Queries) string) error {
	issue, err := h.Queries.GetIssue(ctx, e.IssueID)
	if err != nil {
		slog.Warn("run budget: load issue for notice failed", "err", err, "issue_id", uuidToString(e.IssueID))
		return err
	}
	// An anomalous window goes to the day's batch card, not a per-issue one.
	if e.Kind == service.RunBudgetKindExhausted && h.escalateRunBudgetWindow(ctx, issue, e, recovery) {
		// The escalation claimed the window's notice, so a replay lands here
		// again and finds it claimed: retiring the hook once is enough.
		if hook.id.Valid {
			return h.Queries.RetireRunBudgetRefusalHook(ctx, db.RetireRunBudgetRefusalHookParams{ID: hook.id, Generation: hook.generation, Reason: "escalated"})
		}
		return nil
	}
	usage := h.runBudgetUsageNote(ctx, issue)
	recent := ""
	if e.Kind == service.RunBudgetKindExhausted {
		recent = h.runBudgetRecentRunsNote(ctx, issue)
	}
	runBudgetBeforeNotice()
	var content, title string
	var comment db.Comment
	var revision int64
	var items []db.InboxItem
	var decisions []decisionRequestResult
	recovered := false
	err = h.withPgxTx(ctx, func(tx pgx.Tx) error {
		qtx := h.Queries.WithTx(tx)
		noticeID, err := qtx.ClaimRunBudgetNotice(ctx, db.ClaimRunBudgetNoticeParams{
			ID: dbid.NewV7(), WorkspaceID: issue.WorkspaceID, IssueID: issue.ID,
			WindowID: e.Since.UTC().Format(time.RFC3339Nano), Kind: e.Kind,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			if !hook.id.Valid {
				return nil
			}
			// A notice for this window exists already: nothing is left to write.
			return h.retireHookInTx(ctx, qtx, hook, "notice_exists")
		}
		if err != nil {
			return fmt.Errorf("claim notice: %w", err)
		}
		recipients, err := runBudgetRecipients(ctx, qtx, issue)
		if err != nil {
			return fmt.Errorf("recipients: %w", err)
		}
		// Decision locks come before the comment, which locks the issue: the
		// order every decision transaction takes.
		var plan runBudgetDecisionPlan
		if e.Kind == service.RunBudgetKindExhausted {
			owners, err := runBudgetCardRecipients(ctx, qtx, issue, recipients)
			if err != nil {
				return fmt.Errorf("card recipients: %w", err)
			}
			if plan, err = h.planRunBudgetDecisions(ctx, qtx, issue, owners); err != nil {
				return fmt.Errorf("plan decisions: %w", err)
			}
		}
		// The budget lock is the issue lock the comment takes next, so it comes
		// after the decision locks too. Under it, a top-up or grant that
		// committed first has ended this window's refusal: no comment, no
		// decision, and the claim rolls back with the transaction.
		if e.Kind == service.RunBudgetKindExhausted {
			if err := runBudgetRefusalStillHolds(ctx, qtx, e); err != nil {
				if hook.id.Valid && errors.Is(err, errRefusalLifted) {
					return &hookIneligible{reason: "budget_restored"}
				}
				return err
			}
			if hook.id.Valid {
				// Read under the row lock: a requester that joined the hook before
				// this point is in the taint the recovery run carries.
				taint, err := lockHookStep(ctx, qtx, hook.id, hook.generation, "blocked")
				if err != nil {
					return err
				}
				if recovery != nil {
					// The issue lock is held, so an assignment that committed first is read here.
					assigned, err := qtx.GetIssueRunTaint(ctx, e.IssueID)
					if err != nil {
						return err
					}
					recovery.WithTaint(append(taint, assigned...))
				}
				if err := qtx.SetRunBudgetRefusalHook(ctx, db.SetRunBudgetRefusalHookParams{ID: hook.id, HookPhase: "done"}); err != nil {
					return fmt.Errorf("finish hook: %w", err)
				}
			}
		}
		// The recovery run debits the ledger, so it commits with the notice
		// that states the balance: both or neither. A run that cannot be
		// inserted undoes only itself (Claim's savepoint): the notice still
		// goes out, worded without a debit that did not commit, and the
		// window's claim stays free for a later refusal.
		if recovery != nil && e.Kind == service.RunBudgetKindExhausted {
			switch err := recovery.Claim(ctx, tx, e.Since); {
			case err == nil:
				recovered = true
			case errors.Is(err, service.ErrRecoveryClaimed), errors.Is(err, service.ErrRecoveryBlockEnded):
			case errors.Is(err, service.ErrRecoveryWindowLifted):
				if hook.id.Valid {
					return &hookIneligible{reason: "budget_restored"}
				}
				return errRefusalLifted
			default:
				slog.Warn("run budget: recovery run not enqueued", "err", err, "issue_id", uuidToString(issue.ID))
			}
		}
		// Worded under the budget lock, so a command in it describes the ledger
		// this transaction commits against.
		content, title = runBudgetNoticeText(usage, recent, e, text(ctx, qtx))
		// The platform opens the top-up card itself and the notice links it:
		// one card, one passkey click, no agent run (CHE-5303).
		if e.Kind == service.RunBudgetKindExhausted && len(plan.lanes) > 0 {
			links, opened, err := h.openRunBudgetActionCards(ctx, qtx, issue, plan, "top_up", 0, e.Since.UTC().Format(time.RFC3339Nano), runBudgetRefusalAsk(issue))
			if err != nil {
				return fmt.Errorf("open top-up card: %w", err)
			}
			decisions = opened
			if len(opened) > 0 {
				if err := recordRunBudgetAsk(ctx, qtx, issue, "system", pgtype.UUID{}, runBudgetTopUpType, e.Since.UTC().Format(time.RFC3339Nano)); err != nil {
					return fmt.Errorf("record ask: %w", err)
				}
			}
			content += "\n\n[Approve the top-up](" + links[0].ApproveURL + ")"
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
	if errors.Is(err, errRefusalLifted) {
		return nil
	}
	if err != nil {
		var ineligible *hookIneligible
		if !errors.Is(err, errHookAdvanced) && !errors.As(err, &ineligible) {
			slog.Warn("run budget: write notice failed", "err", err, "issue_id", uuidToString(issue.ID), "kind", e.Kind)
		}
		return err
	}
	if recovered {
		recovery.Published(ctx)
	}
	if !comment.ID.Valid {
		return nil
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
	return nil
}

// retireHookInTx retires an unfinished hook in the caller's transaction and
// reports it as finished.
func (h *Handler) retireHookInTx(ctx context.Context, q *db.Queries, hook hookRef, reason string) error {
	return q.RetireRunBudgetRefusalHook(ctx, db.RetireRunBudgetRefusalHookParams{ID: hook.id, Generation: hook.generation, Reason: reason})
}

// runBudgetRefusalStillHolds takes the budget lock and reports errRefusalLifted
// when the exhausted window is no longer the current, spent one.
func runBudgetRefusalStillHolds(ctx context.Context, q *db.Queries, e service.RunBudgetEvent) error {
	if _, err := q.LockIssueRunBudget(ctx, e.IssueID); err != nil {
		return fmt.Errorf("lock budget: %w", err)
	}
	still, err := service.RunBudgetStillRefuses(ctx, q, e.IssueID, e.Since)
	if err != nil {
		return err
	}
	if !still {
		return errRefusalLifted
	}
	return nil
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

// runBudgetUsageNote is the token figure a notice carries, best-effort: a
// failed read leaves it as "unknown" rather than losing the notice. It reads on
// the pool, so it runs before the notice's transaction opens.
func (h *Handler) runBudgetUsageNote(ctx context.Context, issue db.Issue) string {
	u, err := h.Queries.GetIssueUsageSummary(ctx, issue.ID)
	if err != nil {
		slog.Warn("run budget: usage summary failed", "err", err, "issue_id", uuidToString(issue.ID))
		return "unknown"
	}
	return fmt.Sprintf("%d (%d runs have not reported usage)", u.TotalInputTokens+u.TotalOutputTokens, u.UnreportedTaskCount)
}

// recentRunsShown is how many finished runs the exhaustion notice lists.
const recentRunsShown = 3

// recentRunSnippetLen caps one run's result or error in the notice.
const recentRunSnippetLen = 200

// runBudgetRecentRunsNote lists the issue's newest finished runs with their
// outcome (redacted, as run output may hold credentials), so whoever reads the exhaustion notice sees what the budget was spent
// on without opening the history. Best effort like the token figure: a failed
// read drops the list, never the notice. It reads on the pool, before the
// notice's transaction opens.
func (h *Handler) runBudgetRecentRunsNote(ctx context.Context, issue db.Issue) string {
	runs, err := h.Queries.ListRecentIssueRunResults(ctx, db.ListRecentIssueRunResultsParams{IssueID: issue.ID, MaxRuns: recentRunsShown})
	if err != nil {
		slog.Warn("run budget: recent runs failed", "err", err, "issue_id", uuidToString(issue.ID))
		return ""
	}
	if len(runs) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "\n\nLast %d runs, newest first:", len(runs))
	for _, run := range runs {
		detail := strings.TrimSpace(run.Error)
		if detail == "" {
			detail = strings.TrimSpace(run.Output)
		}
		if detail == "" {
			detail = "no result recorded"
		}
		// Redact the whole text before cutting it: a credential that straddles the
		// cut would otherwise survive as a prefix no pattern matches.
		detail = truncateRunes(redact.Text(detail), recentRunSnippetLen)
		fmt.Fprintf(&b, "\n- %s, %s: %s", run.AgentName, run.Status, strings.Join(strings.Fields(detail), " "))
	}
	return b.String()
}

// runBudgetNoticeText appends the token figure and the recent runs to the
// notice text.
func runBudgetNoticeText(tokens, recentRuns string, e service.RunBudgetEvent, text string) (content, title string) {
	sep := " "
	if e.Kind == service.RunBudgetKindWarn80 {
		sep = "\n\n" // the text ends in a command
	}
	content = fmt.Sprintf("%s%sTokens used so far (input and output): %s.%s", text, sep, tokens, recentRuns)
	switch e.Kind {
	case service.RunBudgetKindWarn80:
		return content, "Run budget at 80%"
	}
	return content, "Run budget used up"
}
