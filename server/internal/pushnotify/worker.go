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

	workerTick    = 2 * time.Second
	batchSize     = 32
	parallelSends = 8
	// Long enough for a full batch of slow sends (batchSize/parallelSends x sendTimeout).
	leaseDuration = 2 * time.Minute
	sendTimeout   = 15 * time.Second
	retryBase     = 30 * time.Second
	retryCap      = time.Hour
)

// Worker sends due outbox rows. Every pass re-checks that the push is still
// owed (GetPushDeliveryContext): iOS requires each delivered push to show a
// notification, so a record read in the meantime must never be sent.
type Worker struct {
	q      *db.Queries
	sender Sender
	done   chan struct{}
}

func NewWorker(q *db.Queries, sender Sender) *Worker {
	return &Worker{q: q, sender: sender, done: make(chan struct{})}
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

func (w *Worker) deliver(ctx context.Context, claim db.ClaimPushDeliveriesRow) {
	row, err := w.q.GetPushDeliveryContext(ctx, claim.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		w.log(w.q.DropPushDelivery(ctx, db.DropPushDeliveryParams{ID: claim.ID, ClaimedAt: claim.ClaimedAt}))
		return
	}
	if err != nil {
		// Leave the lease to run out; the row is claimed again afterwards.
		slog.Warn("push delivery context failed", "delivery_id", util.UUIDToString(claim.ID), "error", err)
		return
	}
	body, err := json.Marshal(buildPayload(row))
	if err != nil {
		w.log(w.q.DropPushDelivery(ctx, db.DropPushDeliveryParams{ID: claim.ID, ClaimedAt: claim.ClaimedAt}))
		return
	}
	sendCtx, cancel := context.WithTimeout(ctx, sendTimeout)
	status, err := w.sender.Send(sendCtx, Subscription{Endpoint: row.Endpoint, P256dh: row.P256dh, Auth: row.Auth}, body)
	cancel()
	if ctx.Err() != nil {
		return // shutting down; the lease expires and the row is retried
	}
	switch {
	case err == nil && status >= 200 && status < 300:
		w.log(w.q.FinishPushDelivery(ctx, db.FinishPushDeliveryParams{ID: claim.ID, ClaimedAt: claim.ClaimedAt}))
		w.log(w.q.TouchPushSubscription(ctx, row.SubscriptionID))
	case err == nil && (status == 404 || status == 410):
		w.log(w.q.DeletePushSubscription(ctx, row.SubscriptionID))
	case claim.Attempts >= MaxAttempts:
		slog.Warn("push delivery given up", "delivery_id", util.UUIDToString(claim.ID), "status", status, "error", err)
		w.log(w.q.DropPushDelivery(ctx, db.DropPushDeliveryParams{ID: claim.ID, ClaimedAt: claim.ClaimedAt}))
	default:
		slog.Info("push delivery will retry", "delivery_id", util.UUIDToString(claim.ID), "attempt", claim.Attempts, "status", status, "error", err)
		w.log(w.q.RetryPushDelivery(ctx, db.RetryPushDeliveryParams{ID: claim.ID, ClaimedAt: claim.ClaimedAt, BackoffSecs: backoff(claim.Attempts).Seconds()}))
	}
}

func (w *Worker) log(err error) {
	if err != nil {
		slog.Warn("push worker write failed", "error", err)
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
