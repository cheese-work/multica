package handler

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/governance"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// startJevShadowRouting runs the shadow evaluation off the request path so a slow
// or failing Jev call can never delay or alter the leader enqueue (CHE-1745).
func (h *Handler) startJevShadowRouting(ctx context.Context, issue db.Issue, squad db.Squad, authorType, authorID string, settings governance.WorkspaceSettings, decisionID string, triggerCommentID ...pgtype.UUID) {
	ctx = context.WithoutCancel(ctx)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				slog.Error("jev squad shadow: panic", "issue_id", uuidToString(issue.ID), "panic", r)
			}
		}()
		h.shadowJevSquadRouting(ctx, issue, squad, authorType, authorID, settings, decisionID, triggerCommentID...)
	}()
}

// shadowJevSquadRouting asks Jev for a routing decision and logs it. It never
// dispatches, comments, assigns or changes status. The log line carries the
// issue and trigger comment ids so it can be joined with what the leader did.
func (h *Handler) shadowJevSquadRouting(ctx context.Context, issue db.Issue, squad db.Squad, authorType, authorID string, settings governance.WorkspaceSettings, decisionID string, triggerCommentID ...pgtype.UUID) {
	start := time.Now()
	decision, reason := h.evaluateJevSquadRouting(ctx, issue, squad, authorType, authorID, settings, decisionID)

	attrs := []any{
		"workspace_id", uuidToString(issue.WorkspaceID),
		"issue_id", uuidToString(issue.ID),
		"squad_id", uuidToString(squad.ID),
		"leader_id", uuidToString(squad.LeaderID),
		"author_type", authorType,
		"decision_id", decisionID,
		"decision_source", "jev",
		"latency_ms", time.Since(start).Milliseconds(),
	}
	if len(triggerCommentID) > 0 && triggerCommentID[0].Valid {
		attrs = append(attrs, "trigger_comment_id", uuidToString(triggerCommentID[0]))
	}
	if decision == nil {
		slog.Info(jevShadowMessage, append(attrs, "outcome", "error", "error", reason)...)
		return
	}

	minConfidence := jevMinConfidence(settings)
	attrs = append(attrs,
		"outcome", "decision",
		"winner_id", decision.winnerOption,
		"winner_label", decision.winnerLabel,
		"confidence", decision.confidence,
		"runner_up_id", decision.runnerUpOption,
		"runner_up_confidence", decision.runnerUpProb,
		"min_confidence", minConfidence,
		"meets_threshold", decision.winnerOption != jevEscalateOption && decision.confidence >= minConfidence,
	)
	slog.Info(jevShadowMessage, attrs...)
}
