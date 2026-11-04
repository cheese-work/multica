package pushnotify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const (
	// MaxAttempts counts claims, so a holder that died mid-send used one up.
	MaxAttempts   = 6
	SentRetention = time.Hour

	workerTick = 2 * time.Second
	// A batch is claimed in one go and its rows queue behind each other when they
	// share a device, so batchSize x sendTimeout (the worst case: every row on one
	// device, every send at its timeout) must stay under leaseDuration.
	batchSize     = 8
	parallelSends = 4
	leaseDuration = 3 * time.Minute
	sendTimeout   = 15 * time.Second
	// deliveryTimeout bounds one attempt from the pool to the commit: pool
	// acquisition, the device lock, the send and the outcome writes. It also
	// bounds how long a delivery holds the device lock, and so how long a
	// registration for that device can be made to wait.
	deliveryTimeout = 25 * time.Second
	// A delivery that cannot get the device within deviceLockWait is deferred
	// (attempt refunded) rather than queued, so contested devices never pin pool
	// connections behind another send.
	deviceLockWait = 2 * time.Second
	// TeardownBudget bounds every rollback and error-path write. It sits outside
	// the operation budgets: an attempt or request that ran out of time still
	// gets this long to roll back, and a rollback that cannot finish in it closes
	// the connection, which the pool replaces.
	TeardownBudget   = 2 * time.Second
	contestedBackoff = 5 * time.Second
	retryBase        = 30 * time.Second
	retryCap         = time.Hour
)

// Worker sends due outbox rows. Every pass re-checks that the push is still
// owed (GetPushDeliveryContext): iOS requires each delivered push to show a
// notification, so a record read in the meantime must never be sent.
type Worker struct {
	q      *db.Queries
	tx     TxStarter
	sender Sender
	done   chan struct{}
}

// TxStarter is the pool; *pgxpool.Pool satisfies it.
type TxStarter interface {
	Begin(context.Context) (pgx.Tx, error)
}

func NewWorker(q *db.Queries, tx TxStarter, sender Sender) *Worker {
	return &Worker{q: q, tx: tx, sender: sender, done: make(chan struct{})}
}

func (w *Worker) Run(ctx context.Context) {
	defer close(w.done)
	ticker := time.NewTicker(workerTick)
	defer ticker.Stop()
	for ctx.Err() == nil {
		n, err := w.process(ctx)
		if err != nil && ctx.Err() == nil {
			slog.Warn("push worker pass failed", "error", err)
		}
		if n == batchSize && err == nil {
			continue // a full batch: more is probably due
		}
		select {
		case <-ctx.Done():
		case <-ticker.C:
		}
	}
}

func (w *Worker) WaitWithTimeout(timeout time.Duration) bool {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-w.done:
		return true
	case <-timer.C:
		return false
	}
}

// ProcessOnce claims the due rows and delivers them.
func (w *Worker) ProcessOnce(ctx context.Context) error {
	_, err := w.process(ctx)
	return err
}

func (w *Worker) process(ctx context.Context) (int, error) {
	if err := w.q.DeleteSentPushDeliveries(ctx, SentRetention.Seconds()); err != nil {
		return 0, err
	}
	claims, err := w.q.ClaimPushDeliveries(ctx, db.ClaimPushDeliveriesParams{Batch: batchSize, LeaseSecs: leaseDuration.Seconds()})
	if err != nil {
		return 0, err
	}
	// Rows of one device go through one goroutine in claim order: they would
	// serialize on the device lock anyway, and this keeps them from timing out
	// on each other.
	var order []string
	byDevice := map[string][]db.ClaimPushDeliveriesRow{}
	for _, claim := range claims {
		key := util.UUIDToString(claim.SubscriptionID)
		if _, seen := byDevice[key]; !seen {
			order = append(order, key)
		}
		byDevice[key] = append(byDevice[key], claim)
	}
	var wg sync.WaitGroup
	slots := make(chan struct{}, parallelSends)
	for _, key := range order {
		slots <- struct{}{}
		wg.Add(1)
		go func() {
			defer func() { <-slots; wg.Done() }()
			for _, claim := range byDevice[key] {
				if ctx.Err() != nil {
					return
				}
				w.deliver(ctx, claim)
			}
		}()
	}
	wg.Wait()
	return len(claims), nil
}

var (
	errDeviceBusy = errors.New("device is held by another send")
	errLeaseShort = errors.New("claim too close to expiry to send")
)

// deliver checks, sends and records one claimed row in a single transaction.
// The device row is locked first and the check runs after the lock, the send
// happens before the commit, so a device cannot change hands between the
// ownership check and the send, and the outcome below is only ever written for
// the owner the check saw. Every statement carries the claim token: a claimant
// whose lease expired while it queued reads nothing, sends nothing and writes
// nothing. The whole attempt is bounded by deliveryTimeout.
//
// Delivery is at-least-once. If the commit fails after a send, or the process
// dies between them, the row is claimed again after the lease and sent again;
// nothing here claims exactly-once across that boundary.
func (w *Worker) deliver(ctx context.Context, claim db.ClaimPushDeliveriesRow) {
	attemptCtx, cancel := context.WithTimeout(ctx, deliveryTimeout)
	defer cancel()
	id := util.UUIDToString(claim.ID)
	tx, err := w.tx.Begin(attemptCtx)
	if err != nil {
		slog.Warn("push delivery transaction failed", "delivery_id", id, "error", err)
		return
	}
	// Rolled back on every path that does not commit, even after the attempt expired.
	defer RollbackBounded(ctx, tx)
	if _, err := tx.Exec(attemptCtx, fmt.Sprintf("SET LOCAL lock_timeout = '%dms'", deviceLockWait.Milliseconds())); err != nil {
		slog.Warn("push delivery lock timeout setup failed", "delivery_id", id, "error", err)
		return
	}
	err = w.deliverLocked(attemptCtx, ctx, w.q.WithTx(tx), claim)
	switch {
	case err == nil:
		if err := tx.Commit(attemptCtx); err != nil {
			slog.Warn("push delivery commit failed", "delivery_id", id, "error", err)
		}
	case errors.Is(err, errDeviceBusy):
		// The aborted transaction is rolled back first; the deferral is its own
		// statement, with its own deadline because the attempt's may be spent.
		RollbackBounded(ctx, tx)
		retryCtx, stop := context.WithTimeout(context.WithoutCancel(ctx), TeardownBudget)
		defer stop()
		_, rerr := w.q.RetryPushDelivery(retryCtx, db.RetryPushDeliveryParams{
			ID: claim.ID, ClaimToken: claim.ClaimToken, ClaimedAt: claim.ClaimedAt, Refund: 1, BackoffSecs: contestedBackoff.Seconds(),
		})
		if rerr != nil {
			slog.Warn("push delivery deferral failed", "delivery_id", id, "error", rerr)
		}
	case errors.Is(err, errLeaseShort):
		slog.Info("push delivery left for reclaim: lease too short to send", "delivery_id", id)
	default:
		// Rolled back: the row keeps its claim and is retried when the lease runs out.
		slog.Warn("push delivery failed", "delivery_id", id, "error", err)
	}
}

// RollbackBounded rolls tx back under its own TeardownBudget, whatever state
// ctx is in.
func RollbackBounded(ctx context.Context, tx pgx.Tx) {
	rollbackCtx, stop := context.WithTimeout(context.WithoutCancel(ctx), TeardownBudget)
	defer stop()
	_ = tx.Rollback(rollbackCtx)
}

func isLockTimeout(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "55P03"
}

// deliverLocked runs inside the transaction. attemptCtx bounds the attempt;
// shutdownCtx is the worker's own, so a stop is told apart from a timeout.
func (w *Worker) deliverLocked(attemptCtx, shutdownCtx context.Context, q *db.Queries, claim db.ClaimPushDeliveriesRow) error {
	drop := func() error {
		_, err := q.DropPushDelivery(attemptCtx, db.DropPushDeliveryParams{ID: claim.ID, ClaimToken: claim.ClaimToken, ClaimedAt: claim.ClaimedAt})
		return err
	}
	device, err := q.LockPushSubscription(attemptCtx, claim.SubscriptionID)
	if err == nil {
		// Shared session lock, held to the end of the transaction: logout takes the
		// exclusive one after it commits the session's end, so by the time logout
		// answers, no send admitted before the marker is left to start.
		err = q.LockPushSessionShared(attemptCtx, device.SessionID)
	}
	if err != nil {
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return drop() // the device is gone
		case isLockTimeout(err):
			return errDeviceBusy
		default:
			return err
		}
	}
	row, err := q.GetPushDeliveryContext(attemptCtx, db.GetPushDeliveryContextParams{ID: claim.ID, ClaimToken: claim.ClaimToken, MinLeaseSecs: deliveryTimeout.Seconds()})
	if errors.Is(err, pgx.ErrNoRows) {
		return drop() // no longer owed, or no longer our claim (the drop is fenced)
	}
	if err != nil {
		return err
	}
	if !row.LeaseOk {
		return errLeaseShort
	}
	body, err := json.Marshal(buildPayload(row))
	if err != nil {
		return drop()
	}
	sendCtx, cancel := context.WithTimeout(attemptCtx, sendTimeout)
	status, sendErr := w.sender.Send(sendCtx, Subscription{Endpoint: row.Endpoint, P256dh: row.P256dh, Auth: row.Auth}, body)
	cancel()
	if shutdownCtx.Err() != nil {
		return shutdownCtx.Err() // shutting down; the lease expires and the row is retried
	}
	switch {
	case sendErr == nil && status >= 200 && status < 300:
		n, err := q.FinishPushDelivery(attemptCtx, db.FinishPushDeliveryParams{ID: claim.ID, ClaimToken: claim.ClaimToken, ClaimedAt: claim.ClaimedAt})
		if err != nil || n == 0 {
			return err
		}
		return q.TouchPushSubscription(attemptCtx, row.SubscriptionID)
	case sendErr == nil && (status == 404 || status == 410):
		return q.PrunePushSubscription(attemptCtx, db.PrunePushSubscriptionParams{DeliveryID: claim.ID, ClaimToken: claim.ClaimToken, ID: row.SubscriptionID})
	case claim.Attempts >= MaxAttempts:
		slog.Warn("push delivery given up", "delivery_id", util.UUIDToString(claim.ID), "status", status, "error", sendErr)
		return drop()
	default:
		slog.Info("push delivery will retry", "delivery_id", util.UUIDToString(claim.ID), "attempt", claim.Attempts, "status", status, "error", sendErr)
		_, err := q.RetryPushDelivery(attemptCtx, db.RetryPushDeliveryParams{
			ID: claim.ID, ClaimToken: claim.ClaimToken, ClaimedAt: claim.ClaimedAt, BackoffSecs: backoff(claim.Attempts).Seconds(),
		})
		return err
	}
}

func backoff(attempt int32) time.Duration {
	d := retryBase << max(attempt-1, 0)
	if d > retryCap || d <= 0 {
		return retryCap
	}
	return d
}

// payload is everything a notification may say. It never carries message text,
// an issue body, a decision ask or a DM title (derived from the first message).
type payload struct {
	RecordID      string `json:"record_id"`
	WorkspaceSlug string `json:"workspace_slug"`
	Kind          string `json:"kind"`
	ActorName     string `json:"actor_name"`
	Label         string `json:"label"`
	ReplyCount    int    `json:"reply_count"`
	URL           string `json:"url"`
}

func buildPayload(row db.GetPushDeliveryContextRow) payload {
	p := payload{RecordID: util.UUIDToString(row.RecordID), WorkspaceSlug: row.WorkspaceSlug, Kind: row.Type, ActorName: row.ActorName}
	var details struct {
		ReplyCount int `json:"reply_count"`
	}
	_ = json.Unmarshal(row.Details, &details)
	p.ReplyCount = details.ReplyCount
	base := "/" + row.WorkspaceSlug
	switch {
	case row.Type == "dm_reply":
		p.Label, p.URL = "New reply", dmURL(base, row.ChatSessionID)
	case row.ChatSessionID.Valid:
		p.Kind, p.Label, p.URL = "decision_request", "Decision needed", dmURL(base, row.ChatSessionID)
	case row.IssueKey != "":
		p.Label, p.URL = fmt.Sprintf("%s %s", row.IssueKey, row.Title), base+"/issues/"+row.IssueKey
	default:
		// Other titles can echo user input; only issue titles are sent.
		p.Label, p.URL = "Notification", base+"/inbox"
	}
	return p
}

func dmURL(base string, session pgtype.UUID) string {
	return base + "/chat?session=" + util.UUIDToString(session)
}
