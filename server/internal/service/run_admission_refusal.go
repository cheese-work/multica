package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// RunAdmissionRefusal reports, without writing, why a derived run of agentID on
// the issue would be refused now: ErrAgentArchived, ErrAgentPaused,
// ErrWeeklyRunGuard or ErrRunBudgetExhausted, in the order the enqueue door
// checks them. Nil means the run would start. The step watchdog classifies a
// stalled step from this typed answer, never from text (CHE-6116).
func RunAdmissionRefusal(ctx context.Context, q *db.Queries, issue db.Issue, agentID pgtype.UUID) error {
	if agent, err := q.GetAgent(ctx, agentID); err == nil && agent.ArchivedAt.Valid {
		return ErrAgentArchived
	} else if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("run admission: load agent: %w", err)
	}
	if err := guardAgentNotPaused(ctx, q, agentID); err != nil {
		return err
	}
	admits, err := weeklyGuardAdmits(ctx, q, issue.ID, agentID)
	if err != nil {
		return fmt.Errorf("run admission: %w", err)
	}
	if !admits {
		return ErrWeeklyRunGuard
	}
	// ponytail: the first-review-head exception to a spent budget (reviewHeadOf)
	// is not modelled, so a spent budget blocks an in-review step the enqueue
	// door would admit once; the watchdog then only notifies. Model it if seen.
	_, usage, balance, err := openRunBudget(readOnlyRunBudget(ctx), q, issue.ID, pgtype.UUID{})
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && usage == nil) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("run admission: %w", err)
	}
	if balance < 1 && runBudgetMode(ctx, q, issue.WorkspaceID) == RunBudgetModeBlock {
		return ErrRunBudgetExhausted
	}
	return nil
}
