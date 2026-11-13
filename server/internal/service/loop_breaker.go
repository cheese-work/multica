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

// The loop breaker (CHE-5135): an agent that starts too many runs on one issue
// in a short window is a loop (a wakeup that re-arms itself, two agents that
// hand an issue back and forth). The run that would be the max-th in the window
// is refused, the issue is blocked and Tofu is alerted. It sits at the same
// queue door as the run budget, for the same reason: every path enqueues
// through guardIssueRunGate. A person naming an agent by hand (OriginNamed)
// never reaches it, and chat tasks have no issue.

const (
	loopBreakerMaxRunsEnv = "MULTICA_LOOP_BREAKER_MAX_RUNS"
	loopBreakerWindowEnv  = "MULTICA_LOOP_BREAKER_WINDOW"
	// DefaultLoopBreakerMaxRuns is the run that is refused: the 6th in the window.
	DefaultLoopBreakerMaxRuns = 6
	// DefaultLoopBreakerWindow is how far back the breaker counts.
	DefaultLoopBreakerWindow = 30 * time.Minute
)

// ErrLoopBreaker is returned when the breaker refuses a run. It wraps
// ErrRunBudgetExhausted so every enqueue caller already treats it as a refusal.
var ErrLoopBreaker = fmt.Errorf("loop breaker: the agent started too many runs on this issue: %w", ErrRunBudgetExhausted)

// LoopBreakerConfig is the breaker's limit: refuse the MaxRuns-th run an agent
// starts on one issue within Window. MaxRuns <= 0 turns the breaker off.
type LoopBreakerConfig struct {
	MaxRuns int
	Window  time.Duration
}

// LoopBreakerSettings reads the limits from the environment, falling back to
// the defaults for an unset or invalid value.
func LoopBreakerSettings() LoopBreakerConfig {
	cfg := LoopBreakerConfig{MaxRuns: DefaultLoopBreakerMaxRuns, Window: DefaultLoopBreakerWindow}
	if v, err := strconv.Atoi(os.Getenv(loopBreakerMaxRunsEnv)); err == nil {
		cfg.MaxRuns = v
	}
	if d, err := time.ParseDuration(os.Getenv(loopBreakerWindowEnv)); err == nil && d > 0 {
		cfg.Window = d
	}
	return cfg
}

// LoopBreakerTripped describes one refusal: the agent, the runs it already
// started in the window, the trigger types of its latest runs (newest first,
// the refused run's own trigger first) and the issue status at refusal.
type LoopBreakerTripped struct {
	IssueID  pgtype.UUID
	AgentID  pgtype.UUID
	Runs     int64
	Window   time.Duration
	Triggers []string
	Status   string
}

var onLoopBreakerTripped atomic.Pointer[func(context.Context, LoopBreakerTripped)]

// SetOnLoopBreakerTripped registers the function that blocks the issue and
// writes the alert (nil clears it). The handler registers it; the service has
// no business writing issue statuses or comments.
func SetOnLoopBreakerTripped(fn func(context.Context, LoopBreakerTripped)) {
	if fn == nil {
		onLoopBreakerTripped.Store(nil)
		return
	}
	onLoopBreakerTripped.Store(&fn)
}

// guardLoopBreaker refuses the run when the agent already started MaxRuns-1
// runs on the issue in the window. The count reads the task table through q,
// so it sees the enqueue transaction's own earlier inserts.
func guardLoopBreaker(ctx context.Context, q *db.Queries, issueID pgtype.UUID, target RunTarget) error {
	cfg := LoopBreakerSettings()
	if cfg.MaxRuns <= 0 || !target.AgentID.Valid {
		return nil
	}
	since := pgtype.Timestamptz{Time: time.Now().Add(-cfg.Window), Valid: true}
	runs, err := q.CountAgentIssueRunsSince(ctx, db.CountAgentIssueRunsSinceParams{IssueID: issueID, AgentID: target.AgentID, CreatedAt: since})
	if err != nil {
		return fmt.Errorf("loop breaker: %w", err)
	}
	if runs < int64(cfg.MaxRuns-1) {
		return nil
	}
	if hook := onLoopBreakerTripped.Load(); hook != nil {
		event := LoopBreakerTripped{IssueID: issueID, AgentID: target.AgentID, Runs: runs, Window: cfg.Window, Triggers: []string{target.Trigger}}
		// Best effort: the alert loses its trigger list, never the refusal.
		if kinds, err := q.ListAgentIssueRunTriggersSince(ctx, db.ListAgentIssueRunTriggersSinceParams{
			IssueID: issueID, AgentID: target.AgentID, CreatedAt: since, Limit: int32(cfg.MaxRuns - 1),
		}); err == nil {
			event.Triggers = append(event.Triggers, kinds...)
		}
		if issue, err := q.GetIssue(ctx, issueID); err == nil {
			event.Status = issue.Status
		}
		// The guard runs inside the enqueue transaction; the hook writes the
		// issue on its own connection after the caller is done with it.
		goRunBudgetHook(func() { (*hook)(context.WithoutCancel(ctx), event) })
	}
	return ErrLoopBreaker
}
