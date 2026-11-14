// Package runinput holds the schema barrier of the run_input owner migration
// (CHE-5266 slice 2): a startup gate and a shared lease that keep a new server
// off a database without the migration, and keep the down migration off a
// database that new servers still use.
package runinput

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Lease key (classid, objid) of the shared advisory lock every new server
// holds. The down migration tries the exclusive form of the same key.
const (
	LeaseClass = 5266
	LeaseObj   = 1
)

// Querier is the part of a pool, connection or transaction the gate reads.
type Querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// schemaProblemsSQL reads the catalog only: it takes no table lock, so a
// migration in flight does not block it.
const schemaProblemsSQL = `
SELECT array_remove(ARRAY[
  CASE WHEN EXISTS (SELECT 1 FROM pg_attribute WHERE attrelid = to_regclass('run_input')
         AND attname = 'owner_task_id' AND NOT attisdropped)
       THEN NULL ELSE 'column run_input.owner_task_id' END,
  CASE WHEN EXISTS (SELECT 1 FROM pg_attribute WHERE attrelid = to_regclass('run_input')
         AND attname = 'thread_key' AND NOT attisdropped)
       THEN NULL ELSE 'column run_input.thread_key' END,
  CASE WHEN EXISTS (SELECT 1 FROM pg_constraint WHERE conrelid = to_regclass('run_input')
         AND conname = 'run_input_owned_has_owner' AND contype = 'c' AND convalidated)
       THEN NULL ELSE 'validated check run_input_owned_has_owner' END,
  CASE WHEN EXISTS (SELECT 1 FROM pg_trigger WHERE tgrelid = to_regclass('run_input')
         AND tgname = 'run_input_retire_held' AND NOT tgisinternal AND tgenabled = 'O')
       THEN NULL ELSE 'enabled trigger run_input_retire_held' END,
  CASE WHEN EXISTS (SELECT 1 FROM pg_constraint WHERE conrelid = to_regclass('run_input')
         AND conname = 'run_input_state_check' AND contype = 'c' AND convalidated
         AND pg_get_constraintdef(oid) LIKE '%''owned''%')
       THEN NULL ELSE 'check run_input_state_check with state owned' END
], NULL)::text[]`

// SchemaProblems lists what the catalog lacks. Empty means the migration is applied.
func SchemaProblems(ctx context.Context, q Querier) ([]string, error) {
	var problems []string
	if err := q.QueryRow(ctx, schemaProblemsSQL).Scan(&problems); err != nil {
		return nil, fmt.Errorf("read run_input schema: %w", err)
	}
	return problems, nil
}

// RequireSchema retries every interval until the schema is complete or window
// passes. A migration in flight is waited for; a read error is retried too.
func RequireSchema(ctx context.Context, q Querier, interval, window time.Duration) error {
	deadline := time.Now().Add(window)
	for {
		problems, err := SchemaProblems(ctx, q)
		if err == nil && len(problems) == 0 {
			return nil
		}
		if err == nil {
			err = fmt.Errorf("run_input migration not applied: missing %s", strings.Join(problems, "; "))
		}
		if time.Now().Add(interval).After(deadline) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
}

// Lease is the shared advisory lock a new server holds for its whole life, on a
// connection of its own.
type Lease struct {
	conn *pgx.Conn
	stop chan struct{}
	done chan struct{}
	once sync.Once
}

// HoldLease takes the lease and re-runs the schema gate once it is held: a down
// migration holds the exclusive form of the lock while it runs, so the request
// waits for it and the re-run then fails. A guard probes the connection every
// guardEvery and calls onLost once if a probe fails (fail closed).
func HoldLease(ctx context.Context, pool *pgxpool.Pool, guardEvery time.Duration, onLost func(error)) (*Lease, error) {
	pc, err := pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire lease connection: %w", err)
	}
	conn := pc.Hijack() // a session of its own: closing it drops the lock
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock_shared($1::int, $2::int)`, LeaseClass, LeaseObj); err != nil {
		_ = conn.Close(context.Background())
		return nil, fmt.Errorf("take run_input lease: %w", err)
	}
	problems, err := SchemaProblems(ctx, conn)
	if err == nil && len(problems) > 0 {
		err = fmt.Errorf("run_input migration was rolled back: missing %s", strings.Join(problems, "; "))
	}
	if err != nil {
		_ = conn.Close(context.Background())
		return nil, err
	}
	l := &Lease{conn: conn, stop: make(chan struct{}), done: make(chan struct{})}
	go l.guard(guardEvery, onLost)
	return l, nil
}

func (l *Lease) guard(every time.Duration, onLost func(error)) {
	defer close(l.done)
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-l.stop:
			return
		case <-t.C:
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_, err := l.conn.Exec(ctx, `SELECT 1`)
			cancel()
			if err != nil {
				select {
				case <-l.stop: // released meanwhile
				default:
					onLost(err)
				}
				return
			}
		}
	}
}

// Release drops the lease by closing its connection.
func (l *Lease) Release() {
	l.once.Do(func() {
		close(l.stop)
		<-l.done
		_ = l.conn.Close(context.Background())
	})
}
