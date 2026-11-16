package service

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// The weekly run guard (CHE-5720): an issue that already started more than
// MaxRuns runs in the last 7 days takes no more derived runs, whatever its run
// budget window says. A member moving the status reopens a budget window, so
// an issue that loops across windows is bounded only by this guard. It sits at
// the same queue door as the loop breaker and the run budget. A person naming
// an agent by hand (OriginNamed) never reaches it.

const (
	weeklyRunGuardMaxRunsEnv = "MULTICA_WEEKLY_RUN_GUARD_MAX_RUNS"
	// DefaultWeeklyRunGuardMaxRuns is how many runs an issue may start in 7 days;
	// the next one is refused.
	DefaultWeeklyRunGuardMaxRuns = 30
	// WeeklyRunGuardWindow is how far back the guard counts.
	WeeklyRunGuardWindow = 7 * 24 * time.Hour
)

// ErrWeeklyRunGuard is returned when the guard refuses a run. It wraps
// ErrRunBudgetExhausted so every enqueue caller already treats it as a refusal.
var ErrWeeklyRunGuard = fmt.Errorf("weekly run guard: the issue already started too many runs in 7 days: %w", ErrRunBudgetExhausted)

// WeeklyRunGuardMaxRuns reads the limit from the environment; 0 or less turns
// the guard off, an unset or invalid value gives the default.
func WeeklyRunGuardMaxRuns() int {
	if v, err := strconv.Atoi(os.Getenv(weeklyRunGuardMaxRunsEnv)); err == nil {
		return v
	}
	return DefaultWeeklyRunGuardMaxRuns
}

// WeeklyRunGuardTripped describes one refusal: the runs the issue started in
// the window and the status of its last three runs, newest first.
type WeeklyRunGuardTripped struct {
	IssueID  pgtype.UUID
	Runs     int64
	Outcomes []string
	// Target is the refused run, kept so an approved card can run it once.
	Target RunTarget
}

var onWeeklyRunGuardTripped atomic.Pointer[func(context.Context, WeeklyRunGuardTripped)]

// SetOnWeeklyRunGuardTripped registers the function that records the incident
// (nil clears it). The handler registers it; the service has no business
// writing activity rows.
func SetOnWeeklyRunGuardTripped(fn func(context.Context, WeeklyRunGuardTripped)) {
	if fn == nil {
		onWeeklyRunGuardTripped.Store(nil)
		return
	}
	onWeeklyRunGuardTripped.Store(&fn)
}

// WeeklyRunGuardLimit is the runs an issue may start in the window: the
// configured maximum plus what approved cards added in the window (CHE-6172).
// max <= 0 means the guard is off.
func WeeklyRunGuardLimit(ctx context.Context, q *db.Queries, issueID pgtype.UUID) (limit int64, err error) {
	max := WeeklyRunGuardMaxRuns()
	if max <= 0 {
		return 0, nil
	}
	extra, err := q.SumWeeklyRunGuardAllowance(ctx, db.SumWeeklyRunGuardAllowanceParams{
		IssueID: issueID, CreatedAt: pgtype.Timestamptz{Time: time.Now().Add(-WeeklyRunGuardWindow), Valid: true},
	})
	return int64(max) + extra, err
}

type weeklyGuardProgressKey struct{}

// withProgressEvent marks an enqueue that follows a progress event no loop can
// produce, such as an answered decision card (CHE-6239). The weekly guard lets
// it through; the loop breaker and the run budget still judge it. The caller
// must admit at most one run per event.
func withProgressEvent(ctx context.Context) context.Context {
	return context.WithValue(ctx, weeklyGuardProgressKey{}, true)
}

// guardWeeklyRuns refuses the run when the issue already started its limit of
// runs in the window.
func guardWeeklyRuns(ctx context.Context, q *db.Queries, issueID pgtype.UUID, target RunTarget) error {
	if progress, _ := ctx.Value(weeklyGuardProgressKey{}).(bool); progress {
		return nil
	}
	limit, err := WeeklyRunGuardLimit(ctx, q, issueID)
	if err != nil {
		return fmt.Errorf("weekly run guard: %w", err)
	}
	if limit <= 0 {
		return nil
	}
	runs, err := q.CountIssueRunsSince(ctx, db.CountIssueRunsSinceParams{
		IssueID: issueID, Since: pgtype.Timestamptz{Time: time.Now().Add(-WeeklyRunGuardWindow), Valid: true},
	})
	if err != nil {
		return fmt.Errorf("weekly run guard: %w", err)
	}
	if runs < limit {
		return nil
	}
	if hook := onWeeklyRunGuardTripped.Load(); hook != nil && !hookSuppressed(ctx) {
		event := WeeklyRunGuardTripped{IssueID: issueID, Runs: runs, Target: target}
		// Best effort: the incident loses its outcomes, never the refusal.
		if statuses, err := q.ListIssueLastRunStatuses(ctx, db.ListIssueLastRunStatusesParams{IssueID: issueID, Limit: 3}); err == nil {
			event.Outcomes = statuses
		}
		// The guard runs inside the enqueue transaction; the hook writes on its
		// own connection after the caller is done with it.
		goRunBudgetHook(func() { (*hook)(context.WithoutCancel(ctx), event) })
	}
	return ErrWeeklyRunGuard
}
