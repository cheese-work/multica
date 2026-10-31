package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/webhookguard"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// webhookNonceSweepInterval paces nonce cleanup. Rows only need to outlive
// webhookguard.NonceRetention; an hourly delete keeps the table small.
const webhookNonceSweepInterval = time.Hour

// sweepWebhookNonces deletes the stored nonces of inbound workflow webhooks
// that are older than the replay window, and reports how many it removed.
func sweepWebhookNonces(ctx context.Context, q *db.Queries) (int64, error) {
	cutoff := time.Now().Add(-webhookguard.NonceRetention)
	return q.DeleteExpiredWorkflowWebhookNonces(ctx, pgtype.Timestamptz{Time: cutoff, Valid: true})
}

func runWebhookNonceSweeper(ctx context.Context, q *db.Queries) {
	ticker := time.NewTicker(webhookNonceSweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if removed, err := sweepWebhookNonces(ctx, q); err != nil {
				slog.Warn("workflow webhook nonce cleanup failed", "error", err)
			} else if removed > 0 {
				slog.Info("workflow webhook nonce cleanup completed", "count", removed)
			}
		}
	}
}
