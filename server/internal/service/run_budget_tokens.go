package service

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

// Token entries of the ledger (CHE-2297 L2b). All are written under the issue
// lock, in the window of the task's run entry.
//
//	reserve:<task id>      -E, written at admission with the E in force then.
//	tokens:<task id>:<n>   -(weighted tokens) not charged yet: the first report
//	                       writes all of it, a correction writes the difference.
//	release:<task id>      +the stored reservation, once, at the first usage
//	                       report. A task admitted before activation has no
//	                       reservation and so no release.
//
// Activation (L2e) turns token accounting on for one issue, once, when the
// workflow stores both A and E:
//
//	token_activation       unique per issue; its window is the epoch.
//	allowance:<window id>  +A, at most one per window, claimed through
//	                       claimRunBudgetAllowance by activation and by every
//	                       later window's opening.
const (
	runBudgetKindReserve    = "reserve"
	runBudgetKindRelease    = "release"
	runBudgetKindTokens     = "tokens"
	runBudgetKindActivation = "token_activation"
	runBudgetKindAllowance  = "allowance"
)

// RunBudgetTokens is the token side of an issue's budget: the tokens E held per
// admitted run and the start of the token epoch (tasks created before it are
// outside token accounting).
type RunBudgetTokens struct {
	Reserve int64
	Epoch   time.Time
}

type runBudgetTokensFunc func(context.Context, *db.Queries, db.Issue) (RunBudgetTokens, bool, error)

var runBudgetTokensSource atomic.Pointer[runBudgetTokensFunc]

// SetRunBudgetTokens overrides where the token side is read (nil restores the
// workflow's stored A and E with the issue's activation). Tests use it to pin
// E and the epoch without a stored workflow value.
func SetRunBudgetTokens(fn func(context.Context, *db.Queries, db.Issue) (RunBudgetTokens, bool, error)) {
	if fn == nil {
		runBudgetTokensSource.Store(nil)
		return
	}
	f := runBudgetTokensFunc(fn)
	runBudgetTokensSource.Store(&f)
}

// runBudgetTokens reads the token side for an issue; ok is false while token
// accounting is off for it. Under the issue lock, the first call after the
// workflow stores A and E activates the issue.
func runBudgetTokens(ctx context.Context, q *db.Queries, issue db.Issue, usage *runBudgetUsage) (RunBudgetTokens, bool, error) {
	if fn := runBudgetTokensSource.Load(); fn != nil {
		return (*fn)(ctx, q, issue)
	}
	return activateRunBudgetTokens(ctx, q, issue, usage)
}

// activationEpoch reads the issue's activation: its epoch, the start of the
// window open when it happened.
func activationEpoch(ctx context.Context, q *db.Queries, issueID pgtype.UUID) (time.Time, bool, error) {
	entry, err := q.GetIssueRunBudgetEntry(ctx, db.GetIssueRunBudgetEntryParams{IssueID: issueID, Kind: runBudgetKindActivation, EventKey: runBudgetKindActivation})
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, fmt.Errorf("run budget: activation: %w", err)
	}
	epoch, err := time.Parse(time.RFC3339Nano, entry.WindowID)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("run budget: activation epoch: %w", err)
	}
	return epoch, true, nil
}

// activateRunBudgetTokens returns the issue's token side, activating it first
// when the workflow stores A and E and no activation exists. The caller holds
// the issue lock, so two first events activate once. Activation writes, in this
// order, the window's allowance, the charge of every task from the epoch on
// that already reported usage, and the activation entry last: a failure before
// the entry leaves nothing activated, and a retry replays the same keys.
func activateRunBudgetTokens(ctx context.Context, q *db.Queries, issue db.Issue, usage *runBudgetUsage) (RunBudgetTokens, bool, error) {
	if usage == nil || !usage.tokensStored() {
		return RunBudgetTokens{}, false, nil
	}
	epoch, found, err := activationEpoch(ctx, q, issue.ID)
	if err != nil {
		return RunBudgetTokens{}, false, err
	}
	tokens := RunBudgetTokens{Reserve: usage.TokenReserve}
	if found {
		tokens.Epoch = epoch
		return tokens, true, nil
	}
	windowID := runBudgetWindowID(usage.Since)
	if err := recordRunBudgetTokens(ctx, q, issue, windowID, runBudgetKindAllowance, runBudgetKindAllowance+":"+windowID, usage.TokenAllowance); err != nil {
		return RunBudgetTokens{}, false, fmt.Errorf("run budget: activation allowance: %w", err)
	}
	ids, err := q.ListIssueTaskIDsWithUsageSince(ctx, db.ListIssueTaskIDsWithUsageSinceParams{IssueID: issue.ID, Since: pgtype.Timestamptz{Time: usage.Since, Valid: true}})
	if err != nil {
		return RunBudgetTokens{}, false, fmt.Errorf("run budget: activation tasks: %w", err)
	}
	for _, id := range ids {
		moved, err := settleTaskTokens(ctx, q, issue, id, windowID)
		if err != nil {
			return RunBudgetTokens{}, false, err
		}
		usage.ActivationMoved += moved
	}
	if err := recordRunBudgetTokens(ctx, q, issue, windowID, runBudgetKindActivation, runBudgetKindActivation, 0); err != nil {
		return RunBudgetTokens{}, false, fmt.Errorf("run budget: activation entry: %w", err)
	}
	tokens.Epoch = usage.Since
	return tokens, true, nil
}

// ClaimRunBudgetAllowance is the one way a window gets its token allowance
// besides activation: opening a window (a member status move, a top-up)
// credits +A only once the issue is activated, and at most once per window. The
// caller holds the issue lock. Before activation it writes nothing: the
// activation credits the window open then, so a move or top-up before it still
// yields one allowance.
func ClaimRunBudgetAllowance(ctx context.Context, q *db.Queries, issue db.Issue, windowID string) error {
	usage, err := loadRunBudgetUsage(ctx, q, issue)
	if err != nil || usage == nil {
		return err
	}
	return claimRunBudgetAllowance(ctx, q, issue, usage, windowID)
}

func claimRunBudgetAllowance(ctx context.Context, q *db.Queries, issue db.Issue, usage *runBudgetUsage, windowID string) error {
	if !usage.tokensStored() {
		return nil
	}
	epoch, found, err := activationEpoch(ctx, q, issue.ID)
	if err != nil || !found {
		return err
	}
	// A person's status move opens a window even when nothing is admitted in it
	// (a move that starts no run), so every move since the epoch gets its
	// allowance here, not only the latest window.
	moves, err := q.ListMemberStatusChangesAfter(ctx, db.ListMemberStatusChangesAfterParams{IssueID: issue.ID, After: pgtype.Timestamptz{Time: epoch, Valid: true}})
	if err != nil {
		return fmt.Errorf("run budget: member windows: %w", err)
	}
	ids := []string{windowID}
	for _, m := range moves {
		ids = append(ids, runBudgetWindowID(m.Time))
	}
	for _, id := range ids {
		if err := recordRunBudgetTokens(ctx, q, issue, id, runBudgetKindAllowance, runBudgetKindAllowance+":"+id, usage.TokenAllowance); err != nil {
			return fmt.Errorf("run budget: window allowance: %w", err)
		}
	}
	return nil
}

func recordRunBudgetTokens(ctx context.Context, q *db.Queries, issue db.Issue, windowID, kind, key string, delta int64) error {
	return q.InsertIssueRunBudgetTokenEntry(ctx, db.InsertIssueRunBudgetTokenEntryParams{
		ID: dbid.NewV7(), WorkspaceID: issue.WorkspaceID, IssueID: issue.ID,
		WindowID: windowID, Kind: kind, EventKey: key, TokensDelta: delta,
	})
}

// reserveRunTokens holds E tokens for the run just admitted, once token
// accounting is on for its issue. The guard refuses on the balance; the
// reservation can overshoot it.
func reserveRunTokens(ctx context.Context, q *db.Queries, issue db.Issue, usage *runBudgetUsage, windowID string, taskID pgtype.UUID) error {
	tokens, ok, err := runBudgetTokens(ctx, q, issue, usage)
	if err != nil || !ok || tokens.Reserve <= 0 {
		return err
	}
	return recordRunBudgetTokens(ctx, q, issue, windowID, runBudgetKindReserve, runBudgetKindReserve+":"+util.UUIDToString(taskID), -tokens.Reserve)
}

// SettleRunBudgetUsage charges a run's reported tokens to the ledger and
// releases its reservation. The caller runs it in the transaction that stored
// the report, so a report and its charge commit together. It recomputes from
// every usage row of the task, so a retried report writes nothing and a
// corrected one writes the difference. A task created before the token epoch is
// never settled; a terminal run that reports no usage keeps its reservation.
func SettleRunBudgetUsage(ctx context.Context, q *db.Queries, task db.AgentTaskQueue) error {
	if !task.IssueID.Valid {
		return nil
	}
	issue, usage, _, err := openRunBudget(ctx, q, task.IssueID, pgtype.UUID{})
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && usage == nil) {
		return nil
	}
	if err != nil {
		return err
	}
	tokens, ok, err := runBudgetTokens(ctx, q, issue, usage)
	if err != nil || !ok || task.CreatedAt.Time.Before(tokens.Epoch) {
		return err
	}
	moved, err := settleTaskTokens(ctx, q, issue, task.ID, runBudgetWindowID(usage.Since))
	if err != nil {
		return err
	}
	return warnTokensAtEighty(ctx, q, issue, usage, moved+usage.ActivationMoved)
}

// settleTaskTokens charges one task's reported tokens and releases its
// reservation, and returns the net change of the issue's token balance.
// windowID is the window to charge when the task has no run entry.
func settleTaskTokens(ctx context.Context, q *db.Queries, issue db.Issue, task pgtype.UUID, windowID string) (int64, error) {
	taskID := util.UUIDToString(task)
	run, err := q.GetIssueRunBudgetEntry(ctx, db.GetIssueRunBudgetEntryParams{IssueID: issue.ID, Kind: runBudgetKindRun, EventKey: runBudgetKindRun + ":" + taskID})
	if err == nil {
		windowID = run.WindowID
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("run budget: run entry: %w", err)
	}
	used, err := q.SumTaskWeightedTokens(ctx, task)
	if err != nil {
		return 0, fmt.Errorf("run budget: weighted tokens: %w", err)
	}
	if used.ModelRows == 0 {
		return 0, nil
	}
	held, err := q.SumIssueRunBudgetTaskTokens(ctx, db.SumIssueRunBudgetTaskTokensParams{IssueID: issue.ID, KeyPrefix: runBudgetKindTokens + ":" + taskID + ":"})
	if err != nil {
		return 0, fmt.Errorf("run budget: charged tokens: %w", err)
	}
	var moved int64
	if diff := -used.Weighted - held.Charged; diff != 0 {
		key := fmt.Sprintf("%s:%s:%d", runBudgetKindTokens, taskID, held.Entries+1)
		if err := recordRunBudgetTokens(ctx, q, issue, windowID, runBudgetKindTokens, key, diff); err != nil {
			return 0, fmt.Errorf("run budget: charge tokens: %w", err)
		}
		moved += diff
	}
	reserved, err := q.GetIssueRunBudgetEntry(ctx, db.GetIssueRunBudgetEntryParams{IssueID: issue.ID, Kind: runBudgetKindReserve, EventKey: runBudgetKindReserve + ":" + taskID})
	if errors.Is(err, pgx.ErrNoRows) {
		return moved, nil
	}
	if err != nil {
		return 0, fmt.Errorf("run budget: reservation: %w", err)
	}
	if _, err := q.GetIssueRunBudgetEntry(ctx, db.GetIssueRunBudgetEntryParams{IssueID: issue.ID, Kind: runBudgetKindRelease, EventKey: runBudgetKindRelease + ":" + taskID}); err == nil {
		return moved, nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("run budget: release: %w", err)
	}
	if err := recordRunBudgetTokens(ctx, q, issue, reserved.WindowID, runBudgetKindRelease, runBudgetKindRelease+":"+taskID, -reserved.TokensDelta); err != nil {
		return 0, err
	}
	return moved - reserved.TokensDelta, nil
}

// warnTokensAtEighty fires the 80% warning when a usage report takes the
// balance from above 20% of the window's allowance to at or below it. moved is
// the change the report made. The report is the only event that spends tokens
// (fact 7), so the warning needs no later enqueue; the notice claim keeps it
// once per window.
func warnTokensAtEighty(ctx context.Context, q *db.Queries, issue db.Issue, usage *runBudgetUsage, moved int64) error {
	hook := onRunBudgetEvent.Load()
	if hook == nil || moved >= 0 || !usage.tokensStored() {
		return nil
	}
	if _, found, err := activationEpoch(ctx, q, issue.ID); err != nil || !found {
		return err
	}
	after, err := q.SumIssueRunBudgetTokens(ctx, issue.ID)
	if err != nil {
		return fmt.Errorf("run budget: token balance: %w", err)
	}
	base := usage.TokenAllowance
	if before := after - moved; before*5 <= base || after*5 > base {
		return nil
	}
	xid, _ := q.CurrentXactID(ctx)
	go (*hook)(context.WithoutCancel(ctx), RunBudgetEvent{IssueID: issue.ID, Kind: RunBudgetKindWarn80Tokens, Since: usage.Since, Xid: xid, Tokens: after, TokenBase: base})
	return nil
}
