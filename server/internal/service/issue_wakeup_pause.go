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

// settlePaused settles a rule of a paused workspace and reports whether
// dispatch must stop. Inputs that arrived meanwhile are consumed with outcome
// "paused" and never replayed; recurring schedules skip their missed slots. A
// one-shot that came due and a condition keep their state, so they fire once on
// resume if they still hold. Only an explicit person's trigger (wakeup.manual)
// goes on to dispatch.
//
// The caller holds the workspace row (KEY SHARE, which is also where it read
// the pause) and the wakeup row, in the lock order workspace, issue, wakeup that
// the pause and resume writers share.
// ponytail: a manual trigger on a paused one-shot that is also due fires both
// together, as one run; split them if that matters.
func settlePaused(ctx context.Context, tx pgx.Tx, q *db.Queries, w db.IssueWakeup) (bool, error) {
	if _, err := q.DropPausedWakeupReceipts(ctx, w.ID); err != nil {
		return false, err
	}
	if err := skipMissedSlots(ctx, tx, q, w); err != nil {
		return false, err
	}
	manual, err := q.CountPendingManualWakeupReceipts(ctx, w.ID)
	return manual == 0, err
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
	// Receipts that a lock-free producer inserted before this moment but commits
	// after it are dropped at dispatch by their event time (dropLateIfResumed).
	if err := q.StampWakeupsResumed(ctx, workspaceID); err != nil {
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

// dropLateIfResumed drops the receipts of a workspace that is not paused any
// more but whose event time falls before its last resume. The producers that
// take no workspace lock (issue, task, label and delete triggers) can commit
// such a receipt after the resume swept; it arrived while paused. The caller
// holds the workspace row (KEY SHARE) it read state from, and the wakeup row.
func dropLateIfResumed(ctx context.Context, q *db.Queries, w db.IssueWakeup, state db.WorkspaceWakeupsPausedSharedRow) error {
	if state.Paused || !state.ResumedAt.Valid {
		return nil
	}
	_, err := q.DropLatePausedReceipts(ctx, db.DropLatePausedReceiptsParams{ID: w.ID, ResumedAt: state.ResumedAt})
	return err
}
