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

// statusAgentReview is the Default workflow's agent review step, a custom
// started status that hands the issue to its reviewer.
const statusAgentReview = "agent_review"

// RunTarget is the run an enqueue asks for: the agent that would receive it
// and what asked (comment, mention, retry...). The guard needs both to tell the
// reviewer's first run on a head from other bookkeeping, and to say what it
// refused. ReviewRun marks a path that can start the review (a comment,
// handoff or mention); retries and system wakeups never do.
type RunTarget struct {
	AgentID   pgtype.UUID
	Trigger   string
	ReviewRun bool
}

// ReviewGate is what the guard hands the enqueue: the PR head the run reviews
// (empty when the run is not the issue's reviewer) and whether the budget is
// spent, so the head's admission is the only way in. The enqueue writes the
// admission with the task row (CreateAgentTask), never the guard.
type ReviewGate struct {
	Head     string
	Required bool
}

// Apply sets the gate on the task row the enqueue is about to insert. A review
// run is stamped with the head the gate admits, never a head resolved again
// later: the admission and the task must name the one head the run reviews,
// even when the PR moves between the guard and the insert or between
// preparing a mention and inserting it.
func (g ReviewGate) Apply(p *db.CreateAgentTaskParams) {
	if g.Head != "" {
		p.HeadSha = pgtype.Text{String: g.Head, Valid: true}
	}
	p.GateHead = pgtype.Text{String: g.Head, Valid: g.Head != ""}
	p.GateRequired = pgtype.Bool{Bool: g.Required, Valid: g.Required}
}

// createAgentTask inserts the task row. A review run over a spent budget that
// lost the head's admission to a concurrent enqueue writes no row and counts as
// a duplicate: the winner is the review.
func createAgentTask(ctx context.Context, q *db.Queries, p db.CreateAgentTaskParams) (db.AgentTaskQueue, error) {
	acting, err := ActingTaskTaint(ctx, q)
	if err != nil {
		return db.AgentTaskQueue{}, err
	}
	p.TaintIds = append(p.TaintIds, acting...)
	task, err := q.CreateAgentTask(ctx, p)
	if errors.Is(err, pgx.ErrNoRows) && p.GateRequired.Bool {
		return db.AgentTaskQueue{}, ErrDuplicatePendingTask
	}
	if err == nil {
		err = noteRunInserted(ctx, q, p.IssueID, task.ID)
	}
	return task, err
}

// RunBudgetExhausted describes the refusal that fired the hook: how many runs
// the issue has and the cap it hit, and the run that was refused (Target's
// trigger, for Target's agent, while the issue sat in Status).
type RunBudgetExhausted struct {
	IssueID pgtype.UUID
	Runs    int64
	Max     int
	Since   time.Time
	Status  string
	Target  RunTarget
	// TokensSpent is true when the token balance, not the run count, refused
	// the run; Tokens is that balance.
	TokensSpent bool
	Tokens      int64
}

// Run-budget notice kinds (CHE-2297 L1a). They are also the kind half of the
// notice claim key.
const (
	RunBudgetKindWarn80    = "warn80"
	RunBudgetKindExhausted = "exhausted"
	// RunBudgetKindWarn80Tokens is the 80% warning of the token balance.
	RunBudgetKindWarn80Tokens = "warn80_tokens"
)

// RunBudgetEvent describes one budget notice: its kind, how many runs the issue
// has and the cap, and the start of the budget window it belongs to. Xid is the
// transaction that inserted the run behind a warning (0 when unknown), so the
// handler can follow it to commit or rollback.
type RunBudgetEvent struct {
	IssueID pgtype.UUID
	Kind    string
	Runs    int64
	Max     int
	Since   time.Time
	Xid     int64
	// Tokens and TokenBase are the token balance and the window's allowance
	// behind a RunBudgetKindWarn80Tokens event.
	Tokens    int64
	TokenBase int64
}

var onRunBudgetEvent atomic.Pointer[func(context.Context, RunBudgetEvent)]

// SetOnRunBudgetEvent registers the function that writes the 80% warning (nil
// clears it). The handler registers it; the service has no business writing
// comments or inbox items.
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

// noteRunInserted fires the 80% warning when the task row q just inserted is the
// run that reaches it. It runs after the insert, on the insert's own handle, so
// the run count includes the new row and the transaction id is the inserting
// transaction's: the handler follows that transaction to commit or rollback.
// It first debits the run to the ledger: a failed debit is returned, so the
// enqueue transaction rolls the run back. The warning is best effort: a failed
// read loses the warning, never the run.
func noteRunInserted(ctx context.Context, q *db.Queries, issueID, taskID pgtype.UUID) error {
	if !issueID.Valid {
		return nil
	}
	if err := debitRun(ctx, q, issueID, taskID); err != nil {
		return fmt.Errorf("run budget: debit run: %w", err)
	}
	hook := onRunBudgetEvent.Load()
	if hook == nil {
		return nil
	}
	issue, err := q.GetIssue(ctx, issueID)
	if err != nil {
		return nil
	}
	usage, err := loadRunBudgetUsage(ctx, q, issue)
	if err != nil || usage == nil || usage.Runs != runBudgetWarnAt(usage.Max) {
		return nil
	}
	// A failed read leaves Xid 0: the handler then only counts committed runs.
	xid, _ := q.CurrentXactID(ctx)
	go (*hook)(context.WithoutCancel(ctx), RunBudgetEvent{IssueID: issue.ID, Kind: RunBudgetKindWarn80, Runs: usage.Runs, Max: usage.Max, Since: usage.Since, Xid: xid})
	return nil
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
func guardRunBudget(ctx context.Context, q *db.Queries, issueID pgtype.UUID, target RunTarget) (ReviewGate, error) {
	issue, usage, balance, err := openRunBudget(ctx, q, issueID, pgtype.UUID{})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ReviewGate{}, nil
		}
		return ReviewGate{}, fmt.Errorf("run budget: %w", err)
	}
	if usage == nil {
		return ReviewGate{}, nil
	}
	runs, max := usage.Runs, usage.Max
	spent := balance < 1
	tokens, tokensSpent, err := tokenBalanceSpent(ctx, q, issue, usage)
	if err != nil {
		return ReviewGate{}, fmt.Errorf("run budget: %w", err)
	}
	spent = spent || tokensSpent
	// The budget stops loops. It does not decide the end of an issue's work, so
	// a derived run for a finished issue is dropped and its status kept.
	if spent && isFinishedStatus(ctx, q, issue) {
		return ReviewGate{}, ErrRunBudgetExhausted
	}
	head, admitted, err := reviewHeadOf(ctx, q, issue, target)
	if err != nil {
		return ReviewGate{}, fmt.Errorf("run budget: review gate: %w", err)
	}
	if !spent {
		return ReviewGate{Head: head}, nil
	}
	if head != "" && !admitted {
		return ReviewGate{Head: head, Required: true}, nil
	}
	if hook := onRunBudgetExhausted.Load(); hook != nil {
		// The guard runs inside the enqueue transaction; the hook writes the
		// issue on its own connection after the caller is done with it.
		go (*hook)(context.WithoutCancel(ctx), RunBudgetExhausted{
			IssueID: issue.ID, Runs: runs, Max: max, Since: usage.Since,
			Status: issue.Status, Target: target, TokensSpent: tokensSpent && balance >= 1, Tokens: tokens,
		})
	}
	return ReviewGate{}, ErrRunBudgetExhausted
}

// IsReviewStatus reports whether a status hands the issue to its reviewer: the
// built-in in_review, and the Default workflow's agent_review step. The
// workflow definition has no review marker, so the two keys are named here.
func IsReviewStatus(status string) bool {
	return status == issuestatus.InReview || status == statusAgentReview
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

// reviewHeadOf names the PR head the run reviews and whether that head's first
// review was already admitted. Only the issue's assignee while the issue is in
// a review status (IsReviewStatus) is the reviewer, so a leader's, a mention's or an earlier
// in_progress run never counts, and only the one head the run is pinned to
// (GetIssueReviewHeadSha) does. The guard only reads: the enqueue writes the
// admission together with the task row, so a failed, deduplicated or rolled
// back enqueue leaves the head unreviewed.
func reviewHeadOf(ctx context.Context, q *db.Queries, issue db.Issue, target RunTarget) (head string, admitted bool, err error) {
	if !target.ReviewRun || !IsReviewStatus(issue.Status) || issue.AssigneeType.String != "agent" ||
		!target.AgentID.Valid || target.AgentID != issue.AssigneeID {
		return "", false, nil
	}
	head, err = issueReviewHead(ctx, q, issue.ID)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && head == "") {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	admitted, err = q.HasIssueReviewAdmission(ctx, db.HasIssueReviewAdmissionParams{IssueID: issue.ID, HeadSha: head})
	return head, admitted, err
}
