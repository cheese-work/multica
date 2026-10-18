package handler

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
)

// raceUntilBlocked fires its hook when the write is blocked on the competing
// transaction. A lock waiter that belongs to someone else, held up by a third
// transaction, must not count: the hook then runs before the write has reached
// its lock, and the interleaving under test never happens.
func TestRaceHelperIgnoresAnUnrelatedLockWaiter(t *testing.T) {
	seedTestCatalog(t)
	ctx := context.Background()
	rowA := dbfx.Issue(t, "race helper unrelated row", testutil.Cols{"status": "todo"})
	rowB := dbfx.Issue(t, "race helper held row", testutil.Cols{"status": "todo"})

	// An unrelated waiter: blocked by a transaction the helper knows nothing of.
	other, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { other.Rollback(ctx) })
	if _, err := other.Exec(ctx, `UPDATE issue SET title = title WHERE id = $1`, util.MustParseUUID(rowA)); err != nil {
		t.Fatal(err)
	}
	waiterDone := make(chan struct{})
	go func() {
		defer close(waiterDone)
		testPool.Exec(ctx, `UPDATE issue SET title = title WHERE id = $1`, util.MustParseUUID(rowA))
	}()
	for deadline := time.Now().Add(5 * time.Second); ; {
		var waiting bool
		dbfx.QueryRow(t, `SELECT count(*) > 0 FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&waiting)
		if waiting {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the unrelated waiter never blocked")
		}
		time.Sleep(10 * time.Millisecond)
	}

	var attempting atomic.Bool
	hookSawWriterBlocked := false
	raceUntilBlocked(t, func(tx pgx.Tx) {
		if _, err := tx.Exec(ctx, `UPDATE issue SET title = title WHERE id = $1`, util.MustParseUUID(rowB)); err != nil {
			t.Fatal(err)
		}
	}, func() {
		// The write reaches its lock only after a pause; until then no backend
		// waits on the competing transaction.
		time.Sleep(600 * time.Millisecond)
		attempting.Store(true)
		testPool.Exec(ctx, `UPDATE issue SET title = title WHERE id = $1`, util.MustParseUUID(rowB))
	}, func(tx pgx.Tx) {
		hookSawWriterBlocked = attempting.Load()
	})

	if !hookSawWriterBlocked {
		t.Fatal("the hook fired before the write reached its lock: an unrelated waiter was taken for it")
	}
	other.Rollback(ctx)
	<-waiterDone
}
