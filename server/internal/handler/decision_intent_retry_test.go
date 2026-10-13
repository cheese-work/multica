package handler

import (
	"context"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/multica-ai/multica/server/internal/decisiontx"
)

type intentRetryStarter struct {
	base     txStarter
	attempts int
	onBusy   func()
}

func (starter *intentRetryStarter) Begin(ctx context.Context) (pgx.Tx, error) {
	starter.attempts++
	transaction, err := starter.base.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return intentRetryTransaction{Tx: transaction, onBusy: starter.onBusy}, nil
}

type intentRetryTransaction struct {
	pgx.Tx
	onBusy func()
}

func (transaction intentRetryTransaction) QueryRow(ctx context.Context, query string, args ...any) pgx.Row {
	return intentRetryRow{Row: transaction.Tx.QueryRow(ctx, query, args...), onBusy: transaction.onBusy}
}

type intentRetryRow struct {
	pgx.Row
	onBusy func()
}

func (row intentRetryRow) Scan(values ...any) error {
	err := row.Row.Scan(values...)
	if decisiontx.IsRetryable(err) && row.onBusy != nil {
		row.onBusy()
	}
	return err
}

func TestDecisionIntentRetriesTaskLock(test *testing.T) {
	fixture := newIntentFixture(test)
	ctx := context.Background()
	holder, err := testPool.Begin(ctx)
	if err != nil {
		test.Fatal(err)
	}
	defer holder.Rollback(ctx)
	if _, err := holder.Exec(ctx, `SELECT id FROM agent_task_queue WHERE id = $1 FOR UPDATE`, fixture.taskID); err != nil {
		test.Fatal(err)
	}
	originalStarter := testHandler.TxStarter
	starter := &intentRetryStarter{base: originalStarter, onBusy: func() {
		if err := holder.Commit(ctx); err != nil {
			test.Fatal(err)
		}
	}}
	testHandler.TxStarter = starter
	test.Cleanup(func() { testHandler.TxStarter = originalStarter })
	response := fixture.post(test, "task_token", fixture.taskID, fixture.mention(), nil)
	response.Want(http.StatusCreated)
	if starter.attempts != 2 {
		test.Fatalf("transaction attempts = %d, want 2", starter.attempts)
	}
}
