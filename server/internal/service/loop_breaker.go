package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
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
	// Deferred means the refused run is a review run of a PR in its owner's
	// stage queue: it waits there and the release job starts it once the window
	// clears, so the handler does not block the issue (CHE-9722).
	Deferred bool
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
	// A review run of a queued PR is deferred, not dropped: the refusal is
	// recorded as a stage wait and the release job retries it until the window
	// clears, so the run rate stays at the breaker's limit.
	deferred, err := deferLoopBreakerRefusal(ctx, q, issueID, target)
	if err != nil {
		return err
	}
	if stageReleaseFrom(ctx) {
		// The release job re-tries every tick: refuse again, quietly.
		return refusalOrDeferred(deferred)
	}
	if hook := onLoopBreakerTripped.Load(); hook != nil {
		event := LoopBreakerTripped{IssueID: issueID, AgentID: target.AgentID, Runs: runs, Window: cfg.Window, Triggers: []string{target.Trigger}, Deferred: deferred != nil}
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
	return refusalOrDeferred(deferred)
}

func refusalOrDeferred(deferred *stageFullError) error {
	if deferred == nil {
		return ErrLoopBreaker
	}
	return &loopBreakerDeferredError{full: deferred}
}

// loopBreakerDeferredError is ErrLoopBreaker for a run that waits in the stage
// queue. It is deliberately not ErrStageQueueFull: callers keep reporting the
// loop breaker. errors.As(err, **stageFullError) finds the wait that
// recordStageRefusal writes again after a rollback.
type loopBreakerDeferredError struct{ full *stageFullError }

func (*loopBreakerDeferredError) Error() string { return ErrLoopBreaker.Error() }
func (*loopBreakerDeferredError) Unwrap() error { return ErrLoopBreaker }
func (e *loopBreakerDeferredError) As(target any) bool {
	if t, ok := target.(**stageFullError); ok {
		*t = e.full
		return true
	}
	return false
}

// deferLoopBreakerRefusal records a review run the breaker refused as a stage
// wait, when the issue is a PR in its owner's stage queue. It returns nil, nil
// for every other run: those keep the breaker's block.
func deferLoopBreakerRefusal(ctx context.Context, q *db.Queries, issueID pgtype.UUID, target RunTarget) (*stageFullError, error) {
	if !target.ReviewRun || !target.AgentID.Valid || !issueID.Valid {
		return nil, nil
	}
	issue, err := q.GetIssue(ctx, issueID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("loop breaker: load issue: %w", err)
	}
	stage, ok := stageOf(issue, target.AgentID)
	if !ok {
		return nil, nil
	}
	entries, err := stageEntries(ctx, q, stage, target.AgentID, issueID)
	if err != nil {
		return nil, fmt.Errorf("loop breaker: list stage: %w", err)
	}
	ref := util.UUIDToString(issueID)
	inStage := false
	for _, e := range entries {
		inStage = inStage || e.Ref == ref
	}
	if !inStage {
		return nil, nil
	}
	taint, err := ActingTaskTaint(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("loop breaker: read requester taint: %w", err)
	}
	wait := &stageFullError{
		issueID: issueID, agentID: target.AgentID, actor: target.ActorUserID, comment: target.TriggerCommentID,
		taint: append(append([]string(nil), taint...), stageTaint(ctx)...),
	}
	if err := markStageWaiting(ctx, q, wait); err != nil {
		return nil, err
	}
	return wait, nil
}
