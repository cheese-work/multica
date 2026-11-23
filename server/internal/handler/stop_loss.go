package handler

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/multica-ai/multica/server/internal/decisionaction"
	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// CHE-9340: the stop-loss reuses the weekly run guard's refusal, card and
// allowance. It differs in what it counts (the issue's whole life, with the
// tokens), what ends it (a linked live PR), who may release it (never the
// auto-approver) and where the issue waits (need_decision).

const stopLossWarnedAction = "stop_loss_warned"

// stopLossCost is the card's cost line: runs, tokens, last state change and
// what is left.
func stopLossCost(issue db.Issue, e service.WeeklyRunGuardTripped) string {
	last := "unknown"
	if issue.UpdatedAt.Valid {
		last = issue.UpdatedAt.Time.In(ictLocation).Format("2 Jan 2006 15:04") + " ICT"
	}
	return fmt.Sprintf("%d runs, %.1f M tokens, last update %s, no open or merged PR linked; the stop-loss is %d runs or %d M tokens.",
		e.Runs, float64(e.Tokens)/1e6, last, service.StopLossRuns, service.StopLossTokens/1_000_000)
}

func stopLossCardAsk(issue db.Issue, e service.WeeklyRunGuardTripped, trigger, agentName string) string {
	return fmt.Sprintf("**Stop-loss**\nThe platform refused a run on [Issue](mention://issue/%s): %s\nRefused trigger: %s, for %s.\nApprove with your passkey to allow %d more runs and run that trigger once, or decline to keep it stopped.",
		uuidToString(issue.ID), stopLossCost(issue, e), trigger, agentName, decisionaction.RunGuardAllowRuns)
}

func stopLossComment(issue db.Issue, e service.WeeklyRunGuardTripped, trigger, agentName, approveURL string) string {
	return fmt.Sprintf("Stop-loss: %s Refused: %s, for %s. The issue waits for a decision; only an owner's approval releases it.\n\n[Approve %d more runs](%s)",
		stopLossCost(issue, e), trigger, agentName, decisionaction.RunGuardAllowRuns, approveURL)
}

// warnStopLoss records a warn-mode would-be refusal once per issue and ICT
// day; the brief lists them.
func (h *Handler) warnStopLoss(ctx context.Context, issue db.Issue, details []byte) error {
	return h.warnGuard(ctx, issue, details, stopLossWarnedAction, "stop-loss-warn:")
}

// onStopLossTripped handles a stop-loss event (CHE-9340). Warn mode lists the
// issue and refuses nothing. Block mode opens the owner's card and parks the
// issue; the auto-approver is not asked, because only a person releases it.
func (h *Handler) onStopLossTripped(ctx context.Context, issue db.Issue, e service.WeeklyRunGuardTripped, details []byte) {
	if e.Warn {
		if err := h.warnStopLoss(ctx, issue, details); err != nil {
			slog.Warn("stop-loss: write warning failed", "err", err, "issue_id", uuidToString(issue.ID))
		}
		return
	}
	if err := h.openWeeklyRunGuardCard(ctx, issue, e); err != nil {
		slog.Warn("stop-loss: open card failed", "err", err, "issue_id", uuidToString(issue.ID))
	}
}
