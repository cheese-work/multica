package service

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/issueworkflow"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// The run budget (CHE-2152): a workflow caps how many runs one issue can use,
// so a loop of retries, bounces and wakeups cannot spend without limit. The cap
// sits at the same queue door as the Triage rule, for the same reason: several
// paths enqueue, and only the last point every one of them passes through can
// refuse them all.
//
// A person naming an agent by hand (OriginNamed) is not counted against the
// door: that run is a conversation the person chose to pay for.

// ErrRunBudgetExhausted is returned by a derived enqueue path when the issue
// already used its workflow's run budget. Handlers render it as 403
// run_budget_exhausted; background callers log it and drop the trigger.
var ErrRunBudgetExhausted = errors.New("the issue used its workflow's run budget, so nothing derived from it runs")

// Run-budget notice kinds (CHE-2297 L1a). They are also the kind half of the
// notice claim key.
const (
	RunBudgetKindWarn80    = "warn80"
	RunBudgetKindExhausted = "exhausted"
)

// RunBudgetEvent describes the admission or refusal that fired the hook: the
// notice kind, how many runs the issue has and the cap, and the start of the
// budget window the event belongs to.
type RunBudgetEvent struct {
	IssueID pgtype.UUID
	Kind    string
	Runs    int64
	Max     int
	Since   time.Time
}

var onRunBudgetEvent atomic.Pointer[func(context.Context, RunBudgetEvent)]

// SetOnRunBudgetEvent registers the function that writes the notices and blocks
// an issue once its budget is spent (nil clears it). The handler registers it;
// the service has no business writing issue statuses or inbox items.
func SetOnRunBudgetEvent(fn func(context.Context, RunBudgetEvent)) {
	if fn == nil {
		onRunBudgetEvent.Store(nil)
		return
	}
	onRunBudgetEvent.Store(&fn)
}

// runBudgetWarnAt is the run count that triggers the 80% warning.
func runBudgetWarnAt(max int) int64 {
	return int64((max*4 + 4) / 5)
}

func fireRunBudgetEvent(ctx context.Context, e RunBudgetEvent) {
	if hook := onRunBudgetEvent.Load(); hook != nil {
		// The guard runs inside the enqueue transaction; the hook writes on its
		// own connection after the caller is done with it.
		go (*hook)(context.WithoutCancel(ctx), e)
	}
}

// RunBudgetStart is when an issue's budget window opens: the later of the
// workflow's budget start (issues in flight at rollout begin fresh) and the
// last time a person moved the issue's status (a person unblocking an issue
// grants it a new budget).
func RunBudgetStart(ctx context.Context, q *db.Queries, issueID pgtype.UUID, def *issueworkflow.Definition) (time.Time, error) {
	since := def.RunBudgetSince
	moved, err := q.LatestMemberStatusChange(ctx, issueID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, err
	}
	if err == nil && moved.Valid && moved.Time.After(since) {
		since = moved.Time
	}
	return since, nil
}

// guardRunBudget refuses a derived enqueue for an issue whose workflow budget
// is spent. An issue with no workflow, or a workflow with no stored budget
// start (the built-in Default nobody saved), is not budgeted.
func guardRunBudget(ctx context.Context, q *db.Queries, issueID pgtype.UUID) error {
	issue, err := q.GetIssue(ctx, issueID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return fmt.Errorf("run budget: load issue: %w", err)
	}
	if !issue.ProjectID.Valid && !issue.WorkflowID.Valid {
		return nil
	}
	def, _, err := issueworkflow.ForIssue(ctx, q, issue.WorkspaceID, issue.WorkflowID, issue.ProjectID)
	if errors.Is(err, issueworkflow.ErrOverrideMissing) || (err == nil && (def == nil || def.RunBudgetSince.IsZero())) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("run budget: load workflow: %w", err)
	}
	since, err := RunBudgetStart(ctx, q, issue.ID, def)
	if err != nil {
		return fmt.Errorf("run budget: window start: %w", err)
	}
	runs, err := q.CountIssueRunsSince(ctx, db.CountIssueRunsSinceParams{IssueID: issue.ID, Since: pgtype.Timestamptz{Time: since, Valid: true}})
	if err != nil {
		return fmt.Errorf("run budget: count runs: %w", err)
	}
	max := def.EffectiveMaxRuns()
	if runs < int64(max) {
		// ponytail: exact match, so a named run that skips the count can skip the
		// warning; the exhaustion notice still fires.
		if runs+1 == runBudgetWarnAt(max) {
			fireRunBudgetEvent(ctx, RunBudgetEvent{IssueID: issue.ID, Kind: RunBudgetKindWarn80, Runs: runs + 1, Max: max, Since: since})
		}
		return nil
	}
	fireRunBudgetEvent(ctx, RunBudgetEvent{IssueID: issue.ID, Kind: RunBudgetKindExhausted, Runs: runs, Max: max, Since: since})
	return ErrRunBudgetExhausted
}
