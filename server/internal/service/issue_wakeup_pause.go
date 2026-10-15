package service

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// WakeupsPausedWarning is the API warning a create or enable carries while the
// workspace's wakeups are paused (CHE-1727).
const WakeupsPausedWarning = "workspace_wakeups_paused"

// WorkspaceWakeupsPaused reads the workspace switch in workspace.settings.
func WorkspaceWakeupsPaused(ctx context.Context, q *db.Queries, workspaceID pgtype.UUID) (bool, error) {
	return q.WorkspaceWakeupsPaused(ctx, workspaceID)
}

// heldByPause settles a rule of a paused workspace and reports whether dispatch
// must stop. Inputs that arrived meanwhile are consumed with outcome "paused"
// and never replayed; recurring schedules skip their missed slots. A one-shot
// that came due and a condition keep their state, so they fire once on resume
// if they still hold. Only an explicit person's trigger (wakeup.manual) goes on
// to dispatch.
// ponytail: a manual trigger on a paused one-shot that is also due fires both
// together, as one run; split them if that matters.
func (s *IssueWakeupService) heldByPause(ctx context.Context, prev db.IssueWakeup) (bool, error) {
	paused, err := s.Tasks.Queries.WorkspaceWakeupsPaused(ctx, prev.WorkspaceID)
	if err != nil || !paused {
		return false, err
	}
	tx, err := s.Tasks.TxStarter.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	q := s.Tasks.Queries.WithTx(tx)
	w, err := q.LockIssueWakeup(ctx, prev.ID)
	if err != nil {
		return false, err
	}
	if _, err = q.DropPausedWakeupReceipts(ctx, db.DropPausedWakeupReceiptsParams{ID: w.ID, OneShot: w.Kind == "at"}); err != nil {
		return false, err
	}
	if err := skipMissedSlots(ctx, tx, q, w); err != nil {
		return false, err
	}
	manual, err := q.CountPendingManualWakeupReceipts(ctx, w.ID)
	if err != nil {
		return false, err
	}
	return manual == 0, tx.Commit(ctx)
}

// skipMissedSlots moves a recurring rule past every slot that came due while
// the workspace was paused, so none of them fires.
func skipMissedSlots(ctx context.Context, tx pgx.Tx, q *db.Queries, w db.IssueWakeup) error {
	var now time.Time
	if err := tx.QueryRow(ctx, "SELECT now()").Scan(&now); err != nil {
		return err
	}
	if !w.Enabled || (w.Kind != "every" && w.Kind != "cron") || !w.NextFireAt.Valid || w.NextFireAt.Time.After(now) {
		return nil
	}
	next := w.NextFireAt
	enabled := true
	if w.Kind == "every" {
		step := time.Duration(w.IntervalSeconds.Int64) * time.Second
		next.Time = next.Time.Add((now.Sub(next.Time)/step + 1) * step)
	} else {
		var err error
		if next.Time, err = NextOccurrenceAfterUTC(w.CronExpression.String, w.Timezone, now); err != nil {
			return err
		}
		if next.Time.IsZero() {
			next.Valid, enabled = false, false
		}
	}
	return q.AdvanceIssueWakeup(ctx, db.AdvanceIssueWakeupParams{ID: w.ID, Enabled: enabled, NextFireAt: next})
}

// SettleWakeupsOnResume applies the pause to everything it held, in the
// caller's transaction that also lifts the switch. Without it, inputs that
// arrived and slots that came due while paused would fire on the first tick
// after an admin resumes before any tick ran.
func (s *IssueWakeupService) SettleWakeupsOnResume(ctx context.Context, tx pgx.Tx, workspaceID pgtype.UUID) error {
	q := s.Tasks.Queries.WithTx(tx)
	if _, err := q.DropPausedWorkspaceReceipts(ctx, workspaceID); err != nil {
		return err
	}
	overdue, err := q.ListOverdueRecurringWakeups(ctx, workspaceID)
	if err != nil {
		return err
	}
	for _, w := range overdue {
		if err := skipMissedSlots(ctx, tx, q, w); err != nil {
			return err
		}
	}
	return nil
}
