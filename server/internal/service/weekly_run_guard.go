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

// guardWeeklyRuns refuses the run when the issue already started MaxRuns runs
// in the window.
func guardWeeklyRuns(ctx context.Context, q *db.Queries, issueID pgtype.UUID) error {
	max := WeeklyRunGuardMaxRuns()
	if max <= 0 {
		return nil
	}
	runs, err := q.CountIssueRunsSince(ctx, db.CountIssueRunsSinceParams{
		IssueID: issueID, Since: pgtype.Timestamptz{Time: time.Now().Add(-WeeklyRunGuardWindow), Valid: true},
	})
	if err != nil {
		return fmt.Errorf("weekly run guard: %w", err)
	}
	if runs < int64(max) {
		return nil
	}
	if hook := onWeeklyRunGuardTripped.Load(); hook != nil {
		event := WeeklyRunGuardTripped{IssueID: issueID, Runs: runs}
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
