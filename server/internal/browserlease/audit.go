package browserlease

import (
	"context"

	"github.com/jackc/pgx/v5"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Audit kinds written for lease events (CHE-4744, W3-15). The handler package registers them.
const (
	AuditStart  = "browser.lease.start"  // a queued lease was granted an instance
	AuditEnd    = "browser.lease.end"    // an active lease ended by itself: release, expiry, failed revalidation, sign failure
	AuditRevoke = "browser.lease.revoke" // a lease was ended from outside: task end, kill switch, plugin off
)

// AuditFunc writes one audit row for a lease event. q is bound to the transaction that makes
// the transition, so an audit failure rolls the transition back and no event goes unrecorded.
// The actor comes from ctx. A nil AuditFunc records nothing (tests of the queue alone).
type AuditFunc func(ctx context.Context, q *db.Queries, kind string, l db.BrowserLease) error

// TxBeginner starts a transaction. *pgxpool.Pool satisfies it.
type TxBeginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}
