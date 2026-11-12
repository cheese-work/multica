package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/decisiontx"
	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

var (
	// errAutoHandled: another transaction already claimed this window's notice.
	errAutoHandled = errors.New("run budget auto: window already handled")
	// errAutoNotApproved: under the locks the window no longer qualifies.
	errAutoNotApproved = errors.New("run budget auto: window not approved")
)

// anomalyBatchFingerprint is the one escalation card a workspace gets per ICT day.
func anomalyBatchFingerprint(now time.Time) string {
	return serverDecisionFingerprint + "run_budget_anomaly_batch:" + service.RunBudgetAutoDayStart(now).Format("2006-01-02")
}

var runBudgetAnomalyText = map[string]string{
	service.RunBudgetAnomalyLoop:       "the same failing check or error repeated in 3 or more runs of the window",
	service.RunBudgetAnomalyNoop:       "80% or more of the window's runs delivered nothing",
	service.RunBudgetAnomalySpike:      "tokens per run are over 3 times the workspace median",
	service.RunBudgetAnomalyChurn:      "3 or more top-ups on this issue in 24 hours",
	service.RunBudgetAnomalyOpenAIPool: "an OpenAI-pool agent ran while the OpenAI cut is in force",
}

// autoRunBudgetWindow lets the auto-approver act on a spent window before a
// person is asked. It reports true when it settled the window (approved,
// escalated, or another transaction already did) and false when the per-issue
// card path must run: the kill switch is off, the window made no progress, the
// daily envelope is spent, or the auto path failed. Failing closed hands the
// window to a person, never approves it.
func (h *Handler) autoRunBudgetWindow(ctx context.Context, issue db.Issue, e service.RunBudgetEvent) bool {
	verdict, err := service.EvaluateRunBudgetAuto(ctx, h.Queries, issue, e.Since, time.Now())
	if err != nil {
		slog.Warn("run budget auto: evaluate failed", "err", err, "issue_id", uuidToString(issue.ID))
		return false
	}
	var handled bool
	switch {
	case verdict.Approve:
		handled, err = h.autoApproveRunBudget(ctx, issue, e, verdict)
	case verdict.Anomaly != "":
		handled, err = h.escalateRunBudgetAnomaly(ctx, issue, e, verdict.Anomaly)
	default:
		return false
	}
	if err != nil {
		slog.Warn("run budget auto: action failed, falling back to a card", "err", err, "issue_id", uuidToString(issue.ID))
	}
	return handled
}

func (h *Handler) autoApproveRunBudget(ctx context.Context, issue db.Issue, e service.RunBudgetEvent, verdict service.RunBudgetAutoVerdict) (bool, error) {
	details, err := json.Marshal(map[string]any{"auto": service.RunBudgetAutoRule, "runs": verdict.Runs})
	if err != nil {
		return false, err
	}
	var comment db.Comment
	var revision int64
	_, err = h.topUpRunBudget(ctx, issue, "system", pgtype.UUID{}, details, func(ctx context.Context, q *db.Queries) error {
		noticeID, err := claimRunBudgetNotice(ctx, q, issue, e)
		if err != nil {
			return err
		}
		// One approval at a time per workspace, so the envelope is not overspent.
		if err := q.LockRunBudgetAutoEnvelope(ctx, uuidToString(issue.WorkspaceID)); err != nil {
			return fmt.Errorf("lock envelope: %w", err)
		}
		if err := runBudgetRefusalStillHolds(ctx, q, e); err != nil {
			return err
		}
		v, err := service.EvaluateRunBudgetAuto(ctx, q, issue, e.Since, time.Now())
		if err != nil {
			return err
		}
		if !v.Approve {
			return errAutoNotApproved
		}
		text := fmt.Sprintf("Run budget: %s approved %d more runs. The last window made progress, showed no anomaly and the workspace daily envelope has room. To stop auto-approvals, set the workspace setting `run_budget_auto` to `{\"enabled\": false}`.", service.RunBudgetAutoRule, v.Runs)
		comment, revision, err = createRunBudgetAuditComment(ctx, q, issue, noticeID, text)
		return err
	})
	switch {
	case err == nil:
		h.publishRunBudgetAudit(issue, comment, revision)
		return true, nil
	case errors.Is(err, errAutoHandled), errors.Is(err, errRefusalLifted):
		return true, nil
	case errors.Is(err, errAutoNotApproved):
		return false, nil
	}
	return false, err
}

func (h *Handler) escalateRunBudgetAnomaly(ctx context.Context, issue db.Issue, e service.RunBudgetEvent, kind string) (bool, error) {
	var comment db.Comment
	var revision int64
	var decisions []decisionRequestResult
	err := h.withPgxTx(ctx, func(tx pgx.Tx) error {
		q := h.Queries.WithTx(tx)
		noticeID, err := claimRunBudgetNotice(ctx, q, issue, e)
		if err != nil {
			return err
		}
		// The batch card goes to the workspace owners: they alone see anomalies.
		recipients, err := q.ListWorkspaceOwnerUserIDs(ctx, issue.WorkspaceID)
		if err == nil && len(recipients) == 0 {
			recipients, err = runBudgetRecipients(ctx, q, issue)
		}
		if err != nil {
			return fmt.Errorf("recipients: %w", err)
		}
		// Decision locks first, then the budget lock the comment takes.
		plan, err := h.planRunBudgetDecisions(ctx, q, issue, recipients)
		if err != nil {
			return fmt.Errorf("plan decisions: %w", err)
		}
		if err := runBudgetRefusalStillHolds(ctx, q, e); err != nil {
			return err
		}
		details, err := json.Marshal(map[string]any{"kind": kind, "rule": service.RunBudgetAutoRule})
		if err != nil {
			return err
		}
		if _, err := q.CreateActivity(ctx, db.CreateActivityParams{
			WorkspaceID: issue.WorkspaceID, IssueID: issue.ID, ActorType: pgtype.Text{String: "system", Valid: true},
			Action: "run_budget_auto_escalated", Details: details,
		}); err != nil {
			return fmt.Errorf("record escalation: %w", err)
		}
		text := fmt.Sprintf("Run budget: %s did not approve a new window (%s: %s). A person decides; the owners get one batch card per day for these.", service.RunBudgetAutoRule, kind, runBudgetAnomalyText[kind])
		if comment, revision, err = createRunBudgetAuditComment(ctx, q, issue, noticeID, text); err != nil {
			return err
		}
		fingerprint := anomalyBatchFingerprint(time.Now())
		// The workspace lock planRunBudgetDecisions took serializes this check.
		opened, err := q.CountWorkspaceDecisionsByFingerprint(ctx, db.CountWorkspaceDecisionsByFingerprintParams{WorkspaceID: issue.WorkspaceID, Fingerprint: fingerprint})
		if err != nil || opened > 0 {
			return err
		}
		decisions, err = h.openRunBudgetAsk(ctx, q, issue, runBudgetAnomalyAsk(issue, comment, kind), fingerprint, plan)
		return err
	})
	switch {
	case err == nil:
		h.publishRunBudgetAudit(issue, comment, revision)
		h.publishRunBudgetDecisions(issue, decisions)
		return true, nil
	case errors.Is(err, errAutoHandled), errors.Is(err, errRefusalLifted):
		return true, nil
	}
	return false, err
}

// claimRunBudgetNotice takes the window's exhausted-notice claim, the key that
// lets one transaction alone settle a window.
func claimRunBudgetNotice(ctx context.Context, q *db.Queries, issue db.Issue, e service.RunBudgetEvent) (pgtype.UUID, error) {
	id, err := q.ClaimRunBudgetNotice(ctx, db.ClaimRunBudgetNoticeParams{
		ID: dbid.NewV7(), WorkspaceID: issue.WorkspaceID, IssueID: issue.ID,
		WindowID: e.Since.UTC().Format(time.RFC3339Nano), Kind: e.Kind,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return pgtype.UUID{}, errAutoHandled
	}
	if err != nil {
		return pgtype.UUID{}, fmt.Errorf("claim notice: %w", err)
	}
	return id, nil
}

func createRunBudgetAuditComment(ctx context.Context, q *db.Queries, issue db.Issue, noticeID pgtype.UUID, text string) (db.Comment, int64, error) {
	created, err := q.CreateComment(ctx, db.CreateCommentParams{
		ID: dbid.NewV7(), IssueID: issue.ID, WorkspaceID: issue.WorkspaceID,
		AuthorType: "system", AuthorID: pgtype.UUID{Valid: true}, Content: text, Type: "system",
	})
	if err != nil {
		return db.Comment{}, 0, fmt.Errorf("create comment: %w", err)
	}
	comment := created.Comment()
	if err := q.SetRunBudgetNoticeComment(ctx, db.SetRunBudgetNoticeCommentParams{ID: noticeID, CommentID: comment.ID}); err != nil {
		return db.Comment{}, 0, fmt.Errorf("set notice comment: %w", err)
	}
	return comment, created.IssueRevision, nil
}

func (h *Handler) publishRunBudgetAudit(issue db.Issue, comment db.Comment, revision int64) {
	h.publish(protocol.EventCommentCreated, uuidToString(issue.WorkspaceID), "system", "", map[string]any{
		"comment":             commentToResponse(comment, nil, nil),
		"issue_title":         issue.Title,
		"issue_assignee_type": textToPtr(issue.AssigneeType),
		"issue_assignee_id":   uuidToPtr(issue.AssigneeID),
		"issue_status":        issue.Status,
		"issue_revision":      revision,
	})
}

// runBudgetAnomalyAsk is the batch card: the first anomaly of the ICT day opens
// it; later ones only add their issue comment, and the brief counts them all.
func runBudgetAnomalyAsk(issue db.Issue, notice db.Comment, kind string) decisiontx.Ask {
	ask := runBudgetDecisionAsk(issue, notice)
	ask.ContextELI10 = fmt.Sprintf("The run-budget auto-approver held back a top-up on %q: %s. It never approves an anomaly.", issue.Title, runBudgetAnomalyText[kind])
	ask.AgentDid = "The platform refused a new run on this issue and did not approve a new window. This is the day's one card for such anomalies; later ones are only in each issue's comments and the brief."
	ask.StakesIfWrong = "If you reopen a looping or idle issue it can spend a full new window of runs and tokens. If you do nothing, the issue stays stopped."
	ask.Recommendation = "Read the issue's last comments. Reopen it only when you see why the anomaly is harmless."
	return ask
}

// runBudgetAutoBrief is the auto-approver's audit line for the brief: what it
// approved and escalated since a time. It is empty when it did nothing.
func (h *Handler) runBudgetAutoBrief(ctx context.Context, workspaceID pgtype.UUID, since time.Time) (string, error) {
	rows, err := h.Queries.ListRunBudgetAutoDecisionsSince(ctx, db.ListRunBudgetAutoDecisionsSinceParams{
		WorkspaceID: workspaceID, Since: pgtype.Timestamptz{Time: since, Valid: true},
	})
	if err != nil || len(rows) == 0 {
		return "", err
	}
	var approved, escalated int64
	var kinds []string
	for _, r := range rows {
		if r.Kind == "approved" {
			approved += r.N
			continue
		}
		escalated += r.N
		kinds = append(kinds, fmt.Sprintf("%s %d", r.Kind, r.N))
	}
	sort.Strings(kinds)
	line := fmt.Sprintf("Run budget %s: approved %d, escalated %d", service.RunBudgetAutoRule, approved, escalated)
	if len(kinds) > 0 {
		line += " (" + strings.Join(kinds, ", ") + ")"
	}
	return line + ".", nil
}
