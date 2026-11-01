package service

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

// Budget recovery reserve (CHE-2297 L3): when the exhaustion move blocks an
// issue, the blocked step's handler gets one run per budget window to record
// why. The server limits that run; the run's own good behavior is not relied on.

const (
	// RunBudgetKindRecovery is the kind half of the recovery claim key.
	RunBudgetKindRecovery = "recovery"
	// BudgetRecoveryTaskKind marks the recovery run's task row.
	BudgetRecoveryTaskKind = "budget_recovery"
	// BudgetRecoveryLimit is the wall-clock limit after which the server
	// cancels a recovery run. It applies to every runtime.
	BudgetRecoveryLimit = 20 * time.Minute
)

// ErrRecoveryClaimed is returned when the window's recovery run was already
// claimed.
var ErrRecoveryClaimed = errors.New("the window's recovery run was already claimed")

// ErrRecoveryWindowLifted is returned when the window the recovery run was for
// is no longer the issue's current window, or no longer refuses a run.
var ErrRecoveryWindowLifted = errors.New("the window's refusal no longer applies")

// RecoveryRunClaimed reports whether the window's recovery run was claimed.
func RecoveryRunClaimed(ctx context.Context, q *db.Queries, issueID pgtype.UUID, since time.Time) (bool, error) {
	return q.HasRunBudgetNotice(ctx, db.HasRunBudgetNoticeParams{IssueID: issueID, WindowID: runBudgetWindowID(since), Kind: RunBudgetKindRecovery})
}

// EnqueueBudgetRecoveryRun enqueues the window's one recovery run for the
// issue's current assignee (the blocked step's handler, after the move). The
// claim and the task commit together; a second claim in the same window returns
// ErrRecoveryClaimed. A recovery run bypasses the exhaustion refusal, so the
// claim revalidates the window under the budget lock: a top-up or grant that
// landed since the refusal returns ErrRecoveryWindowLifted and no task.
func (s *TaskService) EnqueueBudgetRecoveryRun(ctx context.Context, issue db.Issue, since time.Time) (db.AgentTaskQueue, error) {
	params, _, err := s.prepareIssueTaskWithQueriesAndOverlay(readOnlyRunBudget(ctx), s.Queries, issue, pgtype.UUID{}, nil, false, "", pgtype.UUID{}, pgtype.UUID{}, pgtype.Timestamptz{}, OriginBudgetRecovery, nil)
	if err != nil {
		return db.AgentTaskQueue{}, err
	}
	var task db.AgentTaskQueue
	txCtx, notifyAfterCommit := deferEnqueuedTaskHook(ctx)
	err = s.runInTx(ctx, func(qtx *db.Queries) error {
		if _, err := qtx.ClaimRunBudgetNotice(ctx, db.ClaimRunBudgetNoticeParams{
			ID: dbid.NewV7(), WorkspaceID: issue.WorkspaceID, IssueID: issue.ID,
			WindowID: runBudgetWindowID(since), Kind: RunBudgetKindRecovery,
		}); errors.Is(err, pgx.ErrNoRows) {
			return ErrRecoveryClaimed
		} else if err != nil {
			return err
		}
		if _, err := qtx.LockIssueRunBudget(ctx, issue.ID); err != nil {
			return err
		}
		if still, err := RunBudgetStillRefuses(ctx, qtx, issue.ID, since); err != nil {
			return err
		} else if !still {
			return ErrRecoveryWindowLifted
		}
		task, err = s.insertPreparedIssueTask(txCtx, qtx, issue, params, pgtype.Timestamptz{}, OriginBudgetRecovery)
		if err != nil {
			return err
		}
		return qtx.MarkBudgetRecoveryTask(ctx, task.ID)
	})
	if err == nil {
		notifyAfterCommit()
		s.PublishTaskEnqueued(ctx, task)
	}
	return task, err
}

// CancelExpiredBudgetRecoveryRuns cancels recovery runs past the wall-clock
// limit. A time limit is the one bound the server can enforce on every
// runtime; it cannot measure tokens during a run.
func (s *TaskService) CancelExpiredBudgetRecoveryRuns(ctx context.Context) {
	ids, err := s.Queries.ListExpiredBudgetRecoveryTasks(ctx, BudgetRecoveryLimit.Seconds())
	if err != nil {
		return
	}
	for _, id := range ids {
		if _, err := s.CancelTaskWithReason(ctx, id, "recovery run reached its 20-minute limit", "budget_recovery_time_limit"); err != nil {
			slog.Warn("budget recovery: cancel failed", "task_id", util.UUIDToString(id), "err", err)
		}
	}
}
