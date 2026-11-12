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
	// RunBudgetKindBlocked marks a window whose refusal moved the issue to
	// blocked. It commits with that move, and is what tells a later refusal of
	// the window that the budget parked the issue, not a person.
	RunBudgetKindBlocked = "blocked"
	// BudgetRecoveryTaskKind marks the recovery run's task row.
	BudgetRecoveryTaskKind = "budget_recovery"
	// BudgetRecoveryLimit is the wall-clock limit after which the server
	// cancels a recovery run. It applies to every runtime.
	BudgetRecoveryLimit = 20 * time.Minute
)

// ErrRecoveryClaimed is returned when the window's recovery run was already
// claimed.
var ErrRecoveryClaimed = errors.New("the window's recovery run was already claimed")

// ErrRecoveryBlockEnded is returned when the budget's block of the issue no
// longer stands: the issue left blocked since the run was read, so a block now
// is another writer's.
var ErrRecoveryBlockEnded = errors.New("the budget's block of the issue ended")

// ErrRecoveryWindowLifted is returned when the window the recovery run was for
// is no longer the issue's current window, or no longer refuses a run.
var ErrRecoveryWindowLifted = errors.New("the window's refusal no longer applies")

// BudgetRecoveryRun is a recovery run read ahead of the transaction that
// commits it. Reads that use the pool happen in PrepareBudgetRecoveryRun, so
// Claim runs in a caller's transaction without taking a second connection.
type BudgetRecoveryRun struct {
	svc               *TaskService
	issue             db.Issue
	params            db.CreateAgentTaskParams
	txCtx             context.Context
	notifyAfterCommit func()
	task              db.AgentTaskQueue
}

// PrepareBudgetRecoveryRun reads what the recovery run for the issue's current
// assignee needs. It writes nothing.
func (s *TaskService) PrepareBudgetRecoveryRun(ctx context.Context, issue db.Issue) (*BudgetRecoveryRun, error) {
	params, _, err := s.prepareIssueTaskWithQueriesAndOverlay(readOnlyRunBudget(ctx), s.Queries, issue, pgtype.UUID{}, nil, false, "", pgtype.UUID{}, pgtype.UUID{}, pgtype.Timestamptz{}, OriginBudgetRecovery, nil)
	if err != nil {
		return nil, err
	}
	txCtx, notifyAfterCommit := deferEnqueuedTaskHook(ctx)
	return &BudgetRecoveryRun{svc: s, issue: issue, params: params, txCtx: txCtx, notifyAfterCommit: notifyAfterCommit}, nil
}

// WithTaint makes the run carry taint on top of that of its acting task.
func (r *BudgetRecoveryRun) WithTaint(taint []string) {
	r.txCtx = WithTaint(r.txCtx, taint)
}

// Claim claims the window's one recovery run and inserts its task, in the
// caller's transaction: a second claim in the same window returns
// ErrRecoveryClaimed. A recovery run bypasses the exhaustion refusal, so the
// claim revalidates the window under the budget lock: a top-up or grant that
// landed since the refusal returns ErrRecoveryWindowLifted and no task, and so
// does an issue whose budget block ended since the run was read
// (ErrRecoveryBlockEnded); the claim holds the marker until the transaction ends. The run
// exists only when the caller's transaction commits; Published follows it.
//
// Claim works in a savepoint, so a failed claim or insert (the handler's pending
// slot is taken, say) undoes only itself: the caller's transaction stays usable,
// and the window's claim stays free for a later refusal.
func (r *BudgetRecoveryRun) Claim(ctx context.Context, tx pgx.Tx, since time.Time) error {
	sp, err := tx.Begin(ctx)
	if err != nil {
		return err
	}
	defer sp.Rollback(ctx)
	qtx := r.svc.Queries.WithTx(sp)
	if _, err := qtx.ClaimRunBudgetNotice(ctx, db.ClaimRunBudgetNoticeParams{
		ID: dbid.NewV7(), WorkspaceID: r.issue.WorkspaceID, IssueID: r.issue.ID,
		WindowID: runBudgetWindowID(since), Kind: RunBudgetKindRecovery,
	}); errors.Is(err, pgx.ErrNoRows) {
		return ErrRecoveryClaimed
	} else if err != nil {
		return err
	}
	if _, err := qtx.LockIssueRunBudget(ctx, r.issue.ID); err != nil {
		return err
	}
	if still, err := RunBudgetStillRefuses(ctx, qtx, r.issue.ID, since); err != nil {
		return err
	} else if !still {
		return ErrRecoveryWindowLifted
	}
	if _, err := qtx.LockRunBudgetBlockMarker(ctx, db.LockRunBudgetBlockMarkerParams{
		IssueID: r.issue.ID, WindowID: runBudgetWindowID(since),
	}); errors.Is(err, pgx.ErrNoRows) {
		return ErrRecoveryBlockEnded
	} else if err != nil {
		return err
	}
	task, err := r.svc.insertPreparedIssueTask(r.txCtx, qtx, r.issue, r.params, pgtype.Timestamptz{}, OriginBudgetRecovery)
	if err != nil {
		return err
	}
	if err := qtx.MarkBudgetRecoveryTask(ctx, task.ID); err != nil {
		return err
	}
	if err := sp.Commit(ctx); err != nil {
		return err
	}
	r.task = task
	return nil
}

// Published announces the run after the transaction that claimed it committed.
func (r *BudgetRecoveryRun) Published(ctx context.Context) {
	r.notifyAfterCommit()
	r.svc.PublishTaskEnqueued(ctx, r.task)
}

// EnqueueBudgetRecoveryRun enqueues the window's one recovery run for the
// issue's current assignee (the blocked step's handler, after the move) in its
// own transaction; see BudgetRecoveryRun.Claim.
func (s *TaskService) EnqueueBudgetRecoveryRun(ctx context.Context, issue db.Issue, since time.Time) (db.AgentTaskQueue, error) {
	run, err := s.PrepareBudgetRecoveryRun(ctx, issue)
	if err != nil {
		return db.AgentTaskQueue{}, err
	}
	tx, err := s.TxStarter.Begin(ctx)
	if err != nil {
		return db.AgentTaskQueue{}, err
	}
	defer tx.Rollback(ctx)
	if err := run.Claim(ctx, tx, since); err != nil {
		return db.AgentTaskQueue{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return db.AgentTaskQueue{}, err
	}
	run.Published(ctx)
	return run.task, nil
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
