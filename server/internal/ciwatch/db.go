package ciwatch

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/ghbudget"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// PgxTx runs the pipeline's writes in one pgx transaction.
func PgxTx(b interface {
	Begin(context.Context) (pgx.Tx, error)
}) InTx {
	return func(ctx context.Context, fn func(Writers) error) error {
		tx, err := b.Begin(ctx)
		if err != nil {
			return err
		}
		q := db.New(tx)
		if err := fn(Writers{Events: q, Observations: q}); err != nil {
			_ = tx.Rollback(ctx)
			return err
		}
		return tx.Commit(ctx)
	}
}

// ConfigReader is the per-repo routing config; found=false means not watched.
type ConfigReader interface {
	GetCIWatchConfigForRepo(ctx context.Context, arg db.GetCIWatchConfigForRepoParams) (db.GetCIWatchConfigForRepoRow, error)
}

func readConfig(ctx context.Context, r ConfigReader, ws pgtype.UUID, owner, repo string) (cfg Config, found bool, err error) {
	row, err := r.GetCIWatchConfigForRepo(ctx, db.GetCIWatchConfigForRepoParams{WorkspaceID: ws, Lower: owner, Lower_2: repo})
	if errors.Is(err, pgx.ErrNoRows) {
		return Config{}, false, nil
	}
	if err != nil {
		return Config{}, false, err
	}
	return Config{Mode: row.Mode, Branches: row.Branches, Events: row.Events,
		WorkflowsInclude: row.WorkflowsInclude, WorkflowsExclude: row.WorkflowsExclude}, true, nil
}

// DBRuns answers supersession from the github_workflow_run mirror.
type DBRuns struct {
	Q interface {
		CIWatchNewerRuns(ctx context.Context, arg db.CIWatchNewerRunsParams) (db.CIWatchNewerRunsRow, error)
	}
}

func (r DBRuns) Newer(ctx context.Context, run Run) (exists, succeeded bool, err error) {
	var ws pgtype.UUID
	if err := ws.Scan(run.WorkspaceID); err != nil {
		return false, false, err
	}
	row, err := r.Q.CIWatchNewerRuns(ctx, db.CIWatchNewerRunsParams{
		WorkspaceID: ws, Lower: run.RepoOwner, Lower_2: run.RepoName,
		WorkflowPath: run.WorkflowPath, HeadBranch: run.HeadBranch, RunID: run.RunID,
	})
	return row.NewerExists, row.NewerSucceeded, err
}

// LedgerBudget adapts the shared installation ledger to Budget.
type LedgerBudget struct{ L *ghbudget.Ledger }

func (b LedgerBudget) Reserve(ctx context.Context, inst int64, cost int) (func(), error) {
	r, err := b.L.Reserve(ctx, inst, cost)
	if err != nil {
		var d *ghbudget.DeferredError
		if errors.As(err, &d) && d.Reason == ghbudget.ReasonBackoff {
			return nil, ErrBackoff
		}
		return nil, ErrNoBudget
	}
	// The call is sent before release runs, so settle it as spent. A call that
	// was limited is recorded by Limited, which also sets the backoff.
	return func() { _ = r.Complete(context.WithoutCancel(ctx), nil) }, nil
}

func (b LedgerBudget) Limited(ctx context.Context, inst int64, e *RateLimitError) {
	h := http.Header{}
	h.Set("Retry-After", strconv.Itoa(int(e.RetryAfter.Seconds())))
	_, _ = b.L.RecordLimited(ctx, inst, h)
}
