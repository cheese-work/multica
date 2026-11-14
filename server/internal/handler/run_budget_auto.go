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
	"github.com/multica-ai/multica/server/internal/issuestatus"
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

// autoApproveExhausted lets the auto-approver settle a spent window before the
// issue is blocked: an approved window resumes the refused run in the same
// transaction as the top-up, so the issue never stops. It reports true when
// the window is settled. It reports false, and the block and notice path runs,
// when the kill switch is off, the window shows an anomaly or no progress, the
// envelope is spent, the issue is not waiting on an agent's run it can resume,
// or the auto path failed. Failing closed hands the window to a person.
func (h *Handler) autoApproveExhausted(ctx context.Context, e service.RunBudgetExhausted) bool {
	issue, ok := exhaustedIssueWith(ctx, h.Queries, e)
	if !ok || issue.Status == issuestatus.Blocked || issue.AssigneeType.String != "agent" || issue.AssigneeID != e.Target.AgentID {
		return false
	}
	event := service.RunBudgetEvent{IssueID: e.IssueID, Kind: service.RunBudgetKindExhausted, Runs: e.Runs, Max: e.Max, Since: e.Since}
	verdict, err := service.EvaluateRunBudgetAuto(ctx, h.Queries, issue, e.Since, time.Now())
	if err != nil {
		slog.Warn("run budget auto: evaluate failed", "err", err, "issue_id", uuidToString(issue.ID))
		return false
	}
	if !verdict.Approve {
		return false
	}
	handled, err := h.autoApproveRunBudget(ctx, issue, event, verdict, e.Status, e.Target)
	if err != nil {
		slog.Warn("run budget auto: approve failed, falling back to a card", "err", err, "issue_id", uuidToString(issue.ID))
	}
	return handled
}

// escalateRunBudgetWindow lets the auto-approver route an anomalous window to
// the day's batch card instead of a per-issue card. It reports true when it
// settled the window and false when the per-issue card path must run. recovery
// is the blocked issue's recovery run, claimed in the escalation's transaction.
func (h *Handler) escalateRunBudgetWindow(ctx context.Context, issue db.Issue, e service.RunBudgetEvent, recovery *service.BudgetRecoveryRun) bool {
	verdict, err := service.EvaluateRunBudgetAuto(ctx, h.Queries, issue, e.Since, time.Now())
	if err != nil {
		slog.Warn("run budget auto: evaluate failed", "err", err, "issue_id", uuidToString(issue.ID))
		return false
	}
	if verdict.Anomaly == "" {
		return false
	}
	handled, err := h.escalateRunBudgetAnomaly(ctx, issue, e, verdict.Anomaly, recovery)
	if err != nil {
		slog.Warn("run budget auto: escalation failed, falling back to a card", "err", err, "issue_id", uuidToString(issue.ID))
	}
	return handled
}

func (h *Handler) autoApproveRunBudget(ctx context.Context, issue db.Issue, e service.RunBudgetEvent, verdict service.RunBudgetAutoVerdict, refusedStatus string, target service.RunTarget) (bool, error) {
	details, err := json.Marshal(map[string]any{"auto": service.RunBudgetAutoRule, "runs": verdict.Runs})
	if err != nil {
		return false, err
	}
	var comment db.Comment
	var revision int64
	// Built before the transaction, as the workflow edges do.
	overlay := h.TaskService.PrepareIssueRunOverlay(ctx, issue, target.ActorUserID)
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
		text := fmt.Sprintf("Run budget: %s approved %d more runs and resumed the refused run. The last window made progress, showed no anomaly and the workspace daily envelope has room. To stop auto-approvals, set the workspace setting `run_budget_auto` to `{\"enabled\": false}`.", service.RunBudgetAutoRule, v.Runs)
		comment, revision, err = createRunBudgetAuditComment(ctx, q, issue, noticeID, text)
		return err
	}, func(ctx context.Context, q *db.Queries) (func(), error) {
		// The refused run resumes with the window: under the boundary lock the
		// issue must still wait for that run, or the approval is not given.
		fresh, err := q.GetIssue(ctx, issue.ID)
		if err != nil {
			return nil, err
		}
		if fresh.Status != refusedStatus || fresh.AssigneeType.String != "agent" || fresh.AssigneeID != target.AgentID {
			return nil, errAutoNotApproved
		}
		_, afterCommit, err := h.TaskService.EnqueueIssueRunInTx(ctx, q, fresh, pgtype.UUID{}, target.ActorUserID, overlay)
		if errors.Is(err, service.ErrDuplicatePendingTask) {
			return nil, nil
		}
		return afterCommit, err
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

func (h *Handler) escalateRunBudgetAnomaly(ctx context.Context, issue db.Issue, e service.RunBudgetEvent, kind string, recovery *service.BudgetRecoveryRun) (bool, error) {
	var comment db.Comment
	var revision int64
	var decisions []decisionRequestResult
	var followUps []anomalyFollowUp
	recovered := false
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
		// The blocked issue's recovery run commits with the escalation, as it
		// does with a per-issue notice: both or neither.
		if recovery != nil {
			switch err := recovery.Claim(ctx, tx, e.Since); {
			case err == nil:
				recovered = true
			case errors.Is(err, service.ErrRecoveryClaimed), errors.Is(err, service.ErrRecoveryBlockEnded):
			case errors.Is(err, service.ErrRecoveryWindowLifted):
				return errRefusalLifted
			default:
				slog.Warn("run budget: recovery run not enqueued", "err", err, "issue_id", uuidToString(issue.ID))
			}
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
		cards, err := q.ListWorkspaceDecisionsByFingerprint(ctx, db.ListWorkspaceDecisionsByFingerprintParams{WorkspaceID: issue.WorkspaceID, Fingerprint: fingerprint})
		if err != nil {
			return err
		}
		if len(cards) == 0 {
			decisions, err = h.openRunBudgetAsk(ctx, q, issue, runBudgetAnomalyAsk(issue, comment, kind), fingerprint, plan)
			return err
		}
		// The day's card is already out, and its owner may have decided it: a
		// later anomaly reaches them as a message in the card's chat.
		followUps, err = h.addAnomalyToCards(ctx, q, issue, kind, cards)
		return err
	})
	switch {
	case err == nil:
		if recovered {
			recovery.Published(ctx)
		}
		h.publishRunBudgetAudit(issue, comment, revision)
		h.publishRunBudgetDecisions(issue, decisions)
		h.publishAnomalyFollowUps(issue, followUps)
		return true, nil
	case errors.Is(err, errAutoHandled), errors.Is(err, errRefusalLifted):
		return true, nil
	}
	return false, err
}

type anomalyFollowUp struct {
	agentID, humanID string
	message          db.ChatMessage
}

// addAnomalyToCards writes the anomaly as one chat message in each card's
// session. The issue link is the actionable reference.
func (h *Handler) addAnomalyToCards(ctx context.Context, q *db.Queries, issue db.Issue, kind string, cards []db.ListWorkspaceDecisionsByFingerprintRow) ([]anomalyFollowUp, error) {
	title := strings.NewReplacer("[", "(", "]", ")").Replace(issue.Title)
	content := fmt.Sprintf("Another anomaly today, %s: [%s-%d %s](mention://issue/%s). The auto-approver did not approve a new window (%s).",
		kind, h.getIssuePrefix(ctx, issue.WorkspaceID), issue.Number, title, uuidToString(issue.ID), runBudgetAnomalyText[kind])
	out := make([]anomalyFollowUp, 0, len(cards))
	for _, card := range cards {
		message, err := q.CreateChatMessage(ctx, db.CreateChatMessageParams{ID: dbid.NewV7(), ChatSessionID: card.ChatSessionID, Role: "assistant", Content: content, SentVia: pgtype.Text{String: "system", Valid: true}})
		if err != nil {
			return nil, fmt.Errorf("follow-up message: %w", err)
		}
		if err := q.TouchChatSession(ctx, card.ChatSessionID); err != nil {
			return nil, fmt.Errorf("touch card chat: %w", err)
		}
		out = append(out, anomalyFollowUp{agentID: uuidToString(card.AskingAgentID), humanID: uuidToString(card.HumanUserID), message: message})
	}
	return out, nil
}

func (h *Handler) publishAnomalyFollowUps(issue db.Issue, followUps []anomalyFollowUp) {
	workspaceID := uuidToString(issue.WorkspaceID)
	for _, f := range followUps {
		sessionID := uuidToString(f.message.ChatSessionID)
		h.publishChatFor(protocol.EventChatMessage, workspaceID, "agent", f.agentID, f.humanID, sessionID, protocol.ChatMessagePayload{
			MessageKind: f.message.MessageKind, ChatSessionID: sessionID, MessageID: uuidToString(f.message.ID),
			Role: f.message.Role, Content: f.message.Content, CreatedAt: timestampToString(f.message.CreatedAt),
		})
	}
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
// it; later ones are added to its chat, and the brief lists them all.
func runBudgetAnomalyAsk(issue db.Issue, notice db.Comment, kind string) decisiontx.Ask {
	return decisiontx.Ask{
		ContextELI10:  fmt.Sprintf("The run-budget auto-approver held back a top-up on %q: %s. It never approves an anomaly.", issue.Title, runBudgetAnomalyText[kind]),
		AgentDid:      "The platform refused a new run on this issue and did not approve a new window. This is the day's one card for such anomalies; later ones arrive as messages in this chat, and the brief lists them all.",
		Decision:      "Do you want to open a new run window for this issue?",
		StakesIfWrong: "If you reopen a looping or idle issue it can spend a full new window of runs and tokens. If you do nothing, the issue stays stopped.",
		Options: []decisiontx.AskOption{
			{Label: "Move the status", WhatHappens: "You move the issue to another status on the board. That opens a new run window and the workflow runs again.", Pros: []string{"No command needed"}, Cons: []string{"Runs start at once"}, CostOrTime: "One click, then a new window of runs"},
			{Label: "No action", WhatHappens: "Nothing changes. The issue stays stopped.", Pros: []string{"No new spend"}, Cons: []string{"The work stays stopped"}, CostOrTime: "Free"},
		},
		Recommendation:   "Read the issue's last comments. Reopen it only when you see why the anomaly is harmless.",
		DefaultIfNoReply: "No action. The issue stays stopped and nothing runs on it.",
		SourceIssueID:    uuidToString(issue.ID),
		SourceCommentID:  uuidToString(notice.ID),
	}
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
	line += "."
	// The issues behind the escalations, so the brief points at work to open.
	held, err := h.Queries.ListRunBudgetAutoEscalationsSince(ctx, db.ListRunBudgetAutoEscalationsSinceParams{
		WorkspaceID: workspaceID, Since: pgtype.Timestamptz{Time: since, Valid: true},
	})
	if err != nil || len(held) == 0 {
		return line, err
	}
	prefix := h.getIssuePrefix(ctx, workspaceID)
	refs := make([]string, len(held))
	for i, r := range held {
		refs[i] = fmt.Sprintf("%s %s-%d", r.Kind, prefix, r.Number)
	}
	return line + " Held back: " + strings.Join(refs, ", ") + ".", nil
}
