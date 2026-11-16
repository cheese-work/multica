package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/commentguard"
	"github.com/multica-ai/multica/server/internal/decisionaction"
	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

// CHE-6248: with run_budget_auto on, a weekly run guard refusal follows the
// auto-approver's rules. A window with progress and no anomaly gets its
// allowance without a card; anything else keeps the card for a person. The
// daily envelope and the kill switch are the run budget's own.

// autoApproveRunGuard approves the issue's open refusal, or the refusal e
// describes when none is open, and closes its card as decided. It reports true
// when the allowance was granted and the kept trigger resumed. It reports false
// when the rules do not approve; the caller then leaves or opens the card. A
// failure fails closed the same way.
func (h *Handler) autoApproveRunGuard(ctx context.Context, issue db.Issue, e *service.WeeklyRunGuardTripped) bool {
	verdict, err := service.EvaluateRunGuardAuto(ctx, h.Queries, issue, time.Now(), decisionaction.RunGuardAllowRuns)
	if err != nil {
		slog.Warn("run guard auto: evaluate failed", "err", err, "issue_id", uuidToString(issue.ID))
		return false
	}
	if !verdict.Approve {
		return false
	}
	var refusalID pgtype.UUID
	var closed []decisionRequestResult
	var comment db.Comment
	var revision int64
	err = h.withPgxTx(ctx, func(tx pgx.Tx) error {
		refusalID, closed, comment, revision = pgtype.UUID{}, nil, db.Comment{}, 0
		q := h.Queries.WithTx(tx)
		// One approval at a time per workspace, so the envelope is not overspent.
		if err := q.LockRunBudgetAutoEnvelope(ctx, uuidToString(issue.WorkspaceID)); err != nil {
			return fmt.Errorf("lock envelope: %w", err)
		}
		// The card first, the refusal row second: the human approval locks them in that order.
		card, err := h.closeRunGuardCard(ctx, q, issue)
		if err != nil {
			return err
		}
		closed = card
		open, err := q.LockOpenWeeklyRunGuardRefusal(ctx, issue.ID)
		switch {
		case err == nil:
			refusalID = open.ID
		case errors.Is(err, pgx.ErrNoRows) && e != nil:
			row, err := q.InsertWeeklyRunGuardRefusal(ctx, db.InsertWeeklyRunGuardRefusalParams{
				ID: dbid.NewV7(), WorkspaceID: issue.WorkspaceID, IssueID: issue.ID, AgentID: e.Target.AgentID, Trigger: e.Target.Trigger,
				ActorUserID: e.Target.ActorUserID, TriggerCommentID: e.Target.TriggerCommentID,
				SourceWakeupID: e.Target.SourceWakeupID, SourceTaskID: e.Target.SourceTaskID, Runs: e.Runs,
			})
			if err != nil {
				return err
			}
			refusalID = row.ID
		case errors.Is(err, pgx.ErrNoRows):
			return errAutoNotApproved // a person answered it first
		default:
			return err
		}
		v, err := service.EvaluateRunGuardAuto(ctx, q, issue, time.Now(), decisionaction.RunGuardAllowRuns)
		if err != nil {
			return err
		}
		if !v.Approve {
			return errAutoNotApproved
		}
		decisionID := dbid.NewV7()
		if moved, err := q.ApproveWeeklyRunGuardRefusal(ctx, db.ApproveWeeklyRunGuardRefusalParams{ID: refusalID, IssueID: issue.ID, ActionID: decisionID}); err != nil {
			return err
		} else if moved == 0 {
			return errAutoNotApproved
		}
		if err := q.CreateWeeklyRunGuardAllowance(ctx, db.CreateWeeklyRunGuardAllowanceParams{
			ID: dbid.NewV7(), WorkspaceID: issue.WorkspaceID, IssueID: issue.ID, Runs: int32(v.Runs), ActionID: decisionID,
		}); err != nil {
			return err
		}
		details, err := json.Marshal(map[string]any{"auto": service.RunBudgetAutoRule, "runs": v.Runs})
		if err != nil {
			return err
		}
		if _, err := q.CreateActivity(ctx, db.CreateActivityParams{
			WorkspaceID: issue.WorkspaceID, IssueID: issue.ID, ActorType: pgtype.Text{String: "system", Valid: true},
			Action: "run_guard_auto_approved", Details: details,
		}); err != nil {
			return err
		}
		text := fmt.Sprintf("Weekly run guard: %s approved %d more runs for 7 days and resumed the refused run. The issue made progress in the window and showed no anomaly, and the workspace daily envelope has room. To stop auto-approvals, set the workspace setting `run_budget_auto` to `{\"enabled\": false}`.", service.RunBudgetAutoRule, v.Runs)
		created, err := q.CreateComment(ctx, db.CreateCommentParams{
			ID: dbid.NewV7(), IssueID: issue.ID, WorkspaceID: issue.WorkspaceID,
			AuthorType: "system", AuthorID: pgtype.UUID{Valid: true}, Content: text, Type: "system",
		})
		if err != nil {
			return fmt.Errorf("create comment: %w", err)
		}
		comment, revision = created.Comment(), created.IssueRevision
		return nil
	})
	if err != nil {
		if !errors.Is(err, errAutoNotApproved) {
			slog.Warn("run guard auto: approve failed, the card stays", "err", err, "issue_id", uuidToString(issue.ID))
		}
		return false
	}
	h.publishRunBudgetAudit(issue, comment, revision)
	for _, c := range closed {
		h.publishDecisionWithdrawal(uuidToString(issue.WorkspaceID), c)
	}
	h.resumeWeeklyRefusal(ctx, refusalID)
	return true
}

// closeRunGuardCard cancels the issue's open run guard card, if any, with the
// locks a withdrawal takes. The card ends as decided by the auto-approver.
func (h *Handler) closeRunGuardCard(ctx context.Context, q *db.Queries, issue db.Issue) ([]decisionRequestResult, error) {
	found, err := q.FindOpenWeeklyRunGuardCard(ctx, db.FindOpenWeeklyRunGuardCardParams{WorkspaceID: issue.WorkspaceID, IssueID: issue.ID})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	card, err := q.GetDecisionRequest(ctx, db.GetDecisionRequestParams{WorkspaceID: issue.WorkspaceID, ID: found.RequestID})
	if err != nil {
		return nil, err
	}
	if _, err := q.LockDecisionIssue(ctx, db.LockDecisionIssueParams{WorkspaceID: card.WorkspaceID, ID: card.IssueID}); err != nil {
		return nil, err
	}
	if _, err := q.LockDecisionSession(ctx, db.LockDecisionSessionParams{WorkspaceID: card.WorkspaceID, ID: card.ChatSessionID}); err != nil {
		return nil, err
	}
	root, err := commentguard.LockThreadForReplyAndLoadRoot(ctx, q, card.WorkspaceID, card.IssueCommentID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	current, err := q.LockDecisionRequest(ctx, db.LockDecisionRequestParams{WorkspaceID: card.WorkspaceID, ID: card.ID})
	if err != nil {
		return nil, err
	}
	if current.Status != "open" {
		return nil, nil
	}
	result, err := withdrawOpenDecision(ctx, q, current, root, "Approved by "+service.RunBudgetAutoRule+": the issue made progress and showed no anomaly.", "system", pgtype.UUID{Valid: true}, pgtype.UUID{}, pgtype.UUID{})
	if err != nil {
		return nil, err
	}
	return []decisionRequestResult{result}, nil
}

// sweepRunGuardAuto re-evaluates every open refusal: a card that waited while
// the auto-approver was off, or whose window has since made progress, is
// approved and closed. Rules that do not approve leave the card as it is.
func (h *Handler) sweepRunGuardAuto(ctx context.Context) {
	rows, err := h.Queries.ListOpenWeeklyRunGuardRefusals(ctx)
	if err != nil {
		slog.Warn("run guard auto: list open refusals failed", "err", err)
		return
	}
	for _, row := range rows {
		issue, err := h.Queries.GetIssue(ctx, row.IssueID)
		if err != nil {
			continue
		}
		h.autoApproveRunGuard(ctx, issue, nil)
	}
}

// warnWeeklyRunGuard records a warn-mode would-be refusal once per issue and
// ICT day: an activity row the brief reads. Nothing is refused or opened.
func (h *Handler) warnWeeklyRunGuard(ctx context.Context, issue db.Issue, details []byte) error {
	return h.withPgxTx(ctx, func(tx pgx.Tx) error {
		q := h.Queries.WithTx(tx)
		_, err := q.ClaimWeeklyRunGuardIncident(ctx, db.ClaimWeeklyRunGuardIncidentParams{
			ID: dbid.NewV7(), WorkspaceID: issue.WorkspaceID, IssueID: issue.ID,
			WindowID: "warn:" + time.Now().In(ictLocation).Format("2006-01-02"),
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		_, err = q.CreateActivity(ctx, db.CreateActivityParams{
			WorkspaceID: issue.WorkspaceID, IssueID: issue.ID, ActorType: pgtype.Text{String: "system", Valid: true},
			Action: weeklyRunGuardWarnedAction, Details: details,
		})
		return err
	})
}
