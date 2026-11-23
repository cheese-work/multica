package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// The weekly run guard (CHE-5720): an issue that already started more than
// MaxRuns runs in the last 7 days takes no more derived runs, whatever its run
// budget window says. A member moving the status reopens a budget window, so
// an issue that loops across windows is bounded only by this guard. It sits at
// the same queue door as the loop breaker and the run budget. A person naming
// an agent by hand (OriginNamed) never reaches it. A run that follows progress
// the target agent has not yet run on (a new PR head, a merge, a signed review
// verdict, an answered decision request) passes once, below a hard ceiling of
// WeeklyRunGuardCeilingFactor times the limit (CHE-6175).

const (
	weeklyRunGuardMaxRunsEnv = "MULTICA_WEEKLY_RUN_GUARD_MAX_RUNS"
	// DefaultWeeklyRunGuardMaxRuns is how many runs an issue may start in 7 days;
	// the next one is refused.
	DefaultWeeklyRunGuardMaxRuns = 30
	// WeeklyRunGuardWindow is how far back the guard counts.
	WeeklyRunGuardWindow = 7 * 24 * time.Hour
	// WeeklyRunGuardCeilingFactor times the limit is the run count at which even
	// a run that follows progress is refused.
	WeeklyRunGuardCeilingFactor = 2
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
	// Warn marks a would-be refusal in warn mode: the run was not refused.
	Warn bool
	// StopLoss marks a stop-loss refusal (CHE-9340) rather than the weekly limit;
	// Tokens is the issue's token total then.
	StopLoss bool
	Tokens   int64
}

// Guard modes (workspace setting settings.run_guard.mode). Block is the default.
const (
	WeeklyRunGuardOff   = "off"
	WeeklyRunGuardWarn  = "warn"
	WeeklyRunGuardBlock = "block"
)

// WeeklyRunGuardModeFromSettings reads settings.run_guard.mode. Absent or
// unreadable, the guard blocks: the default is today's behavior.
func WeeklyRunGuardModeFromSettings(settings []byte) string {
	var doc struct {
		Guard struct {
			Mode string `json:"mode"`
		} `json:"run_guard"`
	}
	if json.Unmarshal(settings, &doc) != nil {
		return WeeklyRunGuardBlock
	}
	switch doc.Guard.Mode {
	case WeeklyRunGuardOff, WeeklyRunGuardWarn:
		return doc.Guard.Mode
	}
	return WeeklyRunGuardBlock
}

// weeklyRunGuardMode is the workspace's mode for the issue. A read that fails
// blocks, so an unknown mode never lets a run through.
func weeklyRunGuardMode(ctx context.Context, q *db.Queries, issueID pgtype.UUID) string {
	issue, err := q.GetIssue(ctx, issueID)
	if err != nil {
		return WeeklyRunGuardBlock
	}
	ws, err := q.GetWorkspace(ctx, issue.WorkspaceID)
	if err != nil {
		return WeeklyRunGuardBlock
	}
	return WeeklyRunGuardModeFromSettings(ws.Settings)
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

// WithProgressEvent marks an enqueue that follows a progress event no loop can
// produce, such as an answered decision card (CHE-6239). The weekly guard lets
// it through; the loop breaker and the run budget still judge it. The caller
// must admit at most one run per event.
func WithProgressEvent(ctx context.Context) context.Context {
	return context.WithValue(ctx, weeklyGuardProgressKey{}, true)
}

// guardWeeklyRuns refuses the run when the issue already started its limit of
// runs in the window.
func guardWeeklyRuns(ctx context.Context, q *db.Queries, issueID pgtype.UUID, target RunTarget) error {
	// A progress event does not release the stop-loss: only an approved card does.
	if err := guardStopLoss(ctx, q, issueID, target); err != nil {
		return err
	}
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
	mode := weeklyRunGuardMode(ctx, q, issueID)
	if mode == WeeklyRunGuardOff {
		return nil
	}
	// Past the limit, a run that follows progress passes once (CHE-6175), up to
	// the hard ceiling; the loop breaker has already judged it.
	if runs < limit*WeeklyRunGuardCeilingFactor && target.AgentID.Valid {
		// Serialize with the task insert: the issue row lock is held to the end of
		// the enqueue transaction, so a concurrent trigger reads this run as the
		// agent's last run and finds the event used.
		if _, err := q.LockIssueRunBudget(ctx, issueID); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("weekly run guard: %w", err)
		}
		events, err := q.CountIssueProgressEventsSince(ctx, db.CountIssueProgressEventsSinceParams{
			IssueID: issueID, AgentID: target.AgentID,
			WindowStart: pgtype.Timestamptz{Time: time.Now().Add(-WeeklyRunGuardWindow), Valid: true},
		})
		if err != nil {
			return fmt.Errorf("weekly run guard: %w", err)
		}
		if events > 0 {
			return nil
		}
	}
	if hook := onWeeklyRunGuardTripped.Load(); hook != nil && !hookSuppressed(ctx) {
		event := WeeklyRunGuardTripped{IssueID: issueID, Runs: runs, Target: target, Warn: mode == WeeklyRunGuardWarn}
		// Best effort: the incident loses its outcomes, never the refusal.
		if statuses, err := q.ListIssueLastRunStatuses(ctx, db.ListIssueLastRunStatusesParams{IssueID: issueID, Limit: 3}); err == nil {
			event.Outcomes = statuses
		}
		// The guard runs inside the enqueue transaction; the hook writes on its
		// own connection after the caller is done with it.
		goRunBudgetHook(func() { (*hook)(context.WithoutCancel(ctx), event) })
	}
	if mode == WeeklyRunGuardWarn {
		return nil
	}
	return ErrWeeklyRunGuard
}

// weeklyGuardAdmits reports whether the guard would let a run of the agent
// through now: under the limit, the guard off, or past the limit with an unused
// progress event and below the ceiling. It only reads.
func weeklyGuardAdmits(ctx context.Context, q *db.Queries, issueID, agentID pgtype.UUID) (bool, error) {
	limit, err := WeeklyRunGuardLimit(ctx, q, issueID)
	if err != nil || limit <= 0 || weeklyRunGuardMode(ctx, q, issueID) != WeeklyRunGuardBlock {
		return err == nil, err
	}
	since := pgtype.Timestamptz{Time: time.Now().Add(-WeeklyRunGuardWindow), Valid: true}
	runs, err := q.CountIssueRunsSince(ctx, db.CountIssueRunsSinceParams{IssueID: issueID, Since: since})
	if err != nil || runs < limit {
		return err == nil, err
	}
	if runs >= limit*WeeklyRunGuardCeilingFactor || !agentID.Valid {
		return false, nil
	}
	events, err := q.CountIssueProgressEventsSince(ctx, db.CountIssueProgressEventsSinceParams{IssueID: issueID, AgentID: agentID, WindowStart: since})
	return events > 0, err
}
