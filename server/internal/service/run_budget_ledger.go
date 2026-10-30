package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

// The run-budget ledger (CHE-2297 L2a). The guard no longer counts task rows:
// it reads the balance of the issue's open window, the sum of its entries.
//
//	window:<window id>   the credit that opens a window: the cap in force minus
//	                     the runs already counted in it. Written lazily by the
//	                     first admission or debit, so an issue at 38 of 40 on
//	                     the day of deploy starts with 2.
//	run:<task id>        -1 for each run inserted.
//	raise:<activity id>  the runs a person added with a raise.
//	cap:<uuid>           the difference when the workflow's cap was edited, so
//	                     the open window follows the cap in force.
//
// The window id is the window's start time (RunBudgetStart), the same value
// the L1a notices use as their claim key. A new window starts from its own
// credit: runs left or overspent in the old one do not carry over.
const (
	runBudgetKindWindow = "window"
	runBudgetKindRun    = "run"
	runBudgetKindRaise  = "raise"
	runBudgetKindCap    = "cap"
)

type readOnlyRunBudgetKey struct{}

// readOnlyRunBudget marks a guard that runs on the pool, before the admission
// transaction. It reads the budget without the issue lock and writes no ledger
// entry: a write outside the lock can land between a locked admission's cap
// read and its balance read. The locked guard in the transaction decides.
func readOnlyRunBudget(ctx context.Context) context.Context {
	return context.WithValue(ctx, readOnlyRunBudgetKey{}, true)
}

func runBudgetWindowID(since time.Time) string {
	return since.UTC().Format(time.RFC3339Nano)
}

func recordRunBudgetEntry(ctx context.Context, q *db.Queries, issue db.Issue, windowID, kind, key string, delta, capDelta int) error {
	return q.InsertIssueRunBudgetEntry(ctx, db.InsertIssueRunBudgetEntryParams{
		ID: dbid.NewV7(), WorkspaceID: issue.WorkspaceID, IssueID: issue.ID,
		WindowID: windowID, Kind: kind, EventKey: key, RunsDelta: int32(delta), CapDelta: int32(capDelta),
	})
}

// openRunBudget locks the issue row until the caller's transaction ends, opens
// the current window's ledger and returns the issue, its usage and the window's
// balance. The lock is what makes the balance read and the debit one step:
// callers that enqueue outside a transaction get no atomicity from it.
// exceptTask is the run whose own debit is about to be written, so the credit
// does not count it (zero UUID: none). usage is nil for an issue that is not
// budgeted; a missing issue returns pgx.ErrNoRows.
func openRunBudget(ctx context.Context, q *db.Queries, issueID, exceptTask pgtype.UUID) (db.Issue, *runBudgetUsage, int64, error) {
	readOnly, _ := ctx.Value(readOnlyRunBudgetKey{}).(bool)
	if !readOnly {
		if _, err := q.LockIssueRunBudget(ctx, issueID); err != nil {
			return db.Issue{}, nil, 0, err
		}
	}
	issue, err := q.GetIssue(ctx, issueID)
	if err != nil {
		return db.Issue{}, nil, 0, err
	}
	usage, err := loadRunBudgetUsage(ctx, q, issue)
	if err != nil || usage == nil {
		return issue, nil, 0, err
	}
	windowID := runBudgetWindowID(usage.Since)
	counted, err := q.CountIssueRunsSinceExcept(ctx, db.CountIssueRunsSinceExceptParams{
		IssueID: issue.ID, Since: pgtype.Timestamptz{Time: usage.Since, Valid: true}, ExceptTaskID: exceptTask,
	})
	if err != nil {
		return issue, nil, 0, fmt.Errorf("run budget: count runs: %w", err)
	}
	if readOnly {
		balance, err := peekRunBudgetBalance(ctx, q, issue, usage, windowID, int(counted))
		return issue, usage, balance, err
	}
	if err := recordRunBudgetEntry(ctx, q, issue, windowID, runBudgetKindWindow, runBudgetKindWindow+":"+windowID, usage.Max-int(counted), usage.Max); err != nil {
		return issue, nil, 0, fmt.Errorf("run budget: open window: %w", err)
	}
	if err := claimRunBudgetAllowance(ctx, q, issue, usage, windowID); err != nil {
		return issue, nil, 0, err
	}
	// A cap edited since the ledger last saw it moves the balance by the same
	// amount; the lock makes the read-and-write one step.
	held, err := q.SumIssueRunBudgetCap(ctx, db.SumIssueRunBudgetCapParams{IssueID: issue.ID, WindowID: windowID})
	if err != nil {
		return issue, nil, 0, fmt.Errorf("run budget: held cap: %w", err)
	}
	if diff := usage.Max - int(held); diff != 0 {
		if err := recordRunBudgetEntry(ctx, q, issue, windowID, runBudgetKindCap, runBudgetKindCap+":"+util.UUIDToString(dbid.NewV7()), diff, diff); err != nil {
			return issue, nil, 0, fmt.Errorf("run budget: follow cap: %w", err)
		}
	}
	balance, err := q.SumIssueRunBudgetWindow(ctx, db.SumIssueRunBudgetWindowParams{IssueID: issue.ID, WindowID: windowID})
	if err != nil {
		return issue, nil, 0, fmt.Errorf("run budget: balance: %w", err)
	}
	return issue, usage, balance, nil
}

// peekRunBudgetBalance is the balance openRunBudget would return, computed
// without writing: the window's entries plus the cap difference a write would
// credit. An unopened window holds the cap minus the runs counted.
func peekRunBudgetBalance(ctx context.Context, q *db.Queries, issue db.Issue, usage *runBudgetUsage, windowID string, counted int) (int64, error) {
	opened, err := q.HasIssueRunBudgetWindow(ctx, db.HasIssueRunBudgetWindowParams{IssueID: issue.ID, WindowID: windowID})
	if err != nil {
		return 0, fmt.Errorf("run budget: window: %w", err)
	}
	if !opened {
		return int64(usage.Max - counted), nil
	}
	sum, err := q.SumIssueRunBudgetWindow(ctx, db.SumIssueRunBudgetWindowParams{IssueID: issue.ID, WindowID: windowID})
	if err != nil {
		return 0, fmt.Errorf("run budget: balance: %w", err)
	}
	held, err := q.SumIssueRunBudgetCap(ctx, db.SumIssueRunBudgetCapParams{IssueID: issue.ID, WindowID: windowID})
	if err != nil {
		return 0, fmt.Errorf("run budget: held cap: %w", err)
	}
	return sum + int64(usage.Max) - held, nil
}

// OpenRunBudgetWindow is openRunBudget for a caller outside the service that
// changes the cap: it locks the issue, brings the window to the cap in force
// and returns that cap (0: the issue has no budget). The caller raises from
// this cap, not from one it read before the lock.
func OpenRunBudgetWindow(ctx context.Context, q *db.Queries, issueID pgtype.UUID) (int, error) {
	_, usage, _, err := openRunBudget(ctx, q, issueID, pgtype.UUID{})
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && usage == nil) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return usage.Max, nil
}

// RecordRunBudgetRaise credits the runs a person added to the issue's cap. The
// caller opened the window first (OpenRunBudgetWindow), so the raise is the
// only difference between the ledger's cap and the issue's.
// eventKey is the audit row's id, so a replay is a no-op.
func RecordRunBudgetRaise(ctx context.Context, q *db.Queries, issue db.Issue, eventKey pgtype.UUID, delta int) error {
	usage, err := loadRunBudgetUsage(ctx, q, issue)
	if err != nil || usage == nil {
		return err
	}
	return recordRunBudgetEntry(ctx, q, issue, runBudgetWindowID(usage.Since), runBudgetKindRaise, runBudgetKindRaise+":"+util.UUIDToString(eventKey), delta, delta)
}

// debitRun charges the run just inserted to the open window.
func debitRun(ctx context.Context, q *db.Queries, issueID, taskID pgtype.UUID) error {
	issue, usage, _, err := openRunBudget(ctx, q, issueID, taskID)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && usage == nil) {
		return nil
	}
	if err != nil {
		return err
	}
	windowID := runBudgetWindowID(usage.Since)
	if err := recordRunBudgetEntry(ctx, q, issue, windowID, runBudgetKindRun, runBudgetKindRun+":"+util.UUIDToString(taskID), -1, 0); err != nil {
		return err
	}
	return reserveRunTokens(ctx, q, issue, usage, windowID, taskID)
}

// tokenBalanceSpent reports whether the issue's token balance refuses a new
// run: activated, and at or below zero. Under the issue lock the first
// admission attempt activates the issue; the read-only guard before the
// admission transaction only reads an activation that already exists.
func tokenBalanceSpent(ctx context.Context, q *db.Queries, issue db.Issue, usage *runBudgetUsage) (int64, bool, error) {
	if !usage.tokensStored() {
		return 0, false, nil
	}
	readOnly, _ := ctx.Value(readOnlyRunBudgetKey{}).(bool)
	if readOnly {
		if _, found, err := activationEpoch(ctx, q, issue.ID); err != nil || !found {
			return 0, false, err
		}
	} else if _, ok, err := activateRunBudgetTokens(ctx, q, issue, usage); err != nil || !ok {
		return 0, false, err
	}
	balance, err := q.SumIssueRunBudgetTokens(ctx, issue.ID)
	if err != nil {
		return 0, false, fmt.Errorf("token balance: %w", err)
	}
	if readOnly {
		// The admission transaction credits the window's allowance when it opens
		// the window; the guard before it counts that credit as already there.
		windowID := runBudgetWindowID(usage.Since)
		if _, err := q.GetIssueRunBudgetEntry(ctx, db.GetIssueRunBudgetEntryParams{IssueID: issue.ID, Kind: runBudgetKindAllowance, EventKey: runBudgetKindAllowance + ":" + windowID}); errors.Is(err, pgx.ErrNoRows) {
			balance += usage.TokenAllowance
		} else if err != nil {
			return 0, false, fmt.Errorf("token balance: %w", err)
		}
	}
	return balance, balance <= 0, nil
}
