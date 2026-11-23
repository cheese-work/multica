package service

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// The next-turn bridge transaction is bounded on the server and on the client
// (CHE-9240, CHE-9177 step 4). Every test here calls the production helper
// setPREventBounds or the production queueNextTurnPREvents; none copies them.

// backendProbe is session B: a second connection that watches another backend.
type backendProbe struct {
	at       time.Duration // time since start of the first sample that saw the backend gone; -1 while alive
	maxAge   time.Duration // oldest transaction seen on the backend
	locks    int           // transactionid locks at the last sample
	lastSeen time.Duration // last sample that saw the backend alive
}

// watchBackend polls pg_stat_activity and pg_locks for pid every 250 ms until
// the backend is gone and the given offset passed, or until `until`.
func watchBackend(t *testing.T, pool *pgx.Conn, pid int32, start time.Time, until time.Duration, stopWhenGone bool) backendProbe {
	t.Helper()
	p := backendProbe{at: -1}
	for {
		elapsed := time.Since(start)
		var ageSeconds *float64
		err := pool.QueryRow(context.Background(),
			`SELECT EXTRACT(EPOCH FROM clock_timestamp() - xact_start)::float8 FROM pg_stat_activity WHERE pid=$1`, pid).Scan(&ageSeconds)
		alive := err == nil
		if err != nil && err != pgx.ErrNoRows {
			t.Fatalf("probe pg_stat_activity: %v", err)
		}
		if alive {
			p.lastSeen = elapsed
			if ageSeconds != nil {
				if age := time.Duration(*ageSeconds * float64(time.Second)); age > p.maxAge {
					p.maxAge = age
				}
			}
		}
		if err := pool.QueryRow(context.Background(),
			`SELECT count(*) FROM pg_locks WHERE pid=$1 AND locktype='transactionid' AND granted`, pid).Scan(&p.locks); err != nil {
			t.Fatalf("probe pg_locks: %v", err)
		}
		if !alive && p.at < 0 {
			p.at = elapsed
			if stopWhenGone {
				return p
			}
		}
		if elapsed >= until {
			return p
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// stuckBridge holds the bridge transaction open for 12 s at the given stage,
// holding a real row lock, and watches its backend from a second connection.
func stuckBridge(t *testing.T, stage string) {
	t.Helper()
	pool := ownConn(t, sharedTestPool(t))
	e := newPREventEnv(t)
	e.endRun(t, e.run)
	pids := make(chan int32, 1)
	e.s.prEventSeam = func(ctx context.Context, tx pgx.Tx, at string, _ []db.ListNextTurnPREventsRow) error {
		if at != stage {
			return nil
		}
		var pid int32
		if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT 1 FROM issue_pr_event WHERE id=$1 FOR UPDATE`, e.event); err != nil {
			return err
		}
		pids <- pid
		_, err := tx.Exec(ctx, `SELECT pg_sleep(12)`)
		return err
	}
	start := time.Now()
	done := make(chan error, 1)
	go func() {
		done <- e.s.queueNextTurnPREvents(context.Background(), []pgtype.UUID{parseTestUUID(t, e.f.WorkspaceID)})
	}()
	var pid int32
	select {
	case pid = <-pids:
	case err := <-done:
		t.Fatalf("the bridge ended before the seam ran: %v", err)
	case <-time.After(20 * time.Second):
		t.Fatal("the seam never ran")
	}
	p := watchBackend(t, pool, pid, start, 13*time.Second, false)
	if p.maxAge > 11*time.Second {
		t.Errorf("a transaction was open for %s, want at most 11s", p.maxAge)
	}
	if p.locks != 0 {
		t.Errorf("%d lock(s) still held after 12s", p.locks)
	}
	<-done
	if n := e.f.Count(t, `SELECT count(*) FROM issue_wakeup_receipt WHERE payload->>'event_id'=$1`, e.event); n != 0 {
		t.Errorf("an expired transaction left %d receipt(s)", n)
	}
}

// (i) A read that hangs frees its transaction and locks.
func TestPREventBridgeSlowReadReleasesLocks(t *testing.T) { stuckBridge(t, prEventStageRead) }

// (ii) A bridge that hangs frees its transaction and locks.
func TestPREventBridgeSlowBridgeReleasesLocks(t *testing.T) { stuckBridge(t, prEventStageBridge) }

// ownConn dials a connection of the test database that no pool owns, so no
// client cleanup can run on it.
func ownConn(t *testing.T, pool *pgxpool.Pool) *pgx.Conn {
	t.Helper()
	conn, err := pgx.ConnectConfig(context.Background(), pool.Config().ConnConfig.Copy())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		conn.Close(ctx)
	})
	return conn
}

// serverDeadline is the isolated test of the server bound: session A applies
// the production bounds, disables every other timer, takes a row lock, then
// sends nothing (idle) or runs pg_sleep(30) (busy). Nothing in A rolls back or
// cancels.
func serverDeadline(t *testing.T, busy, withoutTransactionTimeout bool) (p backendProbe, nowaitOK bool, nowaitAt time.Duration) {
	t.Helper()
	shared := sharedTestPool(t)
	e := newPREventEnv(t)
	conn := ownConn(t, shared)
	pool := ownConn(t, shared) // session B
	ctx := context.Background()
	start := time.Now()
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	limits := prEventLimits{Transaction: 10 * time.Second, Statement: 8 * time.Second, withoutTransactionTimeout: withoutTransactionTimeout}
	if err := setPREventBounds(ctx, tx, limits.orDefault()); err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{`SET LOCAL statement_timeout = 0`, `SET LOCAL idle_in_transaction_session_timeout = 0`} {
		if _, err := tx.Exec(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tx.Exec(ctx, `SELECT id FROM issue_pr_event WHERE id=$1 FOR UPDATE`, e.event); err != nil {
		t.Fatal(err)
	}
	pid := int32(conn.PgConn().PID())
	if busy {
		go func() { _, _ = tx.Exec(ctx, `SELECT pg_sleep(30)`) }()
	}
	until := 13 * time.Second
	p = watchBackend(t, pool, pid, start, until, !withoutTransactionTimeout)
	if p.at >= 0 {
		btx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		_, lockErr := btx.Exec(ctx, `SELECT id FROM issue_pr_event WHERE id=$1 FOR UPDATE NOWAIT`, e.event)
		nowaitOK, nowaitAt = lockErr == nil, time.Since(start)
		_ = btx.Rollback(ctx)
	}
	if withoutTransactionTimeout {
		// The control leaves its session running on purpose; end it now rather
		// than wait for pg_sleep(30) or the connection close.
		_, _ = pool.Exec(ctx, `SELECT pg_terminate_backend($1)`, pid)
	}
	return p, nowaitOK, nowaitAt
}

// (iv) and (iv-b): with the production helper alone, the server ends an idle
// or busy session between 10 s and 12 s and frees its row lock. Negative
// control (v): without the transaction_timeout line the lock is still held at
// 13 s; if it is not, (iv) proves nothing.
func TestPREventBridgeServerDeadline(t *testing.T) {
	for _, mode := range []struct {
		name string
		busy bool
	}{{"idle", false}, {"busy", true}} {
		t.Run(mode.name, func(t *testing.T) {
			t.Parallel()
			p, nowaitOK, nowaitAt := serverDeadline(t, mode.busy, false)
			if p.at < 10*time.Second || p.at > 12*time.Second {
				t.Errorf("the backend was gone at %s (last seen %s), want between 10s and 12s", p.at, p.lastSeen)
			}
			if p.locks != 0 {
				t.Errorf("%d lock(s) left after the backend was gone", p.locks)
			}
			if !nowaitOK || nowaitAt >= 15*time.Second {
				t.Errorf("FOR UPDATE NOWAIT succeeded=%v at %s, want success before 15s", nowaitOK, nowaitAt)
			}
		})
		t.Run(mode.name+" negative control", func(t *testing.T) {
			t.Parallel()
			p, _, _ := serverDeadline(t, mode.busy, true)
			if p.at >= 0 || p.locks == 0 {
				t.Fatalf("without transaction_timeout the backend was gone at %s with %d lock(s) at 13s: the control sees no held lock, so the deadline test proves nothing", p.at, p.locks)
			}
		})
	}
}

// pastEvents inserts n next-turn events of the env's PR, oldest first.
func pastEvents(t *testing.T, e prEventEnv, n int) []string {
	t.Helper()
	var prID string
	e.f.QueryRow(t, `SELECT pull_request_id FROM issue_pr_event WHERE id=$1`, e.event).Scan(&prID)
	e.f.Exec(t, `UPDATE issue_pr_event SET status='superseded' WHERE id=$1`, e.event)
	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		var id string
		e.f.QueryRow(t, `INSERT INTO issue_pr_event (workspace_id, issue_id, pull_request_id, kind, head_sha, base_sha, observed_at, dedupe_key, status)
			VALUES ($1, $2, $3, 'conflict', $4, 'base-1', now() - interval '2 days' + make_interval(secs => $5), $6, 'next_turn') RETURNING id`,
			e.f.WorkspaceID, e.issue, prID, fmt.Sprintf("head-%02d", i), float64(i), fmt.Sprintf("conflict|%02d", i)).Scan(&id)
		ids = append(ids, id)
	}
	return ids
}

func receiptsOf(t *testing.T, e prEventEnv, event string, inFlightOnly bool) int {
	t.Helper()
	q := `SELECT count(*) FROM issue_wakeup_receipt WHERE payload->>'event_id'=$1`
	if inFlightOnly {
		q += ` AND processed_at IS NULL`
	}
	return e.f.Count(t, q, event)
}

type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *logBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// (iii) The first 10 events time out on every attempt. Later events still get
// receipts within 3 ticks, the skipped ones are retried on the next walk, and
// no event ever holds two in-flight receipts. The same run checks the timing log.
func TestPREventBridgeSkipsAnExpiringBatchAndMovesOn(t *testing.T) {
	logs := &logBuffer{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })

	e := newPREventEnv(t)
	e.endRun(t, e.run)
	ids := pastEvents(t, e, 15)
	slow := map[string]bool{}
	for _, id := range ids[:10] {
		slow[id] = true
	}
	var stuck = true
	e.s.prEventLimits = prEventLimits{Transaction: 4 * time.Second, Statement: 3 * time.Second, Client: time.Second, Rollback: 2 * time.Second}
	e.s.prEventSeam = func(ctx context.Context, tx pgx.Tx, stage string, events []db.ListNextTurnPREventsRow) error {
		if stage != prEventStageBridge || !stuck {
			return nil
		}
		for _, ev := range events {
			if slow[util.UUIDToString(ev.ID)] {
				_, err := tx.Exec(ctx, `SELECT pg_sleep(3)`)
				return err
			}
		}
		return nil
	}
	workspaces := []pgtype.UUID{parseTestUUID(t, e.f.WorkspaceID)}
	tick := func(n int, wantErr bool) {
		t.Helper()
		if err := e.s.queueNextTurnPREvents(context.Background(), workspaces); (err != nil) != wantErr {
			t.Fatalf("tick %d: err = %v, want error %v", n, err, wantErr)
		}
	}
	tick(1, true) // batch 100 holds slow events: expires
	tick(2, true) // batch 10 is all slow events: expires, the cursor skips them
	tick(3, false)
	for i, id := range ids {
		want := 0
		if i >= 10 {
			want = 1
		}
		if got := receiptsOf(t, e, id, false); got != want {
			t.Errorf("after 3 ticks event %d has %d receipt(s), want %d", i, got, want)
		}
	}
	stuck = false
	tick(4, false)
	for i, id := range ids {
		if got := receiptsOf(t, e, id, true); got != 1 {
			t.Errorf("after the walk restarted event %d has %d in-flight receipt(s), want exactly 1", i, got)
		}
	}

	out := logs.String()
	for _, want := range []string{"level=DEBUG", "read_ms=", "tx_ms=", "level=WARN", "expired", "batch=10", "skipped_ids="} {
		if !strings.Contains(out, want) {
			t.Errorf("the timing log lacks %q:\n%s", want, out)
		}
	}
}

// rollbackSpy records the context of every Rollback call.
type rollbackStarter struct {
	inner TxStarter
	mu    sync.Mutex
	seen  []rollbackCall
}

type rollbackCall struct {
	err      error
	deadline time.Duration // time left; zero when the context has no deadline
}

type rollbackTx struct {
	pgx.Tx
	starter *rollbackStarter
}

func (s *rollbackStarter) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := s.inner.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return &rollbackTx{Tx: tx, starter: s}, nil
}

func (tx *rollbackTx) Rollback(ctx context.Context) error {
	call := rollbackCall{err: ctx.Err()}
	if d, ok := ctx.Deadline(); ok {
		call.deadline = time.Until(d)
	}
	tx.starter.mu.Lock()
	tx.starter.seen = append(tx.starter.seen, call)
	tx.starter.mu.Unlock()
	return tx.Tx.Rollback(ctx)
}

// The deferred rollback runs on a fresh 2 s context, not the caller's: a
// caller context that is already cancelled must not decide whether the rollback runs.
func TestPREventBridgeRollsBackOnAFreshContext(t *testing.T) {
	e := newPREventEnv(t)
	e.endRun(t, e.run)
	spy := &rollbackStarter{inner: e.s.Tasks.TxStarter}
	e.s.Tasks.TxStarter = spy
	ctx, cancel := context.WithCancel(context.Background())
	e.s.prEventSeam = func(context.Context, pgx.Tx, string, []db.ListNextTurnPREventsRow) error {
		cancel()
		return context.Canceled
	}
	if err := e.s.queueNextTurnPREvents(ctx, []pgtype.UUID{parseTestUUID(t, e.f.WorkspaceID)}); err == nil {
		t.Fatal("a cancelled bridge returned no error")
	}
	if len(spy.seen) != 1 {
		t.Fatalf("Rollback ran %d times, want 1", len(spy.seen))
	}
	if got := spy.seen[0]; got.err != nil || got.deadline <= 0 || got.deadline > prEventRollbackTimeout {
		t.Fatalf("rollback context: err=%v deadline=%s, want a live context with at most %s left", got.err, got.deadline, prEventRollbackTimeout)
	}
}
