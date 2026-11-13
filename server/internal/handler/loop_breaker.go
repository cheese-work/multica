package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/issuestatus"
	"github.com/multica-ai/multica/server/internal/issueworkflow"
	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// loopBreakerReason is the blocked_reason of a loop-breaker block.
func loopBreakerReason(agent string, e service.LoopBreakerTripped) string {
	return fmt.Sprintf("loop-breaker: %s started %d runs in %s", agent, e.Runs, loopBreakerWindowText(e.Window))
}

func loopBreakerWindowText(d time.Duration) string {
	if d%time.Minute == 0 {
		return fmt.Sprintf("%d min", int(d/time.Minute))
	}
	return d.String()
}

// onLoopBreakerTripped runs after the breaker refused a run (CHE-5135). It
// blocks the issue (unless it is finished, already blocked, or its workflow has
// no blocked status) and posts one system comment per issue and agent per hour
// that names the agent, lists the trigger types of its last runs and mentions
// the blocked step's agent handler (Tofu).
func (h *Handler) onLoopBreakerTripped(ctx context.Context, e service.LoopBreakerTripped) {
	issue, err := h.Queries.GetIssue(ctx, e.IssueID)
	if err != nil {
		slog.Warn("loop breaker: load issue failed", "err", err, "issue_id", uuidToString(e.IssueID))
		return
	}
	agentName := uuidToString(e.AgentID)
	if agent, err := h.Queries.GetAgent(ctx, e.AgentID); err == nil {
		agentName = agent.Name
	}
	reason := loopBreakerReason(agentName, e)
	if !service.IsFinishedStatus(ctx, h.Queries, issue.WorkspaceID, issue.Status) {
		h.blockLoopBreakerIssue(ctx, issue, reason, e)
	}
	h.postLoopBreakerNotice(ctx, issue, agentName, reason, e)
}

func (h *Handler) blockLoopBreakerIssue(ctx context.Context, issue db.Issue, reason string, e service.LoopBreakerTripped) {
	if issue.Status == issuestatus.Blocked {
		return
	}
	def, _, err := issueworkflow.ForIssue(ctx, h.Queries, issue.WorkspaceID, issue.WorkflowID, issue.ProjectID)
	if err != nil || def == nil || !def.Has(issuestatus.Blocked) {
		slog.Warn("loop breaker: cannot block issue", "err", err, "issue_id", uuidToString(issue.ID))
		return
	}
	details := map[string]any{"reason": reason, "runs": e.Runs, "agent_id": uuidToString(e.AgentID), "triggers": e.Triggers}
	moved, _, err := h.tryMoveIssueOnEdge(ctx, issue, issuestatus.Blocked, "loop_breaker", details, false)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		slog.Warn("loop breaker: block failed", "err", err, "issue_id", uuidToString(issue.ID))
	}
	if !moved {
		return
	}
	value, _ := json.Marshal(reason)
	if _, err := h.Queries.SetIssueMetadataKey(ctx, db.SetIssueMetadataKeyParams{Key: "blocked_reason", Value: value, ID: issue.ID, WorkspaceID: issue.WorkspaceID}); err != nil {
		slog.Warn("loop breaker: set blocked_reason failed", "err", err, "issue_id", uuidToString(issue.ID))
	}
}

func (h *Handler) postLoopBreakerNotice(ctx context.Context, issue db.Issue, agentName, reason string, e service.LoopBreakerTripped) {
	agentKey := uuidToString(e.AgentID)
	var comment db.Comment
	var revision int64
	err := h.withPgxTx(ctx, func(tx pgx.Tx) error {
		qtx := h.Queries.WithTx(tx)
		noticeID, err := qtx.ClaimLoopBreakerNotice(ctx, db.ClaimLoopBreakerNoticeParams{
			ID: dbid.NewV7(), WorkspaceID: issue.WorkspaceID, IssueID: issue.ID,
			Column4: agentKey + ":" + time.Now().UTC().Format(time.RFC3339Nano), Column5: agentKey + ":%",
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("claim notice: %w", err)
		}
		created, err := qtx.CreateComment(ctx, db.CreateCommentParams{
			ID: dbid.NewV7(), IssueID: issue.ID, WorkspaceID: issue.WorkspaceID,
			AuthorType: "system", AuthorID: pgtype.UUID{Valid: true}, Content: h.loopBreakerNoticeText(ctx, issue, agentName, reason, e), Type: "system",
		})
		if err != nil {
			return fmt.Errorf("create comment: %w", err)
		}
		comment, revision = created.Comment(), created.IssueRevision
		return qtx.SetRunBudgetNoticeComment(ctx, db.SetRunBudgetNoticeCommentParams{ID: noticeID, CommentID: comment.ID})
	})
	if err != nil {
		slog.Warn("loop breaker: write notice failed", "err", err, "issue_id", uuidToString(issue.ID))
		return
	}
	if !comment.ID.Valid {
		return
	}
	h.publish(protocol.EventCommentCreated, uuidToString(issue.WorkspaceID), "system", "", map[string]any{
		"comment":             commentToResponse(comment, nil, nil),
		"issue_title":         issue.Title,
		"issue_assignee_type": textToPtr(issue.AssigneeType),
		"issue_assignee_id":   uuidToPtr(issue.AssigneeID),
		"issue_status":        issue.Status,
		"issue_revision":      revision,
	})
}

func (h *Handler) loopBreakerNoticeText(ctx context.Context, issue db.Issue, agentName, reason string, e service.LoopBreakerTripped) string {
	var b strings.Builder
	fmt.Fprintf(&b, "**Loop breaker.** %s. The next run was refused", reason)
	if issue.Status != issuestatus.Blocked && !service.IsFinishedStatus(ctx, h.Queries, issue.WorkspaceID, issue.Status) {
		b.WriteString(" and the issue is blocked")
	}
	fmt.Fprintf(&b, ".\n\n- Agent: %s\n- Trigger types of the last %d runs, newest first: %s\n", agentName, len(e.Triggers), strings.Join(e.Triggers, ", "))
	if id, ok := h.blockedAgentHandler(ctx, issue); ok {
		name := "Tofu"
		if tofu, err := h.Queries.GetAgent(ctx, parseUUID(id)); err == nil {
			name = tofu.Name
		}
		fmt.Fprintf(&b, "\n[@%s](mention://agent/%s): find the cause of the loop, then move the issue out of blocked.", name, id)
	}
	return b.String()
}
