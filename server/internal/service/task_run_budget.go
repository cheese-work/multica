package service

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/issuestatus"
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

// statusMerged is the "Code Accepted" status: a linked PR merged. It sits in the
// started category, so the budget names it besides the done and closed ones.
const statusMerged = "merged"

// RunTarget is the run an enqueue asks for: the agent that would receive it
// and what asked (comment, mention, retry...). The guard needs both to tell the
// reviewer's first run on a head from other bookkeeping, and to say what it
// refused.
type RunTarget struct {
	AgentID pgtype.UUID
	Trigger string
}

// RunBudgetExhausted describes the refusal that fired the hook: how many runs
// the issue has and the cap it hit, and the run that was refused (Target's
// trigger, for Target's agent, while the issue sat in Status).
type RunBudgetExhausted struct {
	IssueID pgtype.UUID
	Runs    int64
	Max     int
	Status  string
	Target  RunTarget
}

var onRunBudgetExhausted atomic.Pointer[func(context.Context, RunBudgetExhausted)]

// SetOnRunBudgetExhausted registers the function that blocks an issue once its
// budget is spent (nil clears it). The handler registers it; the service has
// no business writing issue statuses.
func SetOnRunBudgetExhausted(fn func(context.Context, RunBudgetExhausted)) {
	if fn == nil {
		onRunBudgetExhausted.Store(nil)
		return
	}
	onRunBudgetExhausted.Store(&fn)
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
func guardRunBudget(ctx context.Context, q *db.Queries, issueID pgtype.UUID, target RunTarget) error {
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
	if issue.MaxRunsOverride.Valid && int(issue.MaxRunsOverride.Int32) > max {
		max = int(issue.MaxRunsOverride.Int32)
	}
	// The claim runs before the cap check so that a run inside the budget also
	// marks its head as reviewed: a second run on that head stays budgeted.
	first, err := claimFirstReviewOfHead(ctx, q, issue, target)
	if err != nil {
		return fmt.Errorf("run budget: review admission: %w", err)
	}
	if runs < int64(max) {
		return nil
	}
	// The budget stops loops. It does not decide the end of an issue's work, so
	// a derived run for a finished issue is dropped and its status kept.
	if isFinishedStatus(ctx, q, issue) {
		return ErrRunBudgetExhausted
	}
	if first {
		return nil
	}
	if hook := onRunBudgetExhausted.Load(); hook != nil {
		// The guard runs inside the enqueue transaction; the hook writes the
		// issue on its own connection after the caller is done with it.
		go (*hook)(context.WithoutCancel(ctx), RunBudgetExhausted{
			IssueID: issue.ID, Runs: runs, Max: max,
			Status: issue.Status, Target: target,
		})
	}
	return ErrRunBudgetExhausted
}

// IsFinishedStatus reports whether a status ends the issue's work: merged, or
// any done or closed category status.
func IsFinishedStatus(ctx context.Context, q issuestatus.Querier, workspaceID pgtype.UUID, status string) bool {
	if status == statusMerged {
		return true
	}
	category := issuestatus.Category(ctx, q, workspaceID, status)
	return category == issuestatus.CategoryDone || category == issuestatus.CategoryClosed
}

func isFinishedStatus(ctx context.Context, q *db.Queries, issue db.Issue) bool {
	return IsFinishedStatus(ctx, q, issue.WorkspaceID, issue.Status)
}

// claimFirstReviewOfHead reports whether the run is the first one for the
// issue's reviewer on the PR head under review: the review gate, which the
// budget does not refuse. Only the issue's assignee while the issue is in
// in_review counts as the reviewer, so a leader's or a mention's run never
// spends the allowance, and only the one head the run is pinned to
// (GetIssueReviewHeadSha) is claimed. The claim is one insert on the
// (issue, head) key, so it is atomic on a pool-scoped handle as well as in a
// transaction, and concurrent enqueues for one head cannot both win.
// ponytail: a pool-scoped enqueue that fails after the claim (a dedup against a
// pending run on the same head, an archived agent) leaves the head spent; a
// person raises the cap with `multica issue budget raise`. Upgrade to a
// task-scoped claim if such failures prove common.
func claimFirstReviewOfHead(ctx context.Context, q *db.Queries, issue db.Issue, target RunTarget) (bool, error) {
	if issue.Status != issuestatus.InReview || issue.AssigneeType.String != "agent" ||
		!target.AgentID.Valid || target.AgentID != issue.AssigneeID {
		return false, nil
	}
	head, err := q.GetIssueReviewHeadSha(ctx, issue.ID)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && head == "") {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	_, err = q.ClaimIssueReviewHead(ctx, db.ClaimIssueReviewHeadParams{
		WorkspaceID: issue.WorkspaceID, IssueID: issue.ID, HeadSha: head, AgentID: target.AgentID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}
