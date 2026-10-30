package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/issueworkflow"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// RunBudgetSnapshot is what a client reads about one issue's run budget: the
// runs used in the open window against the limit in force, and what a raise
// would do. Token fields join it when CHE-2297 turns token accounting on.
type RunBudgetSnapshot struct {
	Budgeted        bool      `json:"budgeted"`
	RunsUsed        int64     `json:"runs_used"`
	RunsLimit       int       `json:"runs_limit"`
	RunsSource      string    `json:"runs_source,omitempty"` // "workflow" or "issue"
	WindowStartedAt time.Time `json:"window_started_at"`
	RunsState       string    `json:"runs_state,omitempty"`
	State           string    `json:"state,omitempty"`
	Blocking        []string  `json:"blocking"`
	// TokensActive is true once the issue is activated for token accounting;
	// the balance is then A credits minus the tokens used and held, and a
	// negative balance is overshoot.
	TokensActive    bool              `json:"tokens_active"`
	TokensBalance   int64             `json:"tokens_balance"`
	TokensOvershoot int64             `json:"tokens_overshoot"`
	CanRaise        bool              `json:"can_raise"`
	RaisePreview    *RunBudgetPreview `json:"raise_preview,omitempty"`
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

// NewRunBudgetSnapshot builds the snapshot from the counted runs and the limit
// in force. The state is near from 80% of the limit, at_limit when the limit is
// spent (the guard refuses the next derived run) and over when named runs went
// past it.
func NewRunBudgetSnapshot(used int64, limit int, source string, windowStart time.Time) RunBudgetSnapshot {
	snap := RunBudgetSnapshot{
		Budgeted: true, RunsUsed: used, RunsLimit: limit, RunsSource: source,
		WindowStartedAt: windowStart, Blocking: []string{},
	}
	switch {
	case used > int64(limit):
		snap.RunsState = RunBudgetOver
	case used == int64(limit):
		snap.RunsState = RunBudgetAtLimit
	case used*5 >= int64(limit)*4:
		snap.RunsState = RunBudgetNear
	default:
		snap.RunsState = RunBudgetOK
	}
	snap.State = snap.RunsState
	if used >= int64(limit) {
		snap.Blocking = append(snap.Blocking, "runs")
	}
	// Runs past the platform maximum still read as used; the clamp only keeps
	// the raise math on int.
	usedInt := int(min(used, int64(issueworkflow.MaxRunsPerIssueLimit)))
	if room := MaxAllowedRaise(limit, usedInt); room > 0 {
		add := SuggestedRaise(limit, usedInt)
		if raise, err := PreviewRunBudgetRaise(limit, usedInt, add); err == nil {
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
	// TokenAllowance (A) and TokenReserve (E) are the workflow's stored token
	// values; both are 0 while token accounting is off for the workflow.
	TokenAllowance int64
	TokenReserve   int64
	// ActivationMoved is the balance change activation made in this call, so the
	// usage report that activates can still be judged against 80%.
	ActivationMoved int64
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
	usage := &runBudgetUsage{
		Runs: runs, Max: def.EffectiveMaxRuns(), Source: "workflow", Since: since,
		TokenAllowance: def.TokenAllowance, TokenReserve: def.TokenReserve,
	}
	if issue.MaxRunsOverride.Valid && int(issue.MaxRunsOverride.Int32) > usage.Max {
		usage.Max, usage.Source = int(issue.MaxRunsOverride.Int32), "issue"
	}
	return usage, nil
}

// LoadRunBudgetSnapshot reads one issue's budget. An issue that is not
// budgeted gets a snapshot with Budgeted false.
func LoadRunBudgetSnapshot(ctx context.Context, q *db.Queries, issue db.Issue) (RunBudgetSnapshot, error) {
	usage, err := loadRunBudgetUsage(ctx, q, issue)
	if err != nil || usage == nil {
		return RunBudgetSnapshot{Blocking: []string{}}, err
	}
	snap := NewRunBudgetSnapshot(usage.Runs, usage.Max, usage.Source, usage.Since)
	return withTokenBalance(ctx, q, issue, usage, snap)
}

// withTokenBalance adds the token side to a snapshot once the issue is
// activated. A negative balance is overshoot: runs already admitted spent past
// it.
func withTokenBalance(ctx context.Context, q *db.Queries, issue db.Issue, usage *runBudgetUsage, snap RunBudgetSnapshot) (RunBudgetSnapshot, error) {
	if !usage.tokensStored() {
		return snap, nil
	}
	if _, found, err := activationEpoch(ctx, q, issue.ID); err != nil || !found {
		return snap, err
	}
	balance, err := q.SumIssueRunBudgetTokens(ctx, issue.ID)
	if err != nil {
		return snap, fmt.Errorf("run budget: token balance: %w", err)
	}
	snap.TokensActive, snap.TokensBalance = true, balance
	if balance < 0 {
		snap.TokensOvershoot = -balance
	}
	if balance <= 0 {
		snap.Blocking = append(snap.Blocking, "tokens")
	}
	return snap, nil
}

// tokensStored reports whether the workflow stores both token values.
func (u *runBudgetUsage) tokensStored() bool { return u.TokenAllowance > 0 && u.TokenReserve > 0 }
