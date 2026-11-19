package service

import (
	"context"

	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// WakeupRunAdmitted reports whether a derived run for agent on the issue would
// pass the paused-agent and run-budget guards now (CHE-6973). It writes nothing
// and does not fire the budget-exhausted hook; a read error counts as refused.
func WakeupRunAdmitted(ctx context.Context, q *db.Queries, issueID, agentID pgtype.UUID) bool {
	ctx = WithoutRunBudgetHook(readOnlyRunBudget(ctx))
	if err := guardAgentNotPaused(ctx, q, agentID); err != nil {
		return false
	}
	_, err := guardRunBudget(ctx, q, issueID, RunTarget{AgentID: agentID, Trigger: "wakeup"})
	return err == nil
}
