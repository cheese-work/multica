package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Time bounds of the next-turn PR-event bridge transaction (CHE-9240, CHE-9177
// step 4). The transaction takes row locks on events, issues and wakeups, so a
// read or bridge that hangs must not hold them. Two independent bounds:
//
//   - server: transaction_timeout ends the transaction and frees its locks even
//     if this process hangs or loses the connection mid-statement;
//   - client: a context around read and bridge, and a rollback on a fresh
//     context, so a cancelled caller context does not skip the cleanup.
const (
	prEventTransactionTimeout = 10 * time.Second
	prEventStatementTimeout   = 8 * time.Second
	prEventClientTimeout      = 10 * time.Second
	prEventRollbackTimeout    = 2 * time.Second
	// prEventShrunkBatch is the batch size after an expired attempt, until a commit.
	prEventShrunkBatch = 10
	// prEventSlowTransaction is the transaction length that logs a warning.
	prEventSlowTransaction = time.Second
)

// prEventLimits are the bridge's time bounds. The zero value means production.
type prEventLimits struct {
	Transaction time.Duration // server transaction_timeout
	Statement   time.Duration // server statement_timeout
	Client      time.Duration // context around read and bridge
	Rollback    time.Duration // fresh context of the deferred rollback
	// withoutTransactionTimeout leaves transaction_timeout unset: the negative
	// control of the server-deadline test. Test only.
	withoutTransactionTimeout bool
}

func (l prEventLimits) orDefault() prEventLimits {
	if l.Transaction <= 0 {
		l.Transaction = prEventTransactionTimeout
	}
	if l.Statement <= 0 {
		l.Statement = prEventStatementTimeout
	}
	if l.Client <= 0 {
		l.Client = prEventClientTimeout
	}
	if l.Rollback <= 0 {
		l.Rollback = prEventRollbackTimeout
	}
	return l
}

// prEventExecer is what the bound needs of a transaction; pgx.Tx satisfies it.
type prEventExecer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// setPREventBounds issues the server-side bounds. It must be the first call in
// the transaction: transaction_timeout, then statement_timeout. SET LOCAL takes
// no bind parameters; the values are integer milliseconds built here.
func setPREventBounds(ctx context.Context, tx prEventExecer, l prEventLimits) error {
	if !l.withoutTransactionTimeout {
		if _, err := tx.Exec(ctx, fmt.Sprintf("SET LOCAL transaction_timeout = '%dms'", l.Transaction.Milliseconds())); err != nil {
			return fmt.Errorf("set transaction_timeout: %w", err)
		}
	}
	if _, err := tx.Exec(ctx, fmt.Sprintf("SET LOCAL statement_timeout = '%dms'", l.Statement.Milliseconds())); err != nil {
		return fmt.Errorf("set statement_timeout: %w", err)
	}
	return nil
}

// prEventExpired reports whether err is a bound firing: the client context
// ran out while the caller's did not, or the server cancelled the statement
// (57014) or ended the transaction (25P04).
func prEventExpired(parent, bounded context.Context, err error) bool {
	if err == nil {
		return false
	}
	if parent.Err() == nil && errors.Is(bounded.Err(), context.DeadlineExceeded) {
		return true
	}
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && (pgErr.Code == "57014" || pgErr.Code == "25P04")
}

// Seam stages of the bridge transaction, for tests that hold it open.
const (
	prEventStageRead   = "read"
	prEventStageBridge = "bridge"
)

// prEventSeam is called inside the transaction before the read (events nil) and
// before the bridge (events set). Nil in production.
type prEventSeam func(ctx context.Context, tx pgx.Tx, stage string, events []db.ListNextTurnPREventsRow) error

func prEventIDs(events []db.ListNextTurnPREventsRow) []pgtype.UUID {
	ids := make([]pgtype.UUID, len(events))
	for i, e := range events {
		ids[i] = e.ID
	}
	return ids
}
