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
//
// The window id is the window's start time (RunBudgetStart), the same value
// the L1a notices use as their claim key. A new window starts from its own
// credit: runs left or overspent in the old one do not carry over.
const (
	runBudgetKindWindow = "window"
	runBudgetKindRun    = "run"
	runBudgetKindRaise  = "raise"
)

func runBudgetWindowID(since time.Time) string {
	return since.UTC().Format(time.RFC3339Nano)
}

func recordRunBudgetEntry(ctx context.Context, q *db.Queries, issue db.Issue, windowID, kind, key string, delta int) error {
	return q.InsertIssueRunBudgetEntry(ctx, db.InsertIssueRunBudgetEntryParams{
		ID: dbid.NewV7(), WorkspaceID: issue.WorkspaceID, IssueID: issue.ID,
		WindowID: windowID, Kind: kind, EventKey: key, RunsDelta: int32(delta),
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
	if _, err := q.LockIssueRunBudget(ctx, issueID); err != nil {
		return db.Issue{}, nil, 0, err
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
	if err := recordRunBudgetEntry(ctx, q, issue, windowID, runBudgetKindWindow, runBudgetKindWindow+":"+windowID, usage.Max-int(counted)); err != nil {
		return issue, nil, 0, fmt.Errorf("run budget: open window: %w", err)
	}
	balance, err := q.SumIssueRunBudgetWindow(ctx, db.SumIssueRunBudgetWindowParams{IssueID: issue.ID, WindowID: windowID})
	if err != nil {
		return issue, nil, 0, fmt.Errorf("run budget: balance: %w", err)
	}
	return issue, usage, balance, nil
}

// OpenRunBudgetWindow is openRunBudget for a caller outside the service that
// changes the cap: it must open the window before it changes the cap, or the
// lazy credit would count the raise twice.
func OpenRunBudgetWindow(ctx context.Context, q *db.Queries, issueID pgtype.UUID) error {
	_, _, _, err := openRunBudget(ctx, q, issueID, pgtype.UUID{})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	return err
}

// RecordRunBudgetRaise credits the runs a person added to the issue's cap.
// eventKey is the audit row's id, so a replay is a no-op.
func RecordRunBudgetRaise(ctx context.Context, q *db.Queries, issue db.Issue, eventKey pgtype.UUID, delta int) error {
	_, usage, _, err := openRunBudget(ctx, q, issue.ID, pgtype.UUID{})
	if err != nil || usage == nil {
		return err
	}
	return recordRunBudgetEntry(ctx, q, issue, runBudgetWindowID(usage.Since), runBudgetKindRaise, runBudgetKindRaise+":"+util.UUIDToString(eventKey), delta)
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
	return recordRunBudgetEntry(ctx, q, issue, runBudgetWindowID(usage.Since), runBudgetKindRun, runBudgetKindRun+":"+util.UUIDToString(taskID), -1)
}
