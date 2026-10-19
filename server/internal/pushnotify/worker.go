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
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const (
	// MaxAttempts counts claims, so a holder that died mid-send used one up.
	MaxAttempts   = 6
	SentRetention = time.Hour

	workerTick = 2 * time.Second
	batchSize  = 32
	// Each in-flight send holds a transaction, so this is also its share of the pool.
	parallelSends = 4
	// Long enough for a full batch of slow sends (batchSize/parallelSends x sendTimeout).
	leaseDuration = 3 * time.Minute
	sendTimeout   = 15 * time.Second
	retryBase     = 30 * time.Second
	retryCap      = time.Hour
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
		if err := w.ProcessOnce(ctx); err != nil && ctx.Err() == nil {
			slog.Warn("push worker pass failed", "error", err)
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
	if err := w.q.DeleteSentPushDeliveries(ctx, SentRetention.Seconds()); err != nil {
		return err
	}
	claims, err := w.q.ClaimPushDeliveries(ctx, db.ClaimPushDeliveriesParams{Batch: batchSize, LeaseSecs: leaseDuration.Seconds()})
	if err != nil {
		return err
	}
	var wg sync.WaitGroup
	slots := make(chan struct{}, parallelSends)
	for _, claim := range claims {
		slots <- struct{}{}
		wg.Add(1)
		go func() {
			defer func() { <-slots; wg.Done() }()
			w.deliver(ctx, claim)
		}()
	}
	wg.Wait()
	return nil
}

// deliver checks, sends and records one claimed row in a single transaction.
// The check locks the device row (see GetPushDeliveryContext) and the send
// happens before the commit, so a device cannot change hands between the
// ownership check and the send, and the outcome below is only ever written for
// the owner the check saw. ponytail: a registration or logout for a device with
// a send in flight waits for that send, at most sendTimeout; per-device locks
// with a shorter wait if that stall ever matters.
func (w *Worker) deliver(ctx context.Context, claim db.ClaimPushDeliveriesRow) {
	tx, err := w.tx.Begin(ctx)
	if err != nil {
		slog.Warn("push delivery transaction failed", "delivery_id", util.UUIDToString(claim.ID), "error", err)
		return
	}
	defer tx.Rollback(ctx)
	if err := w.deliverLocked(ctx, w.q.WithTx(tx), claim); err != nil {
		// Rolled back: the row keeps its claim and is retried when the lease runs out.
		slog.Warn("push delivery failed", "delivery_id", util.UUIDToString(claim.ID), "error", err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		slog.Warn("push delivery commit failed", "delivery_id", util.UUIDToString(claim.ID), "error", err)
	}
}

func (w *Worker) deliverLocked(ctx context.Context, q *db.Queries, claim db.ClaimPushDeliveriesRow) error {
	drop := func() error {
		return q.DropPushDelivery(ctx, db.DropPushDeliveryParams{ID: claim.ID, ClaimedAt: claim.ClaimedAt})
	}
	row, err := q.GetPushDeliveryContext(ctx, claim.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		return drop()
	}
	if err != nil {
		return err
	}
	body, err := json.Marshal(buildPayload(row))
	if err != nil {
		return drop()
	}
	sendCtx, cancel := context.WithTimeout(ctx, sendTimeout)
	status, sendErr := w.sender.Send(sendCtx, Subscription{Endpoint: row.Endpoint, P256dh: row.P256dh, Auth: row.Auth}, body)
	cancel()
	if ctx.Err() != nil {
		return ctx.Err() // shutting down; the lease expires and the row is retried
	}
	switch {
	case sendErr == nil && status >= 200 && status < 300:
		if err := q.FinishPushDelivery(ctx, db.FinishPushDeliveryParams{ID: claim.ID, ClaimedAt: claim.ClaimedAt}); err != nil {
			return err
		}
		return q.TouchPushSubscription(ctx, row.SubscriptionID)
	case sendErr == nil && (status == 404 || status == 410):
		return q.DeletePushSubscription(ctx, row.SubscriptionID)
	case claim.Attempts >= MaxAttempts:
		slog.Warn("push delivery given up", "delivery_id", util.UUIDToString(claim.ID), "status", status, "error", sendErr)
		return drop()
	default:
		slog.Info("push delivery will retry", "delivery_id", util.UUIDToString(claim.ID), "attempt", claim.Attempts, "status", status, "error", sendErr)
		return q.RetryPushDelivery(ctx, db.RetryPushDeliveryParams{ID: claim.ID, ClaimedAt: claim.ClaimedAt, BackoffSecs: backoff(claim.Attempts).Seconds()})
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
