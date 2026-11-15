package service

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"strings"
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

// ErrAgentArchived is returned by an enqueue whose agent is archived: no retry
// can change it.
var ErrAgentArchived = errors.New("agent is archived")

// ErrRunBudgetExhausted is returned by a derived enqueue path when the issue
// already used its workflow's run budget. Handlers render it as 403
// run_budget_exhausted; background callers log it and drop the trigger.
var ErrRunBudgetExhausted = errors.New("the issue used its workflow's run budget, so nothing derived from it runs")

// ErrStageQueueFull is returned by an enqueue path when the PR's bottleneck
// stage already runs its capacity of PRs (CHE-3611). The PR waits in the stage
// queue; handlers render it as 409 stage_queue_full and background callers
// drop the trigger, which the stage's next free slot re-issues.
var ErrStageQueueFull = errors.New("the stage is at capacity, so the PR is queued")

// statusMerged is the "Code Accepted" status: a linked PR merged. It sits in the
// started category, so the budget names it besides the done and closed ones.
const statusMerged = "merged"

// statusAgentReview is the Default workflow's agent review step, a custom
// started status that hands the issue to its reviewer.
const statusAgentReview = "agent_review"

// statusNeedDecision is the workflow step an issue waits in for a decision answer.
const statusNeedDecision = "need_decision"

// RunTarget is the run an enqueue asks for: the agent that would receive it
// and what asked (comment, mention, retry...). The guard needs both to tell the
// reviewer's first run on a head from other bookkeeping, and to say what it
// refused. ReviewRun marks a path that can start the review (a comment,
// handoff or mention); retries and system wakeups never do.
type RunTarget struct {
	AgentID   pgtype.UUID
	Trigger   string
	ReviewRun bool
	// ActorUserID and TriggerCommentID name who asked for the run and the
	// comment that asked. A stage refusal keeps them so the release starts the
	// run under the same attribution (CHE-3611).
	ActorUserID      pgtype.UUID
	TriggerCommentID pgtype.UUID
	// SourceWakeupID is the wakeup rule, and SourceTaskID the failed run, whose
	// own dispatch the refusal stopped. The refusal keeps them so the resume
	// runs that dispatch again and binds to what it delivers (CHE-2860).
	SourceWakeupID pgtype.UUID
	SourceTaskID   pgtype.UUID
}

// ReviewGate is what the guard hands the enqueue: the PR head the run reviews
// (empty when the run is not the issue's reviewer) and whether the budget is
// spent, so the head's admission is the only way in. The enqueue writes the
// admission with the task row (CreateAgentTask), never the guard.
//
// Pin is the head the review admission judged: it stamps the task but grants no
// budget exception. Expired marks a run that starts because the head's 24 h
// deferral ran out; the handoff note says so.
type ReviewGate struct {
	Head     string
	Required bool
	Pin      string
	Expired  bool
}

// ErrReviewHeadMoved refuses an expired-deferral start when the budget and the
// admission read different heads: neither can vouch for the other.
var ErrReviewHeadMoved = errors.New("review head moved during the enqueue")

// withExpiryNote returns the handoff note with the deferral-expired reason set
// or removed, whatever an earlier Apply left on it.
func withExpiryNote(note pgtype.Text, expired bool) pgtype.Text {
	base := note.String
	if base == deferralExpiredNote {
		base = ""
	} else {
		base = strings.TrimSuffix(base, "\n"+deferralExpiredNote)
	}
	if expired {
		if base != "" {
			base += "\n"
		}
		base += deferralExpiredNote
	}
	return pgtype.Text{String: base, Valid: base != ""}
}

// Apply sets the gate on the task row the enqueue is about to insert. A review
// run is stamped with the head the gate admits, never a head resolved again
// later: the admission and the task must name the one head the run reviews,
// even when the PR moves between the guard and the insert or between
// preparing a mention and inserting it.
func (g ReviewGate) Apply(p *db.CreateAgentTaskParams) {
	if head := cmp.Or(g.Head, g.Pin); head != "" {
		p.HeadSha = pgtype.Text{String: head, Valid: true}
	}
	p.HandoffNote = withExpiryNote(p.HandoffNote, g.Expired)
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
	p.TaintIds = append(p.TaintIds, stageTaint(ctx)...)
	task, err := createProviderHeldAgentTask(ctx, q, p)
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
	// Balance is the admitted run balance the refusal read; the suggested raise
	// starts from it, not from Runs.
	Balance int64
	Since   time.Time
	Status  string
	Target  RunTarget
}

// Run-budget notice kinds (CHE-2297 L1a). They are also the kind half of the
// notice claim key.
const (
	RunBudgetKindWarn80    = "warn80"
	RunBudgetKindExhausted = "exhausted"
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

// runBudgetWarnAt is the balance that triggers the 80% warning: what is left
// when the cap is 80% spent.
func runBudgetWarnAt(max int) int64 {
	return int64(max) - int64((max*4+4)/5)
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
	// The debit above is in the transaction, so the peek sees it.
	_, usage, left, err := openRunBudget(readOnlyRunBudget(ctx), q, issueID, pgtype.UUID{})
	if err != nil || usage == nil || left != runBudgetWarnAt(usage.Max) {
		return nil
	}
	// A failed read leaves Xid 0: the handler then only counts committed runs.
	xid, _ := q.CurrentXactID(ctx)
	event := RunBudgetEvent{IssueID: issue.ID, Kind: RunBudgetKindWarn80, Runs: usage.Runs, Max: usage.Max, Since: usage.Since, Xid: xid}
	goRunBudgetHook(func() { (*hook)(context.WithoutCancel(ctx), event) })
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
// grants it a new budget). Since the S1d cutover only an interactive session's
// move counts, not a PAT's or a CLI's (LatestMemberStatusChange).
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
	if hook := onRunBudgetExhausted.Load(); hook != nil && !hookSuppressed(ctx) {
		// The guard runs inside the enqueue transaction; the hook writes the
		// issue on its own connection after the caller is done with it.
		exhausted := RunBudgetExhausted{
			IssueID: issue.ID, Runs: runs, Max: max, Balance: balance, Since: usage.Since,
			Status: issue.Status, Target: target,
		}
		goRunBudgetHook(func() { (*hook)(context.WithoutCancel(ctx), exhausted) })
	}
	return ReviewGate{}, ErrRunBudgetExhausted
}

type suppressRunBudgetHookKey struct{}

// WithoutRunBudgetHook stops the guard from firing the exhaustion hook for an
// admission inside ctx. A resume attempt that the budget refuses again rolls
// back, so the refusal it would record belongs to a transaction that never
// happened; the attempt records its own outcome on the refusal row.
func WithoutRunBudgetHook(ctx context.Context) context.Context {
	return context.WithValue(ctx, suppressRunBudgetHookKey{}, true)
}

func hookSuppressed(ctx context.Context) bool {
	suppressed, _ := ctx.Value(suppressRunBudgetHookKey{}).(bool)
	return suppressed
}

// RunBudgetStillRefuses reports whether the window a refusal was for is still
// the issue's current window and still refuses a run. A top-up opens a new
// window and a progress grant restores balance, so the refusal's delayed
// callback checks this before it writes.
func RunBudgetStillRefuses(ctx context.Context, q *db.Queries, issueID pgtype.UUID, since time.Time) (bool, error) {
	ctx = readOnlyRunBudget(ctx)
	_, usage, balance, err := openRunBudget(ctx, q, issueID, pgtype.UUID{})
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && (usage == nil || !usage.Since.Equal(since))) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("run budget: %w", err)
	}
	return balance < 1, nil
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

type withoutReviewRunKey struct{}

// withoutReviewRun marks an enqueue whose run is not a review, whatever agent
// it targets: it never takes the review-head exception to a spent budget.
func withoutReviewRun(ctx context.Context) context.Context {
	return context.WithValue(ctx, withoutReviewRunKey{}, true)
}

// reviewHeadOf names the PR head the run reviews and whether that head's first
// review was already admitted. Only the issue's assignee while the issue is in
// a review status (IsReviewStatus) is the reviewer, so a leader's, a mention's or an earlier
// in_progress run never counts, and only the one head the run is pinned to
// (GetIssueReviewHeadSha) does. The guard only reads: the enqueue writes the
// admission together with the task row, so a failed, deduplicated or rolled
// back enqueue leaves the head unreviewed.
func reviewHeadOf(ctx context.Context, q *db.Queries, issue db.Issue, target RunTarget) (head string, admitted bool, err error) {
	if !target.ReviewRun || ctx.Value(withoutReviewRunKey{}) != nil || !IsReviewStatus(issue.Status) || issue.AssigneeType.String != "agent" ||
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

// RunBudgetBindAt runs inside a native dispatch's transaction right after it
// bound a refusal to the run it delivered, before the commit. A test returns
// true to stop there, as a crash would; the dispatch then rolls back.
var RunBudgetBindAt = func(stage string) bool { return false }

// ErrRunBudgetBindCrashed is what a dispatch returns when RunBudgetBindAt stopped it.
var ErrRunBudgetBindCrashed = errors.New("run budget: dispatch stopped before commit")

// bindRefusedRun ends the refusal an approval linked on the run a native
// dispatch (a wakeup tick, a system wakeup or a retry) just delivered for the
// source the refusal stopped (CHE-2860). It runs in the dispatch's transaction,
// under the issue lock the dispatch holds, so the run, the receipts or retry
// it consumed and the refusal end together or not at all. A dispatch no
// refusal waits on binds nothing. The refusal's taint joins the run (the task
// trigger keeps it monotonic).
func bindRefusedRun(ctx context.Context, q *db.Queries, issueID, wakeupID, parentTaskID, taskID pgtype.UUID) error {
	if !wakeupID.Valid && !parentTaskID.Valid {
		return nil
	}
	if _, err := q.LockIssueRunBudget(ctx, issueID); err != nil {
		return err
	}
	taints, err := q.BindRunBudgetRefusalToRun(ctx, db.BindRunBudgetRefusalToRunParams{
		IssueID: issueID, TaskID: taskID, SourceWakeupID: wakeupID, SourceTaskID: parentTaskID,
	})
	if err != nil || len(taints) == 0 {
		return err
	}
	for _, taint := range taints {
		if len(taint) == 0 {
			continue
		}
		if err := q.UnionTaskTaint(ctx, db.UnionTaskTaintParams{ID: taskID, Taint: taint}); err != nil {
			return err
		}
	}
	if RunBudgetBindAt("after_bind") {
		return ErrRunBudgetBindCrashed
	}
	return nil
}
