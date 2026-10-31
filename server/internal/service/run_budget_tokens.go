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
const (
	runBudgetKindReserve = "reserve"
	runBudgetKindRelease = "release"
	runBudgetKindTokens  = "tokens"
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

// SetRunBudgetTokens registers where the token side is read (nil clears it).
// L2e registers the workflow's stored A and E and does the issue's activation
// inside it, under the issue lock. Until one is registered token accounting is
// off and no token entry is written.
func SetRunBudgetTokens(fn func(context.Context, *db.Queries, db.Issue) (RunBudgetTokens, bool, error)) {
	if fn == nil {
		runBudgetTokensSource.Store(nil)
		return
	}
	f := runBudgetTokensFunc(fn)
	runBudgetTokensSource.Store(&f)
}

// runBudgetTokens reads the token side for an issue; ok is false while token
// accounting is off for it.
func runBudgetTokens(ctx context.Context, q *db.Queries, issue db.Issue) (RunBudgetTokens, bool, error) {
	fn := runBudgetTokensSource.Load()
	if fn == nil {
		return RunBudgetTokens{}, false, nil
	}
	return (*fn)(ctx, q, issue)
}

func recordRunBudgetTokens(ctx context.Context, q *db.Queries, issue db.Issue, windowID, kind, key string, delta int64) error {
	return q.InsertIssueRunBudgetTokenEntry(ctx, db.InsertIssueRunBudgetTokenEntryParams{
		ID: dbid.NewV7(), WorkspaceID: issue.WorkspaceID, IssueID: issue.ID,
		WindowID: windowID, Kind: kind, EventKey: key, TokensDelta: delta,
	})
}

// reserveRunTokens holds E tokens for the run just admitted, once token
// accounting is on for its issue. Admission is not refused on tokens here:
// that is L2e's activation.
func reserveRunTokens(ctx context.Context, q *db.Queries, issue db.Issue, windowID string, taskID pgtype.UUID) error {
	tokens, ok, err := runBudgetTokens(ctx, q, issue)
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
	tokens, ok, err := runBudgetTokens(ctx, q, issue)
	if err != nil || !ok || task.CreatedAt.Time.Before(tokens.Epoch) {
		return err
	}
	taskID := util.UUIDToString(task.ID)
	windowID := runBudgetWindowID(usage.Since)
	run, err := q.GetIssueRunBudgetEntry(ctx, db.GetIssueRunBudgetEntryParams{IssueID: issue.ID, Kind: runBudgetKindRun, EventKey: runBudgetKindRun + ":" + taskID})
	if err == nil {
		windowID = run.WindowID
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("run budget: run entry: %w", err)
	}
	used, err := q.SumTaskWeightedTokens(ctx, task.ID)
	if err != nil {
		return fmt.Errorf("run budget: weighted tokens: %w", err)
	}
	if used.ModelRows == 0 {
		return nil
	}
	held, err := q.SumIssueRunBudgetTaskTokens(ctx, db.SumIssueRunBudgetTaskTokensParams{IssueID: issue.ID, KeyPrefix: runBudgetKindTokens + ":" + taskID + ":"})
	if err != nil {
		return fmt.Errorf("run budget: charged tokens: %w", err)
	}
	if diff := -used.Weighted - held.Charged; diff != 0 {
		key := fmt.Sprintf("%s:%s:%d", runBudgetKindTokens, taskID, held.Entries+1)
		if err := recordRunBudgetTokens(ctx, q, issue, windowID, runBudgetKindTokens, key, diff); err != nil {
			return fmt.Errorf("run budget: charge tokens: %w", err)
		}
	}
	reserved, err := q.GetIssueRunBudgetEntry(ctx, db.GetIssueRunBudgetEntryParams{IssueID: issue.ID, Kind: runBudgetKindReserve, EventKey: runBudgetKindReserve + ":" + taskID})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("run budget: reservation: %w", err)
	}
	return recordRunBudgetTokens(ctx, q, issue, reserved.WindowID, runBudgetKindRelease, runBudgetKindRelease+":"+taskID, -reserved.TokensDelta)
}
