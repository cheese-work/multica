package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/issueworkflow"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// RunBudgetSnapshot is what a client reads about one issue's run budget: the
// runs used in the open window against the limit in force, and what a raise
// would do. The budget counts runs only (CHE-5501).
type RunBudgetSnapshot struct {
	Budgeted bool  `json:"budgeted"`
	RunsUsed int64 `json:"runs_used"`
	// RunsLeft is the admitted balance: what the guard reads, so grants and
	// refunds show here. Negative when named runs went past it.
	RunsLeft        int64     `json:"runs_left"`
	RunsLimit       int       `json:"runs_limit"`
	RunsSource      string    `json:"runs_source,omitempty"` // "workflow" or "issue"
	WindowStartedAt time.Time `json:"window_started_at"`
	// WindowID is opaque to clients: a budget action echoes it back so the
	// server can tell whether the window it was asked about is still open.
	WindowID     string            `json:"window_id"`
	RunsState    string            `json:"runs_state,omitempty"`
	State        string            `json:"state,omitempty"`
	Blocking     []string          `json:"blocking"`
	CanRaise     bool              `json:"can_raise"`
	RaisePreview *RunBudgetPreview `json:"raise_preview,omitempty"`
}

// RunBudgetPreview is the suggested "allow N more runs" and its result.
type RunBudgetPreview struct {
	AddRuns       int `json:"add_runs"`
	BalanceAfter  int `json:"balance_after"`
	LimitAfter    int `json:"limit_after"`
	MaxAllowedAdd int `json:"max_allowed_add"`
}

const (
	RunBudgetOK      = "ok"
	RunBudgetNear    = "near"
	RunBudgetAtLimit = "at_limit"
	RunBudgetOver    = "over"
)

// NewRunBudgetSnapshot builds the snapshot from the counted runs, the limit in
// force and the admitted balance. The state is near once the balance is down to
// 20% of the limit, at_limit when the balance is spent (the guard refuses the
// next derived run) and over when named runs went past it.
func NewRunBudgetSnapshot(used int64, limit int, left int64, source string, windowStart time.Time) RunBudgetSnapshot {
	snap := RunBudgetSnapshot{
		Budgeted: true, RunsUsed: used, RunsLeft: left, RunsLimit: limit, RunsSource: source,
		WindowStartedAt: windowStart, WindowID: runBudgetWindowID(windowStart), Blocking: []string{},
	}
	switch {
	case left < 0:
		snap.RunsState = RunBudgetOver
	case left == 0:
		snap.RunsState = RunBudgetAtLimit
	case left*5 <= int64(limit):
		snap.RunsState = RunBudgetNear
	default:
		snap.RunsState = RunBudgetOK
	}
	snap.State = snap.RunsState
	if left < 1 {
		snap.Blocking = append(snap.Blocking, "runs")
	}
	// The raise starts from the admitted balance, not the task count: a grant, a
	// refund or carried debt moves one and not the other.
	base := raiseUsed(limit, left)
	if room := MaxAllowedRaise(limit, base); room > 0 {
		add := SuggestedRaise(limit, base)
		if raise, err := PreviewRunBudgetRaise(limit, base, add); err == nil {
			snap.CanRaise = true
			snap.RaisePreview = &RunBudgetPreview{
				AddRuns: add, BalanceAfter: raise.BalanceAfter, LimitAfter: raise.CreditedAfter, MaxAllowedAdd: room,
			}
		}
	}
	return snap
}

// runBudgetUsage is one issue's budget window as the guard and the snapshot
// both read it.
type runBudgetUsage struct {
	Runs   int64
	Max    int
	Source string
	Since  time.Time
}

// loadRunBudgetUsage reads the issue's workflow cap and the runs counted in its
// open window. It returns nil for an issue that is not budgeted: no workflow,
// or a workflow with no stored budget start (the built-in Default nobody saved).
func loadRunBudgetUsage(ctx context.Context, q *db.Queries, issue db.Issue) (*runBudgetUsage, error) {
	if !issue.ProjectID.Valid && !issue.WorkflowID.Valid {
		return nil, nil
	}
	def, _, err := issueworkflow.ForIssue(ctx, q, issue.WorkspaceID, issue.WorkflowID, issue.ProjectID)
	if errors.Is(err, issueworkflow.ErrOverrideMissing) || (err == nil && (def == nil || def.RunBudgetSince.IsZero())) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("run budget: load workflow: %w", err)
	}
	since, err := RunBudgetStart(ctx, q, issue.ID, def)
	if err != nil {
		return nil, fmt.Errorf("run budget: window start: %w", err)
	}
	runs, err := q.CountIssueRunsSince(ctx, db.CountIssueRunsSinceParams{IssueID: issue.ID, Since: pgtype.Timestamptz{Time: since, Valid: true}})
	if err != nil {
		return nil, fmt.Errorf("run budget: count runs: %w", err)
	}
	usage := &runBudgetUsage{Runs: runs, Max: def.EffectiveMaxRuns(), Source: "workflow", Since: since}
	if issue.MaxRunsOverride.Valid && int(issue.MaxRunsOverride.Int32) > usage.Max {
		usage.Max, usage.Source = int(issue.MaxRunsOverride.Int32), "issue"
	}
	return usage, nil
}

// LoadRunBudgetSnapshot reads one issue's budget. An issue that is not
// budgeted gets a snapshot with Budgeted false.
func LoadRunBudgetSnapshot(ctx context.Context, q *db.Queries, issue db.Issue) (RunBudgetSnapshot, error) {
	// The read-only form of the balance admission reads: it writes no entry.
	_, usage, left, err := openRunBudget(readOnlyRunBudget(ctx), q, issue.ID, pgtype.UUID{})
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && usage == nil) {
		return RunBudgetSnapshot{Blocking: []string{}}, nil
	}
	if err != nil {
		return RunBudgetSnapshot{Blocking: []string{}}, err
	}
	return NewRunBudgetSnapshot(usage.Runs, usage.Max, left, usage.Source, usage.Since), nil
}

// CurrentRunBudget reads the issue's cap and run balance as admission would
// now, without writing. The exhaustion notice prices its raise from this, not
// from the balance the refusal saw: a named run or a grant can land between.
func CurrentRunBudget(ctx context.Context, q *db.Queries, issueID pgtype.UUID) (max int, balance int64, err error) {
	_, usage, balance, err := openRunBudget(readOnlyRunBudget(ctx), q, issueID, pgtype.UUID{})
	if err != nil {
		return 0, 0, err
	}
	if usage == nil {
		return 0, 0, pgx.ErrNoRows
	}
	return usage.Max, balance, nil
}
